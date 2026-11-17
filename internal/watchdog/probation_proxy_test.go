//go:build !windows

package watchdog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestExecThroughProbation(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"commit", "rollback"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			cfg := stagedProbationConfig(t)
			cfg.GracefulTimeout = 100 * time.Millisecond
			// A child crash cannot reach the proxy's safety net inside this test.
			cfg.MaxRestartDelay = time.Minute
			cfg.StableThreshold = time.Hour
			w := New(cfg)
			currentPath, rollbackPath := w.installedBinaryPath(), w.rollbackBinaryPath()
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
			current := "#!/bin/sh\nexec " + quote(os.Args[0]) +
				" -test.run=^TestExecThroughProbationHelper$ -- " + quote(cfg.InstallDir) + " " + outcome + "\n"
			previous := "#!/bin/sh\necho restored > " + quote(filepath.Join(cfg.InstallDir, "restored-run")) + "\nexec sleep 60\n"
			writeFile(t, currentPath, current)
			writeFile(t, rollbackPath, previous)
			recordTestRollbackDigest(t, cfg.InstallDir, cfg.Name, rollbackPath)

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { w.runExecThroughChild(ctx); close(done) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(testsupport.Scale(waitTimeout)):
					t.Error("proxy did not stop")
				}
			})

			deadline := time.Now().Add(testsupport.Scale(12 * time.Second))
			for {
				state, err := daemonpilot.LoadState(cfg.InstallDir, cfg.Name)
				_, rollbackErr := os.Stat(rollbackPath)
				resolved := err == nil && state != nil && state.RollbackDigest == "" && os.IsNotExist(rollbackErr)
				if outcome == "rollback" {
					_, runErr := os.Stat(filepath.Join(cfg.InstallDir, "restored-run"))
					resolved = resolved && runErr == nil
				}
				if resolved {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s not observed: rollback must be consumed, digest cleared, and restored binary running after rollback", outcome)
				}
				time.Sleep(10 * time.Millisecond)
			}

			want := current
			if outcome == "rollback" {
				want = previous
				data, err := os.ReadFile(filepath.Join(cfg.InstallDir, "child-return"))
				if err != nil || string(data) != "<nil>" {
					t.Fatalf("rollback child must return nil for exit 0: %q, %v", data, err)
				}
			} else {
				data, err := os.ReadFile(filepath.Join(cfg.InstallDir, "child-pid"))
				if err != nil || string(data) != strconv.Itoa(w.ChildPID()) {
					t.Fatalf("commit must keep the child alive: pid=%q, proxy pid=%d, error=%v", data, w.ChildPID(), err)
				}
				if _, err := os.Stat(filepath.Join(cfg.InstallDir, "child-return")); !os.IsNotExist(err) {
					t.Fatalf("committed child exited: %v", err)
				}
			}
			// #nosec G304 -- Installed fixture under this test's temporary directory.
			if data, err := os.ReadFile(currentPath); err != nil || string(data) != want {
				t.Fatalf("installed binary differs after %s: %v", outcome, err)
			}
			if restored, err := w.restoreRollbackBinary(rollbackPath, currentPath); restored || err == nil {
				t.Fatalf("spent rollback restored twice: restored=%v, error=%v", restored, err)
			}
		})
	}
}

func TestExecThroughProbationHelper(t *testing.T) {
	if len(os.Args) != 5 || os.Args[2] != "--" {
		return
	}
	cfg := testConfig()
	cfg.InstallDir, cfg.Name = os.Args[3], "probe-agent"
	cfg.ExecThrough = true
	cfg.StableThreshold = testsupport.Scale(300 * time.Millisecond)
	cfg.Update = &daemonpilot.UpdateConfig{
		CheckInterval:            time.Hour,
		RollbackTimeout:          testsupport.Scale(5 * time.Second),
		StagedRollbackOnUnstable: true,
	}
	// Include a health checker so stopping only the worker cannot end Run.
	cfg.HealthInterval = time.Hour
	cfg.HealthFunc = func(context.Context) error { return nil }
	w := New(cfg)
	w.updateCheckFn = func(context.Context) error { return nil }
	w.SetSpecFunc(mockSpecFunc(time.Minute, 0))
	if os.Args[4] == "rollback" {
		// A readiness signal alone must not commit a crashing worker.
		w.workerEverReady.Store(true)
		w.SetSpecFunc(mockSpecFunc(10*time.Millisecond, 1))
	}
	writeFile(t, filepath.Join(cfg.InstallDir, "child-pid"), strconv.Itoa(os.Getpid()))
	err := w.Run(t.Context())
	writeFile(t, filepath.Join(cfg.InstallDir, "child-return"), fmt.Sprint(err))
	if err != nil {
		t.Fatal(err)
	}
}
