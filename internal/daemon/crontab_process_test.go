//go:build !windows

package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

type crontabProcessExecutor struct {
	t     *testing.T
	entry string
}

func (e crontabProcessExecutor) Run(name string, args ...string) ([]byte, error) {
	switch name {
	case "crontab":
		return []byte(e.entry), nil
	case "pgrep":
		return nil, errors.New("service has not started")
	default:
		ctx, cancel := context.WithTimeout(e.t.Context(), testsupport.Scale(2*time.Second))
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.WaitDelay = testsupport.Scale(200 * time.Millisecond)
		return cmd.CombinedOutput()
	}
}

func TestCrontabStartDetachesAndKeepsLog(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "service")
	script := "#!/bin/sh\nprintf 'service output\\n'\nwhile [ ! -f " + shellQuote(filepath.Join(dir, "stop")) + " ]; do sleep 0.05; done\ntouch " + shellQuote(filepath.Join(dir, "done")) + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(filepath.Join(dir, "stop"), nil, 0o600); err != nil {
			t.Error(err)
		}
		deadline := time.Now().Add(testsupport.Scale(3 * time.Second))
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(dir, "done")); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("detached service did not finish")
	})
	cfg := ServiceConfig{Name: "service", ExecPath: binary, WorkingDir: dir, LogDir: dir, Arguments: []string{"run"}}
	mgr := &crontabManager{serviceName: "service", executor: crontabProcessExecutor{t: t, entry: FormatCrontabEntry(cfg)}}
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testsupport.Scale(time.Second))
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(dir, "service.log"))
		if strings.Contains(string(data), "service output") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("manual start discarded configured log output")
}
