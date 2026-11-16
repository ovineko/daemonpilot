package daemonpilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/daemon"
	"github.com/ovineko/procguard"
)

type mockServiceManager struct {
	installCalled        bool
	uninstallCalled      bool
	startCalled          bool
	stopCalled           bool
	enableCalled         bool
	disableCalled        bool
	statusCalled         bool
	generateCalled       bool
	writeCalled          bool
	installConfig        daemon.ServiceConfig
	installErr           error
	uninstallErr         error
	startErr             error
	stopErr              error
	enableErr            error
	disableErr           error
	statusResult         daemon.ServiceStatus
	statusErr            error
	manifestPath         string
	generateContent      string
	generateErr          error
	writeContent         string
	writeErr             error
	readInstalledContent string
	readInstalledErr     error
	supportsEnableVal    *bool
	supportsStartVal     *bool
	supportsStatusVal    *bool
	supportsUserScopeVal *bool
	nameVal              string
}

func (m *mockServiceManager) Install(config daemon.ServiceConfig) error {
	m.installCalled = true
	m.installConfig = config
	return m.installErr
}

func (m *mockServiceManager) Uninstall() error {
	m.uninstallCalled = true
	return m.uninstallErr
}

func (m *mockServiceManager) Start() error {
	m.startCalled = true
	return m.startErr
}

func (m *mockServiceManager) Stop() error {
	m.stopCalled = true
	return m.stopErr
}

func (m *mockServiceManager) Enable() error {
	m.enableCalled = true
	return m.enableErr
}

func (m *mockServiceManager) Disable() error {
	m.disableCalled = true
	return m.disableErr
}

func (m *mockServiceManager) Status() (daemon.ServiceStatus, error) {
	m.statusCalled = true
	return m.statusResult, m.statusErr
}

func (m *mockServiceManager) GetManifestPath() string {
	return m.manifestPath
}

func (m *mockServiceManager) GenerateConfig(_ daemon.ServiceConfig) (string, error) {
	m.generateCalled = true
	return m.generateContent, m.generateErr
}

func (m *mockServiceManager) WriteConfig(content string) error {
	m.writeCalled = true
	m.writeContent = content
	return m.writeErr
}

func (m *mockServiceManager) ReadInstalledConfig() (string, error) {
	return m.readInstalledContent, m.readInstalledErr
}

func (m *mockServiceManager) CanRestartWatchdog() bool {
	return false
}

func (m *mockServiceManager) SupportsEnable() bool {
	if m.supportsEnableVal != nil {
		return *m.supportsEnableVal
	}
	return true
}

func (m *mockServiceManager) SupportsStart() bool {
	if m.supportsStartVal != nil {
		return *m.supportsStartVal
	}
	return true
}

func (m *mockServiceManager) SupportsStatus() bool {
	if m.supportsStatusVal != nil {
		return *m.supportsStatusVal
	}
	return true
}

func (m *mockServiceManager) SupportsScope(scope daemon.Scope) bool {
	if scope == daemon.ScopeSystem {
		return true
	}
	if m.supportsUserScopeVal != nil {
		return *m.supportsUserScopeVal
	}
	return false
}

func (m *mockServiceManager) Name() string {
	if m.nameVal != "" {
		return m.nameVal
	}
	return "mock"
}

type orderTrackingManager struct {
	inner     *mockServiceManager
	callOrder *[]string
}

func (o *orderTrackingManager) Install(config daemon.ServiceConfig) error {
	*o.callOrder = append(*o.callOrder, "install")
	return o.inner.Install(config)
}

func (o *orderTrackingManager) Uninstall() error {
	*o.callOrder = append(*o.callOrder, "uninstall")
	return o.inner.Uninstall()
}

func (o *orderTrackingManager) Start() error {
	*o.callOrder = append(*o.callOrder, "start")
	return o.inner.Start()
}

func (o *orderTrackingManager) Stop() error {
	*o.callOrder = append(*o.callOrder, "stop")
	return o.inner.Stop()
}

func (o *orderTrackingManager) Enable() error {
	*o.callOrder = append(*o.callOrder, "enable")
	return o.inner.Enable()
}

func (o *orderTrackingManager) Disable() error {
	*o.callOrder = append(*o.callOrder, "disable")
	return o.inner.Disable()
}

func (o *orderTrackingManager) Status() (daemon.ServiceStatus, error) {
	*o.callOrder = append(*o.callOrder, "status")
	return o.inner.Status()
}

func (o *orderTrackingManager) GetManifestPath() string {
	return o.inner.GetManifestPath()
}

func (o *orderTrackingManager) GenerateConfig(config daemon.ServiceConfig) (string, error) {
	*o.callOrder = append(*o.callOrder, "generateConfig")
	return o.inner.GenerateConfig(config)
}

func (o *orderTrackingManager) WriteConfig(content string) error {
	*o.callOrder = append(*o.callOrder, "writeConfig")
	return o.inner.WriteConfig(content)
}

func (o *orderTrackingManager) ReadInstalledConfig() (string, error) {
	return o.inner.ReadInstalledConfig()
}

func (o *orderTrackingManager) CanRestartWatchdog() bool {
	return o.inner.CanRestartWatchdog()
}

func (o *orderTrackingManager) SupportsEnable() bool { return o.inner.SupportsEnable() }
func (o *orderTrackingManager) SupportsStart() bool  { return o.inner.SupportsStart() }
func (o *orderTrackingManager) SupportsStatus() bool { return o.inner.SupportsStatus() }

func (o *orderTrackingManager) SupportsScope(scope daemon.Scope) bool {
	return o.inner.SupportsScope(scope)
}

func (o *orderTrackingManager) Name() string {
	return o.inner.Name()
}

type savedCommandVars struct {
	detectManager    func() (daemon.ServiceManager, error)
	detectForScope   func(daemon.Scope) (daemon.Detection, error)
	newManagerByName func(string) (daemon.ServiceManager, error)

	initManager   func(daemon.ServiceManager, daemon.ManagerOptions) error
	getExecutable func() (string, error)
	mkdirAll      func(string, os.FileMode) error
	statFileFn    func(string) (os.FileInfo, error)
	copyFileFn    func(string, string) error
	saveStateFn   func(string, *StateData) error
	loadStateFn   func(string, string) (*StateData, error)
	deleteStateFn func(string, string) error
	timeNow       func() time.Time

	preflightInstall func(*Config, string) error
	preflightExec    func(*Config, string) error
}

// saveCommandVars snapshots the command seams and, for the duration of the
// test, routes construction-by-name to whatever manager the test substitutes
// for detection: nearly every test drives a mock manager and asserts handler
// behavior, not init-system construction. Tests that exercise
// construction-by-name substitute newManagerByName themselves.
func saveCommandVars() savedCommandVars {
	saved := savedCommandVars{
		detectManager:    detectManager,
		detectForScope:   detectForScope,
		newManagerByName: newManagerByName,

		initManager:   initManager,
		getExecutable: getExecutable,
		mkdirAll:      mkdirAll,
		statFileFn:    statFileFn,
		copyFileFn:    copyFileFn,
		saveStateFn:   saveStateFn,
		loadStateFn:   loadStateFn,
		deleteStateFn: deleteStateFn,
		timeNow:       timeNow,

		preflightInstall: preflightInstall,
		preflightExec:    preflightExec,
	}
	// Handler tests drive mock managers over paths that do not exist on the
	// test host, so the real write and exec probes would fail every one of
	// them. Both have their own tests in preflight_test.go, against real files.
	preflightInstall = func(*Config, string) error { return nil }
	preflightExec = func(*Config, string) error { return nil }
	newManagerByName = func(string) (daemon.ServiceManager, error) { return detectManager() }
	detectForScope = func(daemon.Scope) (daemon.Detection, error) {
		mgr, err := detectManager()
		if err != nil {
			return daemon.Detection{}, err
		}
		// HostInitSystem mirrors what real detection reports: the detector name,
		// which is also what install records as StateData.InitSystem. Tests that
		// substitute initSystemTypeFn therefore see a consistent host name in the
		// drift check.
		return daemon.Detection{Manager: mgr, HostInitSystem: detectInitSystemType(mgr)}, nil
	}
	return saved
}

// installedState stubs the state file of a service installed in system scope
// under the mock init system, which lifecycle commands now require.
func installedState(name string) func(string, string) (*StateData, error) {
	return func(string, string) (*StateData, error) {
		return &StateData{ServiceName: name, InitSystem: "mock"}, nil
	}
}

func restoreCommandVars2(s savedCommandVars) {
	detectManager = s.detectManager
	detectForScope = s.detectForScope
	newManagerByName = s.newManagerByName
	initManager = s.initManager
	getExecutable = s.getExecutable
	mkdirAll = s.mkdirAll
	statFileFn = s.statFileFn
	copyFileFn = s.copyFileFn
	saveStateFn = s.saveStateFn
	loadStateFn = s.loadStateFn
	deleteStateFn = s.deleteStateFn
	timeNow = s.timeNow
	preflightInstall = s.preflightInstall
	preflightExec = s.preflightExec
}

func TestPrepareManager(t *testing.T) {
	tests := []struct {
		name       string
		detect     func() (daemon.ServiceManager, error)
		opts       daemon.ManagerOptions
		wantErr    string
		wantOpts   daemon.ManagerOptions
		wantCalled bool
	}{
		{
			name:       "passes options through to the initializer",
			detect:     func() (daemon.ServiceManager, error) { return &mockServiceManager{}, nil },
			opts:       daemon.ManagerOptions{ServiceName: "testapp", InstallDir: "/opt/testapp"},
			wantOpts:   daemon.ManagerOptions{ServiceName: "testapp", InstallDir: "/opt/testapp"},
			wantCalled: true,
		},
		{
			name:    "detection failure is wrapped",
			detect:  func() (daemon.ServiceManager, error) { return nil, errors.New("no manager") },
			opts:    daemon.ManagerOptions{ServiceName: "testapp"},
			wantErr: "detecting service manager",
		},
		{
			name:       "initialization failure is wrapped",
			detect:     func() (daemon.ServiceManager, error) { return &mockServiceManager{}, nil },
			opts:       daemon.ManagerOptions{},
			wantErr:    "initializing service manager",
			wantCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := saveCommandVars()
			defer restoreCommandVars2(saved)

			var gotOpts daemon.ManagerOptions
			var called bool
			detectManager = tt.detect
			initManager = func(mgr daemon.ServiceManager, opts daemon.ManagerOptions) error {
				called = true
				gotOpts = opts
				return daemon.InitServiceManagerWithOptions(mgr, opts)
			}

			mgr, err := prepareManager(tt.opts)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
				if mgr != nil {
					t.Errorf("expected nil manager on error, got %T", mgr)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if mgr == nil {
					t.Fatal("expected a manager, got nil")
				}
				if gotOpts != tt.wantOpts {
					t.Errorf("expected options %+v, got %+v", tt.wantOpts, gotOpts)
				}
			}
			if called != tt.wantCalled {
				t.Errorf("expected initManager called=%v, got %v", tt.wantCalled, called)
			}
		})
	}
}

func TestHandleInstall_CrontabRebootCaveat(t *testing.T) {
	tests := []struct {
		name     string
		initName string
		schedule string
		wantWarn bool
	}{
		{name: "crontab with the default schedule warns", initName: "crontab", wantWarn: true},
		{name: "crontab with a time-based schedule stays quiet", initName: "crontab", schedule: "*/5 * * * *"},
		{name: "another init system never warns", initName: "systemd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := saveCommandVars()
			defer restoreCommandVars2(saved)

			mock := &mockServiceManager{nameVal: tt.initName}
			detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
			initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
			getExecutable = func() (string, error) { return filepath.Join("/opt/testapp", InstalledBinaryName("testapp")), nil }
			mkdirAll = func(string, os.FileMode) error { return nil }
			copyFileFn = func(string, string) error { return nil }
			saveStateFn = func(string, *StateData) error { return nil }

			logger := &recordingLogger{}
			cfg := &Config{
				Name:            "testapp",
				InstallDir:      "/opt/testapp",
				CrontabSchedule: tt.schedule,
				Logger:          logger,
			}

			if err := handleInstall(cfg); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := hasCall(logger, "warn", "FEATURE_CROND_SPECIAL_TIMES"); got != tt.wantWarn {
				t.Errorf("caveat warning = %v, want %v; calls: %v", got, tt.wantWarn, logger.calls)
			}
		})
	}
}

func TestHandleInstall(t *testing.T) {
	t.Run("success with binary copy", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		mock := &mockServiceManager{}
		var mkdirPath string
		var copySrc, copyDst string

		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = daemon.InitServiceManagerWithOptions
		getExecutable = func() (string, error) { return "/usr/bin/myapp", nil }
		saveStateFn = func(_ string, _ *StateData) error { return nil }
		mkdirAll = func(path string, _ os.FileMode) error {
			mkdirPath = path
			return nil
		}
		copyFileFn = func(src, dst string) error {
			copySrc = src
			copyDst = dst
			return nil
		}

		cfg := &Config{
			Name:        "testapp",
			Description: "Test application",
			InstallDir:  "/opt/testapp",
			Logger:      NewDefaultLogger(LogLevelInfo),
		}

		err := handleInstall(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mock.installCalled {
			t.Fatal("expected Install to be called")
		}
		if mock.installConfig.Name != "testapp" {
			t.Errorf("expected name testapp, got %s", mock.installConfig.Name)
		}
		if mock.installConfig.Description != "Test application" {
			t.Errorf("expected description 'Test application', got %s", mock.installConfig.Description)
		}
		wantBinaryPath := filepath.Join("/opt/testapp", InstalledBinaryName("testapp"))
		if mock.installConfig.ExecPath != wantBinaryPath {
			t.Errorf("expected exec path %s, got %s", wantBinaryPath, mock.installConfig.ExecPath)
		}
		if mock.installConfig.InstallDir != "/opt/testapp" {
			t.Errorf("expected install dir /opt/testapp, got %s", mock.installConfig.InstallDir)
		}
		if mock.installConfig.WorkingDir != "/opt/testapp" {
			t.Errorf("expected working dir /opt/testapp, got %s", mock.installConfig.WorkingDir)
		}
		if mkdirPath != "/opt/testapp" {
			t.Errorf("expected mkdir path /opt/testapp, got %s", mkdirPath)
		}
		if copySrc != "/usr/bin/myapp" {
			t.Errorf("expected copy src /usr/bin/myapp, got %s", copySrc)
		}
		if copyDst != wantBinaryPath {
			t.Errorf("expected copy dst %s, got %s", wantBinaryPath, copyDst)
		}
		if !mock.enableCalled {
			t.Fatal("expected Enable to be called")
		}
		if !mock.startCalled {
			t.Fatal("expected Start to be called")
		}
	})

	t.Run("binary already in install dir", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		mock := &mockServiceManager{}
		mkdirCalled := false
		copyCalled := false

		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = daemon.InitServiceManagerWithOptions
		getExecutable = func() (string, error) { return filepath.Join("/opt/testapp", InstalledBinaryName("testapp")), nil }
		saveStateFn = func(_ string, _ *StateData) error { return nil }
		mkdirAll = func(_ string, _ os.FileMode) error {
			mkdirCalled = true
			return nil
		}
		copyFileFn = func(_, _ string) error {
			copyCalled = true
			return nil
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleInstall(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mkdirCalled {
			t.Error("expected mkdir not to be called when binary is already in install dir")
		}
		if copyCalled {
			t.Error("expected copy not to be called when binary is already in install dir")
		}
		if !mock.installCalled {
			t.Fatal("expected Install to be called")
		}
		if !mock.enableCalled {
			t.Fatal("expected Enable to be called")
		}
		if !mock.startCalled {
			t.Fatal("expected Start to be called")
		}
	})

	t.Run("detect manager error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		detectManager = func() (daemon.ServiceManager, error) {
			return nil, errors.New("no service manager")
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleInstall(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if err.Error() != "detecting service manager: no service manager" {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("get executable error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		getExecutable = func() (string, error) { return "", errors.New("exec error") }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleInstall(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if err.Error() != "getting executable path: exec error" {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("mkdir error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		getExecutable = func() (string, error) { return "/usr/bin/myapp", nil }
		mkdirAll = func(_ string, _ os.FileMode) error {
			return errors.New("permission denied")
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleInstall(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if err.Error() != "creating install directory: permission denied" {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("copy binary error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		getExecutable = func() (string, error) { return "/usr/bin/myapp", nil }
		mkdirAll = func(_ string, _ os.FileMode) error { return nil }
		copyFileFn = func(_, _ string) error { return errors.New("copy failed") }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleInstall(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if err.Error() != "copying binary: copy failed" {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("service install error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{installErr: errors.New("install failed")}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		getExecutable = func() (string, error) { return "/usr/bin/myapp", nil }
		mkdirAll = func(_ string, _ os.FileMode) error { return nil }
		copyFileFn = func(_, _ string) error { return nil }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleInstall(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if err.Error() != "testapp: install failed" {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("enable fails after install", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{enableErr: errors.New("enable failed")}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		getExecutable = func() (string, error) { return "/usr/bin/myapp", nil }
		mkdirAll = func(_ string, _ os.FileMode) error { return nil }
		copyFileFn = func(_, _ string) error { return nil }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleInstall(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if err.Error() != "testapp: enable failed" {
			t.Errorf("unexpected error message: %v", err)
		}
		if !mock.installCalled {
			t.Error("expected Install to be called")
		}
		if !mock.enableCalled {
			t.Error("expected Enable to be called")
		}
		if mock.startCalled {
			t.Error("expected Start NOT to be called when enable fails")
		}
	})

	// A service that did not start is REPORTED, not swallowed. This used to assert
	// the opposite ("non-fatal"), on the reasoning that the operator can start it
	// by hand -- but nobody does, because the installer printed success. That
	// combination hid three separate Windows faults in a row, each a node that
	// enrolled, held an agent row and could never run.
	//
	// What must NOT change is that everything else completes: the registration
	// stands, the state file is written, and `start` retries once the cause is
	// fixed. The install is finished and reported, not unwound.
	t.Run("start failing after enable is reported, and the install still completes", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		mock := &mockServiceManager{startErr: errors.New("start failed")}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		getExecutable = func() (string, error) { return "/usr/bin/myapp", nil }
		mkdirAll = func(_ string, _ os.FileMode) error { return nil }
		copyFileFn = func(_, _ string) error { return nil }
		saveStateFn = func(_ string, _ *StateData) error { return nil }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		stateWritten := false
		saveStateFn = func(_ string, _ *StateData) error {
			stateWritten = true
			return nil
		}

		err := handleInstall(cfg)
		if err == nil {
			t.Fatal("a service that did not start was reported as a successful install")
		}
		if !strings.Contains(err.Error(), "did not start") {
			t.Errorf("error = %v; it must say the service did not start", err)
		}
		if !strings.Contains(err.Error(), "start failed") {
			t.Errorf("error = %v; it must carry the init system's own reason", err)
		}
		if !mock.installCalled {
			t.Error("expected Install to be called")
		}
		if !mock.enableCalled {
			t.Error("expected Enable to be called")
		}
		if !mock.startCalled {
			t.Error("expected Start to be called")
		}
		// The install must be COMPLETE and bindable despite the report: uninstall,
		// start and status all read the state file, so returning early here would
		// turn a service that merely did not start into one nothing can act on.
		if !stateWritten {
			t.Error("the state file was not written; later commands could not bind to the installed service")
		}
		if mock.uninstallCalled {
			t.Error("the install was unwound; a registered service that did not start must be left in place to retry")
		}
	})
}

func TestHandleInstall_LogsManifestPath(t *testing.T) {
	saved := saveCommandVars()
	defer restoreCommandVars2(saved)
	saveIST := initSystemTypeFn
	defer func() { initSystemTypeFn = saveIST }()
	initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

	mock := &mockServiceManager{manifestPath: "/etc/systemd/system/testapp.service"}
	var logBuf bytes.Buffer

	detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
	initManager = daemon.InitServiceManagerWithOptions
	getExecutable = func() (string, error) { return filepath.Join("/opt/testapp", InstalledBinaryName("testapp")), nil }
	mkdirAll = func(_ string, _ os.FileMode) error { return nil }
	copyFileFn = func(_, _ string) error { return nil }
	saveStateFn = func(_ string, _ *StateData) error { return nil }

	handler := slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(handler)

	cfg := &Config{
		Name:       "testapp",
		InstallDir: "/opt/testapp",
		Logger:     logger,
	}

	err := handleInstall(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "/etc/systemd/system/testapp.service") {
		t.Errorf("expected log to contain manifest path, got: %s", logOutput)
	}
}

func TestHandleUninstall(t *testing.T) {
	t.Run("success stops before uninstall", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{}
		var initOpts daemon.ManagerOptions
		var callOrder []string

		deleteStateFn = func(_, _ string) error { return nil }
		loadStateFn = installedState("testapp")
		mock.stopErr = nil

		wrapMock := &orderTrackingManager{
			inner:     mock,
			callOrder: &callOrder,
		}
		detectManager = func() (daemon.ServiceManager, error) { return wrapMock, nil }
		initManager = func(_ daemon.ServiceManager, opts daemon.ManagerOptions) error {
			initOpts = opts
			return nil
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleUninstall(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mock.stopCalled {
			t.Fatal("expected Stop to be called")
		}
		if !mock.uninstallCalled {
			t.Fatal("expected Uninstall to be called")
		}
		want := daemon.ManagerOptions{ServiceName: "testapp", InstallDir: "/opt/testapp"}
		if initOpts != want {
			t.Errorf("init options = %+v, want %+v", initOpts, want)
		}
		if len(callOrder) < 2 || callOrder[0] != "stop" || callOrder[1] != "uninstall" {
			t.Errorf("expected stop before uninstall, got call order: %v", callOrder)
		}
	})

	t.Run("detect manager error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		detectManager = func() (daemon.ServiceManager, error) {
			return nil, errors.New("no service manager")
		}
		loadStateFn = installedState("testapp")

		cfg := &Config{
			Name:   "testapp",
			Logger: NewDefaultLogger(LogLevelInfo),
		}

		err := handleUninstall(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if err.Error() != `service manager for "mock": no service manager` {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("no state file and no manifest reports not installed", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{manifestPath: "/etc/systemd/system/testapp.service"}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(string, string) (*StateData, error) { return nil, nil }
		statFileFn = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleUninstall(cfg)
		if err == nil {
			t.Fatal("expected an error, got success")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("expected a not-installed error, got: %v", err)
		}
		if mock.uninstallCalled {
			t.Error("expected Uninstall not to be called without a state file")
		}
	})

	// An install that fails after writing the manifest never writes a state
	// file; refusing here would leave the unit and binary unremovable.
	t.Run("no state file but manifest present uninstalls", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{manifestPath: "/etc/systemd/system/testapp.service"}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(string, string) (*StateData, error) { return nil, nil }
		deleteStateFn = func(string, string) error { return nil }
		statFileFn = func(string) (os.FileInfo, error) { return nil, nil }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: t.TempDir(),
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		if err := handleUninstall(cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mock.uninstallCalled {
			t.Error("expected Uninstall to be called for an orphaned manifest")
		}
	})

	t.Run("scope mismatch refuses", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(string, string) (*StateData, error) {
			return &StateData{ServiceName: "testapp", InitSystem: "mock", Scope: ScopeUser}, nil
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Scope:      ScopeSystem,
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleUninstall(cfg)
		if err == nil {
			t.Fatal("expected an error, got success")
		}
		if !strings.Contains(err.Error(), "installed in user scope") {
			t.Errorf("expected a scope mismatch error, got: %v", err)
		}
		if mock.uninstallCalled {
			t.Error("expected Uninstall not to be called on scope mismatch")
		}
	})

	// A state file written 0600 is unreadable to anyone but the installer, and a
	// truncated one fails to decode. Refusing would leave exactly the artifacts
	// uninstall exists to remove, so it warns and falls back to detection.
	t.Run("unreadable state file falls back to detection", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{manifestPath: "/etc/systemd/system/testapp.service"}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(string, string) (*StateData, error) {
			return nil, errors.New("reading state file: permission denied")
		}
		statFileFn = func(string) (os.FileInfo, error) { return nil, nil }
		deleteStateFn = func(string, string) error { return nil }

		logger := &recordingLogger{}
		cfg := &Config{Name: "testapp", InstallDir: t.TempDir(), Logger: logger}

		if err := handleUninstall(cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mock.uninstallCalled {
			t.Error("expected Uninstall to be called despite the unreadable state file")
		}
		if !hasCall(logger, "warn", "failed to read the state file") {
			t.Errorf("missing state-read warning, got %v", logger.calls)
		}
	})

	t.Run("uninstall error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{uninstallErr: errors.New("uninstall failed")}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = installedState("testapp")

		cfg := &Config{
			Name:   "testapp",
			Logger: NewDefaultLogger(LogLevelInfo),
		}

		err := handleUninstall(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if err.Error() != "testapp: uninstall failed" {
			t.Errorf("unexpected error message: %v", err)
		}
		if !mock.stopCalled {
			t.Error("expected Stop to be called before uninstall attempt")
		}
	})

	t.Run("missing crontab entry is reported but not fatal", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{
			uninstallErr: fmt.Errorf("%w: testapp", daemon.ErrCrontabEntryNotFound),
		}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		var stateDeleted bool
		deleteStateFn = func(string, string) error { stateDeleted = true; return nil }
		loadStateFn = installedState("testapp")

		logger := &recordingLogger{}
		cfg := &Config{
			Name:   "testapp",
			Logger: logger,
		}

		if err := handleUninstall(cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !hasCall(logger, "warn", "no crontab entry found") {
			t.Errorf("missing crontab warning, got %v", logger.calls)
		}
		if !stateDeleted {
			t.Error("expected the state file to be deleted despite the missing entry")
		}
	})

	t.Run("stop fails but uninstall continues", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{stopErr: errors.New("stop failed")}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		deleteStateFn = func(_, _ string) error { return nil }
		loadStateFn = installedState("testapp")

		cfg := &Config{
			Name:   "testapp",
			Logger: NewDefaultLogger(LogLevelInfo),
		}

		err := handleUninstall(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mock.stopCalled {
			t.Error("expected Stop to be called")
		}
		if !mock.uninstallCalled {
			t.Error("expected Uninstall to be called despite stop failure")
		}
	})

	t.Run("service already stopped succeeds", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{stopErr: errors.New("service already stopped")}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		deleteStateFn = func(_, _ string) error { return nil }
		loadStateFn = installedState("testapp")

		cfg := &Config{
			Name:   "testapp",
			Logger: NewDefaultLogger(LogLevelInfo),
		}

		err := handleUninstall(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mock.stopCalled {
			t.Error("expected Stop to be called")
		}
		if !mock.uninstallCalled {
			t.Error("expected Uninstall to be called")
		}
	})

	// A leftover .rollback survives a reinstall and is acted on by the next run,
	// so uninstall sweeps the working files it left in the install directory.
	t.Run("removes leftover install artifacts", func(t *testing.T) {
		tests := []struct {
			name    string
			present []string
		}{
			{name: "all artifacts present", present: []string{".lock", ".new", ".rollback", ".rollback.invalid"}},
			{name: "only rollback present", present: []string{".rollback"}},
			{name: "no artifacts present", present: nil},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				saved := saveCommandVars()
				defer restoreCommandVars2(saved)

				installDir := t.TempDir()
				// update-config writes its backup next to the manifest, not next
				// to the binary, so the sweep has to reach it from there.
				manifestPath := filepath.Join(t.TempDir(), "testapp.service")
				manifestBackup := manifestPath + ".backup"

				mock := &mockServiceManager{manifestPath: manifestPath}
				detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
				initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
				deleteStateFn = func(string, string) error { return nil }
				loadStateFn = installedState("testapp")

				binaryPath := filepath.Join(installDir, InstalledBinaryName("testapp"))
				lockPath := filepath.Join(installDir, "testapp.lock")
				// #nosec G306 -- Executable fixture owned by this test.
				if err := os.WriteFile(binaryPath, []byte("binary"), 0o755); err != nil {
					t.Fatal(err)
				}
				for _, suffix := range tt.present {
					artifactPath := binaryPath
					switch suffix {
					case ".lock":
						artifactPath = lockPath
					case ".new":
						artifactPath = StagedNewPath(binaryPath)
					default:
						artifactPath += suffix
					}
					// #nosec G306 -- Executable fixture owned by this test.
					if err := os.WriteFile(artifactPath, []byte("artifact"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if len(tt.present) > 0 {
					// #nosec G306 -- Executable fixture owned by this test.
					if err := os.WriteFile(manifestBackup, []byte("manifest backup"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				unrelated := filepath.Join(installDir, "other.rollback")
				// #nosec G306 -- Executable fixture owned by this test.
				if err := os.WriteFile(unrelated, []byte("not ours"), 0o644); err != nil {
					t.Fatal(err)
				}

				cfg := &Config{
					Name:       "testapp",
					InstallDir: installDir,
					Logger:     NewDefaultLogger(LogLevelInfo),
				}

				if err := handleUninstall(cfg); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				for _, suffix := range []string{".rollback", ".rollback.invalid"} {
					path := binaryPath + suffix
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Errorf("expected %s to be removed, stat error = %v", path, err)
					}
				}
				if _, err := os.Stat(StagedNewPath(binaryPath)); !os.IsNotExist(err) {
					t.Errorf("expected %s to be removed, stat error = %v", StagedNewPath(binaryPath), err)
				}
				if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
					t.Errorf("expected %s to be removed, stat error = %v", lockPath, err)
				}
				if _, err := os.Stat(manifestBackup); !os.IsNotExist(err) {
					t.Errorf("expected %s to be removed, stat error = %v", manifestBackup, err)
				}
				if _, err := os.Stat(unrelated); err != nil {
					t.Errorf("expected the unrelated file to be left untouched: %v", err)
				}
				if _, err := os.Stat(installDir); err != nil {
					t.Errorf("expected the install directory to be left in place: %v", err)
				}
			})
		}
	})

	t.Run("artifact removal failure warns and continues", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = installedState("testapp")

		stateDeleted := false
		deleteStateFn = func(string, string) error {
			stateDeleted = true
			return nil
		}

		savedRemove := removeFileFn
		defer func() { removeFileFn = savedRemove }()
		removeFileFn = func(path string) error {
			if strings.HasSuffix(path, ".rollback") {
				return errors.New("permission denied")
			}
			return nil
		}

		logger := &recordingLogger{}
		cfg := &Config{Name: "testapp", InstallDir: "/opt/testapp", Logger: logger}

		if err := handleUninstall(cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !stateDeleted {
			t.Error("expected the state file to be deleted despite the artifact removal failure")
		}
		warned := false
		for _, call := range logger.calls {
			if call.level == "warn" && strings.Contains(call.msg, "failed to remove install artifact") {
				warned = true
			}
		}
		if !warned {
			t.Errorf("expected a warning about the failed removal, got calls: %+v", logger.calls)
		}
	})
}

func TestCopyFile(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "source")
		dst := filepath.Join(dir, "dest")

		content := []byte("test binary content")
		// #nosec G306 -- Executable fixture owned by this test.
		if err := os.WriteFile(src, content, 0o644); err != nil {
			t.Fatal(err)
		}

		if err := copyFile(src, dst); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// #nosec G304 -- Reads a fixture from this test's temporary directory.
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(content) {
			t.Errorf("expected %q, got %q", content, got)
		}

		info, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
			t.Error("expected destination file to be executable")
		}
	})

	t.Run("source not found", func(t *testing.T) {
		dir := t.TempDir()
		err := copyFile(filepath.Join(dir, "nonexistent"), filepath.Join(dir, "dest"))
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("destination dir not found", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "source")
		// #nosec G306 -- Executable fixture owned by this test.
		if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}

		err := copyFile(src, filepath.Join(dir, "nonexistent", "dest"))
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

type mockRunner struct {
	runCalled bool
	runErr    error
	runCtx    context.Context //nolint:containedctx // captured for assertions on the context passed to Run
}

func (m *mockRunner) Run(ctx context.Context) error {
	m.runCalled = true
	m.runCtx = ctx
	return m.runErr
}

type mockUpdaterFacade struct {
	cleanupErr       error
	cleanupCalled    bool
	checkRollbackRet bool
	checkRollbackErr error
	handleRollbackFn func(ctx context.Context, path string, timeout time.Duration) error
}

func (m *mockUpdaterFacade) CleanupStaleFiles() error {
	m.cleanupCalled = true
	return m.cleanupErr
}

func (m *mockUpdaterFacade) CheckRollbackFile(_ context.Context, _ string) (bool, error) {
	return m.checkRollbackRet, m.checkRollbackErr
}

func (m *mockUpdaterFacade) HandleRollback(ctx context.Context, rollbackPath string, rollbackTimeout time.Duration) error {
	if m.handleRollbackFn != nil {
		return m.handleRollbackFn(ctx, rollbackPath, rollbackTimeout)
	}
	return nil
}

func saveRunVars() (
	func(context.Context) (context.Context, context.CancelFunc),
	func(*UpdateConfig, Logger, string, string) UpdaterFacade,
	func(*Config) Runner,
	func(string) error,
	func(context.Context, int, time.Duration) (context.Context, context.CancelFunc, error),
) {
	// Run tests point InstallDir at paths that do not exist on the test host, so
	// the real single-instance lock — now fatal on any error — could never be
	// taken. Tests that assert on lock behavior substitute acquireLockFn
	// themselves; restoreRunVars puts the production lock back.
	acquireLockFn = func(string, lockOwner) (func(), error) { return func() {}, nil }
	return setupSignalHandler, newUpdaterFn, newWatchdogFn, execSelfFn, startParentMonitorFn
}

func restoreRunVars(
	sh func(context.Context) (context.Context, context.CancelFunc),
	nu func(*UpdateConfig, Logger, string, string) UpdaterFacade,
	nw func(*Config) Runner,
	es func(string) error,
	pm func(context.Context, int, time.Duration) (context.Context, context.CancelFunc, error),
) {
	acquireLockFn = acquireInstanceLock
	setupSignalHandler = sh
	newUpdaterFn = nu
	newWatchdogFn = nw
	execSelfFn = es
	startParentMonitorFn = pm
}

func fakeSignalHandler(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}

func TestHandleRun(t *testing.T) {
	t.Run("basic run without update config", func(t *testing.T) {
		sh, nu, nw, es, pm := saveRunVars()
		defer restoreRunVars(sh, nu, nw, es, pm)

		mr := &mockRunner{}
		setupSignalHandler = fakeSignalHandler
		newWatchdogFn = func(_ *Config) Runner { return mr }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleRun(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mr.runCalled {
			t.Fatal("expected watchdog Run to be called")
		}
	})

	t.Run("watchdog run error propagated", func(t *testing.T) {
		sh, nu, nw, es, pm := saveRunVars()
		defer restoreRunVars(sh, nu, nw, es, pm)

		mr := &mockRunner{runErr: errors.New("watchdog failed")}
		setupSignalHandler = fakeSignalHandler
		newWatchdogFn = func(_ *Config) Runner { return mr }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleRun(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if err.Error() != "watchdog failed" {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("manager binding failure warns and runs without auto-restart", func(t *testing.T) {
		sh, nu, nw, es, pm := saveRunVars()
		defer restoreRunVars(sh, nu, nw, es, pm)
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mr := &mockRunner{}
		setupSignalHandler = fakeSignalHandler
		newWatchdogFn = func(_ *Config) Runner { return mr }
		detectManager = func() (daemon.ServiceManager, error) {
			return &mockServiceManager{nameVal: "systemd"}, nil
		}
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error {
			return errors.New("service name is required")
		}

		logger := &recordingLogger{}
		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     logger,
		}

		if err := handleRun(cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mr.runCalled {
			t.Fatal("expected watchdog Run to be called despite the binding failure")
		}
		// The host init system is a property of the host, not of the binding, so
		// it survives a binding failure and keeps the embedded log filter honest.
		if cfg.InitSystemName != "systemd" {
			t.Errorf("InitSystemName = %q, want %q when binding failed", cfg.InitSystemName, "systemd")
		}
		if cfg.CanInitRestartWatchdog {
			t.Error("CanInitRestartWatchdog = true, want false when binding failed")
		}
		warned := false
		for _, c := range logger.calls {
			if c.level == "warn" && strings.Contains(c.msg, "failed to initialize init system") {
				warned = true
			}
		}
		if !warned {
			t.Errorf("expected a warning about init system initialization, got %v", logger.calls)
		}
	})

	t.Run("with update config cleans stale files", func(t *testing.T) {
		sh, nu, nw, es, pm := saveRunVars()
		defer restoreRunVars(sh, nu, nw, es, pm)

		mu := &mockUpdaterFacade{}
		mr := &mockRunner{}
		setupSignalHandler = fakeSignalHandler
		newUpdaterFn = func(_ *UpdateConfig, _ Logger, _, _ string) UpdaterFacade {
			return mu
		}
		newWatchdogFn = func(_ *Config) Runner { return mr }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
			Update: &UpdateConfig{
				RollbackTimeout: 5 * time.Minute,
			},
		}

		err := handleRun(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mu.cleanupCalled {
			t.Fatal("expected CleanupStaleFiles to be called")
		}
		if !mr.runCalled {
			t.Fatal("expected watchdog Run to be called")
		}
	})

	t.Run("cleanup error is logged but does not stop run", func(t *testing.T) {
		sh, nu, nw, es, pm := saveRunVars()
		defer restoreRunVars(sh, nu, nw, es, pm)

		mu := &mockUpdaterFacade{cleanupErr: errors.New("cleanup failed")}
		mr := &mockRunner{}
		setupSignalHandler = fakeSignalHandler
		newUpdaterFn = func(_ *UpdateConfig, _ Logger, _, _ string) UpdaterFacade {
			return mu
		}
		newWatchdogFn = func(_ *Config) Runner { return mr }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
			Update: &UpdateConfig{
				RollbackTimeout: 5 * time.Minute,
			},
		}

		err := handleRun(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mr.runCalled {
			t.Fatal("expected watchdog Run to be called despite cleanup error")
		}
	})

	t.Run("rollback file detected and handled successfully", func(t *testing.T) {
		sh, nu, nw, es, pm := saveRunVars()
		defer restoreRunVars(sh, nu, nw, es, pm)

		var rollbackHandled bool
		mu := &mockUpdaterFacade{
			checkRollbackRet: true,
			handleRollbackFn: func(_ context.Context, path string, timeout time.Duration) error {
				rollbackHandled = true
				wantRollback := filepath.Join("/opt/testapp", InstalledBinaryName("testapp")) + ".rollback"
				if path != wantRollback {
					t.Errorf("unexpected rollback path: %s, want %s", path, wantRollback)
				}
				if timeout != 5*time.Minute {
					t.Errorf("unexpected timeout: %v", timeout)
				}
				return nil
			},
		}
		mr := &mockRunner{}
		setupSignalHandler = fakeSignalHandler
		newUpdaterFn = func(_ *UpdateConfig, _ Logger, _, _ string) UpdaterFacade {
			return mu
		}
		newWatchdogFn = func(_ *Config) Runner { return mr }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
			Update: &UpdateConfig{
				RollbackTimeout: 5 * time.Minute,
			},
		}

		err := handleRun(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !rollbackHandled {
			t.Fatal("expected HandleRollback to be called")
		}
		if !mr.runCalled {
			t.Fatal("expected watchdog Run to be called after rollback handling")
		}
	})

	t.Run("rollback restored triggers exec restart", func(t *testing.T) {
		sh, nu, nw, es, pm := saveRunVars()
		defer restoreRunVars(sh, nu, nw, es, pm)

		mu := &mockUpdaterFacade{
			checkRollbackRet: true,
			handleRollbackFn: func(_ context.Context, _ string, _ time.Duration) error {
				return ErrRollbackRestored
			},
		}
		mr := &mockRunner{}
		setupSignalHandler = fakeSignalHandler
		newUpdaterFn = func(_ *UpdateConfig, _ Logger, _, _ string) UpdaterFacade {
			return mu
		}
		newWatchdogFn = func(_ *Config) Runner { return mr }

		var execPath string
		execSelfFn = func(path string) error {
			execPath = path
			return nil
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
			Update: &UpdateConfig{
				RollbackTimeout: 5 * time.Minute,
			},
		}

		err := handleRun(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mr.runCalled {
			t.Fatal("expected watchdog Run NOT to be called when rollback was restored")
		}
		wantBinaryPath := filepath.Join("/opt/testapp", InstalledBinaryName("testapp"))
		if execPath != wantBinaryPath {
			t.Fatalf("expected execSelfFn called with binary path %q, got %q", wantBinaryPath, execPath)
		}
	})

	t.Run("rollback handling error is logged but does not stop run", func(t *testing.T) {
		sh, nu, nw, es, pm := saveRunVars()
		defer restoreRunVars(sh, nu, nw, es, pm)

		mu := &mockUpdaterFacade{
			checkRollbackRet: true,
			handleRollbackFn: func(_ context.Context, _ string, _ time.Duration) error {
				return errors.New("some rollback error")
			},
		}
		mr := &mockRunner{}
		setupSignalHandler = fakeSignalHandler
		newUpdaterFn = func(_ *UpdateConfig, _ Logger, _, _ string) UpdaterFacade {
			return mu
		}
		newWatchdogFn = func(_ *Config) Runner { return mr }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
			Update: &UpdateConfig{
				RollbackTimeout: 5 * time.Minute,
			},
		}

		err := handleRun(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mr.runCalled {
			t.Fatal("expected watchdog Run to be called despite rollback error")
		}
	})

	t.Run("check rollback error is logged but does not stop run", func(t *testing.T) {
		sh, nu, nw, es, pm := saveRunVars()
		defer restoreRunVars(sh, nu, nw, es, pm)

		mu := &mockUpdaterFacade{
			checkRollbackErr: errors.New("stat error"),
		}
		mr := &mockRunner{}
		setupSignalHandler = fakeSignalHandler
		newUpdaterFn = func(_ *UpdateConfig, _ Logger, _, _ string) UpdaterFacade {
			return mu
		}
		newWatchdogFn = func(_ *Config) Runner { return mr }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
			Update: &UpdateConfig{
				RollbackTimeout: 5 * time.Minute,
			},
		}

		err := handleRun(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mr.runCalled {
			t.Fatal("expected watchdog Run to be called despite check rollback error")
		}
	})

	t.Run("context passed to watchdog from signal handler", func(t *testing.T) {
		sh, nu, nw, es, pm := saveRunVars()
		defer restoreRunVars(sh, nu, nw, es, pm)

		mr := &mockRunner{}
		setupSignalHandler = fakeSignalHandler
		newWatchdogFn = func(_ *Config) Runner { return mr }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleRun(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mr.runCtx == nil {
			t.Fatal("expected context to be passed to watchdog Run")
		}
	})

	t.Run("ErrExecRestart triggers execSelf with correct binary path", func(t *testing.T) {
		sh, nu, nw, es, pm := saveRunVars()
		defer restoreRunVars(sh, nu, nw, es, pm)

		mr := &mockRunner{runErr: ErrExecRestart}
		setupSignalHandler = fakeSignalHandler
		newWatchdogFn = func(_ *Config) Runner { return mr }

		var execPath string
		execSelfFn = func(path string) error {
			execPath = path
			return nil
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleRun(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wantPath := filepath.Join("/opt/testapp", InstalledBinaryName("testapp"))
		if execPath != wantPath {
			t.Errorf("execSelf called with %q, want %q", execPath, wantPath)
		}
	})

	t.Run("ErrExecRestart propagates execSelf error", func(t *testing.T) {
		sh, nu, nw, es, pm := saveRunVars()
		defer restoreRunVars(sh, nu, nw, es, pm)

		mr := &mockRunner{runErr: ErrExecRestart}
		setupSignalHandler = fakeSignalHandler
		newWatchdogFn = func(_ *Config) Runner { return mr }

		execSelfFn = func(_ string) error {
			return errors.New("exec failed")
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleRun(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if err.Error() != "exec failed" {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// TestHandleRunLockSetupErrorIsFatal covers the deliberate behavior change: a
// lock error other than ErrAlreadyRunning used to be downgraded to a warning
// and the watchdog ran on with no single-instance guarantee at all.
func TestHandleRunLockSetupErrorIsFatal(t *testing.T) {
	tests := []struct {
		name        string
		lockErr     error
		wantErr     bool
		wantRunCall bool
	}{
		{
			name:        "setup error refuses to start",
			lockErr:     errors.New("lock file /opt/testapp/testapp.lock is owned by uid 501"),
			wantErr:     true,
			wantRunCall: false,
		},
		{
			name:        "already running refuses to start",
			lockErr:     ErrAlreadyRunning,
			wantErr:     true,
			wantRunCall: false,
		},
		{
			name:        "lock acquired runs",
			wantRunCall: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sh, nu, nw, es, pm := saveRunVars()
			defer restoreRunVars(sh, nu, nw, es, pm)

			mr := &mockRunner{}
			setupSignalHandler = fakeSignalHandler
			newWatchdogFn = func(*Config) Runner { return mr }
			released := false
			acquireLockFn = func(string, lockOwner) (func(), error) {
				return func() { released = true }, tt.lockErr
			}

			cfg := &Config{
				Name:       "testapp",
				InstallDir: "/opt/testapp",
				Logger:     NewDefaultLogger(LogLevelInfo),
			}

			err := handleRun(cfg)
			switch {
			case tt.wantErr && err == nil:
				t.Fatal("expected handleRun to fail when the lock could not be taken")
			case tt.wantErr && !errors.Is(err, tt.lockErr):
				t.Fatalf("error = %v, want it to wrap the lock error", err)
			case !tt.wantErr && err != nil:
				t.Fatalf("unexpected error: %v", err)
			}
			if mr.runCalled != tt.wantRunCall {
				t.Errorf("watchdog Run called = %v, want %v", mr.runCalled, tt.wantRunCall)
			}
			if tt.wantRunCall && !released {
				t.Error("expected the lock to be released after a successful run")
			}
		})
	}
}

func saveStdoutWriter() io.Writer {
	return stdoutWriter
}

func restoreStdoutWriter(w io.Writer) {
	stdoutWriter = w
}

func TestHandleWorker(t *testing.T) {
	t.Run("calls WorkerFunc and returns nil on success", func(t *testing.T) {
		sh, _, _, es, _ := saveRunVars()
		defer restoreRunVars(sh, newUpdaterFn, newWatchdogFn, es, startParentMonitorFn)

		setupSignalHandler = fakeSignalHandler
		workerCalled := false

		cfg := &Config{
			Name:   "testapp",
			Logger: NewDefaultLogger(LogLevelInfo),
			WorkerFunc: func(_ context.Context) error {
				workerCalled = true
				return nil
			},
		}

		err := handleWorker(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !workerCalled {
			t.Fatal("expected WorkerFunc to be called")
		}
	})

	t.Run("returns error from WorkerFunc", func(t *testing.T) {
		sh, _, _, es, _ := saveRunVars()
		defer restoreRunVars(sh, newUpdaterFn, newWatchdogFn, es, startParentMonitorFn)

		setupSignalHandler = fakeSignalHandler

		cfg := &Config{
			Name:   "testapp",
			Logger: NewDefaultLogger(LogLevelInfo),
			WorkerFunc: func(_ context.Context) error {
				return errors.New("worker error")
			},
		}

		err := handleWorker(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if err.Error() != "worker error" {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("returns error when WorkerFunc is nil", func(t *testing.T) {
		sh, _, _, es, _ := saveRunVars()
		defer restoreRunVars(sh, newUpdaterFn, newWatchdogFn, es, startParentMonitorFn)

		setupSignalHandler = fakeSignalHandler

		cfg := &Config{
			Name:   "testapp",
			Logger: NewDefaultLogger(LogLevelInfo),
		}

		err := handleWorker(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if err.Error() != "WorkerFunc is not configured" {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("context is cancellable", func(t *testing.T) {
		sh, _, _, es, _ := saveRunVars()
		defer restoreRunVars(sh, newUpdaterFn, newWatchdogFn, es, startParentMonitorFn)

		setupSignalHandler = fakeSignalHandler
		var receivedCtx context.Context

		cfg := &Config{
			Name:   "testapp",
			Logger: NewDefaultLogger(LogLevelInfo),
			WorkerFunc: func(ctx context.Context) error {
				receivedCtx = ctx //nolint:fatcontext // captured for assertions, not a derived context
				return nil
			},
		}

		err := handleWorker(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if receivedCtx == nil {
			t.Fatal("expected context to be passed to WorkerFunc")
		}
	})

	t.Run("initializes parent monitor with env vars", func(t *testing.T) {
		sh, _, _, es, _ := saveRunVars()
		defer restoreRunVars(sh, newUpdaterFn, newWatchdogFn, es, startParentMonitorFn)

		setupSignalHandler = fakeSignalHandler

		var capturedPID int
		var capturedInterval time.Duration
		startParentMonitorFn = func(parent context.Context, pid int, interval time.Duration) (context.Context, context.CancelFunc, error) {
			capturedPID, capturedInterval = pid, interval
			child, cancel := context.WithCancel(parent)
			return child, cancel, nil
		}

		t.Setenv(EnvParentPID, "12345")
		t.Setenv(EnvDisableParentMonitor, "")

		cfg := &Config{
			Name:   "testapp",
			Logger: NewDefaultLogger(LogLevelInfo),
			WorkerFunc: func(_ context.Context) error {
				return nil
			},
		}

		err := handleWorker(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if capturedPID != 12345 {
			t.Errorf("expected ParentPID=12345, got %d", capturedPID)
		}
		if capturedInterval != 0 {
			t.Error("expected default poll interval")
		}
	})

	t.Run("context chain flows signal through parent monitor to WorkerFunc", func(t *testing.T) {
		sh, _, _, es, _ := saveRunVars()
		defer restoreRunVars(sh, newUpdaterFn, newWatchdogFn, es, startParentMonitorFn)

		signalCtx, signalCancel := context.WithCancel(context.Background())
		defer signalCancel()
		setupSignalHandler = func(_ context.Context) (context.Context, context.CancelFunc) {
			return signalCtx, signalCancel
		}

		t.Setenv(EnvParentPID, "12345")
		t.Setenv(EnvDisableParentMonitor, "")
		var monitorParent context.Context
		startParentMonitorFn = func(parent context.Context, _ int, _ time.Duration) (context.Context, context.CancelFunc, error) {
			monitorParent = parent //nolint:fatcontext // captured for assertions, not a derived context
			child, cancel := context.WithCancel(parent)
			return child, cancel, nil
		}

		var workerCtx context.Context
		cfg := &Config{
			Name:   "testapp",
			Logger: NewDefaultLogger(LogLevelInfo),
			WorkerFunc: func(ctx context.Context) error {
				workerCtx = ctx //nolint:fatcontext // captured for assertions, not a derived context
				return nil
			},
		}

		err := handleWorker(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if monitorParent != signalCtx {
			t.Error("parent monitor should receive signal handler context as parent")
		}
		if workerCtx == nil {
			t.Fatal("WorkerFunc should receive a context")
		}
	})

	t.Run("graceful degradation when parent monitor config is invalid", func(t *testing.T) {
		sh, _, _, es, _ := saveRunVars()
		defer restoreRunVars(sh, newUpdaterFn, newWatchdogFn, es, startParentMonitorFn)

		setupSignalHandler = fakeSignalHandler
		startParentMonitorFn = procguard.WatchParent

		t.Setenv(EnvParentPID, "not-a-number")
		t.Setenv(EnvDisableParentMonitor, "")

		workerCalled := false
		cfg := &Config{
			Name:   "testapp",
			Logger: NewDefaultLogger(LogLevelInfo),
			WorkerFunc: func(_ context.Context) error {
				workerCalled = true
				return nil
			},
		}

		err := handleWorker(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !workerCalled {
			t.Error("WorkerFunc should be called even when monitor config is invalid")
		}
	})

	t.Run("disable parent monitor via env var", func(t *testing.T) {
		sh, _, _, es, _ := saveRunVars()
		defer restoreRunVars(sh, newUpdaterFn, newWatchdogFn, es, startParentMonitorFn)

		setupSignalHandler = fakeSignalHandler

		var capturedPID int
		var capturedInterval time.Duration
		startParentMonitorFn = func(parent context.Context, pid int, interval time.Duration) (context.Context, context.CancelFunc, error) {
			capturedPID, capturedInterval = pid, interval
			child, cancel := context.WithCancel(parent)
			return child, cancel, nil
		}

		t.Setenv(EnvDisableParentMonitor, "true")

		cfg := &Config{
			Name:   "testapp",
			Logger: NewDefaultLogger(LogLevelInfo),
			WorkerFunc: func(_ context.Context) error {
				return nil
			},
		}

		err := handleWorker(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if capturedPID != 0 || capturedInterval != 0 {
			t.Error("disabled monitor must not call WatchParent")
		}
	})
}

func TestParseBool(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"1", true},
		{"true", true},
		{"True", true},
		{"TRUE", true},
		{"t", true},
		{"0", false},
		{"false", false},
		{"False", false},
		{"FALSE", false},
		{"f", false},
		{"", false},
		{"invalid", false},
		{"yes", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseBool(tt.input)
			if got != tt.expected {
				t.Errorf("parseBool(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

func TestHandleVersion(t *testing.T) {
	t.Run("outputs JSON with version fields", func(t *testing.T) {
		origWriter := saveStdoutWriter()
		defer restoreStdoutWriter(origWriter)

		var buf bytes.Buffer
		stdoutWriter = &buf

		cfg := &Config{
			Name:            "testapp",
			Version:         "1.2.3",
			WatchdogVersion: "2",
			Logger:          NewDefaultLogger(LogLevelInfo),
		}

		err := handleVersion(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var result map[string]string
		if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
			t.Fatalf("failed to parse JSON output: %v", err)
		}
		if result["version"] != "1.2.3" {
			t.Errorf("expected version 1.2.3, got %s", result["version"])
		}
		if result["watchdog_version"] != "2" {
			t.Errorf("expected watchdog_version 2, got %s", result["watchdog_version"])
		}
		if result["go_version"] == "" {
			t.Error("expected go_version to be non-empty")
		}
	})

	t.Run("outputs dev defaults", func(t *testing.T) {
		origWriter := saveStdoutWriter()
		defer restoreStdoutWriter(origWriter)

		var buf bytes.Buffer
		stdoutWriter = &buf

		cfg := &Config{
			Name:            "testapp",
			Version:         "dev",
			WatchdogVersion: "1",
			Logger:          NewDefaultLogger(LogLevelInfo),
		}

		err := handleVersion(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var result versionOutput
		if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
			t.Fatalf("failed to parse JSON output: %v", err)
		}
		if result.Version != "dev" {
			t.Errorf("expected version dev, got %s", result.Version)
		}
		if result.WatchdogVersion != "1" {
			t.Errorf("expected watchdog_version 1, got %s", result.WatchdogVersion)
		}
	})

	t.Run("JSON has expected keys only", func(t *testing.T) {
		origWriter := saveStdoutWriter()
		defer restoreStdoutWriter(origWriter)

		var buf bytes.Buffer
		stdoutWriter = &buf

		cfg := &Config{
			Name:            "testapp",
			Version:         "1.0.0",
			WatchdogVersion: "1",
			Logger:          NewDefaultLogger(LogLevelInfo),
		}

		err := handleVersion(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var result map[string]any
		if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
			t.Fatalf("failed to parse JSON output: %v", err)
		}

		expectedKeys := map[string]bool{
			"version":          true,
			"watchdog_version": true,
			"go_version":       true,
		}
		for key := range result {
			if !expectedKeys[key] {
				t.Errorf("unexpected key in output: %s", key)
			}
		}
		if len(result) != len(expectedKeys) {
			t.Errorf("expected %d keys, got %d", len(expectedKeys), len(result))
		}
	})
}

func TestHandleWatchdogVersion(t *testing.T) {
	t.Run("outputs watchdog version as plain text", func(t *testing.T) {
		origWriter := saveStdoutWriter()
		defer restoreStdoutWriter(origWriter)

		var buf bytes.Buffer
		stdoutWriter = &buf

		cfg := &Config{
			Name:            "testapp",
			WatchdogVersion: "3",
			Logger:          NewDefaultLogger(LogLevelInfo),
		}

		err := handleWatchdogVersion(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		got := buf.String()
		if got != "3\n" {
			t.Errorf("expected %q, got %q", "3\n", got)
		}
	})

	t.Run("outputs default watchdog version", func(t *testing.T) {
		origWriter := saveStdoutWriter()
		defer restoreStdoutWriter(origWriter)

		var buf bytes.Buffer
		stdoutWriter = &buf

		cfg := &Config{
			Name:            "testapp",
			WatchdogVersion: "1",
			Logger:          NewDefaultLogger(LogLevelInfo),
		}

		err := handleWatchdogVersion(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		got := buf.String()
		if got != "1\n" {
			t.Errorf("expected %q, got %q", "1\n", got)
		}
	})
}

func TestHandleSelftest(t *testing.T) {
	t.Run("calls SelftestFunc and writes JSON to stdout", func(t *testing.T) {
		sh, _, _, es, _ := saveRunVars()
		defer restoreRunVars(sh, newUpdaterFn, newWatchdogFn, es, startParentMonitorFn)
		origWriter := saveStdoutWriter()
		defer restoreStdoutWriter(origWriter)

		setupSignalHandler = fakeSignalHandler
		var buf bytes.Buffer
		stdoutWriter = &buf

		cfg := &Config{
			Name:   "testapp",
			Logger: NewDefaultLogger(LogLevelInfo),
			SelftestFunc: func(_ context.Context) SelftestResult {
				return SelftestResult{
					OK: true,
					Checks: map[string]CheckResult{
						"db": {OK: true, Message: "connected"},
					},
				}
			},
		}

		err := handleSelftest(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var result SelftestResult
		if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
			t.Fatalf("failed to parse JSON output: %v", err)
		}
		if !result.OK {
			t.Error("expected OK to be true")
		}
		if check, ok := result.Checks["db"]; !ok {
			t.Error("expected db check in result")
		} else if !check.OK {
			t.Error("expected db check to be OK")
		} else if check.Message != "connected" {
			t.Errorf("expected message 'connected', got %q", check.Message)
		}
	})

	t.Run("default success when SelftestFunc is nil", func(t *testing.T) {
		sh, _, _, es, _ := saveRunVars()
		defer restoreRunVars(sh, newUpdaterFn, newWatchdogFn, es, startParentMonitorFn)
		origWriter := saveStdoutWriter()
		defer restoreStdoutWriter(origWriter)

		setupSignalHandler = fakeSignalHandler
		var buf bytes.Buffer
		stdoutWriter = &buf

		cfg := &Config{
			Name:   "testapp",
			Logger: NewDefaultLogger(LogLevelInfo),
		}

		err := handleSelftest(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var result SelftestResult
		if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
			t.Fatalf("failed to parse JSON output: %v", err)
		}
		if !result.OK {
			t.Error("expected OK to be true for default selftest")
		}
		if result.Checks == nil {
			t.Error("expected Checks to be non-nil")
		}
		if len(result.Checks) != 0 {
			t.Errorf("expected empty Checks, got %d", len(result.Checks))
		}
	})

	t.Run("selftest with failure result", func(t *testing.T) {
		sh, _, _, es, _ := saveRunVars()
		defer restoreRunVars(sh, newUpdaterFn, newWatchdogFn, es, startParentMonitorFn)
		origWriter := saveStdoutWriter()
		defer restoreStdoutWriter(origWriter)

		setupSignalHandler = fakeSignalHandler
		var buf bytes.Buffer
		stdoutWriter = &buf

		cfg := &Config{
			Name:   "testapp",
			Logger: NewDefaultLogger(LogLevelInfo),
			SelftestFunc: func(_ context.Context) SelftestResult {
				return SelftestResult{
					OK: false,
					Checks: map[string]CheckResult{
						"db":    {OK: true, Message: "connected"},
						"cache": {OK: false, Message: "connection refused"},
					},
				}
			},
		}

		err := handleSelftest(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var result SelftestResult
		if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
			t.Fatalf("failed to parse JSON output: %v", err)
		}
		if result.OK {
			t.Error("expected OK to be false")
		}
		if len(result.Checks) != 2 {
			t.Errorf("expected 2 checks, got %d", len(result.Checks))
		}
		if result.Checks["cache"].OK {
			t.Error("expected cache check to fail")
		}
		if result.Checks["cache"].Message != "connection refused" {
			t.Errorf("expected 'connection refused', got %q", result.Checks["cache"].Message)
		}
	})
}

func setupMockManager(mock *mockServiceManager) func() {
	saved := saveCommandVars()
	detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
	return func() { restoreCommandVars2(saved) }
}

func TestStatusMessage(t *testing.T) {
	tests := []struct {
		status daemon.ServiceStatus
		want   string
	}{
		{daemon.ServiceStatusRunning, "Service is active and running"},
		{daemon.ServiceStatusStopped, "Service is not running"},
		{daemon.ServiceStatusFailed, "Service has failed"},
		{daemon.ServiceStatusUnknown, "Unable to determine service status"},
	}
	for _, tt := range tests {
		t.Run(tt.status.String(), func(t *testing.T) {
			got := statusMessage(tt.status)
			if got != tt.want {
				t.Errorf("statusMessage(%v) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestHandleStatus(t *testing.T) {
	tests := []struct {
		name         string
		statusResult daemon.ServiceStatus
		statusErr    error
		wantErr      bool
		errContains  string
		wantOutput   string
	}{
		{
			name:         "running",
			statusResult: daemon.ServiceStatusRunning,
			wantOutput:   "Service: testapp\nStatus: running\n  \u21b3 Service is active and running\n",
		},
		{
			name:         "stopped",
			statusResult: daemon.ServiceStatusStopped,
			wantOutput:   "Service: testapp\nStatus: stopped\n  \u21b3 Service is not running\n",
		},
		{
			name:         "failed",
			statusResult: daemon.ServiceStatusFailed,
			wantOutput:   "Service: testapp\nStatus: failed\n  \u21b3 Service has failed\n",
		},
		{
			name:         "unknown",
			statusResult: daemon.ServiceStatusUnknown,
			wantOutput:   "Service: testapp\nStatus: unknown\n  \u21b3 Unable to determine service status\n",
		},
		{
			name:        "status error",
			statusErr:   errors.New("permission denied"),
			wantErr:     true,
			errContains: "testapp: permission denied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockServiceManager{statusResult: tt.statusResult, statusErr: tt.statusErr}
			restore := setupMockManager(mock)
			defer restore()
			origWriter := saveStdoutWriter()
			defer restoreStdoutWriter(origWriter)

			var buf bytes.Buffer
			stdoutWriter = &buf

			cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
			err := handleStatus(cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("handleStatus() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errContains != "" {
				if err == nil || !contains(err.Error(), tt.errContains) {
					t.Errorf("expected error containing %q, got %v", tt.errContains, err)
				}
			}
			if !tt.wantErr {
				if !mock.statusCalled {
					t.Error("expected Status() to be called")
				}
				if got := buf.String(); got != tt.wantOutput {
					t.Errorf("output = %q, want %q", got, tt.wantOutput)
				}
			}
		})
	}

	t.Run("detect manager error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		detectManager = func() (daemon.ServiceManager, error) { return nil, errors.New("no manager") }

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleStatus(cfg)
		if err == nil || !contains(err.Error(), "detecting service manager") {
			t.Errorf("expected detect error, got %v", err)
		}
	})
}

func TestHandleStart(t *testing.T) {
	tests := []struct {
		name        string
		startErr    error
		wantErr     bool
		errContains string
	}{
		{name: "success"},
		{name: "start error", startErr: errors.New("already running"), wantErr: true, errContains: "testapp: already running"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockServiceManager{startErr: tt.startErr}
			restore := setupMockManager(mock)
			defer restore()

			cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
			err := handleStart(cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("handleStart() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errContains != "" {
				if err == nil || !contains(err.Error(), tt.errContains) {
					t.Errorf("expected error containing %q, got %v", tt.errContains, err)
				}
			}
			if !mock.startCalled {
				t.Error("expected Start() to be called")
			}
		})
	}

	t.Run("detect manager error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		detectManager = func() (daemon.ServiceManager, error) { return nil, errors.New("no manager") }

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleStart(cfg)
		if err == nil || !contains(err.Error(), "detecting service manager") {
			t.Errorf("expected detect error, got %v", err)
		}
	})

	t.Run("capability check rejects unsupported start", func(t *testing.T) {
		f := false
		mock := &mockServiceManager{supportsStartVal: &f, nameVal: "mock-init"}
		restore := setupMockManager(mock)
		defer restore()

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleStart(cfg)
		if err == nil {
			t.Fatal("expected error when SupportsStart() returns false")
		}
		if !contains(err.Error(), "not supported") {
			t.Errorf("expected error containing 'not supported', got %v", err)
		}
		if !contains(err.Error(), "mock-init") {
			t.Errorf("expected error containing init system name 'mock-init', got %v", err)
		}
		if mock.startCalled {
			t.Error("Start() should not be called when SupportsStart() returns false")
		}
	})
}

func TestHandleStop(t *testing.T) {
	tests := []struct {
		name        string
		stopErr     error
		wantErr     bool
		errContains string
	}{
		{name: "success"},
		{name: "stop error", stopErr: errors.New("not running"), wantErr: true, errContains: "testapp: not running"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockServiceManager{stopErr: tt.stopErr}
			restore := setupMockManager(mock)
			defer restore()

			cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
			err := handleStop(cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("handleStop() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errContains != "" {
				if err == nil || !contains(err.Error(), tt.errContains) {
					t.Errorf("expected error containing %q, got %v", tt.errContains, err)
				}
			}
			if !mock.stopCalled {
				t.Error("expected Stop() to be called")
			}
		})
	}

	t.Run("detect manager error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		detectManager = func() (daemon.ServiceManager, error) { return nil, errors.New("no manager") }

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleStop(cfg)
		if err == nil || !contains(err.Error(), "detecting service manager") {
			t.Errorf("expected detect error, got %v", err)
		}
	})

	t.Run("capability check rejects unsupported stop", func(t *testing.T) {
		f := false
		mock := &mockServiceManager{supportsStartVal: &f, nameVal: "mock-init"}
		restore := setupMockManager(mock)
		defer restore()

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleStop(cfg)
		if err == nil {
			t.Fatal("expected error when SupportsStart() returns false")
		}
		if !contains(err.Error(), "not supported") {
			t.Errorf("expected error containing 'not supported', got %v", err)
		}
		if !contains(err.Error(), "mock-init") {
			t.Errorf("expected error containing init system name 'mock-init', got %v", err)
		}
		if mock.stopCalled {
			t.Error("Stop() should not be called when SupportsStart() returns false")
		}
	})
}

func TestHandleRestart(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		mock := &mockServiceManager{}
		restore := setupMockManager(mock)
		defer restore()

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleRestart(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mock.stopCalled {
			t.Error("expected Stop() to be called")
		}
		if !mock.startCalled {
			t.Error("expected Start() to be called")
		}
	})

	t.Run("stop fails but status confirms stopped", func(t *testing.T) {
		mock := &mockServiceManager{
			stopErr:      errors.New("stop failed"),
			statusResult: daemon.ServiceStatusStopped,
		}
		restore := setupMockManager(mock)
		defer restore()

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleRestart(cfg)
		if err != nil {
			t.Fatalf("expected no error when stop fails but status is stopped, got %v", err)
		}
		if !mock.stopCalled {
			t.Error("expected Stop() to be called")
		}
		if !mock.startCalled {
			t.Error("expected Start() to be called when status confirms stopped")
		}
	})

	t.Run("stop fails and status check errors", func(t *testing.T) {
		mock := &mockServiceManager{
			stopErr:   errors.New("stop failed"),
			statusErr: errors.New("status unknown"),
		}
		restore := setupMockManager(mock)
		defer restore()

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleRestart(cfg)
		if err == nil || !contains(err.Error(), "cannot verify service state") {
			t.Errorf("expected 'cannot verify service state' error, got %v", err)
		}
		if mock.startCalled {
			t.Error("expected Start() NOT to be called when service state is unknown")
		}
	})

	t.Run("stop fails and service still running", func(t *testing.T) {
		mock := &mockServiceManager{
			stopErr:      errors.New("stop failed"),
			statusResult: daemon.ServiceStatusRunning,
		}
		restore := setupMockManager(mock)
		defer restore()

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleRestart(cfg)
		if err == nil || !contains(err.Error(), "still running") {
			t.Errorf("expected 'still running' error, got %v", err)
		}
		if !mock.stopCalled {
			t.Error("expected Stop() to be called")
		}
		if mock.startCalled {
			t.Error("expected Start() NOT to be called when service is still running")
		}
	})

	t.Run("stop fails and status is unknown without error", func(t *testing.T) {
		mock := &mockServiceManager{
			stopErr:      errors.New("stop failed"),
			statusResult: daemon.ServiceStatusUnknown,
		}
		restore := setupMockManager(mock)
		defer restore()

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleRestart(cfg)
		if err == nil || !contains(err.Error(), "service state is unknown") {
			t.Errorf("expected 'service state is unknown' error, got %v", err)
		}
		if mock.startCalled {
			t.Error("expected Start() NOT to be called when service state is unknown")
		}
	})

	t.Run("start fails", func(t *testing.T) {
		mock := &mockServiceManager{startErr: errors.New("start failed")}
		restore := setupMockManager(mock)
		defer restore()

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleRestart(cfg)
		if err == nil || !contains(err.Error(), "testapp: start failed") {
			t.Errorf("expected start error, got %v", err)
		}
		if !mock.stopCalled {
			t.Error("expected Stop() to be called")
		}
		if !mock.startCalled {
			t.Error("expected Start() to be called")
		}
	})

	t.Run("detect manager error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		detectManager = func() (daemon.ServiceManager, error) { return nil, errors.New("no manager") }

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleRestart(cfg)
		if err == nil || !contains(err.Error(), "detecting service manager") {
			t.Errorf("expected detect error, got %v", err)
		}
	})
}

func TestHandleEnable(t *testing.T) {
	tests := []struct {
		name        string
		enableErr   error
		wantErr     bool
		errContains string
	}{
		{name: "success"},
		{name: "enable error", enableErr: errors.New("permission denied"), wantErr: true, errContains: "testapp: permission denied"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockServiceManager{enableErr: tt.enableErr}
			restore := setupMockManager(mock)
			defer restore()

			cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
			err := handleEnable(cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("handleEnable() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errContains != "" {
				if err == nil || !contains(err.Error(), tt.errContains) {
					t.Errorf("expected error containing %q, got %v", tt.errContains, err)
				}
			}
			if !mock.enableCalled {
				t.Error("expected Enable() to be called")
			}
		})
	}

	t.Run("detect manager error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		detectManager = func() (daemon.ServiceManager, error) { return nil, errors.New("no manager") }

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleEnable(cfg)
		if err == nil || !contains(err.Error(), "detecting service manager") {
			t.Errorf("expected detect error, got %v", err)
		}
	})

	t.Run("capability check rejects unsupported enable", func(t *testing.T) {
		f := false
		mock := &mockServiceManager{supportsEnableVal: &f, nameVal: "launchd"}
		restore := setupMockManager(mock)
		defer restore()

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleEnable(cfg)
		if err == nil {
			t.Fatal("expected error when SupportsEnable() returns false")
		}
		if !contains(err.Error(), "not supported") {
			t.Errorf("expected error containing 'not supported', got %v", err)
		}
		if !contains(err.Error(), "launchd") {
			t.Errorf("expected error containing init system name 'launchd', got %v", err)
		}
		if mock.enableCalled {
			t.Error("Enable() should not be called when SupportsEnable() returns false")
		}
	})

	t.Run("capability check suggestion for launchd", func(t *testing.T) {
		f := false
		mock := &mockServiceManager{supportsEnableVal: &f, nameVal: "launchd"}
		restore := setupMockManager(mock)
		defer restore()

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleEnable(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if !contains(err.Error(), "RunAtLoad") {
			t.Errorf("expected launchd suggestion containing 'RunAtLoad', got %v", err)
		}
	})
}

func TestHandleDisable(t *testing.T) {
	tests := []struct {
		name        string
		disableErr  error
		wantErr     bool
		errContains string
	}{
		{name: "success"},
		{name: "disable error", disableErr: errors.New("permission denied"), wantErr: true, errContains: "testapp: permission denied"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockServiceManager{disableErr: tt.disableErr}
			restore := setupMockManager(mock)
			defer restore()

			cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
			err := handleDisable(cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("handleDisable() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errContains != "" {
				if err == nil || !contains(err.Error(), tt.errContains) {
					t.Errorf("expected error containing %q, got %v", tt.errContains, err)
				}
			}
			if !mock.disableCalled {
				t.Error("expected Disable() to be called")
			}
		})
	}

	t.Run("detect manager error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		detectManager = func() (daemon.ServiceManager, error) { return nil, errors.New("no manager") }

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleDisable(cfg)
		if err == nil || !contains(err.Error(), "detecting service manager") {
			t.Errorf("expected detect error, got %v", err)
		}
	})

	t.Run("capability check rejects unsupported disable", func(t *testing.T) {
		f := false
		mock := &mockServiceManager{supportsEnableVal: &f, nameVal: "launchd"}
		restore := setupMockManager(mock)
		defer restore()

		cfg := &Config{Name: "testapp", Logger: NewDefaultLogger(LogLevelInfo)}
		err := handleDisable(cfg)
		if err == nil {
			t.Fatal("expected error when SupportsEnable() returns false")
		}
		if !contains(err.Error(), "not supported") {
			t.Errorf("expected error containing 'not supported', got %v", err)
		}
		if !contains(err.Error(), "launchd") {
			t.Errorf("expected error containing init system name 'launchd', got %v", err)
		}
		if mock.disableCalled {
			t.Error("Disable() should not be called when SupportsEnable() returns false")
		}
	})
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestInstallWithPatchers(t *testing.T) {
	saveIST := initSystemTypeFn
	defer func() { initSystemTypeFn = saveIST }()
	initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

	t.Run("zero patchers calls Install directly", func(t *testing.T) {
		mock := &mockServiceManager{}
		svcConfig := daemon.ServiceConfig{Name: "testapp", ExecPath: "/opt/testapp/testapp"}

		err := installWithPatchers(mock, svcConfig, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mock.installCalled {
			t.Fatal("expected Install to be called with no patchers")
		}
		if mock.generateCalled {
			t.Error("expected GenerateConfig NOT to be called with no patchers")
		}
	})

	t.Run("zero patchers with empty slice calls Install directly", func(t *testing.T) {
		mock := &mockServiceManager{}
		svcConfig := daemon.ServiceConfig{Name: "testapp", ExecPath: "/opt/testapp/testapp"}

		err := installWithPatchers(mock, svcConfig, []ConfigPatcher{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mock.installCalled {
			t.Fatal("expected Install to be called with empty patchers slice")
		}
	})

	t.Run("single patcher applied", func(t *testing.T) {
		mock := &mockServiceManager{
			generateContent: "[Unit]\nDescription=Test\n",
			manifestPath:    "/etc/systemd/system/testapp.service",
		}
		svcConfig := daemon.ServiceConfig{Name: "testapp", ExecPath: "/opt/testapp/testapp"}

		patcherCalled := false
		var receivedInfo PatchInfo
		patcher := func(_ string, content string, info PatchInfo) (string, error) {
			patcherCalled = true
			receivedInfo = info
			return content + "# patched\n", nil
		}

		err := installWithPatchers(mock, svcConfig, []ConfigPatcher{patcher})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !patcherCalled {
			t.Fatal("expected patcher to be called")
		}
		if !mock.generateCalled {
			t.Fatal("expected GenerateConfig to be called")
		}
		if !mock.writeCalled {
			t.Fatal("expected WriteConfig to be called")
		}
		if mock.installCalled {
			t.Error("expected Install NOT to be called when patchers are present")
		}
		expectedContent := "[Unit]\nDescription=Test\n# patched\n"
		if mock.writeContent != expectedContent {
			t.Errorf("expected written content %q, got %q", expectedContent, mock.writeContent)
		}
		if receivedInfo.ServiceName != "testapp" {
			t.Errorf("expected ServiceName testapp, got %s", receivedInfo.ServiceName)
		}
		if receivedInfo.InitSystem != "systemd" {
			t.Errorf("expected InitSystem systemd, got %s", receivedInfo.InitSystem)
		}
		if receivedInfo.FilePath != "/etc/systemd/system/testapp.service" {
			t.Errorf("expected FilePath to match manifest path, got %s", receivedInfo.FilePath)
		}
	})

	t.Run("multiple patchers applied in order", func(t *testing.T) {
		mock := &mockServiceManager{
			generateContent: "base",
			manifestPath:    "/etc/systemd/system/testapp.service",
		}
		svcConfig := daemon.ServiceConfig{Name: "testapp"}

		var callOrder []int
		patcher1 := func(_ string, content string, _ PatchInfo) (string, error) {
			callOrder = append(callOrder, 1)
			return content + "-first", nil
		}
		patcher2 := func(_ string, content string, _ PatchInfo) (string, error) {
			callOrder = append(callOrder, 2)
			return content + "-second", nil
		}

		err := installWithPatchers(mock, svcConfig, []ConfigPatcher{patcher1, patcher2})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(callOrder) != 2 || callOrder[0] != 1 || callOrder[1] != 2 {
			t.Errorf("expected patchers called in order [1, 2], got %v", callOrder)
		}
		if mock.writeContent != "base-first-second" {
			t.Errorf("expected chained content 'base-first-second', got %q", mock.writeContent)
		}
	})

	t.Run("patcher error stops chain", func(t *testing.T) {
		mock := &mockServiceManager{
			generateContent: "base",
			manifestPath:    "/etc/systemd/system/testapp.service",
		}
		svcConfig := daemon.ServiceConfig{Name: "testapp"}

		patcher1 := func(_ string, _ string, _ PatchInfo) (string, error) {
			return "", errors.New("patcher failed")
		}
		patcher2Called := false
		patcher2 := func(_ string, content string, _ PatchInfo) (string, error) {
			patcher2Called = true
			return content, nil
		}

		err := installWithPatchers(mock, svcConfig, []ConfigPatcher{patcher1, patcher2})
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "applying config patcher 0") {
			t.Errorf("expected error about patcher 0, got: %v", err)
		}
		if patcher2Called {
			t.Error("expected second patcher NOT to be called after first patcher error")
		}
		if mock.writeCalled {
			t.Error("expected WriteConfig NOT to be called when patcher fails")
		}
	})

	t.Run("generate config error", func(t *testing.T) {
		mock := &mockServiceManager{
			generateErr: errors.New("generate failed"),
		}
		svcConfig := daemon.ServiceConfig{Name: "testapp"}
		patcher := func(_ string, content string, _ PatchInfo) (string, error) {
			return content, nil
		}

		err := installWithPatchers(mock, svcConfig, []ConfigPatcher{patcher})
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "generating config") {
			t.Errorf("expected 'generating config' error, got: %v", err)
		}
	})

	t.Run("write config error", func(t *testing.T) {
		mock := &mockServiceManager{
			generateContent: "content",
			manifestPath:    "/etc/systemd/system/testapp.service",
			writeErr:        errors.New("write failed"),
		}
		svcConfig := daemon.ServiceConfig{Name: "testapp"}
		patcher := func(_ string, content string, _ PatchInfo) (string, error) {
			return content, nil
		}

		err := installWithPatchers(mock, svcConfig, []ConfigPatcher{patcher})
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "writing config") {
			t.Errorf("expected 'writing config' error, got: %v", err)
		}
	})
}

func TestDetectInitSystemType(t *testing.T) {
	saveIST := initSystemTypeFn
	defer func() { initSystemTypeFn = saveIST }()

	initSystemTypeFn = func(_ daemon.ServiceManager) string { return "test-system" }
	mock := &mockServiceManager{}
	got := detectInitSystemType(mock)
	if got != "test-system" {
		t.Errorf("expected 'test-system', got %q", got)
	}
}

func TestHandleInstallWithPatchers(t *testing.T) {
	t.Run("install with patchers applies them", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()

		mock := &mockServiceManager{
			generateContent: "original config",
			manifestPath:    "/etc/systemd/system/testapp.service",
		}
		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		getExecutable = func() (string, error) { return filepath.Join("/opt/testapp", InstalledBinaryName("testapp")), nil }
		saveStateFn = func(_ string, _ *StateData) error { return nil }

		patcherCalled := false
		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
			ConfigPatchers: []ConfigPatcher{
				func(_ string, content string, _ PatchInfo) (string, error) {
					patcherCalled = true
					return content + "\n# custom", nil
				},
			},
		}

		err := handleInstall(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !patcherCalled {
			t.Fatal("expected patcher to be called during install")
		}
		if !mock.generateCalled {
			t.Fatal("expected GenerateConfig to be called")
		}
		if !mock.writeCalled {
			t.Fatal("expected WriteConfig to be called")
		}
		if mock.installCalled {
			t.Error("expected Install NOT to be called when patchers are used")
		}
	})

	t.Run("install without patchers calls Install", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		mock := &mockServiceManager{}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		getExecutable = func() (string, error) { return filepath.Join("/opt/testapp", InstalledBinaryName("testapp")), nil }
		saveStateFn = func(_ string, _ *StateData) error { return nil }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleInstall(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mock.installCalled {
			t.Fatal("expected Install to be called without patchers")
		}
	})
}

func TestHandleInstall_SavesState(t *testing.T) {
	t.Run("saves state after successful install", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()

		fixedTime := time.Date(2026, 3, 20, 15, 30, 0, 0, time.UTC)
		timeNow = func() time.Time { return fixedTime }
		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		mock := &mockServiceManager{manifestPath: "/etc/systemd/system/testapp.service"}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = daemon.InitServiceManagerWithOptions
		getExecutable = func() (string, error) { return filepath.Join("/opt/testapp", InstalledBinaryName("testapp")), nil }

		var savedState *StateData
		var savedInstallDir string
		saveStateFn = func(installDir string, state *StateData) error {
			savedInstallDir = installDir
			savedState = state
			return nil
		}

		cfg := &Config{
			Name:       "testapp",
			Version:    "1.5.0",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleInstall(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if savedState == nil {
			t.Fatal("expected SaveState to be called")
		}
		if savedInstallDir != "/opt/testapp" {
			t.Errorf("expected install dir /opt/testapp, got %s", savedInstallDir)
		}
		if savedState.ServiceName != "testapp" {
			t.Errorf("expected service name testapp, got %s", savedState.ServiceName)
		}
		if savedState.InitSystem != "systemd" {
			t.Errorf("expected init system systemd, got %s", savedState.InitSystem)
		}
		if savedState.ManifestPath != "/etc/systemd/system/testapp.service" {
			t.Errorf("expected manifest path, got %s", savedState.ManifestPath)
		}
		wantBinaryPath := filepath.Join("/opt/testapp", InstalledBinaryName("testapp"))
		if savedState.BinaryPath != wantBinaryPath {
			t.Errorf("expected binary path %s, got %s", wantBinaryPath, savedState.BinaryPath)
		}
		if !savedState.InstalledAt.Equal(fixedTime) {
			t.Errorf("expected installed at %v, got %v", fixedTime, savedState.InstalledAt)
		}
		if savedState.Version != "1.5.0" {
			t.Errorf("expected version 1.5.0, got %s", savedState.Version)
		}
	})

	t.Run("state save failure fails the install", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		mock := &mockServiceManager{}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = daemon.InitServiceManagerWithOptions
		getExecutable = func() (string, error) { return filepath.Join("/opt/testapp", InstalledBinaryName("testapp")), nil }
		saveStateFn = func(_ string, _ *StateData) error {
			return errors.New("write error")
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleInstall(cfg)
		if err == nil {
			t.Fatal("expected an error when the state file cannot be written")
		}
		if !strings.Contains(err.Error(), "state file could not be written") {
			t.Errorf("expected the error to name the state file, got: %v", err)
		}
	})
}

func TestHandleUninstall_DeletesState(t *testing.T) {
	t.Run("deletes state after successful uninstall", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }

		loadStateFn = installedState("testapp")

		var deletedDir, deletedName string
		deleteStateFn = func(installDir, serviceName string) error {
			deletedDir = installDir
			deletedName = serviceName
			return nil
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleUninstall(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if deletedDir != "/opt/testapp" {
			t.Errorf("expected delete install dir /opt/testapp, got %s", deletedDir)
		}
		if deletedName != "testapp" {
			t.Errorf("expected delete service name testapp, got %s", deletedName)
		}
	})

	t.Run("state delete failure is non-fatal", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		mock := &mockServiceManager{}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		deleteStateFn = func(_, _ string) error {
			return errors.New("delete error")
		}
		loadStateFn = installedState("testapp")

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleUninstall(cfg)
		if err != nil {
			t.Fatalf("expected no error when state delete fails (non-fatal), got: %v", err)
		}
		if !mock.uninstallCalled {
			t.Error("expected Uninstall to be called")
		}
	})
}

func TestNormalizeWhitespace(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty string", input: "", want: ""},
		{name: "only whitespace", input: "  \n  \n  ", want: ""},
		{name: "already normalized", input: "line1\nline2", want: "line1\nline2"},
		{name: "trailing spaces", input: "line1  \nline2  ", want: "line1\nline2"},
		{name: "leading spaces", input: "  line1\n  line2", want: "line1\nline2"},
		{name: "empty lines removed", input: "line1\n\n\nline2", want: "line1\nline2"},
		{name: "tabs trimmed", input: "\tline1\n\tline2\t", want: "line1\nline2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeWhitespace(tt.input)
			if got != tt.want {
				t.Errorf("normalizeWhitespace(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildServiceConfig(t *testing.T) {
	cfg := &Config{
		Name:        "testapp",
		Description: "Test application",
		InstallDir:  "/opt/testapp",
	}
	svc := buildServiceConfig(cfg)
	if svc.Name != "testapp" {
		t.Errorf("expected Name testapp, got %s", svc.Name)
	}
	if svc.Description != "Test application" {
		t.Errorf("expected Description 'Test application', got %s", svc.Description)
	}
	wantBinaryPath := filepath.Join("/opt/testapp", InstalledBinaryName("testapp"))
	if svc.ExecPath != wantBinaryPath {
		t.Errorf("expected ExecPath %s, got %s", wantBinaryPath, svc.ExecPath)
	}
	if svc.InstallDir != "/opt/testapp" {
		t.Errorf("expected InstallDir /opt/testapp, got %s", svc.InstallDir)
	}
	if svc.WorkingDir != "/opt/testapp" {
		t.Errorf("expected WorkingDir /opt/testapp, got %s", svc.WorkingDir)
	}
}

func TestBuildServiceConfigNoOptionalFields(t *testing.T) {
	cfg := &Config{
		Name:        "testapp",
		Description: "Test application",
		InstallDir:  "/opt/testapp",
	}
	svc := buildServiceConfig(cfg)
	if svc.Environment != nil {
		t.Error("expected nil Environment")
	}
	if len(svc.Arguments) != 1 || svc.Arguments[0] != "run" {
		t.Errorf("expected [run], got %v", svc.Arguments)
	}
	if svc.User != "" {
		t.Errorf("expected empty User, got %q", svc.User)
	}
	if svc.Group != "" {
		t.Errorf("expected empty Group, got %q", svc.Group)
	}
	if svc.SystemdDirectives != nil {
		t.Error("expected nil SystemdDirectives")
	}
	if svc.LaunchdKeys != nil {
		t.Error("expected nil LaunchdKeys")
	}
	if svc.OpenRCDirectives != nil {
		t.Error("expected nil OpenRCDirectives")
	}
	if svc.CrontabSchedule != "" {
		t.Errorf("expected empty CrontabSchedule, got %q", svc.CrontabSchedule)
	}
}

func TestBuildServiceConfigAllOptionalFields(t *testing.T) {
	cfg := &Config{
		Name:        "testapp",
		Description: "Test application",
		InstallDir:  "/opt/testapp",
		ServiceEnvironment: map[string]string{
			"ENV_VAR": "value",
			"DEBUG":   "true",
		},
		ServiceArguments:  []string{"--port", "8080"},
		ServiceUser:       "appuser",
		ServiceGroup:      "appgroup",
		SystemdDirectives: map[string]string{"LimitNOFILE": "65536"},
		LaunchdKeys:       map[string]any{"KeepAlive": true},
		OpenRCDirectives:  map[string]string{"rc_ulimit": "-n 65536"},
		CrontabSchedule:   "*/5 * * * *",
	}
	svc := buildServiceConfig(cfg)

	if len(svc.Environment) != 2 {
		t.Errorf("expected 2 env vars, got %d", len(svc.Environment))
	}
	if svc.Environment["ENV_VAR"] != "value" {
		t.Errorf("expected ENV_VAR=value, got %q", svc.Environment["ENV_VAR"])
	}
	if svc.Environment["DEBUG"] != "true" {
		t.Errorf("expected DEBUG=true, got %q", svc.Environment["DEBUG"])
	}
	if len(svc.Arguments) != 3 || svc.Arguments[0] != "run" || svc.Arguments[1] != "--port" || svc.Arguments[2] != "8080" {
		t.Errorf("expected [run --port 8080], got %v", svc.Arguments)
	}
	if svc.User != "appuser" {
		t.Errorf("expected User appuser, got %q", svc.User)
	}
	if svc.Group != "appgroup" {
		t.Errorf("expected Group appgroup, got %q", svc.Group)
	}
	if svc.SystemdDirectives["LimitNOFILE"] != "65536" {
		t.Errorf("expected SystemdDirectives LimitNOFILE=65536, got %q", svc.SystemdDirectives["LimitNOFILE"])
	}
	if svc.LaunchdKeys["KeepAlive"] != true {
		t.Errorf("expected LaunchdKeys KeepAlive=true, got %v", svc.LaunchdKeys["KeepAlive"])
	}
	if svc.OpenRCDirectives["rc_ulimit"] != "-n 65536" {
		t.Errorf("expected OpenRCDirectives rc_ulimit='-n 65536', got %q", svc.OpenRCDirectives["rc_ulimit"])
	}
	if svc.CrontabSchedule != "*/5 * * * *" {
		t.Errorf("expected CrontabSchedule '*/5 * * * *', got %q", svc.CrontabSchedule)
	}
}

func TestValidateServiceConfig(t *testing.T) {
	t.Run("matching config succeeds", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		saveReadInstalled := readInstalledConfigFn
		defer func() { readInstalledConfigFn = saveReadInstalled }()

		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		expectedConfig := "[Unit]\nDescription=Test\n\n[Service]\nExecStart=/opt/testapp/testapp run\n"
		mock := &mockServiceManager{
			generateContent:      expectedConfig,
			manifestPath:         "/etc/systemd/system/testapp.service",
			readInstalledContent: expectedConfig,
		}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(_, _ string) (*StateData, error) {
			return &StateData{
				ServiceName:  "testapp",
				InitSystem:   "systemd",
				ManifestPath: "/etc/systemd/system/testapp.service",
				BinaryPath:   "/opt/testapp/testapp",
			}, nil
		}
		readInstalledConfigFn = func(mgr daemon.ServiceManager) (string, error) {
			return mgr.ReadInstalledConfig()
		}

		cfg := &Config{
			Name:        "testapp",
			Description: "Test",
			InstallDir:  "/opt/testapp",
			Logger:      NewDefaultLogger(LogLevelInfo),
		}

		err := validateServiceConfig(cfg)
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if !mock.generateCalled {
			t.Error("expected GenerateConfig to be called")
		}
	})

	t.Run("config drift detected", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		saveReadInstalled := readInstalledConfigFn
		defer func() { readInstalledConfigFn = saveReadInstalled }()

		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		mock := &mockServiceManager{
			generateContent:      "[Unit]\nDescription=Expected\n",
			manifestPath:         "/etc/systemd/system/testapp.service",
			readInstalledContent: "[Unit]\nDescription=Modified manually\n",
		}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(_, _ string) (*StateData, error) {
			return &StateData{
				ServiceName:  "testapp",
				InitSystem:   "systemd",
				ManifestPath: "/etc/systemd/system/testapp.service",
			}, nil
		}
		readInstalledConfigFn = func(mgr daemon.ServiceManager) (string, error) {
			return mgr.ReadInstalledConfig()
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := validateServiceConfig(cfg)
		if err == nil {
			t.Fatal("expected error for config drift")
		}
		if !strings.Contains(err.Error(), "configuration drift detected") {
			t.Errorf("expected drift error, got: %v", err)
		}
	})

	t.Run("init system change detected", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()

		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "openrc" }

		mock := &mockServiceManager{}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(_, _ string) (*StateData, error) {
			return &StateData{
				ServiceName: "testapp",
				InitSystem:  "systemd",
			}, nil
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := validateServiceConfig(cfg)
		if err == nil {
			t.Fatal("expected error for init system change")
		}
		if !strings.Contains(err.Error(), "init system changed") {
			t.Errorf("expected init system change error, got: %v", err)
		}
		if !strings.Contains(err.Error(), "systemd") || !strings.Contains(err.Error(), "openrc") {
			t.Errorf("expected error to mention both init systems, got: %v", err)
		}
	})

	t.Run("missing state file", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		loadStateFn = func(_, _ string) (*StateData, error) {
			return nil, nil
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := validateServiceConfig(cfg)
		if err == nil {
			t.Fatal("expected error for missing state file")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("expected 'not installed' error, got: %v", err)
		}
	})

	t.Run("state load error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		loadStateFn = func(_, _ string) (*StateData, error) {
			return nil, errors.New("read error")
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := validateServiceConfig(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "loading state") {
			t.Errorf("expected 'loading state' error, got: %v", err)
		}
	})

	t.Run("manager construction error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		loadStateFn = func(_, _ string) (*StateData, error) {
			return &StateData{ServiceName: "testapp", InitSystem: "systemd"}, nil
		}
		detectManager = func() (daemon.ServiceManager, error) {
			return nil, errors.New("no manager")
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := validateServiceConfig(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), `service manager for "systemd"`) {
			t.Errorf("expected a manager construction error, got: %v", err)
		}
	})

	t.Run("matching config with whitespace differences succeeds", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		saveReadInstalled := readInstalledConfigFn
		defer func() { readInstalledConfigFn = saveReadInstalled }()

		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		mock := &mockServiceManager{
			generateContent:      "[Unit]\nDescription=Test\n\n[Service]\nExecStart=/opt/testapp/testapp run\n",
			manifestPath:         "/etc/systemd/system/testapp.service",
			readInstalledContent: "  [Unit]  \n  Description=Test  \n\n\n  [Service]  \n  ExecStart=/opt/testapp/testapp run  \n",
		}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(_, _ string) (*StateData, error) {
			return &StateData{
				ServiceName:  "testapp",
				InitSystem:   "systemd",
				ManifestPath: "/etc/systemd/system/testapp.service",
			}, nil
		}
		readInstalledConfigFn = func(mgr daemon.ServiceManager) (string, error) {
			return mgr.ReadInstalledConfig()
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := validateServiceConfig(cfg)
		if err != nil {
			t.Fatalf("expected no error with whitespace differences, got: %v", err)
		}
	})

	t.Run("matching config with patchers", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		saveReadInstalled := readInstalledConfigFn
		defer func() { readInstalledConfigFn = saveReadInstalled }()

		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		mock := &mockServiceManager{
			generateContent:      "base config",
			manifestPath:         "/etc/systemd/system/testapp.service",
			readInstalledContent: "base config\n# patched",
		}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(_, _ string) (*StateData, error) {
			return &StateData{
				ServiceName:  "testapp",
				InitSystem:   "systemd",
				ManifestPath: "/etc/systemd/system/testapp.service",
			}, nil
		}
		readInstalledConfigFn = func(mgr daemon.ServiceManager) (string, error) {
			return mgr.ReadInstalledConfig()
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
			ConfigPatchers: []ConfigPatcher{
				func(_ string, content string, _ PatchInfo) (string, error) {
					return content + "\n# patched", nil
				},
			},
		}

		err := validateServiceConfig(cfg)
		if err != nil {
			t.Fatalf("expected no error with patchers, got: %v", err)
		}
	})

	t.Run("read manifest error", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		saveReadInstalled := readInstalledConfigFn
		defer func() { readInstalledConfigFn = saveReadInstalled }()

		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		mock := &mockServiceManager{
			generateContent:  "config",
			manifestPath:     "/etc/systemd/system/testapp.service",
			readInstalledErr: errors.New("file not found"),
		}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(_, _ string) (*StateData, error) {
			return &StateData{
				ServiceName:  "testapp",
				InitSystem:   "systemd",
				ManifestPath: "/etc/systemd/system/testapp.service",
			}, nil
		}
		readInstalledConfigFn = func(mgr daemon.ServiceManager) (string, error) {
			return mgr.ReadInstalledConfig()
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := validateServiceConfig(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "reading installed config") {
			t.Errorf("expected 'reading installed config' error, got: %v", err)
		}
	})
}

func TestDaemonpilotValidate(t *testing.T) {
	t.Run("delegates to validateServiceConfig", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		loadStateFn = func(_, _ string) (*StateData, error) {
			return nil, nil
		}

		s := &Daemonpilot{config: Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}}

		err := s.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("expected 'not installed' error, got: %v", err)
		}
	})
}

func TestHandleValidate(t *testing.T) {
	t.Run("success when config matches", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		saveReadInstalled := readInstalledConfigFn
		defer func() { readInstalledConfigFn = saveReadInstalled }()

		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		mock := &mockServiceManager{
			manifestPath:         "/etc/systemd/system/testapp.service",
			generateContent:      "generated-config",
			readInstalledContent: "generated-config",
		}

		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(_, _ string) (*StateData, error) {
			return &StateData{
				ServiceName:  "testapp",
				InitSystem:   "systemd",
				ManifestPath: "/etc/systemd/system/testapp.service",
			}, nil
		}
		readInstalledConfigFn = func(mgr daemon.ServiceManager) (string, error) {
			return mgr.ReadInstalledConfig()
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleValidate(cfg)
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	t.Run("error when validation fails", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		loadStateFn = func(_, _ string) (*StateData, error) {
			return nil, nil
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleValidate(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "validation failed") {
			t.Errorf("expected 'validation failed' in error, got: %v", err)
		}
	})

	t.Run("error on config drift", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		saveReadInstalled := readInstalledConfigFn
		defer func() { readInstalledConfigFn = saveReadInstalled }()

		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		mock := &mockServiceManager{
			manifestPath:         "/etc/systemd/system/testapp.service",
			generateContent:      "expected-config",
			readInstalledContent: "different-config",
		}

		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(_, _ string) (*StateData, error) {
			return &StateData{
				ServiceName:  "testapp",
				InitSystem:   "systemd",
				ManifestPath: "/etc/systemd/system/testapp.service",
			}, nil
		}
		readInstalledConfigFn = func(mgr daemon.ServiceManager) (string, error) {
			return mgr.ReadInstalledConfig()
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleValidate(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "drift") {
			t.Errorf("expected 'drift' in error, got: %v", err)
		}
	})

	t.Run("routes via CLI", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		loadStateFn = func(_, _ string) (*StateData, error) {
			return nil, nil
		}

		s := &Daemonpilot{config: Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}}

		orig := osArgs
		osArgs = func() []string { return []string{"testapp", "validate"} }
		defer func() { osArgs = orig }()

		err := s.Run()
		if err == nil {
			t.Fatal("expected error from validate (no state)")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("expected 'not installed' in error, got: %v", err)
		}
	})
}

// update-config reads the state file before it stops the service and writes it
// back after the restart. A staged update that records a rollback digest in that
// window — the whole command for a crontab-managed service, which is never
// stopped — must not be written back out of the stale snapshot.
func TestHandleUpdateConfigKeepsAConcurrentRollbackDigest(t *testing.T) {
	saved := saveCommandVars()
	defer restoreCommandVars2(saved)
	saveIST := initSystemTypeFn
	defer func() { initSystemTypeFn = saveIST }()
	saveReadFile := readFileFn
	defer func() { readFileFn = saveReadFile }()
	saveWriteFile := writeFileFn
	defer func() { writeFileFn = saveWriteFile }()
	saveRemoveFile := removeFileFn
	defer func() { removeFileFn = saveRemoveFile }()

	const digest = "d1d2d3d4"

	initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }
	config := "[Unit]\nDescription=Updated\n\n[Service]\nExecStart=/opt/testapp/testapp run\n"
	mock := &mockServiceManager{generateContent: config, manifestPath: "/etc/systemd/system/testapp.service"}
	detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
	readFileFn = func(string) ([]byte, error) { return []byte(config), nil }
	writeFileFn = func(string, []byte, os.FileMode) error { return nil }
	removeFileFn = func(string) error { return nil }

	// The first read is the snapshot update-config binds to; by the time it
	// re-reads before saving, the updater has recorded a digest.
	reads := 0
	loadStateFn = func(_, _ string) (*StateData, error) {
		reads++
		state := &StateData{
			ServiceName:  "testapp",
			InitSystem:   "systemd",
			ManifestPath: "/etc/systemd/system/testapp.service",
			BinaryPath:   "/opt/testapp/testapp",
		}
		if reads > 1 {
			state.RollbackDigest = digest
		}
		return state, nil
	}

	var savedState *StateData
	saveStateFn = func(_ string, state *StateData) error {
		savedState = state
		return nil
	}

	cfg := &Config{
		Name:        "testapp",
		Description: "Updated",
		InstallDir:  "/opt/testapp",
		Logger:      NewDefaultLogger(LogLevelInfo),
	}
	if err := handleUpdateConfig(cfg); err != nil {
		t.Fatalf("handleUpdateConfig() error: %v", err)
	}
	if savedState == nil {
		t.Fatal("expected SaveState to be called")
	}
	if savedState.RollbackDigest != digest {
		t.Errorf("RollbackDigest = %q, want %q", savedState.RollbackDigest, digest)
	}
}

func TestHandleUpdateConfig(t *testing.T) {
	t.Run("success path", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		saveReadFile := readFileFn
		defer func() { readFileFn = saveReadFile }()
		saveWriteFile := writeFileFn
		defer func() { writeFileFn = saveWriteFile }()
		saveRemoveFile := removeFileFn
		defer func() { removeFileFn = saveRemoveFile }()

		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		newConfig := "[Unit]\nDescription=Updated\n\n[Service]\nExecStart=/opt/testapp/testapp run\n"
		oldConfig := "[Unit]\nDescription=Old\n\n[Service]\nExecStart=/opt/testapp/testapp run\n"

		mock := &mockServiceManager{
			generateContent: newConfig,
			manifestPath:    "/etc/systemd/system/testapp.service",
		}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(_, _ string) (*StateData, error) {
			return &StateData{
				ServiceName:  "testapp",
				InitSystem:   "systemd",
				ManifestPath: "/etc/systemd/system/testapp.service",
				BinaryPath:   "/opt/testapp/testapp",
			}, nil
		}
		readFileFn = func(_ string) ([]byte, error) {
			return []byte(oldConfig), nil
		}

		var backupWritten bool
		var backupPath string
		var backupRemoved bool
		var stateUpdated bool
		writeFileFn = func(path string, _ []byte, _ os.FileMode) error {
			backupWritten = true
			backupPath = path
			return nil
		}
		removeFileFn = func(path string) error {
			if path == backupPath {
				backupRemoved = true
			}
			return nil
		}
		saveStateFn = func(_ string, _ *StateData) error {
			stateUpdated = true
			return nil
		}

		cfg := &Config{
			Name:        "testapp",
			Description: "Updated",
			InstallDir:  "/opt/testapp",
			Logger:      NewDefaultLogger(LogLevelInfo),
		}

		err := handleUpdateConfig(cfg)
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if !mock.generateCalled {
			t.Error("expected GenerateConfig to be called")
		}
		if !mock.stopCalled {
			t.Error("expected Stop to be called")
		}
		if !mock.writeCalled {
			t.Error("expected WriteConfig to be called")
		}
		if mock.writeContent != newConfig {
			t.Errorf("expected WriteConfig with new content, got: %s", mock.writeContent)
		}
		if !mock.startCalled {
			t.Error("expected Start to be called")
		}
		if !backupWritten {
			t.Error("expected backup to be written")
		}
		if !strings.HasSuffix(backupPath, ".backup") {
			t.Errorf("expected backup path to end with .backup, got: %s", backupPath)
		}
		if !backupRemoved {
			t.Error("expected backup to be cleaned up after success")
		}
		if !stateUpdated {
			t.Error("expected state file to be updated after successful config update")
		}
	})

	t.Run("not installed", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)

		loadStateFn = func(_, _ string) (*StateData, error) {
			return nil, nil
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleUpdateConfig(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("expected 'not installed' error, got: %v", err)
		}
	})

	t.Run("init system changed", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()

		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "openrc" }

		mock := &mockServiceManager{}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(_, _ string) (*StateData, error) {
			return &StateData{
				ServiceName: "testapp",
				InitSystem:  "systemd",
			}, nil
		}

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleUpdateConfig(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "init system changed") {
			t.Errorf("expected init system change error, got: %v", err)
		}
	})

	t.Run("start failure triggers rollback", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		saveReadFile := readFileFn
		defer func() { readFileFn = saveReadFile }()
		saveWriteFile := writeFileFn
		defer func() { writeFileFn = saveWriteFile }()
		saveRemoveFile := removeFileFn
		defer func() { removeFileFn = saveRemoveFile }()

		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		newConfig := "[Unit]\nDescription=New\n"
		oldConfig := "[Unit]\nDescription=Old\n"

		startCallCount := 0
		mock := &mockServiceManager{
			generateContent:      newConfig,
			manifestPath:         "/etc/systemd/system/testapp.service",
			readInstalledContent: oldConfig,
		}

		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(_, _ string) (*StateData, error) {
			return &StateData{
				ServiceName:  "testapp",
				InitSystem:   "systemd",
				ManifestPath: "/etc/systemd/system/testapp.service",
			}, nil
		}
		readFileFn = func(_ string) ([]byte, error) {
			return []byte(oldConfig), nil
		}
		writeFileFn = func(_ string, _ []byte, _ os.FileMode) error {
			return nil
		}
		removeFileFn = func(_ string) error { return nil }

		origStart := mock.Start
		_ = origStart
		mock.startErr = errors.New("service crashed on start")

		var writeConfigCalls []string
		origMock := mock
		wrappedMgr := &writeTrackingManager{
			inner:           origMock,
			writeConfigArgs: &writeConfigCalls,
			startCallCount:  &startCallCount,
		}
		detectManager = func() (daemon.ServiceManager, error) { return wrappedMgr, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleUpdateConfig(cfg)
		if err == nil {
			t.Fatal("expected error from start failure")
		}
		if !strings.Contains(err.Error(), "rolled back") {
			t.Errorf("expected 'rolled back' in error, got: %v", err)
		}
		if len(writeConfigCalls) < 2 {
			t.Errorf("expected WriteConfig called at least twice (new + rollback), got %d calls", len(writeConfigCalls))
		}
		if len(writeConfigCalls) >= 2 && writeConfigCalls[1] != oldConfig {
			t.Errorf("expected rollback to write old config, got: %s", writeConfigCalls[1])
		}
	})

	t.Run("with patchers", func(t *testing.T) {
		saved := saveCommandVars()
		defer restoreCommandVars2(saved)
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		saveReadFile := readFileFn
		defer func() { readFileFn = saveReadFile }()
		saveWriteFile := writeFileFn
		defer func() { writeFileFn = saveWriteFile }()
		saveRemoveFile := removeFileFn
		defer func() { removeFileFn = saveRemoveFile }()

		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

		baseConfig := "[Unit]\nDescription=Test\n"
		oldConfig := "[Unit]\nDescription=Old\n"

		mock := &mockServiceManager{
			generateContent: baseConfig,
			manifestPath:    "/etc/systemd/system/testapp.service",
		}
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(_, _ string) (*StateData, error) {
			return &StateData{
				ServiceName:  "testapp",
				InitSystem:   "systemd",
				ManifestPath: "/etc/systemd/system/testapp.service",
			}, nil
		}
		readFileFn = func(_ string) ([]byte, error) {
			return []byte(oldConfig), nil
		}
		writeFileFn = func(_ string, _ []byte, _ os.FileMode) error { return nil }
		removeFileFn = func(_ string) error { return nil }

		patcherCalled := false
		patcher := func(_ string, content string, _ PatchInfo) (string, error) {
			patcherCalled = true
			return content + "# patched\n", nil
		}

		cfg := &Config{
			Name:           "testapp",
			Description:    "Test",
			InstallDir:     "/opt/testapp",
			Logger:         NewDefaultLogger(LogLevelInfo),
			ConfigPatchers: []ConfigPatcher{patcher},
		}

		err := handleUpdateConfig(cfg)
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if !patcherCalled {
			t.Error("expected patcher to be called")
		}
		if !strings.Contains(mock.writeContent, "# patched") {
			t.Errorf("expected patched content to be written, got: %s", mock.writeContent)
		}
	})
}

type writeTrackingManager struct {
	inner           *mockServiceManager
	writeConfigArgs *[]string
	startCallCount  *int
}

func (w *writeTrackingManager) Install(config daemon.ServiceConfig) error {
	return w.inner.Install(config)
}
func (w *writeTrackingManager) Uninstall() error { return w.inner.Uninstall() }
func (w *writeTrackingManager) Start() error {
	*w.startCallCount++
	return w.inner.Start()
}
func (w *writeTrackingManager) Stop() error    { return w.inner.Stop() }
func (w *writeTrackingManager) Enable() error  { return w.inner.Enable() }
func (w *writeTrackingManager) Disable() error { return w.inner.Disable() }
func (w *writeTrackingManager) Status() (daemon.ServiceStatus, error) {
	return w.inner.Status()
}
func (w *writeTrackingManager) GetManifestPath() string { return w.inner.GetManifestPath() }
func (w *writeTrackingManager) GenerateConfig(config daemon.ServiceConfig) (string, error) {
	return w.inner.GenerateConfig(config)
}

func (w *writeTrackingManager) WriteConfig(content string) error {
	*w.writeConfigArgs = append(*w.writeConfigArgs, content)
	return w.inner.WriteConfig(content)
}

func (w *writeTrackingManager) ReadInstalledConfig() (string, error) {
	return w.inner.ReadInstalledConfig()
}
func (w *writeTrackingManager) CanRestartWatchdog() bool { return w.inner.CanRestartWatchdog() }
func (w *writeTrackingManager) SupportsEnable() bool     { return w.inner.SupportsEnable() }
func (w *writeTrackingManager) SupportsStart() bool      { return w.inner.SupportsStart() }
func (w *writeTrackingManager) SupportsStatus() bool     { return w.inner.SupportsStatus() }

func (w *writeTrackingManager) SupportsScope(scope daemon.Scope) bool {
	return w.inner.SupportsScope(scope)
}
func (w *writeTrackingManager) Name() string { return w.inner.Name() }

type mockEnvServiceManager struct {
	mockServiceManager

	writeEnvCalled   bool
	writeEnvErr      error
	readEnvContent   []byte
	readEnvErr       error
	restoreEnvCalled bool
	restoreEnvErr    error
}

func (m *mockEnvServiceManager) WriteEnvironmentFile(_ map[string]string) error {
	m.writeEnvCalled = true
	return m.writeEnvErr
}

func (m *mockEnvServiceManager) ReadEnvironmentFile() ([]byte, error) {
	return m.readEnvContent, m.readEnvErr
}

func (m *mockEnvServiceManager) RestoreEnvironmentFile(_ []byte) error {
	m.restoreEnvCalled = true
	return m.restoreEnvErr
}

func TestInstallWithPatchersEnvFile(t *testing.T) {
	t.Run("writes environment file for EnvironmentFileWriter", func(t *testing.T) {
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "dinit" }

		mock := &mockEnvServiceManager{
			mockServiceManager: mockServiceManager{
				generateContent: "type = process\ncommand = /opt/app/app\n",
				manifestPath:    "/etc/dinit.d/testapp",
			},
		}

		svcConfig := daemon.ServiceConfig{
			Name:        "testapp",
			ExecPath:    "/opt/app/app",
			Environment: map[string]string{"FOO": "bar"},
		}
		patcher := func(_, content string, _ PatchInfo) (string, error) {
			return content, nil
		}

		err := installWithPatchers(mock, svcConfig, []ConfigPatcher{patcher})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mock.writeEnvCalled {
			t.Error("expected WriteEnvironmentFile to be called")
		}
	})

	t.Run("env write failure cleans up", func(t *testing.T) {
		saveIST := initSystemTypeFn
		defer func() { initSystemTypeFn = saveIST }()
		saveRemove := removeFileFn
		defer func() { removeFileFn = saveRemove }()
		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "dinit" }

		var removedPath string
		removeFileFn = func(path string) error {
			removedPath = path
			return nil
		}

		mock := &mockEnvServiceManager{
			mockServiceManager: mockServiceManager{
				generateContent: "type = process\ncommand = /opt/app/app\n",
				manifestPath:    "/etc/dinit.d/testapp",
			},
			writeEnvErr: errors.New("env write failed"),
		}

		svcConfig := daemon.ServiceConfig{
			Name:     "testapp",
			ExecPath: "/opt/app/app",
		}
		patcher := func(_, content string, _ PatchInfo) (string, error) {
			return content, nil
		}

		err := installWithPatchers(mock, svcConfig, []ConfigPatcher{patcher})
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "writing environment file") {
			t.Errorf("expected env file error, got: %v", err)
		}
		if removedPath != "/etc/dinit.d/testapp" {
			t.Errorf("expected manifest to be cleaned up, removed: %s", removedPath)
		}
	})
}

func TestHandleUpdateConfigEnvFile(t *testing.T) {
	setupEnvTest := func(t *testing.T, mock *mockEnvServiceManager) func() {
		t.Helper()
		saved := saveCommandVars()
		saveIST := initSystemTypeFn
		saveReadFile := readFileFn
		saveWriteFile := writeFileFn
		saveRemoveFile := removeFileFn

		initSystemTypeFn = func(_ daemon.ServiceManager) string { return "dinit" }
		detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
		initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
		loadStateFn = func(_, _ string) (*StateData, error) {
			return &StateData{
				ServiceName:  "testapp",
				InitSystem:   "dinit",
				ManifestPath: "/etc/dinit.d/testapp",
				BinaryPath:   "/opt/testapp/testapp",
			}, nil
		}
		readFileFn = func(_ string) ([]byte, error) {
			return []byte("type = process\ncommand = /opt/testapp/testapp\n"), nil
		}
		writeFileFn = func(_ string, _ []byte, _ os.FileMode) error { return nil }
		removeFileFn = func(_ string) error { return nil }
		saveStateFn = func(_ string, _ *StateData) error { return nil }

		return func() {
			restoreCommandVars2(saved)
			initSystemTypeFn = saveIST
			readFileFn = saveReadFile
			writeFileFn = saveWriteFile
			removeFileFn = saveRemoveFile
		}
	}

	t.Run("success with env file read and write", func(t *testing.T) {
		mock := &mockEnvServiceManager{
			mockServiceManager: mockServiceManager{
				generateContent:      "type = process\ncommand = /opt/testapp/testapp\n",
				manifestPath:         "/etc/dinit.d/testapp",
				readInstalledContent: "type = process\ncommand = /opt/testapp/testapp\n",
			},
			readEnvContent: []byte("FOO=old\n"),
		}
		cleanup := setupEnvTest(t, mock)
		defer cleanup()

		cfg := &Config{
			Name:               "testapp",
			Description:        "Test",
			InstallDir:         "/opt/testapp",
			Logger:             NewDefaultLogger(LogLevelInfo),
			ServiceEnvironment: map[string]string{"FOO": "new"},
		}

		err := handleUpdateConfig(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mock.writeEnvCalled {
			t.Error("expected WriteEnvironmentFile to be called")
		}
	})

	t.Run("env read failure triggers config rollback", func(t *testing.T) {
		mock := &mockEnvServiceManager{
			mockServiceManager: mockServiceManager{
				generateContent:      "type = process\ncommand = /opt/testapp/testapp\n",
				manifestPath:         "/etc/dinit.d/testapp",
				readInstalledContent: "type = process\ncommand = /opt/testapp/testapp\n",
			},
			readEnvErr: errors.New("permission denied"),
		}
		cleanup := setupEnvTest(t, mock)
		defer cleanup()

		cfg := &Config{
			Name:       "testapp",
			InstallDir: "/opt/testapp",
			Logger:     NewDefaultLogger(LogLevelInfo),
		}

		err := handleUpdateConfig(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "reading environment file for backup") {
			t.Errorf("expected env read error, got: %v", err)
		}
		if !strings.Contains(err.Error(), "rolled back to previous config") {
			t.Errorf("expected rollback message, got: %v", err)
		}
	})

	t.Run("env write failure with successful env restore", func(t *testing.T) {
		mock := &mockEnvServiceManager{
			mockServiceManager: mockServiceManager{
				generateContent:      "type = process\ncommand = /opt/testapp/testapp\n",
				manifestPath:         "/etc/dinit.d/testapp",
				readInstalledContent: "type = process\ncommand = /opt/testapp/testapp\n",
			},
			readEnvContent: []byte("FOO=old\n"),
			writeEnvErr:    errors.New("disk full"),
		}
		cleanup := setupEnvTest(t, mock)
		defer cleanup()

		cfg := &Config{
			Name:               "testapp",
			InstallDir:         "/opt/testapp",
			Logger:             NewDefaultLogger(LogLevelInfo),
			ServiceEnvironment: map[string]string{"FOO": "new"},
		}

		err := handleUpdateConfig(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "rolled back to previous config") {
			t.Errorf("expected rollback message, got: %v", err)
		}
		if !mock.restoreEnvCalled {
			t.Error("expected RestoreEnvironmentFile to be called")
		}
	})

	t.Run("env write failure with env restore failure", func(t *testing.T) {
		mock := &mockEnvServiceManager{
			mockServiceManager: mockServiceManager{
				generateContent:      "type = process\ncommand = /opt/testapp/testapp\n",
				manifestPath:         "/etc/dinit.d/testapp",
				readInstalledContent: "type = process\ncommand = /opt/testapp/testapp\n",
			},
			readEnvContent: []byte("FOO=old\n"),
			writeEnvErr:    errors.New("disk full"),
			restoreEnvErr:  errors.New("still full"),
		}
		cleanup := setupEnvTest(t, mock)
		defer cleanup()

		cfg := &Config{
			Name:               "testapp",
			InstallDir:         "/opt/testapp",
			Logger:             NewDefaultLogger(LogLevelInfo),
			ServiceEnvironment: map[string]string{"FOO": "new"},
		}

		err := handleUpdateConfig(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "environment file restoration failed") {
			t.Errorf("expected env restoration failure in message, got: %v", err)
		}
	})

	t.Run("start failure with env restore failure", func(t *testing.T) {
		startCount := 0
		mock := &mockEnvServiceManager{
			mockServiceManager: mockServiceManager{
				generateContent:      "type = process\ncommand = /opt/testapp/testapp\n",
				manifestPath:         "/etc/dinit.d/testapp",
				readInstalledContent: "type = process\ncommand = /opt/testapp/testapp\n",
			},
			readEnvContent: []byte("FOO=old\n"),
			restoreEnvErr:  errors.New("restore failed"),
		}
		cleanup := setupEnvTest(t, mock)
		defer cleanup()

		origStart := mock.Start
		_ = origStart
		mock.startErr = nil
		detectManager = func() (daemon.ServiceManager, error) {
			return &envStartTracker{inner: mock, count: &startCount}, nil
		}

		cfg := &Config{
			Name:               "testapp",
			InstallDir:         "/opt/testapp",
			Logger:             NewDefaultLogger(LogLevelInfo),
			ServiceEnvironment: map[string]string{"FOO": "new"},
		}

		err := handleUpdateConfig(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "environment file restoration failed") {
			t.Errorf("expected env restoration failure in message, got: %v", err)
		}
	})
}

type envStartTracker struct {
	inner *mockEnvServiceManager
	count *int
}

func (e *envStartTracker) Install(config daemon.ServiceConfig) error {
	return e.inner.Install(config)
}
func (e *envStartTracker) Uninstall() error { return e.inner.Uninstall() }
func (e *envStartTracker) Start() error {
	*e.count++
	if *e.count == 1 {
		return errors.New("service crashed on start")
	}
	return nil
}
func (e *envStartTracker) Stop() error                           { return e.inner.Stop() }
func (e *envStartTracker) Enable() error                         { return e.inner.Enable() }
func (e *envStartTracker) Disable() error                        { return e.inner.Disable() }
func (e *envStartTracker) Status() (daemon.ServiceStatus, error) { return e.inner.Status() }
func (e *envStartTracker) GetManifestPath() string               { return e.inner.GetManifestPath() }

func (e *envStartTracker) GenerateConfig(c daemon.ServiceConfig) (string, error) {
	return e.inner.GenerateConfig(c)
}

func (e *envStartTracker) WriteConfig(content string) error {
	return e.inner.WriteConfig(content)
}

func (e *envStartTracker) ReadInstalledConfig() (string, error) {
	return e.inner.ReadInstalledConfig()
}
func (e *envStartTracker) CanRestartWatchdog() bool { return e.inner.CanRestartWatchdog() }
func (e *envStartTracker) SupportsEnable() bool     { return e.inner.SupportsEnable() }
func (e *envStartTracker) SupportsStart() bool      { return e.inner.SupportsStart() }
func (e *envStartTracker) SupportsStatus() bool     { return e.inner.SupportsStatus() }

func (e *envStartTracker) SupportsScope(scope daemon.Scope) bool {
	return e.inner.SupportsScope(scope)
}
func (e *envStartTracker) Name() string { return e.inner.Name() }
func (e *envStartTracker) WriteEnvironmentFile(env map[string]string) error {
	return e.inner.WriteEnvironmentFile(env)
}

func (e *envStartTracker) ReadEnvironmentFile() ([]byte, error) {
	return e.inner.ReadEnvironmentFile()
}

func (e *envStartTracker) RestoreEnvironmentFile(content []byte) error {
	return e.inner.RestoreEnvironmentFile(content)
}

func TestHandleInstall_RecordsScope(t *testing.T) {
	tests := []struct {
		name           string
		scope          InstallScope
		wantScope      InstallScope
		wantInstallDir string
	}{
		{
			name:      "system scope leaves the scope fields absent",
			scope:     ScopeSystem,
			wantScope: "",
		},
		{
			name:      "zero value leaves the scope fields absent",
			scope:     "",
			wantScope: "",
		},
		{
			name:           "user scope records scope and install dir",
			scope:          ScopeUser,
			wantScope:      ScopeUser,
			wantInstallDir: "/home/alice/.local/share/testapp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := saveCommandVars()
			defer restoreCommandVars2(saved)

			installDir := "/opt/testapp"
			if tt.scope == ScopeUser {
				installDir = "/home/alice/.local/share/testapp"
			}

			var gotScope daemon.Scope
			detectManager = func() (daemon.ServiceManager, error) { return &mockServiceManager{}, nil }
			initManager = func(_ daemon.ServiceManager, opts daemon.ManagerOptions) error {
				gotScope = opts.Scope
				return nil
			}
			getExecutable = func() (string, error) { return "/usr/bin/testapp", nil }
			mkdirAll = func(string, os.FileMode) error { return nil }
			copyFileFn = func(string, string) error { return nil }

			var written StateData
			saveStateFn = func(_ string, s *StateData) error {
				written = *s
				return nil
			}

			cfg := &Config{
				Name:       "testapp",
				InstallDir: installDir,
				Scope:      tt.scope,
				Logger:     NewDefaultLogger(LogLevelInfo),
			}

			if err := handleInstall(cfg); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if written.Scope != tt.wantScope {
				t.Errorf("state scope = %q, want %q", written.Scope, tt.wantScope)
			}
			if written.InstallDir != tt.wantInstallDir {
				t.Errorf("state install dir = %q, want %q", written.InstallDir, tt.wantInstallDir)
			}
			wantMgrScope := daemon.ScopeSystem
			if tt.scope == ScopeUser {
				wantMgrScope = daemon.ScopeUser
			}
			if gotScope != wantMgrScope {
				t.Errorf("manager scope = %v, want %v", gotScope, wantMgrScope)
			}
		})
	}
}

// TestLifecycleUsesStateInitSystem covers the failure this replaces: a session
// that detects a different init system than the install used must still act on
// the init system recorded in the state file.
func TestLifecycleUsesStateInitSystem(t *testing.T) {
	handlers := map[string]func(*Config) error{
		"status":    handleStatus,
		"start":     handleStart,
		"stop":      handleStop,
		"restart":   handleRestart,
		"enable":    handleEnable,
		"disable":   handleDisable,
		"uninstall": handleUninstall,
	}

	for name, handler := range handlers {
		t.Run(name, func(t *testing.T) {
			saved := saveCommandVars()
			defer restoreCommandVars2(saved)
			origWriter := saveStdoutWriter()
			defer restoreStdoutWriter(origWriter)
			stdoutWriter = &bytes.Buffer{}

			installed := &mockServiceManager{nameVal: "systemd", statusResult: daemon.ServiceStatusRunning}
			detected := &mockServiceManager{nameVal: "crontab"}

			detectManager = func() (daemon.ServiceManager, error) { return detected, nil }
			var byNameArg string
			newManagerByName = func(n string) (daemon.ServiceManager, error) {
				byNameArg = n
				return installed, nil
			}
			initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
			loadStateFn = func(string, string) (*StateData, error) {
				return &StateData{ServiceName: "testapp", InitSystem: "systemd"}, nil
			}
			deleteStateFn = func(string, string) error { return nil }

			cfg := &Config{
				Name:       "testapp",
				InstallDir: "/opt/testapp",
				Logger:     NewDefaultLogger(LogLevelInfo),
			}

			if err := handler(cfg); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if byNameArg != "systemd" {
				t.Errorf("constructed manager for %q, want %q", byNameArg, "systemd")
			}
			if detected.statusCalled || detected.startCalled || detected.stopCalled ||
				detected.enableCalled || detected.disableCalled || detected.uninstallCalled {
				t.Error("expected the detected manager to be left untouched")
			}
		})
	}
}

func TestLifecycleRefusesScopeMismatch(t *testing.T) {
	handlers := map[string]func(*Config) error{
		"status":        handleStatus,
		"start":         handleStart,
		"stop":          handleStop,
		"restart":       handleRestart,
		"enable":        handleEnable,
		"disable":       handleDisable,
		"uninstall":     handleUninstall,
		"validate":      handleValidate,
		"update-config": handleUpdateConfig,
	}

	for name, handler := range handlers {
		t.Run(name, func(t *testing.T) {
			saved := saveCommandVars()
			defer restoreCommandVars2(saved)

			mock := &mockServiceManager{}
			detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
			initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
			loadStateFn = func(string, string) (*StateData, error) {
				return &StateData{ServiceName: "testapp", InitSystem: "systemd", Scope: ScopeUser}, nil
			}

			cfg := &Config{
				Name:       "testapp",
				InstallDir: "/opt/testapp",
				Logger:     NewDefaultLogger(LogLevelInfo),
			}

			err := handler(cfg)
			if err == nil {
				t.Fatal("expected an error, got success")
			}
			if !strings.Contains(err.Error(), "installed in user scope") {
				t.Errorf("expected a scope mismatch error, got: %v", err)
			}
		})
	}
}

// TestLifecycleFallsBackToDetectionWithoutState keeps commands working for a
// service that has no state file, e.g. one registered outside daemonpilot.
func TestLifecycleFallsBackToDetectionWithoutState(t *testing.T) {
	saved := saveCommandVars()
	defer restoreCommandVars2(saved)

	mock := &mockServiceManager{}
	detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
	newManagerByName = func(string) (daemon.ServiceManager, error) {
		t.Fatal("expected detection, not construction by name")
		return nil, nil
	}
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
	loadStateFn = func(string, string) (*StateData, error) { return nil, nil }

	cfg := &Config{
		Name:       "testapp",
		InstallDir: "/opt/testapp",
		Logger:     NewDefaultLogger(LogLevelInfo),
	}

	if err := handleStart(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mock.startCalled {
		t.Error("expected the detected manager to be started")
	}
}

// TestMutatingLifecycleRefusesUnreadableState pins the narrow degraded path: a
// state file that exists but cannot be read or decoded must not be treated as
// absent by a command that mutates the service, because detection can select a
// different init system than the one the install registered.
func TestMutatingLifecycleRefusesUnreadableState(t *testing.T) {
	handlers := map[string]func(*Config) error{
		"start":   handleStart,
		"stop":    handleStop,
		"restart": handleRestart,
		"enable":  handleEnable,
		"disable": handleDisable,
	}

	for name, handler := range handlers {
		t.Run(name, func(t *testing.T) {
			saved := saveCommandVars()
			defer restoreCommandVars2(saved)

			mock := &mockServiceManager{}
			detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
			initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
			loadStateFn = func(string, string) (*StateData, error) {
				return nil, errors.New("decoding state file: unexpected end of JSON input")
			}

			cfg := &Config{
				Name:       "testapp",
				InstallDir: "/opt/testapp",
				Logger:     NewDefaultLogger(LogLevelInfo),
			}

			err := handler(cfg)
			if err == nil {
				t.Fatal("expected an error, got success")
			}
			if !strings.Contains(err.Error(), "decoding state file") {
				t.Errorf("expected the state-read error to surface, got: %v", err)
			}
			if mock.startCalled || mock.stopCalled || mock.enableCalled || mock.disableCalled {
				t.Error("expected the detected manager to be left untouched")
			}
		})
	}
}

// TestLifecycleReadsLegacyStateAsSystemScope covers a state file written before
// the scope field existed.
func TestLifecycleReadsLegacyStateAsSystemScope(t *testing.T) {
	saved := saveCommandVars()
	defer restoreCommandVars2(saved)

	mock := &mockServiceManager{}
	detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
	loadStateFn = func(string, string) (*StateData, error) {
		return &StateData{ServiceName: "testapp", InitSystem: "mock"}, nil
	}

	cfg := &Config{
		Name:       "testapp",
		InstallDir: "/opt/testapp",
		Scope:      ScopeSystem,
		Logger:     NewDefaultLogger(LogLevelInfo),
	}

	if err := handleStart(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mock.startCalled {
		t.Error("expected Start to be called")
	}
}

// TestHandleInstall_RollsBackCopiedBinary covers the window between the binary
// copy and the state file write: a failure there leaves a binary that nothing
// records, so uninstall would report the service as not installed and refuse to
// remove it.
func TestHandleInstall_RollsBackCopiedBinary(t *testing.T) {
	destPath := filepath.Join("/opt/testapp", InstalledBinaryName("testapp"))

	tests := []struct {
		name        string
		preExisting bool
		execErr     error
		installErr  error
		enableErr   error
		removeErr   error
		wantErr     string
		wantRemoved bool
	}{
		{
			name:        "manifest write failure removes a binary this install created",
			installErr:  errors.New("install failed"),
			wantErr:     "testapp: install failed",
			wantRemoved: true,
		},
		{
			name:        "manifest write failure leaves a pre-existing binary alone",
			preExisting: true,
			installErr:  errors.New("install failed"),
			wantErr:     "testapp: install failed",
		},
		{
			name:        "exec probe failure removes the copied binary",
			execErr:     errors.New("noexec"),
			wantErr:     "noexec",
			wantRemoved: true,
		},
		{
			name:        "enable failure removes the copied binary",
			enableErr:   errors.New("enable failed"),
			wantErr:     "testapp: enable failed",
			wantRemoved: true,
		},
		{
			name:        "a failed removal is reported alongside the original error",
			installErr:  errors.New("install failed"),
			removeErr:   errors.New("read-only file system"),
			wantErr:     "could not be removed: read-only file system",
			wantRemoved: true,
		},
		{
			name: "a successful install removes nothing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := saveCommandVars()
			defer restoreCommandVars2(saved)
			savedRemove := removeFileFn
			defer func() { removeFileFn = savedRemove }()

			mock := &mockServiceManager{installErr: tt.installErr, enableErr: tt.enableErr}
			detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
			getExecutable = func() (string, error) { return "/usr/bin/myapp", nil }
			mkdirAll = func(string, os.FileMode) error { return nil }
			copyFileFn = func(string, string) error { return nil }
			saveStateFn = func(string, *StateData) error { return nil }
			statFileFn = func(string) (os.FileInfo, error) {
				if tt.preExisting {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}
			preflightExec = func(*Config, string) error { return tt.execErr }

			var removed []string
			removeFileFn = func(path string) error {
				removed = append(removed, path)
				return tt.removeErr
			}

			cfg := &Config{
				Name:       "testapp",
				InstallDir: "/opt/testapp",
				Logger:     NewDefaultLogger(LogLevelInfo),
			}

			err := handleInstall(cfg)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatal("expected error")
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}

			// The cleanup outcome is reported, never substituted for the cause.
			if tt.installErr != nil && !errors.Is(err, tt.installErr) {
				t.Errorf("expected the original error to stay unwrappable, got %v", err)
			}

			if got := slices.Contains(removed, destPath); got != tt.wantRemoved {
				t.Errorf("removed %v, want the binary removed = %v", removed, tt.wantRemoved)
			}
		})
	}
}

// A state file written by RecordRollbackDigest for a service that was never
// installed through daemonpilot carries no init system, manifest path or binary
// path. Reading it as "installed" would make update-config write a backup to
// the relative path ".backup" in the CLI's working directory and make uninstall
// skip the manifest fallback that decides whether anything is there at all.
func TestStateWithoutAnInstallRecordIsReadAsAbsent(t *testing.T) {
	digestOnly := &StateData{ServiceName: "testapp", RollbackDigest: strings.Repeat("a", 64)}

	tests := []struct {
		name  string
		state *StateData
		want  bool
	}{
		{name: "digest-only state is not an install", state: digestOnly, want: false},
		{name: "install-written state is an install", state: &StateData{ServiceName: "testapp", InitSystem: "mock"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := saveCommandVars()
			defer restoreCommandVars2(saved)
			loadStateFn = func(string, string) (*StateData, error) { return tt.state, nil }

			cfg := &Config{Name: "testapp", InstallDir: t.TempDir(), Logger: NewDefaultLogger(LogLevelInfo)}

			got, err := loadInstalledState(cfg)
			if err != nil {
				t.Fatalf("loadInstalledState: %v", err)
			}
			if (got != nil) != tt.want {
				t.Errorf("loadInstalledState() != nil = %v, want %v", got != nil, tt.want)
			}
			if (readableState(cfg) != nil) != tt.want {
				t.Errorf("readableState() != nil = %v, want %v", readableState(cfg) != nil, tt.want)
			}
		})
	}
}

// run refuses to start without the single-instance lock, so an unprivileged
// ServiceUser that cannot create it in the root-owned install directory would
// crash-loop under Restart=always. Install creates the file and hands it over
// while it still has the privileges to do so.
func TestEnsureServiceUserLockFile(t *testing.T) {
	tests := []struct {
		name        string
		serviceUser string
		preExisting bool
		lookupErr   error
		chownErr    error
		wantCreated bool
		wantChowned bool
		wantErr     string
	}{
		{name: "no ServiceUser leaves the lock file to run", serviceUser: ""},
		{name: "creates and chowns the lock file", serviceUser: "svc", wantCreated: true, wantChowned: true},
		{
			name:        "an existing lock file is re-chowned, never replaced",
			serviceUser: "svc", preExisting: true, wantCreated: false, wantChowned: true,
		},
		{
			// A CGO_ENABLED=0 build cannot resolve an NSS-provided account, and
			// packaging commonly creates the account after the install. Both used
			// to install fine, so this warns and leaves the file to root rather
			// than failing.
			name:        "an unknown ServiceUser warns instead of failing the install",
			serviceUser: "nope", lookupErr: errors.New("unknown user"),
			wantCreated: false, wantChowned: false,
		},
		{
			name:        "a failed chown fails the install",
			serviceUser: "svc", chownErr: errors.New("operation not permitted"),
			wantCreated: true, wantChowned: true,
			wantErr: "giving the single-instance lock file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			savedChown, savedLookup := chownFn, lookupUserFn
			defer func() { chownFn, lookupUserFn = savedChown, savedLookup }()

			lookupUserFn = func(_ string) (*user.User, error) {
				if tt.lookupErr != nil {
					return nil, tt.lookupErr
				}
				return &user.User{Uid: "4242", Gid: "4343"}, nil
			}
			var chowned bool
			var gotUID, gotGID int
			chownFn = func(_ string, uid, gid int) error {
				chowned, gotUID, gotGID = true, uid, gid
				return tt.chownErr
			}

			cfg := &Config{
				Name:        "testapp",
				InstallDir:  t.TempDir(),
				ServiceUser: tt.serviceUser,
				Logger:      NewDefaultLogger(LogLevelInfo),
			}
			lockPath := instanceLockPath(cfg)
			if tt.preExisting {
				if err := os.WriteFile(lockPath, []byte("held"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			created, err := ensureServiceUserLockFile(cfg)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatal("expected an error")
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
			if created != tt.wantCreated {
				t.Errorf("created = %v, want %v", created, tt.wantCreated)
			}
			if chowned != tt.wantChowned {
				t.Errorf("chowned = %v, want %v", chowned, tt.wantChowned)
			}
			if tt.wantChowned && (gotUID != 4242 || gotGID != -1) {
				t.Errorf("chown(uid=%d, gid=%d), want uid=4242 gid=-1 for an unset ServiceGroup", gotUID, gotGID)
			}
			if tt.preExisting {
				// Unlinking would detach the inode a live instance holds.
				// #nosec G304 -- Reads a fixture from this test's temporary directory.
				if b, _ := os.ReadFile(lockPath); string(b) != "held" {
					t.Errorf("lock file = %q, want the existing file kept in place", b)
				}
			}
		})
	}
}

// A staged update records the rollback digest in the state file before it
// displaces the binary, and aborts when it cannot. Root writes that file 0600 in
// a root-owned install directory, so without a chown the unprivileged worker's
// self-update fails on every poll and its rollback copy reads back unverifiable.
func TestChownStateFileToServiceUser(t *testing.T) {
	tests := []struct {
		name        string
		serviceUser string
		lookupErr   error
		chownErr    error
		wantChowned bool
		wantErr     string
	}{
		{name: "no ServiceUser leaves the state file alone", serviceUser: ""},
		{name: "hands the state file to the service account", serviceUser: "svc", wantChowned: true},
		{
			// Same reasoning as the lock file: an account this build cannot
			// resolve must not fail an install that used to succeed.
			name:        "an unknown ServiceUser warns instead of failing the install",
			serviceUser: "nope", lookupErr: errors.New("unknown user"),
			wantChowned: false,
		},
		{
			// Reported as a completed install with a degraded capability: the
			// service is registered and running by the time this runs.
			name:        "a failed chown reports the installed-but-degraded outcome",
			serviceUser: "svc", chownErr: errors.New("operation not permitted"),
			wantChowned: true,
			wantErr:     "was installed, but its state file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			savedChown, savedLookup := chownFn, lookupUserFn
			defer func() { chownFn, lookupUserFn = savedChown, savedLookup }()

			lookupUserFn = func(string) (*user.User, error) {
				if tt.lookupErr != nil {
					return nil, tt.lookupErr
				}
				return &user.User{Uid: "4242", Gid: "4343"}, nil
			}
			var chowned bool
			var gotPath string
			var gotUID, gotGID int
			chownFn = func(path string, uid, gid int) error {
				chowned, gotPath, gotUID, gotGID = true, path, uid, gid
				return tt.chownErr
			}

			cfg := &Config{
				Name:        "testapp",
				InstallDir:  t.TempDir(),
				ServiceUser: tt.serviceUser,
				Logger:      NewDefaultLogger(LogLevelInfo),
			}

			err := chownStateFileToServiceUser(cfg)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatal("expected an error")
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
			if chowned != tt.wantChowned {
				t.Errorf("chowned = %v, want %v", chowned, tt.wantChowned)
			}
			if tt.wantChowned {
				if want := StateFilePath(cfg.InstallDir, cfg.Name); gotPath != want {
					t.Errorf("chowned %q, want %q", gotPath, want)
				}
				if gotUID != 4242 || gotGID != -1 {
					t.Errorf("chown(uid=%d, gid=%d), want uid=4242 gid=-1 for an unset ServiceGroup", gotUID, gotGID)
				}
			}
		})
	}
}

// serviceUserUID answers "who else may own the lock file". "No service account"
// and "a service account this build cannot resolve" are reported apart: the
// second one must stop root from replacing a lock file that may be the service
// account's own.
func TestServiceUserUID(t *testing.T) {
	tests := []struct {
		name        string
		serviceUser string
		user        *user.User
		lookupErr   error
		want        lockOwner
	}{
		{name: "no ServiceUser", serviceUser: "", want: lockOwner{uid: -1}},
		{
			name: "resolves the uid", serviceUser: "svc", user: &user.User{Uid: "4242"},
			want: lockOwner{uid: 4242, name: "svc"},
		},
		{
			name: "unknown user", serviceUser: "nope", lookupErr: errors.New("unknown user"),
			want: lockOwner{uid: -1, unresolved: true, name: "nope"},
		},
		{
			name: "unparsable uid", serviceUser: "svc", user: &user.User{Uid: "S-1-5-21"},
			want: lockOwner{uid: -1, unresolved: true, name: "svc"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			savedLookup := lookupUserFn
			defer func() { lookupUserFn = savedLookup }()
			lookupUserFn = func(string) (*user.User, error) { return tt.user, tt.lookupErr }

			cfg := &Config{Name: "testapp", ServiceUser: tt.serviceUser, Logger: NewDefaultLogger(LogLevelInfo)}
			if got := serviceUserUID(cfg); got != tt.want {
				t.Errorf("serviceUserUID() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// Removing the binary and leaving the registration is the worse half-state:
// launchd auto-loads every plist under ~/Library/LaunchAgents at the next login,
// so the failed install comes back on its own and spawns a path that is gone.
func TestUnwindPartialInstallRemovesTheRegistration(t *testing.T) {
	tests := []struct {
		name          string
		registered    bool
		createdBinary bool
		uninstallErr  error
		wantUninstall bool
	}{
		{
			name:       "a registered service is unregistered",
			registered: true, createdBinary: true, wantUninstall: true,
		},
		{
			name:       "an unregistered install leaves the manager alone",
			registered: false, createdBinary: true,
		},
		{
			name:       "a failed unregistration warns rather than replacing the cause",
			registered: true, createdBinary: true, uninstallErr: errors.New("bootout failed"), wantUninstall: true,
		},
		{
			// A reinstall keeps the binary it found, so the manifest still names
			// a working executable. Unregistering would delete a live service
			// over a transient enable failure, while the untouched state file
			// goes on pointing at it.
			name:       "a reinstall that kept the binary keeps the registration",
			registered: true, createdBinary: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := saveCommandVars()
			defer restoreCommandVars2(saved)

			installDir := t.TempDir()
			destPath := filepath.Join(installDir, "testapp")
			// #nosec G306 -- Executable fixture owned by this test.
			if err := os.WriteFile(destPath, []byte("binary"), 0o755); err != nil {
				t.Fatal(err)
			}

			mock := &mockServiceManager{manifestPath: filepath.Join(installDir, "testapp.service"), uninstallErr: tt.uninstallErr}
			cfg := &Config{Name: "testapp", InstallDir: installDir, Logger: NewDefaultLogger(LogLevelInfo)}

			cause := errors.New("enable failed")
			err := unwindPartialInstall(cfg, mock, destPath, tt.registered, tt.createdBinary, false, cause)
			if !errors.Is(err, cause) {
				t.Errorf("unwind error = %v, want it to wrap the original cause", err)
			}
			if mock.uninstallCalled != tt.wantUninstall {
				t.Errorf("Uninstall called = %v, want %v", mock.uninstallCalled, tt.wantUninstall)
			}
			_, statErr := os.Stat(destPath)
			if tt.createdBinary && !os.IsNotExist(statErr) {
				t.Errorf("expected the copied binary to be removed, stat error = %v", statErr)
			}
			if !tt.createdBinary && statErr != nil {
				t.Errorf("expected a pre-existing binary to be kept, stat error = %v", statErr)
			}
		})
	}
}

// The state file is chowned to Config.ServiceUser so the staged updater can
// record a rollback digest, which puts every recorded value inside an
// unprivileged account's control. update-config turns the manifest path into a
// root-owned 0644 ".backup" write, so it must use the manager's own path.
func TestManifestPathForStatePrefersTheManagerPath(t *testing.T) {
	tests := []struct {
		name         string
		managerPath  string
		recordedPath string
		want         string
	}{
		{
			name:         "agreeing paths",
			managerPath:  "/etc/systemd/system/myapp.service",
			recordedPath: "/etc/systemd/system/myapp.service",
			want:         "/etc/systemd/system/myapp.service",
		},
		{
			name:         "a rewritten state file does not redirect the path",
			managerPath:  "/etc/systemd/system/myapp.service",
			recordedPath: "/etc/sudoers.d/evil",
			want:         "/etc/systemd/system/myapp.service",
		},
		{
			name:         "a manager with no manifest falls back to the recorded path",
			managerPath:  "",
			recordedPath: "/etc/systemd/system/myapp.service",
			want:         "/etc/systemd/system/myapp.service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Name: "myapp", Logger: NewDefaultLogger(LogLevelError)}
			mgr := &mockServiceManager{manifestPath: tt.managerPath}
			state := &StateData{ServiceName: "myapp", ManifestPath: tt.recordedPath}

			if got := manifestPathForState(cfg, mgr, state); got != tt.want {
				t.Errorf("manifestPathForState() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A reinstall over a staged update on probation must carry the recorded digest
// forward: dropping it makes the next start read a valid .rollback copy as
// unverifiable, quarantine it and fire OnCriticalFailure.
func TestHandleInstallPreservesRollbackDigest(t *testing.T) {
	const digest = "b0b1c2d3e4f5"

	tests := []struct {
		name  string
		state *StateData
		want  string
	}{
		{
			name: "a previous install carries its digest forward",
			state: &StateData{
				ServiceName:    "testapp",
				InitSystem:     "systemd",
				ManifestPath:   "/etc/systemd/system/testapp.service",
				RollbackDigest: digest,
			},
			want: digest,
		},
		{
			// RecordRollbackDigest writes a state file with no install record for
			// a service that was never installed through daemonpilot. readableState
			// reports it as absent, but its .rollback copy is live on disk, so
			// the digest still has to survive the install that adopts it.
			name:  "a digest-only state file still carries its digest forward",
			state: &StateData{ServiceName: "testapp", RollbackDigest: digest},
			want:  digest,
		},
		{
			name:  "a first install records no digest",
			state: nil,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := saveCommandVars()
			defer restoreCommandVars2(vars)
			saveIST := initSystemTypeFn
			defer func() { initSystemTypeFn = saveIST }()
			initSystemTypeFn = func(_ daemon.ServiceManager) string { return "systemd" }

			mock := &mockServiceManager{manifestPath: "/etc/systemd/system/testapp.service"}
			detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
			initManager = daemon.InitServiceManagerWithOptions
			getExecutable = func() (string, error) { return filepath.Join("/opt/testapp", InstalledBinaryName("testapp")), nil }
			loadStateFn = func(_, _ string) (*StateData, error) {
				return tt.state, nil
			}

			var savedState *StateData
			saveStateFn = func(_ string, state *StateData) error {
				savedState = state
				return nil
			}

			cfg := &Config{
				Name:       "testapp",
				InstallDir: "/opt/testapp",
				Logger:     NewDefaultLogger(LogLevelInfo),
			}

			if err := handleInstall(cfg); err != nil {
				t.Fatalf("handleInstall() error: %v", err)
			}
			if savedState == nil {
				t.Fatal("expected SaveState to be called")
			}
			if savedState.RollbackDigest != tt.want {
				t.Errorf("RollbackDigest = %q, want %q", savedState.RollbackDigest, tt.want)
			}
		})
	}
}

// uninstall stops the service, but a failed Stop is only a warning, so the
// watchdog may still be running. Unlinking the lock file it is flocking detaches
// the inode, and a later install plus run would create a fresh file that locks
// cleanly — two supervisors over one install directory.
func TestRemoveInstanceLockArtifactRespectsAHeldLock(t *testing.T) {
	tests := []struct {
		name       string
		createLock bool
		lockErr    error
		wantGone   bool
	}{
		{name: "an unheld lock file is removed", createLock: true, wantGone: true},
		{
			name:       "a held lock file is left in place",
			createLock: true, lockErr: ErrAlreadyRunning,
		},
		{
			name:       "a lock that cannot be taken at all is left in place",
			createLock: true, lockErr: errors.New("permission denied"),
		},
		{name: "no lock file is a no-op", createLock: false, wantGone: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := saveCommandVars()
			defer restoreCommandVars2(vars)
			savedAcquire := acquireLockFn
			defer func() { acquireLockFn = savedAcquire }()

			installDir := t.TempDir()
			lockPath := filepath.Join(installDir, "testapp.lock")
			if tt.createLock {
				if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			released := false
			acquireLockFn = func(string, lockOwner) (func(), error) {
				if tt.lockErr != nil {
					return func() {}, tt.lockErr
				}
				return func() { released = true }, nil
			}

			cfg := &Config{Name: "testapp", InstallDir: installDir, Logger: NewDefaultLogger(LogLevelInfo)}
			removeInstanceLockArtifact(cfg, cfg.Logger)

			_, statErr := os.Stat(lockPath)
			if gone := os.IsNotExist(statErr); gone != tt.wantGone {
				t.Errorf("lock file removed = %v, want %v", gone, tt.wantGone)
			}
			if tt.createLock && tt.lockErr == nil && !released {
				t.Error("expected the lock to be released after the removal")
			}
		})
	}
}

// A dangling symlink at the lock path stats as absent, so a Stat-gated sweep
// would leave it — and ensureServiceUserLockFile refuses a symlinked lock path
// outright, so the next install would fail with nothing able to clear the file.
func TestRemoveInstanceLockArtifactClearsASymlinkedLockPath(t *testing.T) {
	vars := saveCommandVars()
	defer restoreCommandVars2(vars)
	savedAcquire := acquireLockFn
	defer func() { acquireLockFn = savedAcquire }()

	installDir := t.TempDir()
	lockPath := filepath.Join(installDir, "testapp.lock")
	if err := os.Symlink(filepath.Join(installDir, "no-such-target"), lockPath); err != nil {
		t.Fatalf("planting a symlink at the lock path: %v", err)
	}

	locked := false
	acquireLockFn = func(string, lockOwner) (func(), error) {
		locked = true
		return func() {}, nil
	}

	cfg := &Config{Name: "testapp", InstallDir: installDir, Logger: NewDefaultLogger(LogLevelInfo)}
	removeInstanceLockArtifact(cfg, cfg.Logger)

	if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
		t.Errorf("the symlinked lock path survived the sweep: %v", err)
	}
	if locked {
		t.Error("a symlink must not be locked: the lock would be on whatever it points at")
	}
}
