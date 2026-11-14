package daemonpilot

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/ipc"
)

func setupIPCServer(t *testing.T, provider ipc.DataProvider) *ipc.PipeFiles {
	t.Helper()
	srv, pipes, err := ipc.NewServer(provider)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx)
	t.Cleanup(func() {
		cancel()
		<-srv.Done()
	})

	return pipes
}

func setupNoIPC(t *testing.T) {
	t.Helper()
	origFactory := ipcClientFactory
	resetIPCClient()
	t.Cleanup(func() {
		ipcClientFactory = origFactory
		resetIPCClient()
	})
	ipcClientFactory = func() (*ipc.Client, error) {
		return nil, &noIPCError{}
	}
}

func setupEnv(t *testing.T, env map[string]string) {
	t.Helper()
	origLookup := envLookup
	t.Cleanup(func() { envLookup = origLookup })
	envLookup = func(key string) string { return env[key] }
}

type noIPCError struct{}

func (e *noIPCError) Error() string { return "no IPC available" }

func TestGetRuntimeInfoStaticOnly(t *testing.T) {
	setupEnv(t, map[string]string{
		EnvMode:            "worker",
		EnvVersion:         "1.0.0",
		EnvWatchdogVersion: "2",
		EnvBinaryPath:      "/opt/myapp/myapp",
		EnvInitSystem:      "systemd",
		EnvCanAutoRestart:  "true",
		EnvIsExecThrough:   "false",
	})
	setupNoIPC(t)

	info, err := GetRuntimeInfo()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Mode != ModeWorker {
		t.Errorf("Mode = %q, want %q", info.Mode, ModeWorker)
	}
	if info.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", info.Version, "1.0.0")
	}
	if info.WatchdogVersion != "2" {
		t.Errorf("WatchdogVersion = %q, want %q", info.WatchdogVersion, "2")
	}
	if info.BinaryPath != "/opt/myapp/myapp" {
		t.Errorf("BinaryPath = %q, want %q", info.BinaryPath, "/opt/myapp/myapp")
	}
	if info.InitSystem != "systemd" {
		t.Errorf("InitSystem = %q, want %q", info.InitSystem, "systemd")
	}
	if !info.CanAutoRestart {
		t.Error("expected CanAutoRestart = true")
	}
	if info.WatchdogPID != 0 {
		t.Errorf("WatchdogPID = %d, want 0 (no IPC)", info.WatchdogPID)
	}
	if info.RestartCount != 0 {
		t.Errorf("RestartCount = %d, want 0 (no IPC)", info.RestartCount)
	}
}

func TestGetRuntimeInfoWithIPC(t *testing.T) {
	setupEnv(t, map[string]string{
		EnvMode:            "worker",
		EnvVersion:         "2.0.0",
		EnvWatchdogVersion: "3",
		EnvBinaryPath:      "/usr/bin/myapp",
		EnvInitSystem:      "busybox",
		EnvCanAutoRestart:  "false",
		EnvIsExecThrough:   "true",
	})

	now := time.Now()
	watchdogStart := now.Add(-10 * time.Minute)
	workerStart := now.Add(-5 * time.Minute)

	pipes := setupIPCServer(t, func() ipc.DynamicInfo {
		return ipc.DynamicInfo{
			WatchdogPID:        1000,
			WorkerPID:          2000,
			WatchdogStartTime:  watchdogStart,
			WorkerStartTime:    workerStart,
			RestartCount:       3,
			CurrentBackoff:     4 * time.Second,
			HealthTimestamp:    now.Add(-30 * time.Second),
			HealthError:        "connection refused",
			HealthFailures:     2,
			UpdateLastCheck:    now.Add(-time.Minute),
			UpdateResult:       "available",
			UpdateAvailableVer: "2.1.0",
			UpdateStatus:       "downloading",
		}
	})

	origFactory := ipcClientFactory
	resetIPCClient()
	t.Cleanup(func() {
		ipcClientFactory = origFactory
		resetIPCClient()
	})
	ipcClientFactory = func() (*ipc.Client, error) {
		return ipc.NewClient(pipes.RequestW, pipes.ResponseR), nil
	}

	info, err := GetRuntimeInfo()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if info.Mode != ModeWorker {
		t.Errorf("Mode = %q, want %q", info.Mode, ModeWorker)
	}
	if info.Version != "2.0.0" {
		t.Errorf("Version = %q, want %q", info.Version, "2.0.0")
	}
	if !info.IsExecThrough {
		t.Error("expected IsExecThrough = true")
	}
	if info.InitSystem != "busybox" {
		t.Errorf("InitSystem = %q, want %q", info.InitSystem, "busybox")
	}
	if info.WatchdogPID != 1000 {
		t.Errorf("WatchdogPID = %d, want 1000", info.WatchdogPID)
	}
	if info.WorkerPID != 2000 {
		t.Errorf("WorkerPID = %d, want 2000", info.WorkerPID)
	}
	if info.RestartCount != 3 {
		t.Errorf("RestartCount = %d, want 3", info.RestartCount)
	}
	if info.CurrentBackoff != 4*time.Second {
		t.Errorf("CurrentBackoff = %v, want 4s", info.CurrentBackoff)
	}
	if info.WatchdogUptime < 9*time.Minute {
		t.Errorf("WatchdogUptime = %v, expected >= 9m", info.WatchdogUptime)
	}
	if info.WorkerUptime < 4*time.Minute {
		t.Errorf("WorkerUptime = %v, expected >= 4m", info.WorkerUptime)
	}
	if info.LastHealthCheck.Error != "connection refused" {
		t.Errorf("LastHealthCheck.Error = %q, want %q", info.LastHealthCheck.Error, "connection refused")
	}
	if info.LastHealthCheck.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2", info.LastHealthCheck.ConsecutiveFailures)
	}
	if info.UpdateStatus.Result != "available" {
		t.Errorf("UpdateResult = %q, want %q", info.UpdateStatus.Result, "available")
	}
	if info.UpdateStatus.AvailableVersion != "2.1.0" {
		t.Errorf("AvailableVersion = %q, want %q", info.UpdateStatus.AvailableVersion, "2.1.0")
	}
	if info.UpdateStatus.CurrentStatus != "downloading" {
		t.Errorf("CurrentStatus = %q, want %q", info.UpdateStatus.CurrentStatus, "downloading")
	}
}

func TestGetRuntimeInfoNoEnvVars(t *testing.T) {
	setupEnv(t, map[string]string{})
	setupNoIPC(t)

	info, err := GetRuntimeInfo()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Mode != "" {
		t.Errorf("Mode = %q, want empty", info.Mode)
	}
	if info.Version != "" {
		t.Errorf("Version = %q, want empty", info.Version)
	}
}

func TestGetRuntimeInfoThreadSafety(t *testing.T) {
	setupEnv(t, map[string]string{
		EnvMode:    "worker",
		EnvVersion: "1.0.0",
	})

	pipes := setupIPCServer(t, func() ipc.DynamicInfo {
		return ipc.DynamicInfo{
			WatchdogPID:  42,
			RestartCount: 7,
		}
	})

	origFactory := ipcClientFactory
	resetIPCClient()
	t.Cleanup(func() {
		ipcClientFactory = origFactory
		resetIPCClient()
	})
	ipcClientFactory = func() (*ipc.Client, error) {
		return ipc.NewClient(pipes.RequestW, pipes.ResponseR), nil
	}

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()
			info, err := GetRuntimeInfo()
			if err != nil {
				errs <- err
				return
			}
			if info.Mode != ModeWorker {
				errs <- &testError{"unexpected mode: " + string(info.Mode)}
				return
			}
			if info.WatchdogPID != 42 {
				errs <- &testError{"unexpected WatchdogPID"}
				return
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("goroutine error: %v", err)
	}
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

func TestGetRuntimeInfoReturnsSnapshot(t *testing.T) {
	setupEnv(t, map[string]string{
		EnvMode:    "worker",
		EnvVersion: "1.0.0",
	})

	var mu sync.Mutex
	restartCount := 1

	pipes := setupIPCServer(t, func() ipc.DynamicInfo {
		mu.Lock()
		defer mu.Unlock()
		return ipc.DynamicInfo{
			WatchdogPID:  100,
			RestartCount: restartCount,
		}
	})

	origFactory := ipcClientFactory
	resetIPCClient()
	t.Cleanup(func() {
		ipcClientFactory = origFactory
		resetIPCClient()
	})
	ipcClientFactory = func() (*ipc.Client, error) {
		return ipc.NewClient(pipes.RequestW, pipes.ResponseR), nil
	}

	info1, err := GetRuntimeInfo()
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if info1.RestartCount != 1 {
		t.Errorf("first RestartCount = %d, want 1", info1.RestartCount)
	}

	mu.Lock()
	restartCount = 5
	mu.Unlock()

	info2, err := GetRuntimeInfo()
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if info2.RestartCount != 5 {
		t.Errorf("second RestartCount = %d, want 5", info2.RestartCount)
	}

	if info1.RestartCount != 1 {
		t.Error("first snapshot was mutated")
	}
}
