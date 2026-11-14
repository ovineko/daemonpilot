package daemonpilot

import (
	"bytes"
	"strings"
	"testing"
)

func TestIsExecThrough(t *testing.T) {
	origLookupEnv := lookupEnv
	defer func() { lookupEnv = origLookupEnv }()

	tests := []struct {
		name   string
		args   []string
		envVal string
		envSet bool
		want   bool
	}{
		{
			name: "flag present",
			args: []string{"myapp", "run", "--exec-through"},
			want: true,
		},
		{
			name: "flag not present",
			args: []string{"myapp", "run"},
			want: false,
		},
		{
			name:   "env var set to 1",
			args:   []string{"myapp", "run"},
			envVal: "1",
			envSet: true,
			want:   true,
		},
		{
			name:   "env var set to 0",
			args:   []string{"myapp", "run"},
			envVal: "0",
			envSet: true,
			want:   false,
		},
		{
			name:   "env var set to empty",
			args:   []string{"myapp", "run"},
			envVal: "",
			envSet: true,
			want:   false,
		},
		{
			name: "flag not present with other args",
			args: []string{"myapp", "run", "--verbose"},
			want: false,
		},
		{
			name: "flag among other args",
			args: []string{"myapp", "run", "--verbose", "--exec-through", "--debug"},
			want: true,
		},
		{
			name:   "both flag and env var",
			args:   []string{"myapp", "run", "--exec-through"},
			envVal: "1",
			envSet: true,
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookupEnv = func(key string) (string, bool) {
				if key == "DAEMONPILOT_EXEC_THROUGH" {
					return tt.envVal, tt.envSet
				}
				return "", false
			}

			got := isExecThrough(tt.args)
			if got != tt.want {
				t.Errorf("isExecThrough() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{name: "install", args: []string{"myapp", "install"}, want: "install"},
		{name: "uninstall", args: []string{"myapp", "uninstall"}, want: "uninstall"},
		{name: "run", args: []string{"myapp", "run"}, want: "run"},
		{name: "worker", args: []string{"myapp", "worker"}, want: "worker"},
		{name: "selftest", args: []string{"myapp", "selftest"}, want: "selftest"},
		{name: "watchdog-version", args: []string{"myapp", "watchdog-version"}, want: "watchdog-version"},
		{name: "version", args: []string{"myapp", "version"}, want: "version"},
		{name: "status", args: []string{"myapp", "status"}, want: "status"},
		{name: "start", args: []string{"myapp", "start"}, want: "start"},
		{name: "stop", args: []string{"myapp", "stop"}, want: "stop"},
		{name: "restart", args: []string{"myapp", "restart"}, want: "restart"},
		{name: "enable", args: []string{"myapp", "enable"}, want: "enable"},
		{name: "disable", args: []string{"myapp", "disable"}, want: "disable"},
		{name: "update-config", args: []string{"myapp", "update-config"}, want: "update-config"},
		{name: "no args", args: []string{"myapp"}, wantErr: true},
		{name: "empty args", args: []string{}, wantErr: true},
		{name: "unknown command", args: []string{"myapp", "foo"}, wantErr: true},
		{name: "extra args ignored", args: []string{"myapp", "run", "--verbose"}, want: "run"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCommand(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseCommand() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseCommandErrorMessages(t *testing.T) {
	_, err := parseCommand([]string{"myapp"})
	if err == nil || err.Error() != "no command specified" {
		t.Errorf("expected 'no command specified' error, got %v", err)
	}

	_, err = parseCommand([]string{"myapp", "bogus"})
	if err == nil || err.Error() != "unknown command: bogus" {
		t.Errorf("expected 'unknown command: bogus' error, got %v", err)
	}
}

func TestPrintUsage(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf, "myapp")

	output := buf.String()

	if len(output) == 0 {
		t.Fatal("printUsage produced no output")
	}

	expectedStrings := []string{
		"Usage: myapp <command>",
		"install",
		"uninstall",
		"run",
		"worker",
		"selftest",
		"watchdog-version",
		"version",
		"status",
		"start",
		"stop",
		"restart",
		"enable",
		"disable",
		"update-config",
	}

	for _, s := range expectedStrings {
		if !strings.Contains(output, s) {
			t.Errorf("printUsage output missing %q", s)
		}
	}
}
