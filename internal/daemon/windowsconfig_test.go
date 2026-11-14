package daemon

import (
	"strings"
	"testing"
)

// TestWindowsQuoteArgRoundTripsThroughArgvRules checks the quoting against a
// reimplementation of CommandLineToArgvW's parsing rules — the thing that will
// actually read the string back. Asserting the expected OUTPUT instead would only
// pin the quoting to itself; what matters is that the process recovers its argv.
func TestWindowsQuoteArgRoundTripsThroughArgvRules(t *testing.T) {
	cases := [][]string{
		{`C:\Program Files\sample\sample-agent.exe`, `run`},
		{`C:\simple\path.exe`},
		{`C:\dir with space\`, `--flag=a b`},
		{`C:\trailing\slashes\\\`, `plain`},
		{`quote"inside`, `back\slash"and quote`},
		{`C:\x.exe`, ``, `after empty`},
		{`tab	separated`},
	}
	for _, argv := range cases {
		line := WindowsCommandLine(argv[0], argv[1:])
		got := parseArgvW(line)
		if len(got) != len(argv) {
			t.Fatalf("%q -> %q: parsed %d args %q, want %d %q", argv, line, len(got), got, len(argv), argv)
		}
		for i := range argv {
			if got[i] != argv[i] {
				t.Errorf("%q -> %q: arg %d = %q, want %q", argv, line, i, got[i], argv[i])
			}
		}
	}
}

// TestWindowsCommandLineQuotesAPathWithSpaces pins the specific failure an
// unquoted binary path produces: the loader is offered `C:\Program.exe` with the
// rest as arguments, so the service either does not start or starts something
// else. A bare-eye check of the whole string is what a reviewer can actually do.
func TestWindowsCommandLineQuotesAPathWithSpaces(t *testing.T) {
	line := WindowsCommandLine(`C:\Program Files\sample\sample-agent.exe`, []string{"run"})
	want := `"C:\Program Files\sample\sample-agent.exe" run`
	if line != want {
		t.Errorf("command line = %q, want %q", line, want)
	}
}

// TestWindowsQuoteKeepsBackslashRuns is the mistake the obvious implementation
// makes: doubling every backslash rather than only those before a quote turns
// `C:\dir\` into a different path and every file under it with it.
func TestWindowsQuoteKeepsBackslashRuns(t *testing.T) {
	line := WindowsCommandLine(`C:\no-spaces\path.exe`, nil)
	if strings.Contains(line, `\\`) {
		t.Errorf("command line = %q; backslashes were doubled in an argument that needs no quoting", line)
	}
}

func TestGenerateAndParseWindowsServiceConfig(t *testing.T) {
	cfg := ServiceConfig{
		Name:        "sample-agent",
		Description: "sample host agent",
		ExecPath:    `C:\Program Files\sample\sample-agent.exe`,
		Arguments:   []string{"run"},
	}
	content, err := GenerateWindowsServiceConfig(cfg, windowsExecPath(cfg))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	def, err := parseWindowsServiceConfig(content)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if def.Name != "sample-agent" {
		t.Errorf("Name = %q", def.Name)
	}
	if def.Description != "sample host agent" {
		t.Errorf("Description = %q", def.Description)
	}
	if def.BinaryPath != `"C:\Program Files\sample\sample-agent.exe" run` {
		t.Errorf("BinaryPath = %q", def.BinaryPath)
	}
}

// TestGenerateWindowsServiceConfigIsDeterministic: the rendering is hashed for the
// rollback digest, so an unstable one reports drift on every reconcile of a service
// nobody touched.
func TestGenerateWindowsServiceConfigIsDeterministic(t *testing.T) {
	cfg := ServiceConfig{Name: "svc", Description: "d", ExecPath: `C:\a\svc.exe`, Arguments: []string{"run", "--x=1"}}
	first, err := GenerateWindowsServiceConfig(cfg, windowsExecPath(cfg))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for range 20 {
		again, err := GenerateWindowsServiceConfig(cfg, windowsExecPath(cfg))
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if again != first {
			t.Fatalf("rendering is not stable:\n%q\n%q", first, again)
		}
	}
}

// TestWindowsRefusesWhatItCannotHonour: dropping these silently produces a service
// that installs green and runs without the setting the operator configured.
func TestWindowsRefusesWhatItCannotHonour(t *testing.T) {
	base := ServiceConfig{Name: "svc", ExecPath: `C:\a\svc.exe`}

	withEnv := base
	withEnv.Environment = map[string]string{"TOKEN": "x", "OTHER": "y"}
	_, err := GenerateWindowsServiceConfig(withEnv, windowsExecPath(withEnv))
	if err == nil {
		t.Fatal("environment was accepted; it would be silently dropped")
	}
	// The refusal has to name what it refused, or an operator cannot act on it.
	for _, want := range []string{"OTHER", "TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}

	withUser := base
	withUser.User = "someone"
	if _, err := GenerateWindowsServiceConfig(withUser, windowsExecPath(withUser)); err == nil {
		t.Error("User was accepted; a Windows service runs as LocalSystem and it would be ignored")
	}
}

// TestWindowsExecPathAppendsExe: CreateProcess does not append the extension to an
// absolute path, so an extensionless BinaryPathName is a service that can never
// start — and the SCM reports that only as a start failure with no reason.
func TestWindowsExecPathAppendsExe(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`C:\a\svc`, `C:\a\svc.exe`},
		{`C:\a\svc.exe`, `C:\a\svc.exe`},
		{`C:\a\svc.EXE`, `C:\a\svc.EXE`}, // already an executable; do not double it
	} {
		got := windowsExecPath(ServiceConfig{Name: "svc", ExecPath: tc.in})
		if got != tc.want {
			t.Errorf("windowsExecPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseWindowsServiceConfigRejectsAnIncompleteManifest(t *testing.T) {
	for _, content := range []string{
		"",
		"# comment only\n",
		"Name=svc\n",                      // no BinaryPath: an unstartable registration
		"BinaryPath=\"C:\\a\\svc.exe\"\n", // no Name: nothing to register as
	} {
		if _, err := parseWindowsServiceConfig(content); err == nil {
			t.Errorf("parse(%q) succeeded, want an error", content)
		}
	}
}

func TestParseWindowsServiceConfigIgnoresUnknownKeys(t *testing.T) {
	def, err := parseWindowsServiceConfig("Name=svc\nBinaryPath=x\nFutureKey=whatever\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if def.Name != "svc" || def.BinaryPath != "x" {
		t.Errorf("def = %+v", def)
	}
	if def.DisplayName != "svc" {
		t.Errorf("DisplayName = %q, want it to default to the name", def.DisplayName)
	}
}

// parseArgvW reimplements CommandLineToArgvW's argument-splitting rules, so the
// quoting above is checked against the reader rather than against itself.
func parseArgvW(line string) []string {
	var (
		args    []string
		cur     strings.Builder
		inQuote bool
		started bool
		slashes int
	)
	flushSlashes := func(beforeQuote bool) {
		if beforeQuote {
			cur.WriteString(strings.Repeat(`\`, slashes/2))
		} else {
			cur.WriteString(strings.Repeat(`\`, slashes))
		}
		slashes = 0
	}
	for i := range len(line) {
		switch c := line[i]; c {
		case '\\':
			slashes++
			started = true
		case '"':
			odd := slashes%2 == 1
			flushSlashes(true)
			if odd {
				cur.WriteByte('"')
			} else {
				inQuote = !inQuote
			}
			started = true
		case ' ', '\t':
			flushSlashes(false)
			if inQuote {
				cur.WriteByte(c)
				continue
			}
			if started {
				args = append(args, cur.String())
				cur.Reset()
				started = false
			}
		default:
			flushSlashes(false)
			cur.WriteByte(c)
			started = true
		}
	}
	flushSlashes(false)
	if started {
		args = append(args, cur.String())
	}
	return args
}

// TestWindowsInitSystemNameIsStable pins a value that behaves like a wire format:
// it is what Name() answers, what the detector is registered under, and what gets
// PERSISTED in install state — and NewServiceManagerByName resolves a manager by
// exactly the string that was recorded. Changing it makes every lifecycle command
// on an already-installed service fail with "unknown init system".
func TestWindowsInitSystemNameIsStable(t *testing.T) {
	if windowsInitSystemName != "windows-service" {
		t.Errorf("init system name = %q; a change strands services installed under the old name", windowsInitSystemName)
	}
}

// TestWindowsManifestPath covers the definition file's location, including the
// fallback taken when no install dir is configured. Separators are deliberately not
// asserted: the function is only ever called on Windows, but its tests run here.
func TestWindowsManifestPath(t *testing.T) {
	withDir := windowsManifestPath(`C:\Program Files\sample`, "sample-agent")
	if !strings.HasSuffix(withDir, "sample-agent"+windowsManifestSuffix) {
		t.Errorf("manifest path %q does not end in the service's definition file", withDir)
	}
	if !strings.Contains(withDir, "sample") {
		t.Errorf("manifest path %q is not under the install dir it was given", withDir)
	}

	// With no install dir the file must still land somewhere machine-wide a
	// LocalSystem service can write, not in a relative path resolved against
	// whatever directory the installer happened to run from.
	fallback := windowsManifestPath("", "sample-agent")
	if !strings.Contains(fallback, "ProgramData") {
		t.Errorf("fallback manifest path %q is not under ProgramData", fallback)
	}
	if !strings.HasSuffix(fallback, "sample-agent"+windowsManifestSuffix) {
		t.Errorf("fallback manifest path %q does not end in the definition file", fallback)
	}
}

// The image path must stay separate from the command line for filesystem checks.
func TestWindowsDefinitionCarriesTheImagePath(t *testing.T) {
	cfg := ServiceConfig{
		Name:      "sample-agent",
		ExecPath:  `C:\ProgramData\sample-agent\sample-agent.exe`,
		Arguments: []string{"run"},
	}
	content, err := GenerateWindowsServiceConfig(cfg, windowsExecPath(cfg))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	def, err := parseWindowsServiceConfig(content)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if def.ImagePath != `C:\ProgramData\sample-agent\sample-agent.exe` {
		t.Errorf("ImagePath = %q, want the unquoted executable alone", def.ImagePath)
	}
	// It must be the executable ALONE — quoting it, or letting the arguments ride
	// along, would make the stat fail on a perfectly good install.
	if strings.Contains(def.ImagePath, `"`) || strings.Contains(def.ImagePath, " run") {
		t.Errorf("ImagePath = %q; it must be the bare path, not the command line", def.ImagePath)
	}
	// And it must agree with what the command line will actually launch.
	if !strings.HasPrefix(def.BinaryPath, `"`+def.ImagePath+`"`) &&
		!strings.HasPrefix(def.BinaryPath, def.ImagePath) {
		t.Errorf("BinaryPath %q does not launch ImagePath %q", def.BinaryPath, def.ImagePath)
	}
}

// TestWindowsDefinitionCarriesArgumentsSeparately is the regression for the
// failure that survived two earlier fixes: the binary was copied correctly, the
// service registered and enabled, and Start answered "The system cannot find the
// file specified" — because CreateService IGNORES cfg.BinaryPathName and escapes
// whatever it is handed as `exepath` into ONE token. Passing the assembled
// command line there produces a stored image path containing quotes and a space.
//
// So the definition must carry the image and the arguments apart, and the
// assembled line must remain exactly their composition.
func TestWindowsDefinitionCarriesArgumentsSeparately(t *testing.T) {
	cfg := ServiceConfig{
		Name:      "sample-agent",
		ExecPath:  `C:\Program Files\sample\sample-agent.exe`,
		Arguments: []string{"run", "--flag=a b"},
	}
	content, err := GenerateWindowsServiceConfig(cfg, windowsExecPath(cfg))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	def, err := parseWindowsServiceConfig(content)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if def.ImagePath != cfg.ExecPath {
		t.Errorf("ImagePath = %q, want the bare executable %q", def.ImagePath, cfg.ExecPath)
	}
	if len(def.Arguments) != 2 || def.Arguments[0] != "run" || def.Arguments[1] != "--flag=a b" {
		t.Fatalf("Arguments = %q, want [run, --flag=a b] — an argument with a space must survive the round trip", def.Arguments)
	}
	// The image path must never be the command line: that substitution is the bug.
	if strings.Contains(def.ImagePath, `"`) || strings.Contains(def.ImagePath, " run") {
		t.Errorf("ImagePath = %q; CreateService would escape that whole string into one token", def.ImagePath)
	}
	// And the two representations must agree, so the create path and the update
	// path register the same service.
	if want := WindowsCommandLine(def.ImagePath, def.Arguments); def.BinaryPath != want {
		t.Errorf("BinaryPath = %q, want %q — the assembled line must be exactly image+args", def.BinaryPath, want)
	}
}
