//go:build !windows

package integration

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func buildLifecycleFixture(t *testing.T) string {
	t.Helper()
	return testsupport.Fixture(t, "lifecycle", "../fixtures/lifecycle")
}

func startFixture(t *testing.T, binary, mode, installDir, events string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	cmd := exec.Command(binary, "run")
	cmd.Env = append(os.Environ(),
		"FIXTURE_MODE="+mode,
		"FIXTURE_INSTALL_DIR="+installDir,
		"FIXTURE_EVENTS="+events,
		"FIXTURE_ATTEMPTS="+filepath.Join(installDir, "attempts"),
		"FIXTURE_CRASHES=3",
		"DAEMONPILOT_DISABLE_PARENT_MONITOR=1",
		"FIXTURE_TEST_SECRET=must-not-appear-in-logs",
	)
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lifecycle fixture: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd, &logs
}

func waitForFileLines(t *testing.T, path string, count int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(testsupport.Scale(timeout))
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Fields(strings.TrimSpace(string(data)))
			if len(lines) >= count {
				return lines
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read fixture event file: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %d events in %s", timeout, count, path)
	return nil
}

func waitCommand(t *testing.T, cmd *exec.Cmd, timeout time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(testsupport.Scale(timeout)):
		_ = cmd.Process.Kill()
		t.Fatalf("timed out after %s waiting for fixture process %d to exit", timeout, cmd.Process.Pid)
		return nil
	}
}

func TestLifecycleProcess_SuperviseRestartAndLogStory(t *testing.T) {
	binary := buildLifecycleFixture(t)
	dir := t.TempDir()
	events := filepath.Join(dir, "events")
	cmd, logs := startFixture(t, binary, "crash_then_hang", dir, events)
	waitForFileLines(t, events, 4, 10*time.Second)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal watchdog: %v", err)
	}
	if err := waitCommand(t, cmd, 5*time.Second); err != nil {
		t.Fatalf("watchdog exit after restart journey: %v\n%s", err, logs.String())
	}
	story := logs.String()
	if strings.Contains(story, "must-not-appear-in-logs") {
		t.Fatalf("lifecycle log leaked an unrelated environment value\n%s", story)
	}
	ordered := []string{`"phase":"run"`, `"step":"lock"`, `"step":"spawn_worker"`, `"outcome":"restart_scheduled"`, `"outcome":"succeeded"`}
	position := 0
	for _, marker := range ordered {
		next := strings.Index(story[position:], marker)
		if next < 0 {
			t.Fatalf("lifecycle log missing ordered marker %s after byte %d\n%s", marker, position, story)
		}
		position += next + len(marker)
	}
}

func TestLifecycleProcess_PermanentStopDoesNotRestart(t *testing.T) {
	binary := buildLifecycleFixture(t)
	dir := t.TempDir()
	events := filepath.Join(dir, "events")
	cmd, logs := startFixture(t, binary, "permanent", dir, events)
	if err := waitCommand(t, cmd, 5*time.Second); err != nil {
		t.Fatalf("permanent-stop watchdog exit: %v\n%s", err, logs.String())
	}
	lines := waitForFileLines(t, events, 1, time.Second)
	if len(lines) != 1 {
		t.Fatalf("worker starts = %d, want 1", len(lines))
	}
	if !strings.Contains(logs.String(), `"outcome":"stopped"`) {
		t.Fatalf("log does not record permanent-stop decision\n%s", logs.String())
	}
}

func TestLifecycleProcess_GracefulShutdown(t *testing.T) {
	binary := buildLifecycleFixture(t)
	dir := t.TempDir()
	events := filepath.Join(dir, "events")
	cmd, logs := startFixture(t, binary, "graceful", dir, events)
	waitForFileLines(t, events, 1, 5*time.Second)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal watchdog: %v", err)
	}
	if err := waitCommand(t, cmd, 5*time.Second); err != nil {
		t.Fatalf("graceful watchdog exit: %v\n%s", err, logs.String())
	}
	lines := waitForFileLines(t, events, 2, time.Second)
	if lines[len(lines)-1] != "terminated" {
		t.Fatalf("worker did not observe graceful termination: %v", lines)
	}
}

func TestLifecycleProcess_GracefulTimeoutKillsWorker(t *testing.T) {
	binary := buildLifecycleFixture(t)
	dir := t.TempDir()
	events := filepath.Join(dir, "events")
	cmd, logs := startFixture(t, binary, "ignore_term", dir, events)
	waitForFileLines(t, events, 1, 5*time.Second)
	started := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal watchdog: %v", err)
	}
	if err := waitCommand(t, cmd, 5*time.Second); err != nil {
		t.Fatalf("watchdog exit after graceful timeout: %v\n%s", err, logs.String())
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
		t.Fatalf("worker was killed after %s, before its graceful window elapsed", elapsed)
	}
	lines := waitForFileLines(t, events, 1, time.Second)
	if len(lines) != 1 {
		t.Fatalf("worker that ignored termination wrote unexpected events: %v", lines)
	}
}

func TestLifecycleProcess_SingleInstance(t *testing.T) {
	binary := buildLifecycleFixture(t)
	dir := t.TempDir()
	first, firstLogs := startFixture(t, binary, "graceful", dir, filepath.Join(dir, "first-events"))
	waitForFileLines(t, filepath.Join(dir, "first-events"), 1, 5*time.Second)
	second, secondLogs := startFixture(t, binary, "graceful", dir, filepath.Join(dir, "second-events"))
	if err := waitCommand(t, second, 5*time.Second); err == nil {
		t.Fatalf("second supervisor unexpectedly succeeded\n%s", secondLogs.String())
	}
	if !strings.Contains(secondLogs.String(), "another instance is already running") {
		t.Fatalf("second supervisor did not report lock contention\n%s", secondLogs.String())
	}
	if err := first.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal first watchdog: %v", err)
	}
	if err := waitCommand(t, first, 5*time.Second); err != nil {
		t.Fatalf("first watchdog exit: %v\n%s", err, firstLogs.String())
	}
}
