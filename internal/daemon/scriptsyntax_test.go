package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGeneratedShellScriptsParse guards every shell-script generator against a
// syntax error. A missing `fi` renders an init script that `sh` refuses to
// parse, which breaks start/stop/status and boot-time startup on the target
// host while every unit test that only asserts on substrings still passes.
func TestGeneratedShellScriptsParse(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}

	config := ServiceConfig{
		Name:        "testservice",
		Description: "Test service",
		ExecPath:    "/usr/local/bin/testservice",
		WorkingDir:  "/var/lib/testservice",
		Arguments:   []string{"run", "--flag"},
		Environment: map[string]string{"FOO": "bar"},
	}
	withUser := config
	withUser.User = "svcuser"

	tests := []struct {
		name     string
		generate func(ServiceConfig) (string, error)
		config   ServiceConfig
	}{
		{"sysvinit", GenerateSysvinitInitScript, config},
		{"sysvinit_user", GenerateSysvinitInitScript, withUser},
		{"openrc", GenerateOpenRCInitScript, config},
		{"openrc_user", GenerateOpenRCInitScript, withUser},
		{
			"busybox",
			func(c ServiceConfig) (string, error) { return GenerateBusyboxInitScript(c, c.ExecPath) },
			config,
		},
		{
			"procd",
			func(c ServiceConfig) (string, error) { return GenerateProcdInitScript(c, c.ExecPath) },
			config,
		},
		{
			"s6",
			func(c ServiceConfig) (string, error) { return GenerateS6RunScript(c, c.ExecPath) },
			config,
		},
		{
			"runit",
			func(c ServiceConfig) (string, error) { return GenerateRunitRunScript(c, c.ExecPath) },
			config,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script, err := tt.generate(tt.config)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			path := filepath.Join(t.TempDir(), "script.sh")
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatalf("write script: %v", err)
			}
			if out, err := exec.Command(shell, "-n", path).CombinedOutput(); err != nil {
				t.Errorf("sh -n rejected the generated script: %v\n%s\n--- script ---\n%s", err, out, script)
			}
		})
	}
}
