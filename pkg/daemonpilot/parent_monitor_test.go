package daemonpilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/procguard"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestWorkerParentMonitor(t *testing.T) {
	for _, tc := range []struct {
		name, pid, disabled, message string
		setupErr                     error
		parentExit, called           bool
		nilLogger                    bool
	}{
		{name: "success", pid: "12345", called: true},
		{name: "race", pid: "12345", called: true, parentExit: true},
		{name: "setup error", pid: "12345", called: true, setupErr: os.ErrPermission},
		{name: "unsupported", pid: "12345", called: true, setupErr: procguard.ErrUnsupported},
		{name: "disabled", pid: "12345", disabled: "1", message: "parent death monitoring disabled"},
		{name: "disabled without PID", disabled: "true", message: "parent death monitoring disabled"},
		{name: "invalid", pid: "not-a-number", message: "invalid DAEMONPILOT_PARENT_PID, parent monitoring disabled"},
		{name: "zero", pid: "0", message: "parent monitor config validation failed, continuing without monitoring"},
		{name: "negative", pid: "-1", message: "parent monitor config validation failed, continuing without monitoring"},
		{name: "absent", message: "parent monitor config validation failed, continuing without monitoring"},
		{name: "nil logger", pid: "12345", nilLogger: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := startParentMonitorFn
			t.Cleanup(func() { startParentMonitorFn = original })
			t.Setenv(EnvParentPID, tc.pid)
			t.Setenv(EnvDisableParentMonitor, tc.disabled)
			calls := 0
			startParentMonitorFn = func(parent context.Context, pid int, interval time.Duration) (context.Context, context.CancelFunc, error) {
				calls++
				if pid != 12345 || interval != 0 {
					t.Fatalf("WatchParent arguments: %d %v", pid, interval)
				}
				if tc.setupErr != nil {
					return nil, nil, tc.setupErr
				}
				child, cancel := context.WithCancelCause(parent)
				if tc.parentExit {
					cancel(&procguard.ParentExitError{ExpectedPID: pid, ObservedPID: 1})
				}
				return child, func() { cancel(nil) }, nil
			}
			var output bytes.Buffer
			var logger Logger = slog.New(slog.NewJSONHandler(&output, nil))
			if tc.nilLogger {
				logger = nil
			}
			parent, stop := context.WithCancel(context.Background())
			defer stop()
			child, cancel := workerParentMonitor(parent, logger)
			if child == nil || cancel == nil {
				t.Fatal("missing context or cancel")
			}
			if !tc.parentExit && child.Err() != nil {
				t.Fatal("monitor cancelled unexpectedly")
			}
			stop()
			select {
			case <-child.Done():
			case <-time.After(testsupport.Scale(time.Second)):
				t.Fatal("parent cancellation did not propagate")
			}
			cancel()
			if (calls == 1) != tc.called {
				t.Fatalf("WatchParent calls: %d", calls)
			}
			if tc.message != "" && !strings.Contains(output.String(), tc.message) {
				t.Fatalf("missing warning: %s", &output)
			}
			if tc.parentExit {
				assertParentExitLog(t, &output, 12345, 1)
			}
			if tc.setupErr != nil {
				level := "WARN"
				message := "parent monitor config validation failed, continuing without monitoring"
				if runtime.GOOS == "linux" {
					message = "prctl PR_SET_PDEATHSIG failed, continuing without kernel-level parent death monitoring"
				}
				if runtime.GOOS == "windows" && errors.Is(tc.setupErr, procguard.ErrUnsupported) {
					level = "INFO"
					message = "parent death monitor not available on Windows; watchdog Job Object provides process-tree cleanup"
				}
				if !strings.Contains(output.String(), `"level":"`+level+`"`) || !strings.Contains(output.String(), message) {
					t.Fatalf("setup error log: %s", &output)
				}
			} else if tc.called && !tc.parentExit {
				message := "parent death monitor started (unix polling)"
				if runtime.GOOS == "linux" {
					message = "parent death signal configured via prctl"
				}
				if !strings.Contains(output.String(), message) || strings.Contains(output.String(), `"level":"WARN"`) {
					t.Fatalf("successful setup or ordinary cancellation log: %s", &output)
				}
			}
		})
	}
}

func TestLinuxParentPIDRaceCondition(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux parent-death logging contract")
	}
	original := startParentMonitorFn
	t.Cleanup(func() { startParentMonitorFn = original })
	t.Setenv(EnvParentPID, "12345")
	t.Setenv(EnvDisableParentMonitor, "")
	startParentMonitorFn = func(parent context.Context, pid int, _ time.Duration) (context.Context, context.CancelFunc, error) {
		child, cancel := context.WithCancelCause(parent)
		cancel(&procguard.ParentExitError{ExpectedPID: pid, ObservedPID: 1})
		return child, func() { cancel(nil) }, nil
	}
	var output bytes.Buffer
	child, cancel := workerParentMonitor(t.Context(), slog.New(slog.NewJSONHandler(&output, nil)))
	if !errors.Is(child.Err(), context.Canceled) {
		t.Fatal("parent race did not cancel worker")
	}
	cancel()
	assertParentExitLog(t, &output, 12345, 1)
}

func assertParentExitLog(t *testing.T, output *bytes.Buffer, expected, observed int) {
	t.Helper()
	message := "parent process died, cancelling context"
	if runtime.GOOS == "linux" {
		message = "parent process already exited (race condition detected)"
	}
	for line := range strings.SplitSeq(strings.TrimSpace(output.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["msg"] == message {
			if entry["level"] != "WARN" || entry["expectedPID"] != float64(expected) || entry["currentPID"] != float64(observed) {
				t.Fatalf("parent-death fields: %v", entry)
			}
			return
		}
	}
	t.Fatalf("missing parent-death warning: %s", output)
}
