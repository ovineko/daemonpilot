package daemonpilot

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/ovineko/daemonpilot/internal/daemon"
)

func TestIntegration_InstallRunUninstall(t *testing.T) {
	saved := saveCommandVars()
	defer restoreCommandVars2(saved)
	sh, nu, nw, es, pm := saveRunVars()
	defer restoreRunVars(sh, nu, nw, es, pm)
	saveIST := initSystemTypeFn
	defer func() { initSystemTypeFn = saveIST }()
	initSystemTypeFn = func(mgr daemon.ServiceManager) string { return "systemd" }

	mock := &mockServiceManager{}
	var initCalled bool

	detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { initCalled = true; return nil }
	getExecutable = func() (string, error) { return "/usr/bin/testapp", nil }
	mkdirAll = func(path string, perm os.FileMode) error { return nil }
	copyFileFn = func(src, dst string) error { return nil }
	saveStateFn = func(installDir string, state *StateData) error { return nil }
	deleteStateFn = func(installDir, serviceName string) error { return nil }
	loadStateFn = installedState("testapp")

	cfg := &Config{
		Name:        "testapp",
		Description: "Integration test app",
		InstallDir:  "/opt/testapp",
		WorkerFunc:  func(ctx context.Context) error { <-ctx.Done(); return nil },
		Logger:      NewDefaultLogger(LogLevelInfo),
	}

	if err := handleInstall(cfg); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	if !mock.installCalled {
		t.Fatal("expected Install to be called during install phase")
	}

	mr := &mockRunner{}
	setupSignalHandler = fakeSignalHandler
	newWatchdogFn = func(c *Config) Runner { return mr }

	if err := handleRun(cfg); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if !mr.runCalled {
		t.Fatal("expected watchdog Run to be called during run phase")
	}

	if err := handleUninstall(cfg); err != nil {
		t.Fatalf("uninstall failed: %v", err)
	}
	if !mock.uninstallCalled {
		t.Fatal("expected Uninstall to be called during uninstall phase")
	}
	if !initCalled {
		t.Fatal("expected InitServiceManager to be called during uninstall phase")
	}
}

func TestIntegration_FullLifecycle(t *testing.T) {
	saved := saveCommandVars()
	defer restoreCommandVars2(saved)
	origWriter := saveStdoutWriter()
	defer restoreStdoutWriter(origWriter)
	saveIST := initSystemTypeFn
	defer func() { initSystemTypeFn = saveIST }()
	initSystemTypeFn = func(mgr daemon.ServiceManager) string { return "systemd" }

	var callOrder []string
	inner := &mockServiceManager{
		statusResult: daemon.ServiceStatusRunning,
		manifestPath: "/etc/systemd/system/testapp.service",
	}
	tracker := &orderTrackingManager{inner: inner, callOrder: &callOrder}

	detectManager = func() (daemon.ServiceManager, error) { return tracker, nil }
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
	getExecutable = func() (string, error) { return "/usr/bin/testapp", nil }
	mkdirAll = func(path string, perm os.FileMode) error { return nil }
	copyFileFn = func(src, dst string) error { return nil }
	saveStateFn = func(installDir string, state *StateData) error { return nil }
	deleteStateFn = func(installDir, serviceName string) error { return nil }
	loadStateFn = installedState("testapp")

	cfg := &Config{
		Name:        "testapp",
		Description: "Full lifecycle test",
		InstallDir:  "/opt/testapp",
		Logger:      NewDefaultLogger(LogLevelInfo),
	}

	var buf bytes.Buffer
	stdoutWriter = &buf

	// install (calls install, enable, start)
	if err := handleInstall(cfg); err != nil {
		t.Fatalf("install failed: %v", err)
	}

	// status (should be running)
	buf.Reset()
	if err := handleStatus(cfg); err != nil {
		t.Fatalf("status failed: %v", err)
	}
	output := buf.String()
	if !contains(output, "running") {
		t.Errorf("expected status output to contain 'running', got %q", output)
	}

	// stop
	if err := handleStop(cfg); err != nil {
		t.Fatalf("stop failed: %v", err)
	}

	// status after stop (simulate stopped)
	inner.statusResult = daemon.ServiceStatusStopped
	buf.Reset()
	if err := handleStatus(cfg); err != nil {
		t.Fatalf("status after stop failed: %v", err)
	}
	output = buf.String()
	if !contains(output, "stopped") {
		t.Errorf("expected status output to contain 'stopped', got %q", output)
	}

	// start
	inner.statusResult = daemon.ServiceStatusRunning
	if err := handleStart(cfg); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	// status after start (should be running again)
	buf.Reset()
	if err := handleStatus(cfg); err != nil {
		t.Fatalf("status after start failed: %v", err)
	}
	output = buf.String()
	if !contains(output, "running") {
		t.Errorf("expected status to be running after start, got %q", output)
	}

	// uninstall (should stop then uninstall)
	callOrder = nil
	if err := handleUninstall(cfg); err != nil {
		t.Fatalf("uninstall failed: %v", err)
	}

	// verify the full call sequence included all expected operations
	if !inner.installCalled {
		t.Error("expected Install to be called")
	}
	if !inner.startCalled {
		t.Error("expected Start to be called")
	}
	if !inner.stopCalled {
		t.Error("expected Stop to be called")
	}
	if !inner.statusCalled {
		t.Error("expected Status to be called")
	}
	if !inner.uninstallCalled {
		t.Error("expected Uninstall to be called")
	}
}

func TestIntegration_EnableDisableToggle(t *testing.T) {
	saved := saveCommandVars()
	defer restoreCommandVars2(saved)
	saveIST := initSystemTypeFn
	defer func() { initSystemTypeFn = saveIST }()
	initSystemTypeFn = func(mgr daemon.ServiceManager) string { return "systemd" }

	var callOrder []string
	inner := &mockServiceManager{}
	tracker := &orderTrackingManager{inner: inner, callOrder: &callOrder}

	detectManager = func() (daemon.ServiceManager, error) { return tracker, nil }
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
	getExecutable = func() (string, error) { return "/usr/bin/testapp", nil }
	mkdirAll = func(path string, perm os.FileMode) error { return nil }
	copyFileFn = func(src, dst string) error { return nil }
	saveStateFn = func(installDir string, state *StateData) error { return nil }

	cfg := &Config{
		Name:        "testapp",
		Description: "Enable/disable test",
		InstallDir:  "/opt/testapp",
		Logger:      NewDefaultLogger(LogLevelInfo),
	}

	// install first
	if err := handleInstall(cfg); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	if !inner.enableCalled {
		t.Fatal("expected Enable to be called during install")
	}

	// reset tracking
	inner.enableCalled = false
	inner.disableCalled = false
	callOrder = nil

	// disable
	if err := handleDisable(cfg); err != nil {
		t.Fatalf("disable failed: %v", err)
	}
	if !inner.disableCalled {
		t.Error("expected Disable to be called")
	}

	// enable again
	if err := handleEnable(cfg); err != nil {
		t.Fatalf("enable failed: %v", err)
	}
	if !inner.enableCalled {
		t.Error("expected Enable to be called")
	}

	// verify order: disable then enable
	if len(callOrder) != 2 || callOrder[0] != "disable" || callOrder[1] != "enable" {
		t.Errorf("expected call order [disable, enable], got %v", callOrder)
	}
}

func TestIntegration_UninstallStopsServiceFirst(t *testing.T) {
	saved := saveCommandVars()
	defer restoreCommandVars2(saved)

	var callOrder []string
	inner := &mockServiceManager{}
	tracker := &orderTrackingManager{inner: inner, callOrder: &callOrder}

	detectManager = func() (daemon.ServiceManager, error) { return tracker, nil }
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
	deleteStateFn = func(installDir, serviceName string) error { return nil }
	loadStateFn = installedState("testapp")

	cfg := &Config{
		Name:   "testapp",
		Logger: NewDefaultLogger(LogLevelInfo),
	}

	if err := handleUninstall(cfg); err != nil {
		t.Fatalf("uninstall failed: %v", err)
	}

	// verify stop is called before uninstall
	if len(callOrder) < 2 {
		t.Fatalf("expected at least 2 calls, got %d: %v", len(callOrder), callOrder)
	}

	stopIdx := -1
	uninstallIdx := -1
	for i, call := range callOrder {
		if call == "stop" && stopIdx == -1 {
			stopIdx = i
		}
		if call == "uninstall" && uninstallIdx == -1 {
			uninstallIdx = i
		}
	}

	if stopIdx == -1 {
		t.Fatal("expected Stop to be called during uninstall")
	}
	if uninstallIdx == -1 {
		t.Fatal("expected Uninstall to be called during uninstall")
	}
	if stopIdx >= uninstallIdx {
		t.Errorf("expected Stop (index %d) before Uninstall (index %d)", stopIdx, uninstallIdx)
	}
}

func TestIntegration_Restart(t *testing.T) {
	saved := saveCommandVars()
	defer restoreCommandVars2(saved)

	var callOrder []string
	inner := &mockServiceManager{}
	tracker := &orderTrackingManager{inner: inner, callOrder: &callOrder}

	detectManager = func() (daemon.ServiceManager, error) { return tracker, nil }
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }

	cfg := &Config{
		Name:   "testapp",
		Logger: NewDefaultLogger(LogLevelInfo),
	}

	if err := handleRestart(cfg); err != nil {
		t.Fatalf("restart failed: %v", err)
	}

	if !inner.stopCalled {
		t.Error("expected Stop to be called during restart")
	}
	if !inner.startCalled {
		t.Error("expected Start to be called during restart")
	}

	// verify stop comes before start
	stopIdx := -1
	startIdx := -1
	for i, call := range callOrder {
		if call == "stop" && stopIdx == -1 {
			stopIdx = i
		}
		if call == "start" && startIdx == -1 {
			startIdx = i
		}
	}
	if stopIdx >= startIdx {
		t.Errorf("expected Stop (index %d) before Start (index %d)", stopIdx, startIdx)
	}
}

func TestIntegration_StatusNonInstalledService(t *testing.T) {
	saved := saveCommandVars()
	defer restoreCommandVars2(saved)

	detectManager = func() (daemon.ServiceManager, error) {
		return nil, errors.New("no supported init system found")
	}
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }

	cfg := &Config{
		Name:   "nonexistent",
		Logger: NewDefaultLogger(LogLevelInfo),
	}

	err := handleStatus(cfg)
	if err == nil {
		t.Fatal("expected error for status on non-installed service")
	}
	if !contains(err.Error(), "detecting service manager") {
		t.Errorf("expected detect error, got: %v", err)
	}
}

func TestIntegration_StartStopNonInstalledService(t *testing.T) {
	saved := saveCommandVars()
	defer restoreCommandVars2(saved)

	detectManager = func() (daemon.ServiceManager, error) {
		return nil, errors.New("no supported init system found")
	}
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }

	cfg := &Config{
		Name:   "nonexistent",
		Logger: NewDefaultLogger(LogLevelInfo),
	}

	t.Run("start non-installed", func(t *testing.T) {
		err := handleStart(cfg)
		if err == nil {
			t.Fatal("expected error for start on non-installed service")
		}
		if !contains(err.Error(), "detecting service manager") {
			t.Errorf("expected detect error, got: %v", err)
		}
	})

	t.Run("stop non-installed", func(t *testing.T) {
		err := handleStop(cfg)
		if err == nil {
			t.Fatal("expected error for stop on non-installed service")
		}
		if !contains(err.Error(), "detecting service manager") {
			t.Errorf("expected detect error, got: %v", err)
		}
	})

	t.Run("restart non-installed", func(t *testing.T) {
		err := handleRestart(cfg)
		if err == nil {
			t.Fatal("expected error for restart on non-installed service")
		}
		if !contains(err.Error(), "detecting service manager") {
			t.Errorf("expected detect error, got: %v", err)
		}
	})

	t.Run("enable non-installed", func(t *testing.T) {
		err := handleEnable(cfg)
		if err == nil {
			t.Fatal("expected error for enable on non-installed service")
		}
		if !contains(err.Error(), "detecting service manager") {
			t.Errorf("expected detect error, got: %v", err)
		}
	})

	t.Run("disable non-installed", func(t *testing.T) {
		err := handleDisable(cfg)
		if err == nil {
			t.Fatal("expected error for disable on non-installed service")
		}
		if !contains(err.Error(), "detecting service manager") {
			t.Errorf("expected detect error, got: %v", err)
		}
	})
}
