//go:build e2e_docker && linux

package docker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/e2e/docker/internal/wire"
	"github.com/ovineko/daemonpilot/internal/daemon"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestInitNativeCrontabEnvironment(t *testing.T) {
	for _, value := range []string{"before", "after with 'quotes' and spaces"} {
		t.Run(value, func(t *testing.T) {
			dir := t.TempDir()
			cfg := daemon.ServiceConfig{Name: wire.Name, InstallDir: dir, WorkingDir: dir, LogDir: dir, ExecPath: "/bin/sh", Arguments: []string{"-c", `printf '%s' "$GO_DAEMONPILOT_E2E_VALUE"`}, Environment: map[string]string{wire.EnvironmentKey: value}}
			entry := daemon.FormatCrontabEntry(cfg)
			_, command, ok := strings.Cut(entry, "@reboot ")
			if !ok {
				t.Fatal("missing reboot schedule")
			}
			ctx, cancel := context.WithTimeout(t.Context(), testsupport.Scale(3*time.Second))
			defer cancel()
			out, err := exec.CommandContext(ctx, "/bin/sh", "-c", command).CombinedOutput()
			if err != nil {
				t.Fatalf("generated cron command: %v: %s", err, out)
			}
			data, err := os.ReadFile(filepath.Join(dir, wire.Name+".log"))
			check(t, err)
			if string(data) != value {
				t.Fatalf("manifest environment did not reach process: got %q want %q", data, value)
			}
		})
	}
}

func TestInitNativeCapabilities(t *testing.T) {
	for _, p := range initProfiles() {
		t.Run(p.name, func(t *testing.T) {
			if p.name == "openrc" {
				t.Skip("OpenRC capability is probed from the running image's supervise-daemon; asserted in Docker")
			}
			mgr, err := daemon.NewServiceManagerByName(p.name)
			check(t, err)
			got := wire.Capabilities{Enable: mgr.SupportsEnable(), Start: mgr.SupportsStart(), Status: mgr.SupportsStatus(), RestartWatchdog: mgr.CanRestartWatchdog()}
			want := wire.Capabilities{Enable: true, Start: true, Status: true, RestartWatchdog: p.restart}
			if got != want {
				t.Fatalf("capabilities %+v want %+v", got, want)
			}
		})
	}
}
