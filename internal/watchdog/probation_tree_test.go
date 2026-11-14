//go:build !windows

package watchdog

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	"github.com/ovineko/procguard"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestDumpProcessDiagnosticsWithoutWorker(t *testing.T) {
	for _, stage := range []string{"before-start", "after-exit"} {
		t.Run(stage, func(t *testing.T) {
			w := New(testConfig())
			if stage == "after-exit" {
				w.SetSpecFunc(mockSpecFunc(0, daemonpilot.ExitPermanent))
				w.manageWorker(t.Context())
			}
			if err := dumpProcessDiagnostics(w.supervisor); !errors.Is(err, os.ErrProcessDone) {
				t.Fatalf("diagnostics without a worker: %v", err)
			}
		})
	}
}

func TestProbation_CrashedWorkerWithLingeringDescendantIsNotStable(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("natural-exit descendant cleanup requires Linux or macOS process enumeration")
	}
	dir := t.TempDir()
	ready, crash := filepath.Join(dir, "ready"), filepath.Join(dir, "crash")
	w := New(testConfig())
	w.SetSpecFunc(func(context.Context) (procguard.Spec, error) {
		return procguard.Spec{
			Path: os.Args[0], Args: []string{"-test.run=^TestProbationTreeHelper$", "--", "leader", ready, crash},
			Env: append(os.Environ(), "GORACE=atexit_sleep_ms=0"), GracePeriod: 2 * time.Second,
		}, nil
	})
	done := make(chan struct{})
	go func() { w.manageWorker(t.Context()); close(done) }()
	t.Cleanup(func() {
		w.Shutdown()
		select {
		case <-done:
		case <-time.After(testsupport.Scale(waitTimeout)):
			t.Error("supervisor did not finish descendant cleanup")
		}
	})
	waitFor(t, "descendant ready and live worker stable", func() bool {
		_, err := os.Stat(ready)
		return err == nil && w.workerStableFor(50*time.Millisecond)
	})
	pid := w.WorkerState().PID
	if err := os.WriteFile(crash, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "leader exit before descendant cleanup", func() bool { return !w.WorkerState().Running })
	state := w.WorkerState()
	if state.PID != pid || state.Last != nil {
		t.Fatalf("expected the crashed launch to still be cleaning descendants: %+v", state)
	}
	for _, duration := range []time.Duration{0, 50 * time.Millisecond, probationReadySoak} {
		t.Run(duration.String(), func(t *testing.T) {
			if w.workerStableFor(duration) {
				t.Fatalf("dead leader counted as stable for %v during descendant cleanup", duration)
			}
		})
	}
}

func TestProbationTreeHelper(t *testing.T) {
	if len(os.Args) != 6 || os.Args[2] != "--" {
		return
	}
	ready, crash := os.Args[4], os.Args[5]
	if os.Args[3] == "descendant" {
		signal.Ignore(syscall.SIGTERM)
		// #nosec G703 -- The parent test supplies a marker under t.TempDir.
		if err := os.WriteFile(ready, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(testsupport.Scale(waitTimeout))
		os.Exit(0)
	}
	// Inherit the worker's process group so procguard owns descendant cleanup.
	// #nosec G204 G702 -- Re-executes this test binary with parent-owned fixture paths.
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProbationTreeHelper$", "--", "descendant", ready, crash)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "crash request", func() bool {
		// #nosec G703 -- The parent test supplies a marker under t.TempDir.
		_, err := os.Stat(crash)
		return err == nil
	})
	os.Exit(1)
}
