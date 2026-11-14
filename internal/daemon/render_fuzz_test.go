package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func FuzzRenderSystemd(f *testing.F)  { fuzzRender(f, "systemd") }
func FuzzRenderLaunchd(f *testing.F)  { fuzzRender(f, "launchd") }
func FuzzRenderOpenRC(f *testing.F)   { fuzzRender(f, "openrc") }
func FuzzRenderSysvinit(f *testing.F) { fuzzRender(f, "sysvinit") }
func FuzzRenderBusybox(f *testing.F)  { fuzzRender(f, "busybox") }
func FuzzRenderRunit(f *testing.F)    { fuzzRender(f, "runit") }
func FuzzRenderS6(f *testing.F)       { fuzzRender(f, "s6") }
func FuzzRenderDinit(f *testing.F)    { fuzzRender(f, "dinit") }
func FuzzRenderFinit(f *testing.F)    { fuzzRender(f, "finit") }
func FuzzRenderProcd(f *testing.F)    { fuzzRender(f, "procd") }
func FuzzRenderCrontab(f *testing.F)  { fuzzRender(f, "crontab") }
func FuzzRenderWindows(f *testing.F)  { fuzzRender(f, "windows") }

func fuzzRender(f *testing.F, backend string) {
	f.Helper()
	f.Fuzz(func(t *testing.T, arg, value, path string) {
		if len(arg)+len(value)+len(path) > 4096 {
			t.Skip("bounded process argument payload")
		}
		cfg := ServiceConfig{Name: "render-probe", ExecPath: "/probe " + path, WorkingDir: "/work " + path, Description: "probe", Arguments: []string{arg, "", "tail"}, Environment: map[string]string{"PROBE_VALUE": value, "SECOND_VALUE": "second"}}
		if backend == "finit" {
			cfg.ExecPath = "/probe"
			cfg.WorkingDir = "/work" + path
		}
		if backend == "windows" {
			cfg.Environment = nil
		}
		checkRenderMetadata(t, backend, cfg, path)
		rendered, err := renderBackend(backend, cfg)
		if err != nil {
			return
		}
		if strings.ContainsRune(arg+path, 0) || backend != "windows" && strings.ContainsRune(value, 0) {
			t.Fatal("NUL accepted by renderer")
		}
		switch backend {
		case "systemd":
			checkSystemd(t, cfg, rendered)
		case "launchd":
			checkPlist(t, cfg, rendered)
		case "dinit", "finit":
			checkInitWords(t, backend, cfg, rendered)
		case "windows":
			def, err := parseWindowsServiceConfig(rendered)
			if err != nil || def.ImagePath != cfg.ExecPath || !slices.Equal(def.Arguments, cfg.Arguments) || !slices.Equal(parseArgvW(def.BinaryPath), append([]string{cfg.ExecPath}, cfg.Arguments...)) {
				t.Fatalf("Windows round trip: %+v, %v", def, err)
			}
		default:
			checkShell(t, backend, cfg)
		}
	})
}

func renderBackend(backend string, cfg ServiceConfig) (string, error) {
	switch backend {
	case "systemd":
		return GenerateUnitFile(cfg)
	case "launchd":
		return GeneratePlistFile(cfg)
	case "openrc":
		return GenerateOpenRCInitScript(cfg)
	case "openrc-supervised":
		return generateOpenRCScript(cfg, true)
	case "sysvinit":
		return GenerateSysvinitInitScript(cfg)
	case "busybox":
		return GenerateBusyboxInitScript(cfg, cfg.ExecPath)
	case "runit":
		return GenerateRunitRunScript(cfg, cfg.ExecPath)
	case "s6":
		return GenerateS6RunScript(cfg, cfg.ExecPath)
	case "dinit":
		return GenerateDinitServiceFile(cfg, cfg.ExecPath)
	case "finit":
		return GenerateFinitServiceFile(cfg, cfg.ExecPath)
	case "procd":
		return GenerateProcdInitScript(cfg, cfg.ExecPath)
	case "crontab":
		return newCrontabManager().GenerateConfig(cfg)
	case "windows":
		return GenerateWindowsServiceConfig(cfg, cfg.ExecPath)
	default:
		panic(backend)
	}
}

type renderObservation struct {
	Args      [][]byte `json:"args"`
	Value     []byte   `json:"value"`
	Second    []byte   `json:"second"`
	Directory []byte   `json:"directory"`
}

func TestRenderProbe(t *testing.T) {
	if !testsupport.RenderProbe() {
		return
	}
	index := slices.Index(os.Args, "--")
	if index < 0 {
		t.Fatal("missing argument separator")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	value, second := testsupport.RenderValues()
	observation := renderObservation{Value: []byte(value), Second: []byte(second), Directory: []byte(wd)}
	for _, arg := range os.Args[index+1:] {
		observation.Args = append(observation.Args, []byte(arg))
	}
	data, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(testsupport.RenderOutput(), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkShell(t *testing.T, backend string, cfg ServiceConfig) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("real /bin/sh unavailable")
	}
	dir := t.TempDir()
	leaf := strings.ReplaceAll(strings.TrimPrefix(cfg.ExecPath, "/probe "), "/", "_")
	if len(leaf) > 64 {
		leaf = leaf[:64]
	}
	cfg.ExecPath = filepath.Join(dir, "probe '$ "+leaf)
	cfg.WorkingDir = filepath.Join(dir, "work '$ "+leaf)
	if backend == "finit" {
		cfg.ExecPath = filepath.Join(dir, "probe")
		cfg.WorkingDir = filepath.Join(dir, "work space")
	}
	cfg.LogDir = dir
	if err := os.Mkdir(cfg.WorkingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(testsupport.Executable(), cfg.ExecPath); err != nil {
		t.Fatal(err)
	}
	args := slices.Clone(cfg.Arguments)
	cfg.Arguments = append([]string{"-test.run=^TestRenderProbe$", "--"}, cfg.Arguments...)
	script, err := renderBackend(backend, cfg)
	if err != nil {
		t.Fatal(err)
	}
	switch backend {
	case "sysvinit", "busybox":
		script, _, _ = strings.Cut(script, "case \"$1\" in")
		script = strings.ReplaceAll(script, "/var/run/", dir+"/")
		if cfg.User != "" {
			script = strings.Replace(script, "su -s /bin/sh", "probe_su -s /bin/sh", 1)
			script += "\nprobe_su() { /bin/sh -c \"$5; wait\"; : > \"$STOPFILE\"; }\n"
		}
		script += "\nsupervise\n"
	case "openrc", "openrc-supervised":
		script = strings.ReplaceAll(script, "supervise-daemon ", "probe_start ")
		script = strings.ReplaceAll(script, "start-stop-daemon --start", "probe_start --start")
		script += "\nprobe_start() { while [ \"$1\" != -- ]; do shift; done; shift; cd \"$directory\" || exit; \"$command\" \"$@\"; }\nstart\n"
	case "procd":
		script = strings.Replace(script, "procd_set_param command ", "probe_command() { ", 1)
		script = strings.Replace(script, "\n\tprocd_set_param respawn", "; }\n\tprocd_set_param respawn", 1)
		script += "\nprocd_open_instance() { :; }\nprocd_set_param() { key=$1; shift; case $key in dir) cd \"$1\" || exit;; env) unset PROBE_VALUE SECOND_VALUE; export \"$@\";; esac; }\nprocd_close_instance() { probe_command; }\nstart_service\n"
	case "finit":
		words, err := parseFinitCommand(script)
		if err != nil {
			t.Fatal(err)
		}
		// The wrapper sources the real environment fragment; point it at a copy.
		envFile := filepath.Join(dir, "finit.env")
		if err := os.WriteFile(envFile, []byte(formatFinitEnvironment(cfg.Environment)), 0o600); err != nil {
			t.Fatal(err)
		}
		for i := range words {
			words[i] = shellQuote(strings.ReplaceAll(words[i], finitEnvFile(cfg), envFile))
		}
		script = "exec " + strings.Join(words, " ") + "\n"
	case "crontab":
		lines := strings.Split(script, "\n")
		script = strings.TrimPrefix(lines[1], "@reboot ") + "\n"
	}
	file := filepath.Join(dir, "render.sh")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), testsupport.Scale(5*time.Second))
	defer cancel()
	if out, err := exec.CommandContext(ctx, "/bin/sh", "-n", file).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s\n%s", err, out, script)
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", file)
	output := filepath.Join(dir, "observation")
	cmd.Env = append(os.Environ(), testsupport.RenderProbeKey+"=1", testsupport.RenderOutputKey+"="+output)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shell execution: %v\n%s\n%s", err, out, script)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, script)
	}
	var got renderObservation
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := make([][]byte, 0, len(args))
	for _, arg := range args {
		want = append(want, []byte(arg))
	}
	if !bytes.Equal(got.Directory, []byte(cfg.WorkingDir)) || !reflect.DeepEqual(got.Args, want) || !bytes.Equal(got.Value, []byte(cfg.Environment["PROBE_VALUE"])) || !bytes.Equal(got.Second, []byte(cfg.Environment["SECOND_VALUE"])) {
		t.Fatalf("%s changed argv/environment: got %+v want args %q env %q", backend, got, args, cfg.Environment)
	}
}

type plistNode struct {
	XMLName  xml.Name    `xml:""`
	Text     string      `xml:",chardata"`
	Children []plistNode `xml:",any"`
}

func plistDictionary(t *testing.T, node plistNode) map[string]plistNode {
	t.Helper()
	result := make(map[string]plistNode)
	if len(node.Children)%2 != 0 {
		t.Fatal("unpaired plist dictionary")
	}
	for i := 0; i < len(node.Children); i += 2 {
		if node.Children[i].XMLName.Local != "key" {
			t.Fatal("non-key in plist dictionary")
		}
		result[node.Children[i].Text] = node.Children[i+1]
	}
	return result
}

func checkPlist(t *testing.T, cfg ServiceConfig, rendered string) {
	t.Helper()
	var doc plistNode
	if err := xml.Unmarshal([]byte(rendered), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.XMLName.Local != "plist" || len(doc.Children) != 1 || doc.Children[0].XMLName.Local != "dict" {
		t.Fatal("invalid plist root")
	}
	values := plistDictionary(t, doc.Children[0])
	args := make([]string, 0, len(values["ProgramArguments"].Children))
	for _, arg := range values["ProgramArguments"].Children {
		if arg.XMLName.Local != "string" {
			t.Fatal("non-string plist argument")
		}
		args = append(args, arg.Text)
	}
	if !slices.Equal(args, append([]string{cfg.ExecPath}, cfg.Arguments...)) {
		t.Fatalf("plist argv: %q", args)
	}
	for key, want := range map[string]string{"WorkingDirectory": cfg.WorkingDir, "UserName": cfg.User, "GroupName": cfg.Group} {
		if values[key].Text != want {
			t.Fatalf("plist changed %s: %q != %q", key, values[key].Text, want)
		}
	}
	logDir := cfg.LogDir
	if logDir == "" {
		logDir = filepath.Join("/var/log", cfg.Name)
	}
	if values["StandardOutPath"].Text != filepath.Join(logDir, cfg.Name)+".out.log" || values["StandardErrorPath"].Text != filepath.Join(logDir, cfg.Name)+".err.log" {
		t.Fatal("plist changed log paths")
	}
	env := plistDictionary(t, values["EnvironmentVariables"])
	if len(env) != len(cfg.Environment) {
		t.Fatal("plist lost environment entries")
	}
	for key, want := range cfg.Environment {
		if env[key].Text != want {
			t.Fatalf("plist changed environment %s", key)
		}
	}
}

// referenceWords implements dinit-service(5) quotes/backslashes, with the
// systemd.syntax(7) C escapes enabled only for systemd. It does not call renderers.
func referenceWords(input string, cEscapes bool) ([]string, error) {
	var result []string
	var word strings.Builder
	quoted, started := false, false
	for i := 0; i < len(input); i++ {
		c := input[i]
		switch {
		case c == '"':
			quoted = !quoted
			started = true
		case c == '\\':
			i++
			if i == len(input) {
				return nil, errors.New("trailing escape")
			}
			started = true
			if cEscapes {
				switch input[i] {
				case 'a':
					word.WriteByte('\a')
				case 'b':
					word.WriteByte('\b')
				case 'f':
					word.WriteByte('\f')
				case 'v':
					word.WriteByte('\v')
				case 'n':
					word.WriteByte('\n')
				case 'r':
					word.WriteByte('\r')
				case 't':
					word.WriteByte('\t')
				case 's':
					word.WriteByte(' ')
				case 'u', 'U':
					width := 4
					if input[i] == 'U' {
						width = 8
					}
					if i+width >= len(input) {
						return nil, errors.New("short Unicode escape")
					}
					n, err := strconv.ParseUint(input[i+1:i+1+width], 16, 32)
					if err != nil {
						return nil, err
					}
					word.WriteRune(rune(n))
					i += width
				case 'x':
					if i+2 >= len(input) {
						return nil, errors.New("short hex escape")
					}
					n, err := strconv.ParseUint(input[i+1:i+3], 16, 8)
					if err != nil {
						return nil, err
					}
					word.WriteByte(byte(n))
					i += 2
				case '\\', '"', '\'':
					word.WriteByte(input[i])
				default:
					return nil, fmt.Errorf("invalid C escape %q", input[i])
				}
			} else {
				word.WriteByte(input[i])
			}
		case !quoted && strings.ContainsRune(" \t\r\v\f", rune(c)):
			if started {
				result = append(result, word.String())
				word.Reset()
				started = false
			}
		case !quoted && c == '#' && !cEscapes:
			i = len(input)
		default:
			word.WriteByte(c)
			started = true
		}
	}
	if quoted {
		return nil, errors.New("unclosed quote")
	}
	if started {
		result = append(result, word.String())
	}
	return result, nil
}

func unescapeExpansion(t *testing.T, s string, marker byte) string {
	t.Helper()
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		out.WriteByte(s[i])
		if s[i] == marker {
			i++
			if i == len(s) || s[i] != marker {
				t.Fatalf("unescaped %c expansion in %q", marker, s)
			}
		}
	}
	return out.String()
}

func checkSystemd(t *testing.T, cfg ServiceConfig, rendered string) {
	t.Helper()
	verifySystemdUnit(t, cfg)
	for line := range strings.SplitSeq(rendered, "\n") {
		if value, ok := strings.CutPrefix(line, "ExecStart="); ok {
			words, err := referenceWords(value, true)
			if err != nil {
				t.Fatal(err)
			}
			for i := range words {
				words[i] = unescapeExpansion(t, unescapeExpansion(t, words[i], '%'), '$')
			}
			if !slices.Equal(words, append([]string{cfg.ExecPath}, cfg.Arguments...)) {
				t.Fatalf("systemd argv: %q want %q", words, cfg.Arguments)
			}
		}
		if value, ok := strings.CutPrefix(line, "Environment="); ok {
			words, err := referenceWords(value, true)
			if err != nil || len(words) != 1 {
				t.Fatalf("environment: %q %v", words, err)
			}
			key, val, _ := strings.Cut(unescapeExpansion(t, words[0], '%'), "=")
			if val != cfg.Environment[key] {
				t.Fatalf("systemd env %q != %q", val, cfg.Environment[key])
			}
		}
	}
}

func checkInitWords(t *testing.T, backend string, cfg ServiceConfig, rendered string) {
	t.Helper()
	if backend == "finit" {
		checkFinit(t, cfg, rendered)
		checkShell(t, backend, cfg)
		return
	}
	for line := range strings.SplitSeq(rendered, "\n") {
		if value, ok := strings.CutPrefix(line, "command = "); ok {
			words, err := referenceWords(value, false)
			if err != nil {
				t.Fatal(err)
			}
			for i := range words {
				words[i] = unescapeExpansion(t, words[i], '$')
			}
			if !slices.Equal(words, append([]string{cfg.ExecPath}, cfg.Arguments...)) {
				t.Fatalf("dinit argv: %q want %q", words, cfg.Arguments)
			}
		}
		if value, ok := strings.CutPrefix(line, "working-dir = "); ok {
			words, err := referenceWords(value, false)
			if err != nil || len(words) != 1 || unescapeExpansion(t, words[0], '$') != cfg.WorkingDir {
				t.Fatalf("dinit directory: %q %v", words, err)
			}
		}
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(formatLineEnvironment(cfg.Environment), "\n"), "\n") {
		key, value, _ := strings.Cut(line, "=")
		if cfg.Environment[key] != value {
			t.Fatal("dinit environment changed")
		}
	}
}

func checkFinit(t *testing.T, cfg ServiceConfig, rendered string) {
	t.Helper()
	words, err := parseFinitCommand(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, " env:") {
		t.Fatalf("finit env: files expand and execute values: %q", rendered)
	}
	if len(cfg.Environment) > 0 || cfg.WorkingDir != "" {
		if len(words) < 4 || words[0] != "/bin/sh" || words[1] != "-c" || words[3] != "daemonpilot" {
			t.Fatalf("missing Finit wrapper: %q", words)
		}
		words = words[4:]
		if cfg.WorkingDir != "" {
			if words[0] != cfg.WorkingDir {
				t.Fatalf("finit directory: %q", words[0])
			}
			words = words[1:]
		}
	}
	if !slices.Equal(words, append([]string{cfg.ExecPath}, cfg.Arguments...)) {
		t.Fatalf("finit argv: %q", words)
	}
}

func TestSystemdAnalyzeRender(t *testing.T) {
	verifySystemdUnit(t, ServiceConfig{Name: "render", ExecPath: "/bin/true", WorkingDir: "/", Arguments: []string{"", "%n", "$HOME", "quotes\" and spaces", "line\nfeed"}, Environment: map[string]string{"VALUE": "%n $HOME"}})
}

func verifySystemdUnit(t *testing.T, cfg ServiceConfig) {
	t.Helper()
	tool, err := exec.LookPath("systemd-analyze")
	if err != nil {
		return
	}
	cfg.ExecPath = "/bin/true"
	cfg.WorkingDir = "/"
	rendered, err := GenerateUnitFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "render.service")
	if err := os.WriteFile(file, []byte(rendered), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), testsupport.Scale(5*time.Second))
	defer cancel()
	out, err := exec.CommandContext(ctx, tool, "verify", file).CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-analyze: %v\n%s\n%s", err, out, rendered)
	}
}

// Finit applies wordexp separately to each reconstructed command argument and
// each environment value. This models the renderer's double-quoted subset.
func referenceFinitWords(input string) ([]string, error) {
	for i := 0; i < len(input); i++ {
		if input[i] == '\\' {
			i++
			continue
		}
		if input[i] == '$' || input[i] == '`' {
			return nil, errors.New("unescaped Finit substitution")
		}
	}
	return referenceWords(input, false)
}

func checkRenderMetadata(t *testing.T, backend string, base ServiceConfig, value string) {
	t.Helper()
	var scripts strings.Builder
	for _, field := range []string{"name", "description", "user", "group", "log_dir", "environment_key"} {
		cfg := base
		switch field {
		case "name":
			cfg.Name = value
		case "description":
			cfg.Description = value
		case "user":
			cfg.User = value
		case "group":
			cfg.Group = value
		case "log_dir":
			cfg.LogDir = value
		case "environment_key":
			cfg.Environment = map[string]string{value: "payload"}
		}
		rendered, err := renderBackend(backend, cfg)
		if err != nil {
			continue
		}
		if strings.ContainsRune(value, 0) {
			t.Fatalf("%s accepted NUL in %s", backend, field)
		}
		switch backend {
		case "openrc", "sysvinit", "busybox", "runit", "s6", "procd":
			scripts.WriteString(rendered)
			scripts.WriteByte('\n')
		}
		if backend == "launchd" {
			checkPlist(t, cfg, rendered)
		}
		if backend == "windows" {
			got, err := parseWindowsServiceConfig(rendered)
			if err != nil || got.Name != cfg.Name || got.Description != cfg.Description {
				t.Fatalf("Windows metadata changed: %+v (%v)", got, err)
			}
		}
		if backend == "systemd" {
			for key, want := range map[string]string{"Description": cfg.Description, "WorkingDirectory": cfg.WorkingDir, "User": cfg.User, "Group": cfg.Group} {
				if want == "" {
					continue
				}
				found := false
				for line := range strings.SplitSeq(rendered, "\n") {
					if got, ok := strings.CutPrefix(line, key+"="); ok {
						found = true
						if unescapeExpansion(t, got, '%') != want {
							t.Fatalf("systemd changed %s", key)
						}
					}
				}
				if !found {
					t.Fatalf("systemd lost %s", key)
				}
			}
		}
	}
	if scripts.Len() > 0 {
		if _, err := os.Stat("/bin/sh"); err != nil {
			t.Skip("real /bin/sh unavailable")
		}
		ctx, cancel := context.WithTimeout(t.Context(), testsupport.Scale(5*time.Second))
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-n")
		cmd.Stdin = strings.NewReader(scripts.String())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("metadata shell syntax: %v\n%s\n%s", err, out, scripts.String())
		}
	}
}

// Model service_register and parse_cmdline_args in Finit's src/service.c:
// the description is split before quoting, then strtok_r splits arguments on
// spaces and reconstructs quotes using only the first/last byte of each token.
func parseFinitCommand(rendered string) ([]string, error) {
	_, line, ok := strings.Cut(rendered, "\n")
	if !ok {
		return nil, errors.New("missing Finit service stanza")
	}
	line, _, _ = strings.Cut(line, "-- ")
	line = strings.TrimSpace(line)
	tokens := strings.FieldsFunc(line, func(r rune) bool { return r == ' ' })
	if len(tokens) < 3 || tokens[0] != "service" || tokens[1] != "[234]" {
		return nil, errors.New("invalid Finit stanza")
	}
	tokens = tokens[2:]
	for len(tokens) > 0 && (strings.HasPrefix(tokens[0], "env:") || strings.HasPrefix(tokens[0], "name:")) {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return nil, errors.New("missing command")
	}
	words := []string{tokens[0]}
	var current strings.Builder
	var sep byte
	for _, token := range tokens[1:] {
		if token[0] == '\'' || token[0] == '"' {
			sep = token[0]
		} else if sep != 0 {
			current.WriteByte(' ')
		}
		current.WriteString(token)
		if sep != 0 && token[len(token)-1] != sep {
			continue
		}
		expanded, err := referenceFinitWords(current.String())
		if err != nil {
			return nil, err
		}
		words = append(words, expanded...)
		current.Reset()
		sep = 0
	}
	// Finit clears the unfinished argument when a closing quote was not found.
	return words, nil
}

func TestManifestArgumentRegressions(t *testing.T) {
	for _, backend := range []string{"systemd", "launchd", "openrc", "openrc-supervised", "sysvinit", "busybox", "runit", "s6", "dinit", "procd", "crontab", "windows"} {
		t.Run(backend, func(t *testing.T) {
			cfg := ServiceConfig{Name: "render-probe", ExecPath: "/probe space", WorkingDir: "/work space", Arguments: []string{"", "a  b", "--", "quote'\"$HOME`id`\\"}, Environment: map[string]string{"PROBE_VALUE": "value \" '$HOME`id`\\", "SECOND_VALUE": "two"}}
			if backend == "windows" {
				cfg.Environment = nil
			}
			rendered, err := renderBackend(backend, cfg)
			if err != nil {
				t.Fatal(err)
			}
			switch backend {
			case "systemd":
				checkSystemd(t, cfg, rendered)
			case "launchd":
				checkPlist(t, cfg, rendered)
			case "dinit", "finit":
				checkInitWords(t, backend, cfg, rendered)
			case "windows":
				got, err := parseWindowsServiceConfig(rendered)
				if err != nil || !slices.Equal(got.Arguments, cfg.Arguments) {
					t.Fatalf("Windows round trip: %+v (%v)", got, err)
				}
			default:
				checkShell(t, backend, cfg)
			}
		})
	}
	t.Run("sysvinit-user", func(t *testing.T) {
		cfg := ServiceConfig{Name: "render-probe", ExecPath: "/probe space", WorkingDir: "/work space", User: "render-user", Arguments: []string{"", "double\"", "$HOME", "two  spaces", "single'", "line\nfeed"}, Environment: map[string]string{"PROBE_VALUE": "value", "SECOND_VALUE": "two"}}
		checkShell(t, "sysvinit", cfg)
	})
}

func TestOpenRCStartPreservesDaemonOptions(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("real /bin/sh unavailable")
	}
	for _, supervised := range []bool{false, true} {
		t.Run(strconv.FormatBool(supervised), func(t *testing.T) {
			cfg := ServiceConfig{Name: "probe", ExecPath: "/probe two  spaces", WorkingDir: "/work dir", Arguments: []string{"arg two  spaces"}, OpenRCDirectives: map[string]string{
				"start_stop_daemon_args": "--nicelevel 10",
				"respawn_delay":          "3",
				"umask":                  "027",
			}}
			script, err := generateOpenRCScript(cfg, supervised)
			if err != nil {
				t.Fatal(err)
			}
			script = strings.ReplaceAll(script, "start-stop-daemon --start", "probe_daemon --start")
			script = strings.ReplaceAll(script, "supervise-daemon ", "probe_daemon ")
			script += "\nprobe_daemon() { printf '%s\\000' \"$@\"; }\nstart\n"
			ctx, cancel := context.WithTimeout(t.Context(), testsupport.Scale(5*time.Second))
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh")
			cmd.Stdin = strings.NewReader(script)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("start: %v: %s", err, out)
			}
			wants := []string{"--nicelevel\x0010\x00", "--umask\x00027\x00", "--chdir\x00/work dir\x00", "/probe two  spaces\x00", "arg two  spaces\x00"}
			if supervised {
				wants = append(wants, "--respawn-delay\x003\x00")
			}
			for _, want := range wants {
				if !bytes.Contains(out, []byte(want)) {
					t.Fatalf("missing option %q in %q", want, out)
				}
			}
		})
	}
}
