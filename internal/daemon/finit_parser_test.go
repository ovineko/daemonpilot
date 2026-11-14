package daemon

import (
	"slices"
	"strings"
	"testing"
)

func TestFinitParserRegressions(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		want          []string
		invalid       bool
	}{
		{name: "simple", command: `/probe run`, want: []string{"/probe", "run"}},
		{name: "single spaces", command: `/probe "hello world" "" tail`, want: []string{"/probe", "hello world", "", "tail"}},
		{name: "collapsed spaces", command: `/probe "hello  world"`, want: []string{"/probe", "hello world"}},
		{name: "quote starts token", command: `/probe "cd '/state' && exec \"\$@\"" daemonpilot /state/probe run`, invalid: true},
		{name: "description inside argument", command: `/probe "a-- b"`, want: []string{"/probe"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFinitCommand("# probe\nservice [234] " + tc.command + " -- probe\n")
			if tc.invalid {
				if err == nil {
					t.Fatalf("misparsed wrapper accepted: %q", got)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got %q (%v), want %q", got, err, tc.want)
			}
		})
	}
}

func TestFinitCommandValidation(t *testing.T) {
	for _, value := range []string{"two  spaces", " leading", "trailing ", "a 'quote'", "a \"quote\"", `a \escape`, "a-- b", "#comment", "a\tb"} {
		for _, field := range []string{"argument", "directory", "executable"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				cfg := ServiceConfig{Name: "probe", ExecPath: "/probe", WorkingDir: "/work"}
				switch field {
				case "argument":
					cfg.Arguments = []string{value}
				case "directory":
					cfg.WorkingDir = "/" + value
				case "executable":
					cfg.ExecPath = "/" + value
				}
				if field == "directory" && value == " leading" {
					cfg.WorkingDir = value
				}
				if _, err := GenerateFinitServiceFile(cfg, cfg.ExecPath); err == nil || !strings.Contains(err.Error(), "finit:") {
					t.Fatalf("unsupported %s accepted: %v", field, err)
				}
			})
		}
	}
}

func TestFinitWorkingDirectory(t *testing.T) {
	cfg := ServiceConfig{Name: "render-probe", ExecPath: "/probe", WorkingDir: "/work directory", Arguments: []string{"", "hello world", "--", `quote'"$HOME`}, Environment: map[string]string{"PROBE_VALUE": `value " '$HOME`, "SECOND_VALUE": "two"}}
	rendered, err := GenerateFinitServiceFile(cfg, cfg.ExecPath)
	if err != nil {
		t.Fatal(err)
	}
	checkInitWords(t, "finit", cfg, rendered)
}

func TestFinitRejectsRelativePaths(t *testing.T) {
	for _, tc := range []struct{ executable, directory string }{
		{"name:probe", "/work"}, {"@probe", "/work"}, {"/probe", "-P"}, {"/probe", "relative"},
	} {
		t.Run(tc.executable+"/"+tc.directory, func(t *testing.T) {
			_, err := GenerateFinitServiceFile(ServiceConfig{Name: "probe", WorkingDir: tc.directory}, tc.executable)
			if err == nil || !strings.Contains(err.Error(), "absolute") {
				t.Fatalf("ambiguous path accepted: %v", err)
			}
		})
	}
}
