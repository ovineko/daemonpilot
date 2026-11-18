package watchdog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/ipc"
	"github.com/ovineko/daemonpilot/internal/updater"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	"github.com/ovineko/procguard"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

// waitTimeout bounds every "this should have happened by now" wait in this package.
//
// It is a FAILURE DETECTOR, not a timing assertion: on a passing run nothing ever waits
// this long, so a generous value costs nothing, while a tight one turns a loaded machine
// into a red build. These tests spawn REAL processes and wait on REAL pipes, so an event
// that lands in a millisecond on an idle laptop can take seconds when the rest of the
// suite runs beside it. The old hand-rolled 5s guards were tight enough to fire
// spuriously, which is part of why a genuine Shutdown-vs-spawn bug read as "flaky tests"
// for as long as it did. Go's own -timeout is the real backstop for a wedged binary.
const waitTimeout = 30 * time.Second

// TestWatchdogBinaryArtifactsShareCanonicalPath is the negative control for
// the old filepath.Join(InstallDir, Config.Name) sites: restoring any producer
// or consumer to the raw name makes its shared helper disagree on Windows.
func TestWatchdogBinaryArtifactsShareCanonicalPath(t *testing.T) {
	cfg := &daemonpilot.Config{Name: "agent", InstallDir: `C:\ProgramData\agent`}
	wantBinary := filepath.Join(cfg.InstallDir, daemonpilot.InstalledBinaryNameFor(cfg.Name, "windows"))
	if got := installedBinaryPathFor(cfg, "windows"); got != wantBinary {
		t.Fatalf("installed binary path = %q, want %q", got, wantBinary)
	}
	if got := rollbackBinaryPathFor(cfg, "windows"); got != wantBinary+".rollback" {
		t.Fatalf("rollback path = %q, want %q", got, wantBinary+".rollback")
	}
}

// waitFor blocks until cond returns true, failing the test if it never does.
//
// It replaces the two shapes that used to be spread through this file, each bad in its
// own way:
//
//	for count.Load() < 3 { time.Sleep(50 * time.Millisecond) }    // unbounded
//	time.Sleep(200 * time.Millisecond); if !done { t.Error(...) } // assumption-based
//
// The first hangs the whole test binary with no message when the condition never comes
// true; the second encodes "200ms is surely enough" — a coin flip under load, and a test
// that silently asserts nothing when it is too generous.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testsupport.Scale(waitTimeout))
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", waitTimeout, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func testConfig() *daemonpilot.Config {
	cfg := &daemonpilot.Config{
		Name:            "test-service",
		Version:         "1.0.0",
		WatchdogVersion: "1",
		MaxRestartDelay: 1 * time.Second,
		StableThreshold: 5 * time.Minute,
		WorkerFunc:      func(ctx context.Context) error { <-ctx.Done(); return nil },
		Logger:          slog.New(slog.DiscardHandler),
	}
	return cfg
}

func mockSpecFunc(exitAfter time.Duration, exitCode int) SpecFunc {
	return func(_ context.Context) (procguard.Spec, error) {
		var cmd procguard.Spec
		if exitCode == 0 {
			cmd = helperSpec(exitAfter, 0)
		} else {
			cmd = helperSpec(exitAfter, exitCode)
		}
		return cmd, nil
	}
}

func mockSpecFuncFailing(err error) SpecFunc {
	return func(_ context.Context) (procguard.Spec, error) {
		return procguard.Spec{}, err
	}
}

func mockSpecFuncCounting(count *atomic.Int32, exitAfter time.Duration) SpecFunc {
	return func(_ context.Context) (procguard.Spec, error) {
		count.Add(1)
		cmd := helperSpec(exitAfter, 0)
		return cmd, nil
	}
}

func buildTestChild(t *testing.T, dir, name, mode string) string {
	t.Helper()
	path := filepath.Join(dir, daemonpilot.InstalledBinaryName(name))
	buildTestChildAtPath(t, path, mode)
	return path
}

func buildTestChildAtPath(t *testing.T, path, mode string) {
	t.Helper()
	fixture := testsupport.Fixture(t, "watchdog-worker-"+mode, "./testdata/portable_worker.go",
		"-ldflags", "-X main.mode="+mode)
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
}

func testInstalledBinaryPath(cfg *daemonpilot.Config) string {
	return filepath.Join(cfg.InstallDir, daemonpilot.InstalledBinaryName(cfg.Name))
}

func testRollbackBinaryPath(cfg *daemonpilot.Config) string {
	return testInstalledBinaryPath(cfg) + ".rollback"
}

func TestNew(t *testing.T) {
	cfg := testConfig()
	w := New(cfg)

	if w.config != cfg {
		t.Error("config not set correctly")
	}
	if w.backoff == nil {
		t.Error("backoff calculator not initialized")
	}
	if w.IsShuttingDown() {
		t.Error("should not be shutting down initially")
	}
	if w.specFunc == nil {
		t.Error("specFunc should be initialized")
	}
}

func TestNew_DefaultLogger(t *testing.T) {
	cfg := testConfig()
	cfg.Logger = nil
	w := New(cfg)

	if w.logger == nil {
		t.Error("default logger not created")
	}
}

func TestShutdownFlag(t *testing.T) {
	cfg := testConfig()
	w := New(cfg)

	if w.IsShuttingDown() {
		t.Error("should not be shutting down before Shutdown()")
	}

	w.Shutdown()

	if !w.IsShuttingDown() {
		t.Error("should be shutting down after Shutdown()")
	}
}

func TestShutdown_CancelsWorker(t *testing.T) {
	w := New(testConfig())
	w.SetSpecFunc(mockSpecFunc(time.Minute, 0))
	done := make(chan struct{})
	go func() { w.manageWorker(t.Context()); close(done) }()
	waitFor(t, "worker running", func() bool { return w.WorkerState().Running })
	w.Shutdown()
	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("worker did not stop")
	}
	if w.WorkerState().Running {
		t.Fatal("worker still running")
	}
}

func TestShutdown_NilWorkerCancel(t *testing.T) {
	cfg := testConfig()
	w := New(cfg)

	w.Shutdown()

	if !w.IsShuttingDown() {
		t.Error("should be shutting down")
	}
}

func TestRun_ShutdownOnContextCancel(t *testing.T) {
	cfg := testConfig()
	w := New(cfg)
	w.specFunc = mockSpecFunc(10*time.Second, 0)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	waitFor(t, "worker running", func() bool { return w.WorkerState().Running })
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error, got: %v", err)
		}
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Error("Run() did not return after context cancel")
	}

	if !w.IsShuttingDown() {
		t.Error("should be shutting down after context cancel")
	}
}

func TestRun_WithUpdateConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Update = &daemonpilot.UpdateConfig{
		CheckInterval: 1 * time.Minute,
	}
	w := New(cfg)
	w.specFunc = mockSpecFunc(10*time.Second, 0)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error, got: %v", err)
		}
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Error("Run() did not return after context cancel")
	}
}

func TestRun_WithoutUpdateConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Update = nil
	w := New(cfg)
	w.specFunc = mockSpecFunc(10*time.Second, 0)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error, got: %v", err)
		}
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Error("Run() did not return after context cancel")
	}
}

func TestManageWorker_StopsOnShutdown(t *testing.T) {
	cfg := testConfig()
	w := New(cfg)
	w.specFunc = mockSpecFunc(10*time.Second, 0)

	ctx := t.Context()

	w.Shutdown()

	w.manageWorker(ctx)
}

func TestManageWorker_StopsOnContextDone(_ *testing.T) {
	cfg := testConfig()
	w := New(cfg)
	w.specFunc = mockSpecFunc(10*time.Second, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w.manageWorker(ctx)
}

func TestBackoffIntegration(t *testing.T) {
	w := New(testConfig())
	if got := w.backoff.Next(); got != time.Second {
		t.Fatalf("delay = %v", got)
	}
	if !w.backoff.Saturated() {
		t.Fatal("configured maximum was not applied")
	}
	if w.backoff.Stable(5*time.Minute-time.Nanosecond) || !w.backoff.Stable(5*time.Minute) {
		t.Fatal("configured stable threshold was not applied")
	}
}

func TestSpawnWorker_Success(t *testing.T) {
	w := New(testConfig())
	w.SetSpecFunc(mockSpecFunc(time.Minute, 0))
	done := make(chan struct{})
	go func() { w.manageWorker(t.Context()); close(done) }()
	waitFor(t, "worker PID", func() bool { return w.WorkerState().PID > 0 })
	if state := w.WorkerState(); !state.Running || state.StartedAt.IsZero() || state.Launches != 1 {
		t.Fatalf("worker state: %+v", state)
	}
	w.Shutdown()
	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("worker did not stop")
	}
}

func TestSpawnWorker_Failure(t *testing.T) {
	w := New(testConfig())
	w.SetSpecFunc(mockSpecFuncFailing(errors.New("mock spawn error")))
	done := make(chan struct{})
	go func() { w.manageWorker(t.Context()); close(done) }()
	waitFor(t, "spawn failure", func() bool { return w.WorkerState().Failures > 0 })
	if state := w.WorkerState(); state.Running || state.PID != 0 || state.Launches != 0 {
		t.Fatalf("failed launch: %+v", state)
	}
	w.Shutdown()
	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("failed spawn did not stop")
	}
}

func TestManageWorker_RestartsAfterExit(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRestartDelay = 100 * time.Millisecond
	w := New(cfg)

	var spawnCount atomic.Int32
	w.specFunc = mockSpecFuncCounting(&spawnCount, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		w.manageWorker(ctx)
		done <- nil
	}()

	waitFor(t, "at least 2 spawns (the restart after exit)", func() bool { return spawnCount.Load() >= 2 })
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("manageWorker did not return")
	}
}

func TestManageWorker_BackoffDelayProgression(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRestartDelay = 10 * time.Second
	w := New(cfg)

	var spawnTimes []time.Time
	var mu sync.Mutex
	var spawnCount atomic.Int32

	w.specFunc = func(_ context.Context) (procguard.Spec, error) {
		mu.Lock()
		spawnTimes = append(spawnTimes, time.Now())
		mu.Unlock()
		spawnCount.Add(1)
		cmd := helperSpec(0, 0)
		return cmd, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		w.manageWorker(ctx)
		done <- nil
	}()

	waitFor(t, "spawnCount >= 3", func() bool { return spawnCount.Load() >= 3 })
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("manageWorker did not return")
	}

	mu.Lock()
	times := make([]time.Time, len(spawnTimes))
	copy(times, spawnTimes)
	mu.Unlock()

	if len(times) < 3 {
		t.Fatalf("expected at least 3 spawn times, got %d", len(times))
	}

	delay1 := times[1].Sub(times[0])
	delay2 := times[2].Sub(times[1])

	if delay2 <= delay1/2 {
		t.Errorf("expected increasing backoff delays, got delay1=%v delay2=%v", delay1, delay2)
	}
}

func TestManageWorker_SpawnFailureBackoff(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRestartDelay = 100 * time.Millisecond
	w := New(cfg)

	var spawnCount atomic.Int32
	w.specFunc = func(_ context.Context) (procguard.Spec, error) {
		count := spawnCount.Add(1)
		if count <= 2 {
			return procguard.Spec{}, errors.New("spawn failed")
		}
		cmd := helperSpec(10*time.Second, 0)
		return cmd, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		w.manageWorker(ctx)
		done <- nil
	}()

	waitFor(t, "spawnCount >= 3", func() bool { return spawnCount.Load() >= 3 })
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("manageWorker did not return")
	}

	if spawnCount.Load() < 3 {
		t.Errorf("expected at least 3 spawn attempts, got %d", spawnCount.Load())
	}
}

func TestManageWorker_NoRestartWhenShuttingDown(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRestartDelay = 100 * time.Millisecond
	w := New(cfg)

	var spawnCount atomic.Int32
	w.specFunc = func(_ context.Context) (procguard.Spec, error) {
		spawnCount.Add(1)
		cmd := helperSpec(0, 0)
		return cmd, nil
	}

	ctx := t.Context()

	done := make(chan error, 1)
	go func() {
		w.manageWorker(ctx)
		done <- nil
	}()

	waitFor(t, "spawnCount >= 1", func() bool { return spawnCount.Load() >= 1 })
	time.Sleep(20 * time.Millisecond)
	w.Shutdown()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error, got: %v", err)
		}
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("manageWorker did not stop after setting shuttingDown")
	}
}

func TestManageWorker_StableUptimeResetsBackoff(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRestartDelay = 100 * time.Millisecond
	cfg.StableThreshold = 50 * time.Millisecond
	w := New(cfg)

	var spawnCount atomic.Int32
	w.specFunc = func(_ context.Context) (procguard.Spec, error) {
		count := spawnCount.Add(1)
		var dur string
		switch count {
		case 1:
			dur = "0.01"
		case 2:
			dur = "0.08"
		default:
			dur = "10"
		}
		cmd := helperSecondsSpec(dur)
		return cmd, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		w.manageWorker(ctx)
		done <- nil
	}()

	waitFor(t, "spawnCount >= 3", func() bool { return spawnCount.Load() >= 3 })
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("manageWorker did not return")
	}

	if w.backoff.Failures() != 1 {
		t.Errorf("expected backoff failures to be 1 after stable run reset, got %d", w.backoff.Failures())
	}
}

func TestManageWorker_WorkerExitCode(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRestartDelay = 100 * time.Millisecond
	w := New(cfg)

	var spawnCount atomic.Int32
	w.specFunc = func(_ context.Context) (procguard.Spec, error) {
		count := spawnCount.Add(1)
		var cmd procguard.Spec
		if count == 1 {
			cmd = helperSpec(0, 42)
		} else {
			cmd = helperSpec(10*time.Second, 0)
		}
		return cmd, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		w.manageWorker(ctx)
		done <- nil
	}()

	waitFor(t, "spawnCount >= 2", func() bool { return spawnCount.Load() >= 2 })
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("manageWorker did not return")
	}
}

func TestWorkerCmd_NilWhenNoWorker(t *testing.T) {
	cfg := testConfig()
	w := New(cfg)

	if w.WorkerState().Running {
		t.Error("WorkerCmd should be nil when no worker is running")
	}
}

func TestCheckUpdates_CanRestartWatchdog_ExitPath(t *testing.T) {
	cfg := testConfig()
	cfg.CanInitRestartWatchdog = true
	cfg.Update = &daemonpilot.UpdateConfig{
		CheckInterval: 50 * time.Millisecond,
	}
	w := New(cfg)
	w.specFunc = mockSpecFunc(10*time.Second, 0)

	called := false
	w.updateCheckFn = func(_ context.Context) error {
		if !called {
			called = true
			return updater.ErrWatchdogRestart
		}
		return nil
	}

	ctx := t.Context()

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, daemonpilot.ErrExecRestart) {
			t.Errorf("expected ErrExecRestart, got: %v", err)
		}
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return")
	}

	if !w.needsExecRestart.Load() {
		t.Error("needsExecRestart should be true")
	}
	if !w.IsShuttingDown() {
		t.Error("should be shutting down")
	}
}

func TestCheckUpdates_CannotRestartWatchdog_ExecThroughPath(t *testing.T) {
	cfg := testConfig()
	cfg.CanInitRestartWatchdog = false
	cfg.InstallDir = t.TempDir()
	cfg.Name = "test-child"
	cfg.MaxRestartDelay = 100 * time.Millisecond
	cfg.Update = &daemonpilot.UpdateConfig{
		CheckInterval: 50 * time.Millisecond,
	}
	w := New(cfg)
	w.specFunc = mockSpecFunc(10*time.Second, 0)

	buildTestChild(t, cfg.InstallDir, cfg.Name, "run")

	called := false
	w.updateCheckFn = func(_ context.Context) error {
		if !called {
			called = true
			return updater.ErrWatchdogRestart
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(3*time.Second))
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	waitFor(t, "proxy mode", w.InProxyMode)

	cancel()

	select {
	case err := <-done:
		if errors.Is(err, daemonpilot.ErrExecRestart) {
			t.Error("should NOT return ErrExecRestart for exec-through path")
		}
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return")
	}

	if w.needsExecRestart.Load() {
		t.Error("needsExecRestart should be false for exec-through")
	}
	if !w.shuttingDown.Load() {
		t.Error("shuttingDown should be true after stopWorkerForExecThrough")
	}
}

func TestStopWorkerForExecThrough(t *testing.T) {
	w := New(testConfig())
	w.SetSpecFunc(mockSpecFunc(time.Minute, 0))
	w.workerDone = make(chan struct{})
	go func() { defer close(w.workerDone); w.manageWorker(t.Context()) }()
	waitFor(t, "worker running", func() bool { return w.WorkerState().Running })
	w.stopWorkerForExecThrough()
	if !w.IsShuttingDown() || w.WorkerState().Running {
		t.Fatal("exec-through did not join worker")
	}
	select {
	case <-w.workerDone:
	default:
		t.Fatal("worker loop not joined")
	}
}

func TestStopWorkerForExecThrough_NilCancel(t *testing.T) {
	cfg := testConfig()
	w := New(cfg)

	w.stopWorkerForExecThrough()

	if !w.shuttingDown.Load() {
		t.Error("shuttingDown should be true")
	}
}

func TestRunExecThroughChild_SpawnsChild(t *testing.T) {
	cfg := testConfig()
	cfg.InstallDir = t.TempDir()
	cfg.Name = "test-child"
	w := New(cfg)

	buildTestChild(t, cfg.InstallDir, cfg.Name, "exit0")

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(2*time.Second))
	defer cancel()

	done := make(chan struct{})
	go func() {
		w.runExecThroughChild(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("runExecThroughChild did not return")
	}

	if !w.InProxyMode() {
		t.Error("should be in proxy mode after runExecThroughChild")
	}
}

func TestRunExecThroughChild_BinaryNotFound(t *testing.T) {
	cfg := testConfig()
	cfg.InstallDir = "/nonexistent/path"
	cfg.Name = "missing-binary"
	cfg.MaxRestartDelay = 100 * time.Millisecond
	w := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(500*time.Millisecond))
	defer cancel()

	done := make(chan struct{})
	go func() {
		w.runExecThroughChild(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("runExecThroughChild should return after context timeout")
	}
}

func TestRunExecThroughChild_RestartsChildOnExit(t *testing.T) {
	cfg := testConfig()
	cfg.InstallDir = t.TempDir()
	cfg.Name = "test-child"
	cfg.MaxRestartDelay = 100 * time.Millisecond
	w := New(cfg)

	countFile := filepath.Join(cfg.InstallDir, "count")
	t.Setenv("WATCHDOG_TEST_COUNT_FILE", countFile)
	buildTestChild(t, cfg.InstallDir, cfg.Name, "count-exit1")

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(5*time.Second))
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.runExecThroughChild(ctx)
		close(done)
	}()
	childSpawns := func() int {
		// #nosec G304 -- Reads a fixture from this test's temporary directory.
		data, err := os.ReadFile(countFile)
		if err != nil {
			return 0
		}
		count := 0
		if _, err := fmt.Sscanf(string(data), "%d", &count); err != nil {
			return 0
		}
		return count
	}
	waitFor(t, "at least 2 child spawns", func() bool { return childSpawns() >= 2 })

	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("runExecThroughChild did not return after cancel")
	}
}

func TestRunExecThroughChild_GracefulShutdownOnSignal(t *testing.T) {
	cfg := testConfig()
	cfg.InstallDir = t.TempDir()
	cfg.Name = "test-child"
	cfg.GracefulTimeout = 5 * time.Second
	w := New(cfg)

	buildTestChild(t, cfg.InstallDir, cfg.Name, "run")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		w.runExecThroughChild(ctx)
		close(done)
	}()

	waitFor(t, "the child cmd to be published while the child runs", func() bool {
		return w.ChildPID() != 0
	})

	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("runExecThroughChild did not return after context cancel")
	}

	if w.ChildPID() != 0 {
		t.Error("child cmd should be nil after proxy mode exits")
	}
}

func TestRunExecThroughChild_SetsProxyMode(t *testing.T) {
	cfg := testConfig()
	cfg.InstallDir = t.TempDir()
	cfg.Name = "test-child"
	w := New(cfg)

	if w.InProxyMode() {
		t.Error("should not be in proxy mode initially")
	}

	buildTestChild(t, cfg.InstallDir, cfg.Name, "run")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		w.runExecThroughChild(ctx)
		close(done)
	}()

	waitFor(t, "proxy mode while runExecThroughChild is running", w.InProxyMode)

	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("runExecThroughChild did not return")
	}
}

func TestRunExecThroughChild_NoRestartAfterSignal(t *testing.T) {
	cfg := testConfig()
	cfg.InstallDir = t.TempDir()
	cfg.Name = "test-child"
	cfg.MaxRestartDelay = 50 * time.Millisecond
	w := New(cfg)

	buildTestChild(t, cfg.InstallDir, cfg.Name, "run")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		w.runExecThroughChild(ctx)
		close(done)
	}()

	// Cancel only once the child is actually up, or the test would prove nothing about
	// what happens to a RUNNING child on signal.
	waitFor(t, "the child to start", func() bool { return w.ChildPID() != 0 })
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("runExecThroughChild did not return after cancel")
	}

	waitFor(t, "the child cmd to be cleared after proxy mode exits", func() bool {
		return w.ChildPID() == 0
	})
}

func TestCheckUpdates_ExecThroughChild_StagedUpdate_ExitsCleanly(t *testing.T) {
	cfg := testConfig()
	cfg.ExecThrough = true
	cfg.CanInitRestartWatchdog = false
	cfg.Update = &daemonpilot.UpdateConfig{
		CheckInterval: 50 * time.Millisecond,
	}
	w := New(cfg)
	w.specFunc = mockSpecFunc(10*time.Second, 0)

	called := false
	w.updateCheckFn = func(_ context.Context) error {
		if !called {
			called = true
			return updater.ErrWatchdogRestart
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(5*time.Second))
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	select {
	case err := <-done:
		if errors.Is(err, daemonpilot.ErrExecRestart) {
			t.Error("exec-through child should NOT return ErrExecRestart")
		}
		if err != nil {
			t.Errorf("expected nil error, got: %v", err)
		}
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return")
	}

	if w.needsExecRestart.Load() {
		t.Error("needsExecRestart should be false for exec-through child")
	}
	if w.InProxyMode() {
		t.Error("exec-through child should NOT enter proxy mode")
	}
	if !w.IsShuttingDown() {
		t.Error("should be shutting down")
	}
}

func TestCheckUpdates_ExecThroughChild_NoUpdateContinuesNormally(t *testing.T) {
	cfg := testConfig()
	cfg.ExecThrough = true
	cfg.Update = &daemonpilot.UpdateConfig{
		CheckInterval: 50 * time.Millisecond,
	}
	w := New(cfg)
	w.specFunc = mockSpecFunc(10*time.Second, 0)

	checkCount := atomic.Int32{}
	w.updateCheckFn = func(_ context.Context) error {
		checkCount.Add(1)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	waitFor(t, "checkCount >= 3", func() bool { return checkCount.Load() >= 3 })
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error, got: %v", err)
		}
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return")
	}
}

func TestCheckUpdates_ExecThroughChild_FastUpdateStillWorks(t *testing.T) {
	cfg := testConfig()
	cfg.ExecThrough = true
	cfg.Update = &daemonpilot.UpdateConfig{
		CheckInterval: 50 * time.Millisecond,
	}
	w := New(cfg)

	workerRestarted := make(chan struct{}, 1)
	spawnCount := atomic.Int32{}
	w.specFunc = func(_ context.Context) (procguard.Spec, error) {
		count := spawnCount.Add(1)
		if count > 1 {
			select {
			case workerRestarted <- struct{}{}:
			default:
			}
		}
		cmd := helperSpec(30*time.Second, 0)
		return cmd, nil
	}

	triggered := false
	w.updateCheckFn = func(_ context.Context) error {
		if triggered {
			return nil
		}
		if !w.WorkerState().Running {
			return nil
		}
		triggered = true
		w.supervisor.Restart(true)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(5*time.Second))
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	select {
	case <-workerRestarted:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("worker was not restarted after fast update")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return")
	}

	if w.InProxyMode() {
		t.Error("should not be in proxy mode for fast update")
	}
}

func TestCheckUpdates_UpdateCheckError_NoRestart(t *testing.T) {
	cfg := testConfig()
	cfg.CanInitRestartWatchdog = true
	cfg.Update = &daemonpilot.UpdateConfig{
		CheckInterval: 50 * time.Millisecond,
	}
	w := New(cfg)
	w.specFunc = mockSpecFunc(10*time.Second, 0)

	checkCount := atomic.Int32{}
	w.updateCheckFn = func(_ context.Context) error {
		checkCount.Add(1)
		return errors.New("network error")
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	waitFor(t, "checkCount >= 2", func() bool { return checkCount.Load() >= 2 })
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error, got: %v", err)
		}
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return")
	}

	if w.needsExecRestart.Load() {
		t.Error("needsExecRestart should be false for non-restart errors")
	}
}

// recordTestRollbackDigest binds a hand-written .rollback file to the state
// file the way ApplyStagedUpdate does, so the restore paths accept it.
func recordTestRollbackDigest(t *testing.T, installDir, serviceName, rollbackPath string) {
	t.Helper()
	if err := daemonpilot.RecordRollbackDigest(installDir, serviceName, rollbackPath); err != nil {
		t.Fatalf("record rollback digest: %v", err)
	}
}

func TestRunExecThroughChild_RollbackOnRepeatedCrashes(t *testing.T) {
	cfg := testConfig()
	cfg.InstallDir = t.TempDir()
	cfg.Name = "test-child"
	cfg.MaxRestartDelay = 100 * time.Millisecond
	cfg.StableThreshold = 1 * time.Hour
	w := New(cfg)

	binaryPath := testInstalledBinaryPath(cfg)
	rollbackPath := testRollbackBinaryPath(cfg)
	buildTestChildAtPath(t, binaryPath, "exit1")
	buildTestChildAtPath(t, rollbackPath, "run")
	rollbackBytes, err := os.ReadFile(rollbackPath)
	if err != nil {
		t.Fatal(err)
	}
	recordTestRollbackDigest(t, cfg.InstallDir, cfg.Name, rollbackPath)

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(5*time.Second))
	defer cancel()

	done := make(chan struct{})
	go func() {
		w.runExecThroughChild(ctx)
		close(done)
	}()

	waitFor(t, "the rollback file to be renamed onto the current binary", func() bool {
		_, err := os.Stat(rollbackPath)
		return os.IsNotExist(err)
	})

	// #nosec G304 -- Reads a fixture from this test's temporary directory.
	restoredData, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("failed to read restored binary: %v", err)
	}
	if !bytes.Equal(restoredData, rollbackBytes) {
		t.Error("current binary should contain the restored executable")
	}

	cancel()
	<-done
}

func TestRunExecThroughChild_StableChildExitPreservesRollback(t *testing.T) {
	cfg := testConfig()
	cfg.InstallDir = t.TempDir()
	cfg.MaxRestartDelay = 100 * time.Millisecond
	cfg.StableThreshold = 100 * time.Millisecond
	w := New(cfg)
	binaryPath := w.installedBinaryPath()
	rollbackPath := w.rollbackBinaryPath()
	marker := filepath.Join(cfg.InstallDir, "starts")
	t.Setenv("WATCHDOG_TEST_MARKER", marker)
	buildTestChildAtPath(t, binaryPath, "marker-exit1")
	writeFile(t, rollbackPath, "PREVIOUS")
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
	waitFor(t, "restart after a stable child exits unsuccessfully", func() bool {
		// #nosec G304 -- Marker written by this test's child process.
		data, _ := os.ReadFile(marker)
		return strings.Count(string(data), "started") >= 2
	})
	if err := daemonpilot.VerifyRollbackBinary(cfg.InstallDir, cfg.Name, rollbackPath); err != nil {
		t.Fatalf("watchdog uptime must not commit worker probation: %v", err)
	}
}

func TestRunExecThroughChild_OnCriticalFailure_CalledWhenBothFail(t *testing.T) {
	cfg := testConfig()
	cfg.InstallDir = t.TempDir()
	cfg.Name = "test-child"
	cfg.MaxRestartDelay = 100 * time.Millisecond
	cfg.StableThreshold = 1 * time.Hour
	w := New(cfg)

	var criticalCalled atomic.Int32
	cfg.OnCriticalFailure = func(_ error) {
		criticalCalled.Add(1)
	}

	binaryPath := testInstalledBinaryPath(cfg)
	rollbackPath := testRollbackBinaryPath(cfg)
	buildTestChildAtPath(t, binaryPath, "exit1")
	buildTestChildAtPath(t, rollbackPath, "exit2")
	recordTestRollbackDigest(t, cfg.InstallDir, cfg.Name, rollbackPath)

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(10*time.Second))
	defer cancel()

	done := make(chan struct{})
	go func() {
		w.runExecThroughChild(ctx)
		close(done)
	}()

	waitFor(t, "OnCriticalFailure to fire when both binaries fail", func() bool {
		return criticalCalled.Load() > 0
	})

	cancel()
	<-done
}

func TestRunExecThroughChild_OnCriticalFailure_NilDoesNotPanic(t *testing.T) {
	cfg := testConfig()
	cfg.InstallDir = t.TempDir()
	cfg.Name = "test-child"
	cfg.MaxRestartDelay = 100 * time.Millisecond
	cfg.StableThreshold = 1 * time.Hour
	cfg.OnCriticalFailure = nil
	w := New(cfg)

	binaryPath := testInstalledBinaryPath(cfg)
	rollbackPath := testRollbackBinaryPath(cfg)
	buildTestChildAtPath(t, binaryPath, "exit1")
	buildTestChildAtPath(t, rollbackPath, "exit1")
	recordTestRollbackDigest(t, cfg.InstallDir, cfg.Name, rollbackPath)

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(3*time.Second))
	defer cancel()

	done := make(chan struct{})
	go func() {
		w.runExecThroughChild(ctx)
		close(done)
	}()

	<-done
}

func TestRunExecThroughChild_NoRollbackFileAvailable(t *testing.T) {
	cfg := testConfig()
	cfg.InstallDir = t.TempDir()
	cfg.Name = "test-child"
	cfg.MaxRestartDelay = 100 * time.Millisecond
	cfg.StableThreshold = 1 * time.Hour
	w := New(cfg)

	binaryPath := testInstalledBinaryPath(cfg)
	buildTestChildAtPath(t, binaryPath, "exit1")

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(3*time.Second))
	defer cancel()

	done := make(chan struct{})
	go func() {
		w.runExecThroughChild(ctx)
		close(done)
	}()

	<-done
}

func TestCheckUpdates_NoUpdateAvailable(t *testing.T) {
	cfg := testConfig()
	cfg.Update = &daemonpilot.UpdateConfig{
		CheckInterval: 50 * time.Millisecond,
	}
	w := New(cfg)
	w.specFunc = mockSpecFunc(10*time.Second, 0)

	checkCount := atomic.Int32{}
	w.updateCheckFn = func(_ context.Context) error {
		checkCount.Add(1)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	waitFor(t, "checkCount >= 3", func() bool { return checkCount.Load() >= 3 })
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error, got: %v", err)
		}
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return")
	}
}

func envSliceToMap(envSlice []string) map[string]string {
	m := make(map[string]string)
	for _, e := range envSlice {
		for i := range len(e) {
			if e[i] == '=' {
				m[e[:i]] = e[i+1:]
				break
			}
		}
	}
	return m
}

func TestSpecFunc_SetsStaticEnvVars(t *testing.T) {
	t.Setenv("GO_DAEMONPILOT_TEST_VAR", "test_value")
	t.Setenv("DAEMONPILOT_STALE_VALUE", "must_be_filtered")
	tests := []struct {
		name       string
		version    string
		wdVersion  string
		initSystem string
		canRestart bool
		execThru   bool
	}{
		{
			name:       "systemd with auto-restart",
			version:    "2.5.0",
			wdVersion:  "7",
			initSystem: "systemd",
			canRestart: true,
			execThru:   false,
		},
		{
			name:       "crontab exec-through",
			version:    "1.0.0",
			wdVersion:  "1",
			initSystem: "crontab",
			canRestart: false,
			execThru:   true,
		},
		{
			name:       "busybox no restart",
			version:    "3.1.4",
			wdVersion:  "2",
			initSystem: "busybox",
			canRestart: false,
			execThru:   false,
		},
		{
			name:       "empty init system",
			version:    "1.0.0",
			wdVersion:  "1",
			initSystem: "",
			canRestart: false,
			execThru:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Version = tt.version
			cfg.WatchdogVersion = tt.wdVersion
			cfg.InitSystemName = tt.initSystem
			cfg.CanInitRestartWatchdog = tt.canRestart
			cfg.ExecThrough = tt.execThru

			w := New(cfg)

			spec, err := w.defaultSpec(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer spec.AfterStart(0, errors.New("inspection complete"))
			if spec.Stdout != os.Stdout || spec.Stderr != os.Stderr {
				t.Fatal("worker must inherit stdout and stderr")
			}
			envMap := envSliceToMap(spec.Env)
			if envMap["GO_DAEMONPILOT_TEST_VAR"] != "test_value" {
				t.Fatal("worker did not inherit application environment")
			}
			if _, exists := envMap["DAEMONPILOT_STALE_VALUE"]; exists {
				t.Fatal("worker inherited stale daemonpilot environment")
			}

			wantVars := map[string]string{
				daemonpilot.EnvMode:            string(daemonpilot.ModeWorker),
				daemonpilot.EnvVersion:         tt.version,
				daemonpilot.EnvWatchdogVersion: tt.wdVersion,
				daemonpilot.EnvInitSystem:      tt.initSystem,
				daemonpilot.EnvCanAutoRestart:  strconv.FormatBool(tt.canRestart),
				daemonpilot.EnvIsExecThrough:   strconv.FormatBool(tt.execThru),
			}

			for key, want := range wantVars {
				got, ok := envMap[key]
				if !ok {
					t.Errorf("env var %s not set", key)
					continue
				}
				if got != want {
					t.Errorf("env var %s = %q, want %q", key, got, want)
				}
			}

			if _, ok := envMap[daemonpilot.EnvBinaryPath]; !ok {
				t.Error("env var DAEMONPILOT_BINARY_PATH not set")
			}
		})
	}
}

func TestSpecFunc_SetsParentPID(t *testing.T) {
	w := New(testConfig())
	spec, err := w.defaultSpec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer spec.AfterStart(0, errors.New("inspection complete"))
	if got := envSliceToMap(spec.Env)[daemonpilot.EnvParentPID]; got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("parent PID = %q", got)
	}
}

func TestReadySignalSetsFlag(t *testing.T) {
	cfg := testConfig()
	w := New(cfg)

	if w.workerReady.Load() {
		t.Fatal("workerReady should be false initially")
	}

	srv, pipes, err := ipc.NewServer(w.ipcDataProvider)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetReadyHandler(func() {
		w.workerReady.Store(true)
	})

	ctx := t.Context()
	go srv.Serve(ctx)

	client := ipc.NewClient(pipes.RequestW, pipes.ResponseR)
	defer func() { _ = client.Close() }()

	if err := client.NotifyReady(); err != nil {
		t.Fatalf("NotifyReady: %v", err)
	}

	waitFor(t, "workerReady after NotifyReady", w.workerReady.Load)
}

func TestReadySignalResetsBackoffOnWorkerExit(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRestartDelay = 100 * time.Millisecond
	cfg.StableThreshold = 10 * time.Minute
	w := New(cfg)
	w.backoff.Next()
	w.backoff.Next()

	var spawnCount atomic.Int32
	w.specFunc = func(_ context.Context) (procguard.Spec, error) {
		count := spawnCount.Add(1)
		if count == 1 {
			cmd := helperSpec(0, 1)
			cmd.AfterStart = func(_ int, err error) {
				if err == nil {
					w.workerReady.Store(true)
					w.supervisor.MarkStable()
				}
			}
			return cmd, nil
		}
		cmd := helperSpec(10*time.Second, 0)
		return cmd, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		w.manageWorker(ctx)
		done <- nil
	}()

	waitFor(t, "spawnCount >= 2", func() bool { return spawnCount.Load() >= 2 })
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("manageWorker did not return")
	}

	if got := w.backoff.Failures(); got != 1 {
		t.Fatalf("readiness did not reset backoff: %d", got)
	}
}

func TestDataRequestDoesNotResetBackoff(t *testing.T) {
	cfg := testConfig()
	w := New(cfg)

	w.backoff.Next()
	w.backoff.Next()
	initialFailures := w.backoff.Failures()

	srv, pipes, err := ipc.NewServer(w.ipcDataProvider)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx := t.Context()
	go srv.Serve(ctx)

	client := ipc.NewClient(pipes.RequestW, pipes.ResponseR)
	defer func() { _ = client.Close() }()

	info, err := client.Request()
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if info.WatchdogPID == 0 {
		t.Error("expected non-zero WatchdogPID from data provider")
	}

	if w.backoff.Failures() != initialFailures {
		t.Errorf("data request should not change backoff: got %d, want %d", w.backoff.Failures(), initialFailures)
	}

	if w.workerReady.Load() {
		t.Error("data request should not set workerReady flag")
	}
}

func TestStableThresholdWithoutNotifyReady(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRestartDelay = 200 * time.Millisecond
	cfg.StableThreshold = 50 * time.Millisecond
	w := New(cfg)

	var spawnCount atomic.Int32
	w.specFunc = func(_ context.Context) (procguard.Spec, error) {
		count := spawnCount.Add(1)
		var dur string
		switch count {
		case 1:
			dur = "0.01"
		case 2:
			dur = "0.08"
		default:
			dur = "10"
		}
		cmd := helperSecondsSpec(dur)
		return cmd, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		w.manageWorker(ctx)
		done <- nil
	}()

	waitFor(t, "spawnCount >= 3", func() bool { return spawnCount.Load() >= 3 })
	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("manageWorker did not return")
	}

	if w.workerReady.Load() {
		t.Error("workerReady should never have been set (NotifyReady not called)")
	}

	// Worker 1: crash (10ms < 50ms threshold) -> failures=1
	// Worker 2: stable (80ms >= 50ms threshold) -> reset to 0, then failures=1 on exit
	// Worker 3: stable (sleep 10, killed after 60ms+ >= 50ms) -> reset to 0, then failures=1
	// The StableThreshold reset happened without NotifyReady, proving backward
	// compatibility: workers are still considered stable after StableThreshold
	// even when NotifyReady is never called.
	if w.backoff.Failures() != 1 {
		t.Errorf("expected backoff failures = 1 (reset after stable worker without NotifyReady), got %d", w.backoff.Failures())
	}
}

func TestHealthCheck_GoroutineStartsWhenConfigured(t *testing.T) {
	cfg := testConfig()
	cfg.HealthInterval = 50 * time.Millisecond
	cfg.HealthFailures = 3

	var checkCount atomic.Int32
	cfg.HealthFunc = func(_ context.Context) error {
		checkCount.Add(1)
		return nil
	}

	w := New(cfg)
	w.SetSpecFunc(mockSpecFunc(10*time.Second, 0))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	waitFor(t, "at least 2 health checks", func() bool { return checkCount.Load() >= 2 })
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return")
	}
}

func TestHealthCheck_NotStartedWhenNil(t *testing.T) {
	cfg := testConfig()
	cfg.HealthFunc = nil

	w := New(cfg)
	w.SetSpecFunc(mockSpecFunc(10*time.Second, 0))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return")
	}
}

func TestHealthCheck_FailureCounterIncrements(t *testing.T) {
	cfg := testConfig()
	cfg.HealthInterval = 30 * time.Millisecond
	cfg.HealthFailures = 10

	cfg.HealthFunc = func(_ context.Context) error {
		return errors.New("unhealthy")
	}

	w := New(cfg)
	w.SetSpecFunc(mockSpecFunc(10*time.Second, 0))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	waitFor(t, "at least 3 health check failures", func() bool { return w.HealthFailures() >= 3 })
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return")
	}
}

func TestHealthCheck_SuccessResetsCounter(t *testing.T) {
	cfg := testConfig()
	cfg.HealthInterval = 30 * time.Millisecond
	cfg.HealthFailures = 10

	var callCount atomic.Int32
	cfg.HealthFunc = func(_ context.Context) error {
		n := callCount.Add(1)
		if n <= 3 {
			return errors.New("unhealthy")
		}
		return nil
	}

	w := New(cfg)
	w.SetSpecFunc(mockSpecFunc(10*time.Second, 0))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	// Wait for a check PAST the seeded failures (calls 1-3 fail, 4+ succeed) — the
	// success is what is supposed to reset the counter, so looking before it lands would
	// assert nothing.
	waitFor(t, "a healthy check after the seeded failures", func() bool { return callCount.Load() >= 4 })
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return")
	}

	failures := w.HealthFailures()
	if failures != 0 {
		t.Errorf("expected 0 health failures after success, got %d", failures)
	}
}

func TestHealthCheck_RestartsWorkerOnThresholdReached(t *testing.T) {
	cfg := testConfig()
	cfg.HealthInterval = 30 * time.Millisecond
	cfg.HealthFailures = 3
	cfg.MaxRestartDelay = 10 * time.Millisecond

	cfg.HealthFunc = func(_ context.Context) error {
		return errors.New("unhealthy")
	}

	var spawnCount atomic.Int32
	w := New(cfg)
	w.SetSpecFunc(mockSpecFuncCounting(&spawnCount, 10*time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	waitFor(t, "at least 2 worker spawns (initial + health restart)", func() bool {
		return spawnCount.Load() >= 2
	})
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return")
	}
}

func TestHealthCheck_ResetsAfterWorkerRestart(t *testing.T) {
	cfg := testConfig()
	cfg.HealthInterval = 20 * time.Millisecond
	cfg.HealthFailures = 2
	cfg.MaxRestartDelay = 10 * time.Millisecond

	var replacementStarted atomic.Bool
	var healthyChecks atomic.Int32
	cfg.HealthFunc = func(_ context.Context) error {
		if replacementStarted.Load() {
			healthyChecks.Add(1)
			return nil
		}
		return errors.New("unhealthy")
	}

	var spawnCount atomic.Int32
	w := New(cfg)
	w.SetSpecFunc(func(_ context.Context) (procguard.Spec, error) {
		n := spawnCount.Add(1)
		spec := helperSpec(10*time.Second, 0)
		if n >= 2 {
			spec.AfterStart = func(_ int, startErr error) {
				if startErr == nil {
					replacementStarted.Store(true)
				}
			}
		}
		return spec, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	waitFor(t, "the replacement worker to start", replacementStarted.Load)
	waitFor(t, "a healthy post-restart check to clear failures", func() bool {
		return healthyChecks.Load() > 0 && w.HealthFailures() == 0
	})
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return")
	}
	if failures := w.HealthFailures(); failures != 0 {
		t.Errorf("health failures after restart and healthy check = %d, want 0", failures)
	}
}

func TestHealthCheck_TableDriven(t *testing.T) {
	tests := []struct {
		name              string
		healthInterval    time.Duration
		healthFailures    int
		healthResults     []error
		waitDuration      time.Duration
		expectMinSpawns   int32
		expectMaxFailures int32
	}{
		{
			name:              "all healthy - no restarts",
			healthInterval:    30 * time.Millisecond,
			healthFailures:    3,
			healthResults:     []error{nil, nil, nil, nil, nil},
			waitDuration:      200 * time.Millisecond,
			expectMinSpawns:   1,
			expectMaxFailures: 0,
		},
		{
			name:              "failures below threshold - no restart",
			healthInterval:    30 * time.Millisecond,
			healthFailures:    5,
			healthResults:     []error{errors.New("err"), errors.New("err"), nil, nil, nil},
			waitDuration:      200 * time.Millisecond,
			expectMinSpawns:   1,
			expectMaxFailures: 0,
		},
		{
			name:              "failures reach threshold - worker restarted",
			healthInterval:    30 * time.Millisecond,
			healthFailures:    3,
			healthResults:     []error{errors.New("err"), errors.New("err"), errors.New("err")},
			waitDuration:      200 * time.Millisecond,
			expectMinSpawns:   2,
			expectMaxFailures: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.HealthInterval = tt.healthInterval
			cfg.HealthFailures = tt.healthFailures
			cfg.MaxRestartDelay = 10 * time.Millisecond

			var callIdx atomic.Int32
			results := tt.healthResults
			cfg.HealthFunc = func(_ context.Context) error {
				idx := int(callIdx.Add(1)) - 1
				if idx < len(results) {
					return results[idx]
				}
				return nil
			}

			var spawnCount atomic.Int32
			w := New(cfg)
			w.SetSpecFunc(mockSpecFuncCounting(&spawnCount, 10*time.Second))

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- w.Run(ctx)
			}()

			waitFor(t, fmt.Sprintf("at least %d spawns", tt.expectMinSpawns), func() bool {
				return spawnCount.Load() >= tt.expectMinSpawns
			})
			cancel()

			select {
			case <-done:
			case <-time.After(testsupport.Scale(waitTimeout)):
				t.Fatal("Run() did not return")
			}
		})
	}
}

func TestHealthCheck_SkippedInProxyMode(t *testing.T) {
	cfg := testConfig()
	cfg.HealthInterval = 30 * time.Millisecond
	cfg.HealthFailures = 1

	var checkCount atomic.Int32
	cfg.HealthFunc = func(_ context.Context) error {
		checkCount.Add(1)
		return errors.New("unhealthy")
	}

	w := New(cfg)
	w.inProxyMode.Store(true)
	w.SetSpecFunc(mockSpecFunc(10*time.Second, 0))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	// Deliberately a fixed elapsed wait, not waitFor: this asserts an ABSENCE (no health
	// check ever runs in proxy mode), and there is no condition to wait for. The health
	// interval is 30ms, so this window is ~6 checks' worth of opportunity.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return")
	}

	if checkCount.Load() > 0 {
		t.Errorf("expected no health checks in proxy mode, got %d", checkCount.Load())
	}

	if w.HealthFailures() != 0 {
		t.Errorf("expected 0 health failures in proxy mode, got %d", w.HealthFailures())
	}
}

func TestHealthCheck_TimeoutViaContext(t *testing.T) {
	cfg := testConfig()
	cfg.HealthInterval = 50 * time.Millisecond
	cfg.HealthFailures = 3

	var timedOut atomic.Bool
	cfg.HealthFunc = func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			timedOut.Store(true)
			return ctx.Err()
		case <-time.After(testsupport.Scale(5 * time.Second)):
			return nil
		}
	}

	w := New(cfg)
	w.SetSpecFunc(mockSpecFunc(10*time.Second, 0))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	waitFor(t, "HealthFunc to be called with a timeout context", timedOut.Load)
	cancel()

	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return")
	}
}

func TestRun_WorkerExitPermanent_StopsWatchdog(t *testing.T) {
	cfg := testConfig()
	w := New(cfg)

	var spawnCount atomic.Int32
	w.specFunc = func(_ context.Context) (procguard.Spec, error) {
		spawnCount.Add(1)
		cmd := helperSpec(0, daemonpilot.ExitPermanent)
		return cmd, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(5*time.Second))
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error for permanent stop, got: %v", err)
		}
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return after ExitPermanent")
	}

	if got := spawnCount.Load(); got != 1 {
		t.Errorf("worker spawned %d times, want 1 (no restart after ExitPermanent)", got)
	}
	if w.needsExecRestart.Load() {
		t.Error("needsExecRestart should be false for ExitPermanent")
	}
}

func TestRun_WorkerExitRespawnWatchdog_ExecRestart(t *testing.T) {
	cfg := testConfig()
	// Update goroutine present to prove stopRun() also unblocks checkUpdates.
	cfg.Update = &daemonpilot.UpdateConfig{CheckInterval: time.Hour}
	w := New(cfg)
	w.updateCheckFn = func(_ context.Context) error { return nil }

	var spawnCount atomic.Int32
	w.specFunc = func(_ context.Context) (procguard.Spec, error) {
		spawnCount.Add(1)
		cmd := helperSpec(0, daemonpilot.ExitRespawnWatchdog)
		return cmd, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(5*time.Second))
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case err := <-done:
		if !errors.Is(err, daemonpilot.ErrExecRestart) {
			t.Errorf("expected ErrExecRestart, got: %v", err)
		}
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() did not return after ExitRespawnWatchdog")
	}

	if got := spawnCount.Load(); got != 1 {
		t.Errorf("worker spawned %d times, want 1 (no restart after ExitRespawnWatchdog)", got)
	}
}

// stagedProbationConfig builds a config with StagedRollbackOnUnstable enabled
// and a short window/threshold for fast tests.
func stagedProbationConfig(t *testing.T) *daemonpilot.Config {
	t.Helper()
	cfg := testConfig()
	cfg.InstallDir = t.TempDir()
	cfg.Name = "probe-agent"
	cfg.StableThreshold = testsupport.Scale(300 * time.Millisecond)
	cfg.Update = &daemonpilot.UpdateConfig{
		CheckInterval:            time.Hour,
		RollbackTimeout:          testsupport.Scale(3 * time.Second),
		StagedRollbackOnUnstable: true,
	}
	return cfg
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	// #nosec G306 G703 -- Executable fixture path supplied by the parent test.
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// A stable worker (stays up past StableThreshold) commits the staged update:
// the .rollback net is deleted and the watchdog keeps running the new binary.
func TestProbation_StableWorker_Commits(t *testing.T) {
	cfg := stagedProbationConfig(t)
	rollbackPath := testRollbackBinaryPath(cfg)
	currentPath := testInstalledBinaryPath(cfg)
	writeFile(t, rollbackPath, "PREVIOUS")
	writeFile(t, currentPath, "NEW")

	w := New(cfg)
	w.updateCheckFn = func(_ context.Context) error { return nil }
	// Worker stays up well past StableThreshold.
	w.specFunc = mockSpecFunc(10*time.Second, 0)

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(5*time.Second))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Wait for the probation goroutine to observe a stable worker and commit.
	// The commit clears onProbation *after* deleting .rollback — the flag must
	// stay set while the net still exists, or a racing update check could stage
	// a second update over it — so wait on the flag, not on the file.
	waitFor(t, ".rollback deleted (committed) for a stable worker", func() bool {
		_, err := os.Stat(rollbackPath)
		return os.IsNotExist(err)
	})
	waitFor(t, "onProbation cleared after commit", func() bool {
		return !w.onProbation.Load()
	})
	if w.needsExecRestart.Load() {
		t.Error("a committed update must not request an exec restart")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run did not return after cancel")
	}
	// Current binary is still the NEW one.
	// #nosec G304 -- Reads a fixture from this test's temporary directory.
	if b, _ := os.ReadFile(currentPath); string(b) != "NEW" {
		t.Errorf("current binary = %q, want NEW", b)
	}
}

// A crash-looping worker that never stabilizes within the window triggers a
// rollback: the previous binary is restored over current and Run returns
// ErrExecRestart so handleRun re-execs it.
func TestProbation_CrashLoop_RollsBack(t *testing.T) {
	cfg := stagedProbationConfig(t)
	rollbackPath := testRollbackBinaryPath(cfg)
	currentPath := testInstalledBinaryPath(cfg)
	writeFile(t, rollbackPath, "PREVIOUS")
	writeFile(t, currentPath, "NEW")
	recordTestRollbackDigest(t, cfg.InstallDir, cfg.Name, rollbackPath)

	w := New(cfg)
	w.updateCheckFn = func(_ context.Context) error { return nil }
	// Worker crashes immediately every time -> never reaches StableThreshold.
	w.specFunc = mockSpecFunc(10*time.Millisecond, 1)

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(10*time.Second))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case err := <-done:
		if !errors.Is(err, daemonpilot.ErrExecRestart) {
			t.Errorf("expected ErrExecRestart after rollback, got: %v", err)
		}
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run did not return; rollback did not fire within window")
	}

	// .rollback restored over current; previous binary now at currentPath.
	if _, err := os.Stat(rollbackPath); !os.IsNotExist(err) {
		t.Error(".rollback should be consumed by the restore rename")
	}
	// #nosec G304 -- Reads a fixture from this test's temporary directory.
	if b, _ := os.ReadFile(currentPath); string(b) != "PREVIOUS" {
		t.Errorf("current binary = %q, want PREVIOUS (rolled back)", b)
	}
	if w.onProbation.Load() {
		t.Error("onProbation should be false after rollback")
	}
}

// A .rollback file that does not match the digest recorded when the staged
// update created it must not be executed: on a host where the watchdog, the
// worker and an attacker share a uid, anything else would run a binary the
// signature chain never saw. It stays on the new binary and escalates.
func TestProbation_RollbackBinaryUnverified_StaysAndEscalates(t *testing.T) {
	cfg := stagedProbationConfig(t)
	rollbackPath := testRollbackBinaryPath(cfg)
	currentPath := testInstalledBinaryPath(cfg)
	writeFile(t, rollbackPath, "PREVIOUS")
	writeFile(t, currentPath, "NEW")
	recordTestRollbackDigest(t, cfg.InstallDir, cfg.Name, rollbackPath)
	// Swap the recorded rollback copy for a planted one. The digest is only
	// consulted at rollback time, so planting it up front exercises the same
	// path as a mid-probation swap without a goroutine racing the deadline.
	writeFile(t, rollbackPath, "PLANTED")

	var criticalErr atomic.Value
	cfg.OnCriticalFailure = func(err error) { criticalErr.Store(err.Error()) }

	w := New(cfg)
	w.updateCheckFn = func(_ context.Context) error { return nil }
	w.specFunc = mockSpecFunc(10*time.Millisecond, 1) // crash-loop -> window elapses

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(10*time.Second))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitFor(t, "OnCriticalFailure to fire for an unverified rollback binary", func() bool {
		got, _ := criticalErr.Load().(string)
		return got != ""
	})
	if got, _ := criticalErr.Load().(string); !strings.Contains(got, "integrity verification") {
		t.Errorf("critical failure = %q, want it to name the failed verification", got)
	}
	if w.needsExecRestart.Load() {
		t.Error("an unverified rollback binary must not trigger an exec restart")
	}
	// #nosec G304 -- Reads a fixture from this test's temporary directory.
	if b, _ := os.ReadFile(currentPath); string(b) != "NEW" {
		t.Errorf("current binary = %q, want NEW (stayed)", b)
	}
	// The file is moved aside rather than left in place: its presence alone
	// re-arms probation at every start, so leaving it would burn a full window
	// and re-escalate forever over a file that can never be restored.
	if _, err := os.Stat(rollbackPath); !os.IsNotExist(err) {
		t.Errorf("stat(%s) = %v, want the unverifiable rollback file to be quarantined", rollbackPath, err)
	}
	// #nosec G304 -- Reads a fixture from this test's temporary directory.
	if b, _ := os.ReadFile(rollbackPath + ".invalid"); string(b) != "PLANTED" {
		t.Errorf("quarantined file = %q, want it kept for inspection", b)
	}

	cancel()
	<-done
}

// A missing rollback binary at rollback time must not exec; it stays on the new
// binary and escalates via OnCriticalFailure.
func TestProbation_RollbackBinaryMissing_StaysAndEscalates(t *testing.T) {
	cfg := stagedProbationConfig(t)
	rollbackPath := testRollbackBinaryPath(cfg)
	currentPath := testInstalledBinaryPath(cfg)
	writeFile(t, rollbackPath, "PREVIOUS")
	writeFile(t, currentPath, "NEW")

	var criticalErr atomic.Value
	cfg.OnCriticalFailure = func(err error) { criticalErr.Store(err.Error()) }

	w := New(cfg)
	w.updateCheckFn = func(_ context.Context) error { return nil }
	w.specFunc = mockSpecFunc(10*time.Millisecond, 1) // crash-loop -> window elapses

	// Remove the rollback binary after probation starts but before the window
	// elapses, so rollbackStaged finds it missing.
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = os.Remove(rollbackPath)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(10*time.Second))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// The window (3s) elapses; rollbackStaged finds no .rollback. Waiting for the
	// escalation is what proves the window actually elapsed, so the negative assertions
	// below are meaningful rather than "we happened to look too early".
	waitFor(t, "OnCriticalFailure to fire for a missing rollback binary", func() bool {
		got, _ := criticalErr.Load().(string)
		return got != ""
	})
	if w.needsExecRestart.Load() {
		t.Error("missing rollback binary must not trigger an exec restart")
	}
	// #nosec G304 -- Reads a fixture from this test's temporary directory.
	if b, _ := os.ReadFile(currentPath); string(b) != "NEW" {
		t.Errorf("current binary = %q, want NEW (stayed)", b)
	}

	cancel()
	<-done
}

// TestManageWorker_ShutdownInSpawnWindowLeavesNoWorker pins the invariant that a
// Shutdown() racing a worker spawn still stops the worker.
func TestManageWorker_ShutdownInSpawnWindowLeavesNoWorker(t *testing.T) {
	for _, stage := range []string{"factory", "after-start"} {
		t.Run(stage, func(t *testing.T) {
			w := New(testConfig())
			var attempts atomic.Int32
			w.SetSpecFunc(func(context.Context) (procguard.Spec, error) {
				attempts.Add(1)
				spec := helperSpec(time.Minute, 0)
				if stage == "factory" {
					w.Shutdown()
				} else {
					spec.AfterStart = func(_ int, _ error) { w.Shutdown() }
				}
				return spec, nil
			})
			done := make(chan struct{})
			go func() { w.manageWorker(t.Context()); close(done) }()
			select {
			case <-done:
			case <-time.After(testsupport.Scale(waitTimeout)):
				t.Fatal("Shutdown racing spawn left worker running")
			}
			state := w.WorkerState()
			if attempts.Load() != 1 || state.Running || state.PID != 0 {
				t.Fatalf("after shutdown: attempts=%d state=%+v", attempts.Load(), state)
			}
			if stage == "after-start" && (state.Last == nil || !state.Last.Requested) {
				t.Fatalf("spawned worker was not stopped: %+v", state)
			}
		})
	}
}

func TestHandleExecThroughMaxBackoff(t *testing.T) {
	tests := []struct {
		name        string
		plant       string
		wantRestore bool
	}{
		{name: "a verified rollback copy is restored", wantRestore: true},
		{name: "an unverified rollback copy is refused", plant: "PLANTED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.InstallDir = t.TempDir()
			binaryPath := testInstalledBinaryPath(cfg)
			rollbackPath := testRollbackBinaryPath(cfg)
			writeFile(t, binaryPath, "NEW")
			writeFile(t, rollbackPath, "PREVIOUS")
			recordTestRollbackDigest(t, cfg.InstallDir, cfg.Name, rollbackPath)
			if tt.plant != "" {
				writeFile(t, rollbackPath, tt.plant)
			}

			w := New(cfg)
			backoff := procguard.NewBackoff(cfg.MaxRestartDelay, cfg.StableThreshold)
			for !backoff.Saturated() {
				backoff.Next()
			}

			if got := w.handleExecThroughMaxBackoff(backoff, false, rollbackPath); got != tt.wantRestore {
				t.Fatalf("handleExecThroughMaxBackoff() = %v, want %v", got, tt.wantRestore)
			}

			state, err := daemonpilot.LoadState(cfg.InstallDir, cfg.Name)
			if err != nil {
				t.Fatalf("load state: %v", err)
			}
			if !tt.wantRestore {
				// The copy is quarantined by CheckRollbackFile, not here; this
				// path only refuses to execute it.
				// #nosec G304 -- Reads a fixture from this test's temporary directory.
				if got, _ := os.ReadFile(binaryPath); string(got) != "NEW" {
					t.Errorf("binary = %q, want the unverified copy not to have been restored", got)
				}
				return
			}
			// #nosec G304 -- Reads a fixture from this test's temporary directory.
			if got, _ := os.ReadFile(binaryPath); string(got) != "PREVIOUS" {
				t.Errorf("binary = %q, want the rollback copy restored", got)
			}
			if state != nil && state.RollbackDigest != "" {
				t.Errorf("rollback digest = %q, want it cleared once the copy was consumed", state.RollbackDigest)
			}
		})
	}
}

// A digest that could not be *read* is not the same verdict as one that did not
// match: the state file is rewritten in place, so a rollback racing an install
// or update-config can observe it truncated. It still stays on the new binary
// and escalates, but the copy must survive for the next attempt.
func TestProbation_RollbackDigestUnreadable_KeepsTheCopy(t *testing.T) {
	cfg := stagedProbationConfig(t)
	rollbackPath := testRollbackBinaryPath(cfg)
	currentPath := testInstalledBinaryPath(cfg)
	writeFile(t, rollbackPath, "PREVIOUS")
	writeFile(t, currentPath, "NEW")
	recordTestRollbackDigest(t, cfg.InstallDir, cfg.Name, rollbackPath)
	// A torn read of the state file: valid JSON is what a complete write leaves.
	writeFile(t, daemonpilot.StateFilePath(cfg.InstallDir, cfg.Name), `{"rollback_dig`)

	var criticalErr atomic.Value
	cfg.OnCriticalFailure = func(err error) { criticalErr.Store(err.Error()) }

	w := New(cfg)
	w.updateCheckFn = func(_ context.Context) error { return nil }
	w.specFunc = mockSpecFunc(10*time.Millisecond, 1) // crash-loop -> window elapses

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(10*time.Second))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitFor(t, "OnCriticalFailure to fire for an unreadable rollback digest", func() bool {
		got, _ := criticalErr.Load().(string)
		return got != ""
	})
	if w.needsExecRestart.Load() {
		t.Error("an unverified rollback binary must not trigger an exec restart")
	}
	// #nosec G304 -- Reads a fixture from this test's temporary directory.
	if b, _ := os.ReadFile(currentPath); string(b) != "NEW" {
		t.Errorf("current binary = %q, want NEW (stayed)", b)
	}
	// #nosec G304 -- Reads a fixture from this test's temporary directory.
	if b, _ := os.ReadFile(rollbackPath); string(b) != "PREVIOUS" {
		t.Errorf("rollback copy = %q, want it left in place for a later attempt", b)
	}
	if _, err := os.Stat(rollbackPath + ".invalid"); !os.IsNotExist(err) {
		t.Error("the rollback copy was quarantined over a read failure")
	}

	cancel()
	<-done
}

// A staged update asks the watchdog to exit so the init system can re-exec it. That
// only happens if Run() actually RETURNS, and Run() returns only after wg.Wait() —
// which waits on every goroutine it started, including runHealthChecks.
//
// This is the same journey as TestCheckUpdates_CanRestartWatchdog_ExitPath with one
// field added: a HealthFunc. That single field is the difference between the test
// that passed for the life of this code and the node that hung in the field, because
// the branch used Shutdown() — which cancels the WORKER's context — where it needed
// stopRun(), which cancels the RUN context that runHealthChecks is selecting on. The
// worker was stopped and reaped, and then nothing happened, forever: a live process
// holding the instance lock, with no worker and no supervised children.
//
// Assert on Run() returning, not on needsExecRestart: the flag was always set
// correctly. It was the exit that never came.
func TestCheckUpdates_ExecRestartCompletesWhenAHealthFuncIsConfigured(t *testing.T) {
	cfg := testConfig()
	cfg.CanInitRestartWatchdog = true
	cfg.HealthFunc = func(context.Context) error { return nil }
	cfg.HealthInterval = 50 * time.Millisecond
	cfg.HealthFailures = 3
	cfg.Update = &daemonpilot.UpdateConfig{CheckInterval: 50 * time.Millisecond}

	w := New(cfg)
	w.specFunc = mockSpecFunc(10*time.Second, 0)

	called := false
	w.updateCheckFn = func(context.Context) error {
		if !called {
			called = true
			return updater.ErrWatchdogRestart
		}
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- w.Run(t.Context()) }()

	select {
	case err := <-done:
		if !errors.Is(err, daemonpilot.ErrExecRestart) {
			t.Errorf("expected ErrExecRestart, got: %v", err)
		}
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() never returned, so the caller never re-execs: the staged-update " +
			"branch stopped only the worker and wg.Wait() is still waiting on runHealthChecks")
	}
}

// The exec-through sibling of the branch above stops for a different reason — the root
// parent restarts it rather than the init system — but it exits through the same
// wg.Wait(), so it hangs on a HealthFunc for the same reason. It must return, and it
// must NOT ask its caller to re-exec.
func TestCheckUpdates_ExecThroughStagedUpdateStopsWhenAHealthFuncIsConfigured(t *testing.T) {
	cfg := testConfig()
	cfg.ExecThrough = true
	cfg.HealthFunc = func(context.Context) error { return nil }
	cfg.HealthInterval = 50 * time.Millisecond
	cfg.HealthFailures = 3
	cfg.Update = &daemonpilot.UpdateConfig{CheckInterval: 50 * time.Millisecond}

	w := New(cfg)
	w.specFunc = mockSpecFunc(10*time.Second, 0)

	called := false
	w.updateCheckFn = func(context.Context) error {
		if !called {
			called = true
			return updater.ErrWatchdogRestart
		}
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- w.Run(t.Context()) }()

	select {
	case err := <-done:
		if errors.Is(err, daemonpilot.ErrExecRestart) {
			t.Error("an exec-through child must not ask its caller to re-exec")
		}
	case <-time.After(testsupport.Scale(waitTimeout)):
		t.Fatal("Run() never returned: the exec-through staged-update branch stopped " +
			"only the worker and wg.Wait() is still waiting on runHealthChecks")
	}
}

func helperSpec(delay time.Duration, code int) procguard.Spec {
	return procguard.Spec{Path: os.Args[0], Args: []string{"-test.run=^TestWorkerHelper$", "--", delay.String(), strconv.Itoa(code)}, Env: append(os.Environ(), "GORACE=atexit_sleep_ms=0"), GracePeriod: 100 * time.Millisecond}
}

func helperSecondsSpec(seconds string) procguard.Spec {
	duration, err := time.ParseDuration(seconds + "s")
	if err != nil {
		panic(err)
	}
	return helperSpec(duration, 0)
}

func TestWorkerHelper(t *testing.T) {
	if len(os.Args) != 5 || os.Args[2] != "--" {
		return
	}
	delay, err := time.ParseDuration(os.Args[3])
	if err != nil {
		t.Fatal(err)
	}
	code, err := strconv.Atoi(os.Args[4])
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(delay)
	os.Exit(code)
}

func TestProbation_ReadyWorkerSoak(t *testing.T) {
	t.Parallel()
	for _, execThrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("exec-through=%t", execThrough), func(t *testing.T) {
			t.Parallel()
			cfg := stagedProbationConfig(t)
			cfg.ExecThrough = execThrough
			cfg.StableThreshold = time.Hour
			cfg.Update.RollbackTimeout = 45 * time.Second
			w := New(cfg)
			rollbackPath := w.rollbackBinaryPath()
			writeFile(t, rollbackPath, "PREVIOUS")
			recordTestRollbackDigest(t, cfg.InstallDir, cfg.Name, rollbackPath)
			w.SetSpecFunc(mockSpecFunc(time.Minute, 0))
			w.updateCheckFn = func(context.Context) error { return nil }
			ctx, cancel := context.WithTimeout(t.Context(), testsupport.Scale(40*time.Second))
			done := make(chan error, 1)
			go func() { done <- w.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(testsupport.Scale(waitTimeout)):
					t.Error("watchdog did not stop")
				}
			})
			waitFor(t, "running worker", func() bool { return w.WorkerState().Running })
			w.workerEverReady.Store(true)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					t.Fatal("ready worker did not commit after the readiness soak")
				case <-ticker.C:
					if _, err := os.Stat(rollbackPath); !os.IsNotExist(err) {
						continue
					}
					if uptime := time.Since(w.WorkerState().StartedAt); uptime < probationReadySoak {
						t.Fatalf("readiness committed before the soak: uptime=%v", uptime)
					}
					return
				}
			}
		})
	}
}

func TestDefaultSpecCgroupWiring(t *testing.T) {
	ctx := context.Background()

	cfg := testConfig()
	cfg.WorkerCgroupMemoryMax = 64 << 20
	cfg.WorkerCgroupParent = "/sys/fs/cgroup/delegated"
	w := New(cfg)
	spec, err := w.defaultSpec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Cgroup == nil {
		t.Fatal("cgroup settings must reach the worker spec")
	}
	if spec.Cgroup.MemoryMax != 64<<20 || spec.Cgroup.Parent != "/sys/fs/cgroup/delegated" {
		t.Fatalf("cgroup spec = %+v", spec.Cgroup)
	}

	bare := New(testConfig())
	spec, err = bare.defaultSpec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Cgroup != nil {
		t.Fatalf("zero cgroup settings must not request isolation, got %+v", spec.Cgroup)
	}
}

// A planted symlink at the rollback path arms probation (Lstat) and the
// expiry path must carry it all the way to quarantine: Lstat in
// rollbackStaged lets the digest verification refuse it — a Stat there would
// follow the link, report the leaf missing, and leave the symlink to re-arm
// probation at every start.
func TestStagedProbationArmsOnSymlinkForQuarantine(t *testing.T) {
	cfg := testConfig()
	cfg.InstallDir = t.TempDir()
	cfg.Update = &daemonpilot.UpdateConfig{StagedRollbackOnUnstable: true}
	var critical atomic.Value
	cfg.OnCriticalFailure = func(err error) { critical.Store(err) }
	w := New(cfg)

	rollback := w.rollbackBinaryPath()
	if err := os.Symlink(filepath.Join(cfg.InstallDir, "missing-target"), rollback); err != nil {
		t.Fatal(err)
	}

	path, ok := w.stagedProbationPath()
	if !ok || path != rollback {
		t.Fatalf("stagedProbationPath = (%q, %v), want the symlink armed", path, ok)
	}

	// The expiry path resolves the armed probation: verification refuses the
	// non-regular file, quarantines it, and escalates — the symlink does not
	// survive to re-arm the next start.
	w.rollbackStaged(rollback)

	if _, err := os.Lstat(rollback); !os.IsNotExist(err) {
		t.Fatalf("symlink must be quarantined away, got err = %v", err)
	}
	if _, err := os.Lstat(rollback + ".invalid"); err != nil {
		t.Fatalf("quarantine copy must exist: %v", err)
	}
	criticalErr, _ := critical.Load().(error)
	if !errors.Is(criticalErr, daemonpilot.ErrRollbackUnverified) {
		t.Fatalf("OnCriticalFailure = %v, want ErrRollbackUnverified", criticalErr)
	}
}

// A probation window that cannot host the uptime fallback is reported, not
// corrected silently: readiness plus the soak still commits, but the warning
// names the consequence for operators relying on uptime alone.
func TestProbationWarnsWhenUptimeFallbackImpossible(t *testing.T) {
	var logs bytes.Buffer
	cfg := testConfig()
	cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	cfg.Update = &daemonpilot.UpdateConfig{StagedRollbackOnUnstable: true, RollbackTimeout: time.Second}
	w := New(cfg)

	w.runProbation(context.Background(), filepath.Join(t.TempDir(), "absent.rollback"))

	if !strings.Contains(logs.String(), "uptime fallback cannot commit") {
		t.Fatalf("probation must warn about an impossible uptime fallback, logs:\n%s", logs.String())
	}
}
