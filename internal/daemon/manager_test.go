package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"strings"
	"testing"
)

func withDetectors(entries []detectorEntry, fn func()) {
	old := detectors
	detectors = entries
	defer func() { detectors = old }()
	fn()
}

func TestServiceStatusString(t *testing.T) {
	tests := []struct {
		status ServiceStatus
		want   string
	}{
		{ServiceStatusUnknown, "unknown"},
		{ServiceStatusRunning, "running"},
		{ServiceStatusStopped, "stopped"},
		{ServiceStatusFailed, "failed"},
		{ServiceStatus(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.status.String(); got != tt.want {
			t.Errorf("ServiceStatus(%d).String() = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestDetectServiceManager_SystemdFirst(t *testing.T) {
	withDetectors([]detectorEntry{
		{name: "systemd", detect: func() bool { return true }, create: func() ServiceManager { return &systemdManager{} }},
		{name: "launchd", detect: func() bool { return true }, create: func() ServiceManager { return &launchdManager{} }},
	}, func() {
		mgr, err := DetectServiceManager()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := mgr.(*systemdManager); !ok {
			t.Errorf("expected systemdManager, got %T", mgr)
		}
	})
}

func TestDetectServiceManager_FallsThrough(t *testing.T) {
	withDetectors([]detectorEntry{
		{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
		{name: "launchd", detect: func() bool { return false }, create: func() ServiceManager { return &launchdManager{} }},
		{name: "openrc", detect: func() bool { return true }, create: func() ServiceManager { return &openrcManager{} }},
		{name: "crontab", detect: func() bool { return true }, create: func() ServiceManager { return &crontabManager{} }},
	}, func() {
		mgr, err := DetectServiceManager()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := mgr.(*openrcManager); !ok {
			t.Errorf("expected openrcManager, got %T", mgr)
		}
	})
}

func TestDetectServiceManager_CrontabFallback(t *testing.T) {
	withDetectors([]detectorEntry{
		{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
		{name: "launchd", detect: func() bool { return false }, create: func() ServiceManager { return &launchdManager{} }},
		{name: "openrc", detect: func() bool { return false }, create: func() ServiceManager { return &openrcManager{} }},
		{name: "dinit", detect: func() bool { return false }, create: func() ServiceManager { return newDinitManager() }},
		{name: "finit", detect: func() bool { return false }, create: func() ServiceManager { return newFinitManager() }},
		{name: "sysvinit", detect: func() bool { return false }, create: func() ServiceManager { return &sysvinitManager{} }},
		{name: "runit", detect: func() bool { return false }, create: func() ServiceManager { return &runitManager{} }},
		{name: "s6", detect: func() bool { return false }, create: func() ServiceManager { return &s6Manager{} }},
		{name: "procd", detect: func() bool { return false }, create: func() ServiceManager { return &procdManager{} }},
		{name: "busybox", detect: func() bool { return false }, create: func() ServiceManager { return &busyboxManager{} }},
		{name: "crontab", detect: func() bool { return true }, create: func() ServiceManager { return &crontabManager{} }},
	}, func() {
		mgr, err := DetectServiceManager()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := mgr.(*crontabManager); !ok {
			t.Errorf("expected crontabManager, got %T", mgr)
		}
	})
}

func TestDetectServiceManager_NoneDetected(t *testing.T) {
	withDetectors([]detectorEntry{
		{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
		{name: "launchd", detect: func() bool { return false }, create: func() ServiceManager { return &launchdManager{} }},
		{name: "openrc", detect: func() bool { return false }, create: func() ServiceManager { return &openrcManager{} }},
		{name: "dinit", detect: func() bool { return false }, create: func() ServiceManager { return newDinitManager() }},
		{name: "finit", detect: func() bool { return false }, create: func() ServiceManager { return newFinitManager() }},
		{name: "sysvinit", detect: func() bool { return false }, create: func() ServiceManager { return &sysvinitManager{} }},
		{name: "runit", detect: func() bool { return false }, create: func() ServiceManager { return &runitManager{} }},
		{name: "s6", detect: func() bool { return false }, create: func() ServiceManager { return &s6Manager{} }},
		{name: "procd", detect: func() bool { return false }, create: func() ServiceManager { return &procdManager{} }},
		{name: "busybox", detect: func() bool { return false }, create: func() ServiceManager { return &busyboxManager{} }},
		{name: "crontab", detect: func() bool { return false }, create: func() ServiceManager { return &crontabManager{} }},
	}, func() {
		mgr, err := DetectServiceManager()
		if err == nil {
			t.Fatal("expected error when no service manager detected")
		}
		if mgr != nil {
			t.Errorf("expected nil manager, got %T", mgr)
		}
	})
}

func TestDetectServiceManager_LaunchdSecond(t *testing.T) {
	withDetectors([]detectorEntry{
		{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
		{name: "launchd", detect: func() bool { return true }, create: func() ServiceManager { return &launchdManager{} }},
		{name: "openrc", detect: func() bool { return true }, create: func() ServiceManager { return &openrcManager{} }},
	}, func() {
		mgr, err := DetectServiceManager()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := mgr.(*launchdManager); !ok {
			t.Errorf("expected launchdManager, got %T", mgr)
		}
	})
}

func TestDetectServiceManager_PriorityOrder(t *testing.T) {
	tests := []struct {
		name     string
		systemd  bool
		launchd  bool
		openrc   bool
		dinit    bool
		finit    bool
		sysvinit bool
		runit    bool
		s6       bool
		procd    bool
		busybox  bool
		crontab  bool
		wantType string
	}{
		{"all available picks systemd", true, true, true, true, true, true, true, true, true, true, true, "*daemon.systemdManager"},
		{"no systemd picks launchd", false, true, true, true, true, true, true, true, true, true, true, "*daemon.launchdManager"},
		{"only openrc and below picks openrc", false, false, true, true, true, true, true, true, true, true, true, "*daemon.openrcManager"},
		{"only dinit and below picks dinit", false, false, false, true, true, true, true, true, true, true, true, "*daemon.dinitManager"},
		{"only finit and below picks finit", false, false, false, false, true, false, true, true, true, true, true, "*daemon.finitManager"},
		{"only runit and below picks runit", false, false, false, false, false, false, true, true, true, true, true, "*daemon.runitManager"},
		{"only s6 and below picks s6", false, false, false, false, false, false, false, true, true, true, true, "*daemon.s6Manager"},
		{"only procd and below picks procd", false, false, false, false, false, false, false, false, true, true, true, "*daemon.procdManager"},
		{"only busybox and below picks busybox", false, false, false, false, false, false, false, false, false, true, true, "*daemon.busyboxManager"},
		{"only sysvinit and crontab picks sysvinit", false, false, false, false, false, true, false, false, false, false, true, "*daemon.sysvinitManager"},
		{"only crontab", false, false, false, false, false, false, false, false, false, false, true, "*daemon.crontabManager"},
		{"none available", false, false, false, false, false, false, false, false, false, false, false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withDetectors([]detectorEntry{
				{name: "systemd", detect: func() bool { return tt.systemd }, create: func() ServiceManager { return &systemdManager{} }},
				{name: "launchd", detect: func() bool { return tt.launchd }, create: func() ServiceManager { return &launchdManager{} }},
				{name: "openrc", detect: func() bool { return tt.openrc }, create: func() ServiceManager { return &openrcManager{} }},
				{name: "dinit", detect: func() bool { return tt.dinit }, create: func() ServiceManager { return newDinitManager() }},
				{name: "finit", detect: func() bool { return tt.finit }, create: func() ServiceManager { return newFinitManager() }},
				{name: "runit", detect: func() bool { return tt.runit }, create: func() ServiceManager { return &runitManager{} }},
				{name: "s6", detect: func() bool { return tt.s6 }, create: func() ServiceManager { return &s6Manager{} }},
				{name: "procd", detect: func() bool { return tt.procd }, create: func() ServiceManager { return &procdManager{} }},
				{name: "busybox", detect: func() bool { return tt.busybox }, create: func() ServiceManager { return &busyboxManager{} }},
				{name: "sysvinit", detect: func() bool { return tt.sysvinit }, create: func() ServiceManager { return &sysvinitManager{} }},
				{name: "crontab", detect: func() bool { return tt.crontab }, create: func() ServiceManager { return &crontabManager{} }},
			}, func() {
				mgr, err := DetectServiceManager()
				if tt.wantType == "" {
					if err == nil {
						t.Fatal("expected error")
					}
					return
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				gotType := fmt.Sprintf("%T", mgr)
				if gotType != tt.wantType {
					t.Errorf("got %s, want %s", gotType, tt.wantType)
				}
			})
		})
	}
}

func TestServiceConfigFields(t *testing.T) {
	cfg := ServiceConfig{
		Name:        "test-service",
		Description: "A test service",
		ExecPath:    "/usr/local/bin/test",
		InstallDir:  "/opt/test",
		WorkingDir:  "/var/lib/test",
	}
	if cfg.Name != "test-service" {
		t.Errorf("Name = %q, want %q", cfg.Name, "test-service")
	}
	if cfg.Description != "A test service" {
		t.Errorf("Description = %q, want %q", cfg.Description, "A test service")
	}
	if cfg.ExecPath != "/usr/local/bin/test" {
		t.Errorf("ExecPath = %q, want %q", cfg.ExecPath, "/usr/local/bin/test")
	}
	if cfg.InstallDir != "/opt/test" {
		t.Errorf("InstallDir = %q, want %q", cfg.InstallDir, "/opt/test")
	}
	if cfg.WorkingDir != "/var/lib/test" {
		t.Errorf("WorkingDir = %q, want %q", cfg.WorkingDir, "/var/lib/test")
	}
}

func TestServiceConfigOptionalFields(t *testing.T) {
	t.Run("zero value has nil optional fields", func(t *testing.T) {
		cfg := ServiceConfig{
			Name:     "test-service",
			ExecPath: "/usr/local/bin/test",
		}
		if cfg.Environment != nil {
			t.Error("expected nil Environment")
		}
		if cfg.Arguments != nil {
			t.Error("expected nil Arguments")
		}
		if cfg.User != "" {
			t.Error("expected empty User")
		}
		if cfg.Group != "" {
			t.Error("expected empty Group")
		}
		if cfg.SystemdDirectives != nil {
			t.Error("expected nil SystemdDirectives")
		}
		if cfg.LaunchdKeys != nil {
			t.Error("expected nil LaunchdKeys")
		}
		if cfg.OpenRCDirectives != nil {
			t.Error("expected nil OpenRCDirectives")
		}
		if cfg.CrontabSchedule != "" {
			t.Error("expected empty CrontabSchedule")
		}
	})

	t.Run("populated optional fields", func(t *testing.T) {
		cfg := ServiceConfig{
			Name:     "test-service",
			ExecPath: "/usr/local/bin/test",
			Environment: map[string]string{
				"APP_ENV":   "production",
				"LOG_LEVEL": "info",
			},
			Arguments: []string{"--port", "8080"},
			User:      "appuser",
			Group:     "appgroup",
			SystemdDirectives: map[string]string{
				"LimitNOFILE": "65536",
			},
			LaunchdKeys: map[string]any{
				"ThrottleInterval": 10,
			},
			OpenRCDirectives: map[string]string{
				"rc_ulimit": "-n 65536",
			},
			CrontabSchedule: "*/5 * * * *",
		}
		if len(cfg.Environment) != 2 {
			t.Errorf("expected 2 environment entries, got %d", len(cfg.Environment))
		}
		if cfg.Environment["APP_ENV"] != "production" {
			t.Errorf("expected APP_ENV=production, got %q", cfg.Environment["APP_ENV"])
		}
		if len(cfg.Arguments) != 2 {
			t.Errorf("expected 2 arguments, got %d", len(cfg.Arguments))
		}
		if cfg.Arguments[0] != "--port" || cfg.Arguments[1] != "8080" {
			t.Errorf("unexpected arguments: %v", cfg.Arguments)
		}
		if cfg.User != "appuser" {
			t.Errorf("expected User=appuser, got %q", cfg.User)
		}
		if cfg.Group != "appgroup" {
			t.Errorf("expected Group=appgroup, got %q", cfg.Group)
		}
		if cfg.SystemdDirectives["LimitNOFILE"] != "65536" {
			t.Errorf("unexpected SystemdDirectives: %v", cfg.SystemdDirectives)
		}
		if cfg.LaunchdKeys["ThrottleInterval"] != 10 {
			t.Errorf("unexpected LaunchdKeys: %v", cfg.LaunchdKeys)
		}
		if cfg.OpenRCDirectives["rc_ulimit"] != "-n 65536" {
			t.Errorf("unexpected OpenRCDirectives: %v", cfg.OpenRCDirectives)
		}
		if cfg.CrontabSchedule != "*/5 * * * *" {
			t.Errorf("expected CrontabSchedule='*/5 * * * *', got %q", cfg.CrontabSchedule)
		}
	})

	t.Run("empty maps and slices", func(t *testing.T) {
		cfg := ServiceConfig{
			Name:              "test-service",
			ExecPath:          "/usr/local/bin/test",
			Environment:       map[string]string{},
			Arguments:         []string{},
			SystemdDirectives: map[string]string{},
			LaunchdKeys:       map[string]any{},
			OpenRCDirectives:  map[string]string{},
		}
		if len(cfg.Environment) != 0 {
			t.Error("expected empty Environment map")
		}
		if len(cfg.Arguments) != 0 {
			t.Error("expected empty Arguments slice")
		}
	})
}

func TestManagerImplementsInterface(t *testing.T) {
	var _ ServiceManager = (*systemdManager)(nil)
	var _ ServiceManager = (*launchdManager)(nil)
	var _ ServiceManager = (*openrcManager)(nil)
	var _ ServiceManager = (*dinitManager)(nil)
	var _ ServiceManager = (*finitManager)(nil)
	var _ ServiceManager = (*sysvinitManager)(nil)
	var _ ServiceManager = (*runitManager)(nil)
	var _ ServiceManager = (*s6Manager)(nil)
	var _ ServiceManager = (*procdManager)(nil)
	var _ ServiceManager = (*busyboxManager)(nil)
	var _ ServiceManager = (*crontabManager)(nil)
}

func TestCanRestartWatchdog(t *testing.T) {
	supervisorPath := func(string) (string, error) { return "/sbin/supervise-daemon", nil }
	noSupervisor := func(string) (string, error) { return "", errors.New("command not found") }

	tests := []struct {
		name string
		mgr  ServiceManager
		want bool
	}{
		{"systemd always restarts", &systemdManager{}, true},
		{"launchd always restarts", &launchdManager{}, true},
		{"openrc with supervisor support", &openrcManager{lookPathFunc: supervisorPath}, true},
		{"openrc without supervisor support", &openrcManager{lookPathFunc: noSupervisor}, false},
		{"dinit always restarts", &dinitManager{}, true},
		{"finit always restarts", &finitManager{}, true},
		{"sysvinit cannot restart", &sysvinitManager{}, false},
		{"runit always restarts", &runitManager{}, true},
		{"s6 always restarts", &s6Manager{}, true},
		{"procd always restarts", &procdManager{}, true},
		{"busybox cannot restart", &busyboxManager{}, false},
		{"crontab cannot restart", &crontabManager{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.mgr.CanRestartWatchdog()
			if got != tt.want {
				t.Errorf("CanRestartWatchdog() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSupportsCapabilities(t *testing.T) {
	tests := []struct {
		name       string
		mgr        ServiceManager
		wantEnable bool
		wantStart  bool
		wantStatus bool
	}{
		{"systemd", &systemdManager{}, true, true, true},
		{"launchd", &launchdManager{}, false, true, true},
		{"openrc", &openrcManager{}, true, true, true},
		{"dinit", &dinitManager{}, true, true, true},
		{"finit", &finitManager{}, true, true, true},
		{"sysvinit with runlevel dirs", &sysvinitManager{statFunc: func(path string) (os.FileInfo, error) {
			if path == "/etc/rc0.d" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}}, true, true, true},
		{"sysvinit BusyBox without K/S", &sysvinitManager{statFunc: func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }}, true, true, true},
		{"sysvinit BusyBox with K/S", &sysvinitManager{supportsKSPrefix: true, statFunc: func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }}, true, true, true},
		{"runit", &runitManager{}, true, true, true},
		{"s6", &s6Manager{}, true, true, true},
		{"procd", &procdManager{}, true, true, true},
		{"busybox without K/S", &busyboxManager{}, true, true, true},
		{"busybox with K/S", &busyboxManager{supportsKSPrefix: true}, true, true, true},
		{"crontab", &crontabManager{}, true, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.mgr.SupportsEnable(); got != tt.wantEnable {
				t.Errorf("SupportsEnable() = %v, want %v", got, tt.wantEnable)
			}
			if got := tt.mgr.SupportsStart(); got != tt.wantStart {
				t.Errorf("SupportsStart() = %v, want %v", got, tt.wantStart)
			}
			if got := tt.mgr.SupportsStatus(); got != tt.wantStatus {
				t.Errorf("SupportsStatus() = %v, want %v", got, tt.wantStatus)
			}
		})
	}
}

func TestName(t *testing.T) {
	tests := []struct {
		mgr  ServiceManager
		want string
	}{
		{&systemdManager{}, "systemd"},
		{&launchdManager{}, "launchd"},
		{&openrcManager{}, "openrc"},
		{&dinitManager{}, "dinit"},
		{&finitManager{}, "finit"},
		{&sysvinitManager{}, "sysvinit"},
		{&runitManager{}, "runit"},
		{&s6Manager{}, "s6"},
		{&procdManager{}, "procd"},
		{&busyboxManager{}, "busybox"},
		{&crontabManager{}, "crontab"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := tt.mgr.Name()
			if got != tt.want {
				t.Errorf("Name() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInitSystemTypeUsesNameMethod(t *testing.T) {
	tests := []struct {
		mgr  ServiceManager
		want string
	}{
		{&systemdManager{}, "systemd"},
		{&launchdManager{}, "launchd"},
		{&openrcManager{}, "openrc"},
		{&dinitManager{}, "dinit"},
		{&finitManager{}, "finit"},
		{&sysvinitManager{}, "sysvinit"},
		{&runitManager{}, "runit"},
		{&s6Manager{}, "s6"},
		{&procdManager{}, "procd"},
		{&busyboxManager{}, "busybox"},
		{&crontabManager{}, "crontab"},
	}
	for _, tt := range tests {
		got := InitSystemType(tt.mgr)
		if got != tt.want {
			t.Errorf("InitSystemType(%T) = %q, want %q", tt.mgr, got, tt.want)
		}
	}
}

func TestInitServiceManager(t *testing.T) {
	t.Run("systemd", func(t *testing.T) {
		mgr := &systemdManager{}
		InitServiceManager(mgr, "myservice")
		if mgr.serviceName != "myservice" {
			t.Errorf("expected serviceName 'myservice', got %q", mgr.serviceName)
		}
	})

	t.Run("launchd", func(t *testing.T) {
		mgr := &launchdManager{}
		InitServiceManager(mgr, "myservice")
		expected := launchdLabel("myservice")
		if mgr.label != expected {
			t.Errorf("expected label %q, got %q", expected, mgr.label)
		}
	})

	t.Run("openrc", func(t *testing.T) {
		mgr := &openrcManager{}
		InitServiceManager(mgr, "myservice")
		if mgr.serviceName != "myservice" {
			t.Errorf("expected serviceName 'myservice', got %q", mgr.serviceName)
		}
	})

	t.Run("dinit", func(t *testing.T) {
		mgr := &dinitManager{}
		InitServiceManager(mgr, "myservice")
		if mgr.serviceName != "myservice" {
			t.Errorf("expected serviceName 'myservice', got %q", mgr.serviceName)
		}
	})

	t.Run("finit", func(t *testing.T) {
		mgr := &finitManager{}
		InitServiceManager(mgr, "myservice")
		if mgr.serviceName != "myservice" {
			t.Errorf("expected serviceName 'myservice', got %q", mgr.serviceName)
		}
	})

	t.Run("sysvinit", func(t *testing.T) {
		mgr := &sysvinitManager{}
		InitServiceManager(mgr, "myservice")
		if mgr.serviceName != "myservice" {
			t.Errorf("expected serviceName 'myservice', got %q", mgr.serviceName)
		}
	})

	t.Run("runit", func(t *testing.T) {
		mgr := &runitManager{}
		InitServiceManager(mgr, "myservice")
		if mgr.serviceName != "myservice" {
			t.Errorf("expected serviceName 'myservice', got %q", mgr.serviceName)
		}
	})

	t.Run("s6", func(t *testing.T) {
		mgr := &s6Manager{}
		InitServiceManager(mgr, "myservice")
		if mgr.serviceName != "myservice" {
			t.Errorf("expected serviceName 'myservice', got %q", mgr.serviceName)
		}
	})

	t.Run("procd", func(t *testing.T) {
		mgr := &procdManager{}
		InitServiceManager(mgr, "myservice")
		if mgr.serviceName != "myservice" {
			t.Errorf("expected serviceName 'myservice', got %q", mgr.serviceName)
		}
	})

	t.Run("busybox", func(t *testing.T) {
		mgr := &busyboxManager{}
		InitServiceManager(mgr, "myservice")
		if mgr.serviceName != "myservice" {
			t.Errorf("expected serviceName 'myservice', got %q", mgr.serviceName)
		}
	})

	t.Run("crontab", func(t *testing.T) {
		mgr := &crontabManager{}
		InitServiceManager(mgr, "myservice")
		if mgr.serviceName != "myservice" {
			t.Errorf("expected serviceName 'myservice', got %q", mgr.serviceName)
		}
	})
}

func TestInitServiceManagerWithOptions(t *testing.T) {
	tests := []struct {
		name        string
		create      func() ServiceManager
		serviceOf   func(ServiceManager) string
		serviceName string
		wantValue   string
	}{
		{"systemd", func() ServiceManager { return &systemdManager{} }, func(m ServiceManager) string { return m.(*systemdManager).serviceName }, "myservice", "myservice"},
		{"launchd", func() ServiceManager { return &launchdManager{} }, func(m ServiceManager) string { return m.(*launchdManager).label }, "myservice", launchdLabel("myservice")},
		{"openrc", func() ServiceManager { return &openrcManager{} }, func(m ServiceManager) string { return m.(*openrcManager).serviceName }, "myservice", "myservice"},
		{"dinit", func() ServiceManager { return &dinitManager{} }, func(m ServiceManager) string { return m.(*dinitManager).serviceName }, "myservice", "myservice"},
		{"finit", func() ServiceManager { return &finitManager{} }, func(m ServiceManager) string { return m.(*finitManager).serviceName }, "myservice", "myservice"},
		{"sysvinit", func() ServiceManager { return &sysvinitManager{} }, func(m ServiceManager) string { return m.(*sysvinitManager).serviceName }, "myservice", "myservice"},
		{"runit", func() ServiceManager { return &runitManager{} }, func(m ServiceManager) string { return m.(*runitManager).serviceName }, "myservice", "myservice"},
		{"s6", func() ServiceManager { return &s6Manager{} }, func(m ServiceManager) string { return m.(*s6Manager).serviceName }, "myservice", "myservice"},
		{"procd", func() ServiceManager { return &procdManager{} }, func(m ServiceManager) string { return m.(*procdManager).serviceName }, "myservice", "myservice"},
		{"busybox", func() ServiceManager { return &busyboxManager{} }, func(m ServiceManager) string { return m.(*busyboxManager).serviceName }, "myservice", "myservice"},
		{"crontab", func() ServiceManager { return &crontabManager{} }, func(m ServiceManager) string { return m.(*crontabManager).serviceName }, "myservice", "myservice"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := tt.create()
			if err := InitServiceManagerWithOptions(mgr, ManagerOptions{ServiceName: tt.serviceName}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := tt.serviceOf(mgr); got != tt.wantValue {
				t.Errorf("expected %q, got %q", tt.wantValue, got)
			}
		})
	}
}

func TestInitServiceManagerWithOptionsInstallDir(t *testing.T) {
	t.Run("crontab install dir reaches the manifest path", func(t *testing.T) {
		mgr := &crontabManager{}
		if err := InitServiceManagerWithOptions(mgr, ManagerOptions{ServiceName: "myservice", InstallDir: "/opt/myservice"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if mgr.installDir != "/opt/myservice" {
			t.Errorf("expected installDir '/opt/myservice', got %q", mgr.installDir)
		}

		want := (&crontabManager{serviceName: "myservice", installDir: "/opt/myservice"}).manifestFilePath()
		if got := mgr.manifestFilePath(); got != want {
			t.Errorf("manifest path = %q, want %q", got, want)
		}
	})

	t.Run("empty install dir leaves manager untouched", func(t *testing.T) {
		mgr := &crontabManager{installDir: "/existing"}
		if err := InitServiceManagerWithOptions(mgr, ManagerOptions{ServiceName: "myservice"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mgr.installDir != "/existing" {
			t.Errorf("expected installDir to be preserved, got %q", mgr.installDir)
		}
	})

	t.Run("install dir is a no-op for non-crontab managers", func(t *testing.T) {
		mgr := &systemdManager{}
		if err := InitServiceManagerWithOptions(mgr, ManagerOptions{ServiceName: "myservice", InstallDir: "/opt/myservice"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mgr.serviceName != "myservice" {
			t.Errorf("expected serviceName 'myservice', got %q", mgr.serviceName)
		}
	})
}

// ManagerOptions.LogDir has to reach the managers that log to files: it is how
// a manager rebuilt from state learns where to create the redirect target its
// manifest already names.
func TestInitServiceManagerWithOptionsLogDir(t *testing.T) {
	t.Run("reaches the crontab manager", func(t *testing.T) {
		mgr := &crontabManager{}
		opts := ManagerOptions{ServiceName: "myservice", Scope: ScopeUser, LogDir: "/home/alice/.local/state/myservice"}
		if err := InitServiceManagerWithOptions(mgr, opts); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mgr.logDir != opts.LogDir {
			t.Errorf("logDir = %q, want %q", mgr.logDir, opts.LogDir)
		}
		if mgr.scope != ScopeUser {
			t.Errorf("scope = %v, want %v", mgr.scope, ScopeUser)
		}
	})

	t.Run("reaches the launchd manager", func(t *testing.T) {
		mgr := &launchdManager{}
		opts := ManagerOptions{ServiceName: "myservice", Scope: ScopeUser, LogDir: "/Users/alice/Library/Logs/myservice"}
		if err := InitServiceManagerWithOptions(mgr, opts); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mgr.logDir != opts.LogDir {
			t.Errorf("logDir = %q, want %q", mgr.logDir, opts.LogDir)
		}
	})
}

func TestInitServiceManagerWithOptionsErrors(t *testing.T) {
	tests := []struct {
		name string
		mgr  ServiceManager
		opts ManagerOptions
	}{
		{"nil manager", nil, ManagerOptions{ServiceName: "myservice"}},
		{"empty service name", &systemdManager{}, ManagerOptions{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := InitServiceManagerWithOptions(tt.mgr, tt.opts); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestDefaultDetectorsOrder(t *testing.T) {
	d := defaultDetectors()
	if len(d) != 11 {
		t.Fatalf("expected 11 detectors, got %d", len(d))
	}
	types := []string{
		"*daemon.systemdManager",
		"*daemon.launchdManager",
		"*daemon.openrcManager",
		"*daemon.dinitManager",
		"*daemon.finitManager",
		"*daemon.runitManager",
		"*daemon.s6Manager",
		"*daemon.procdManager",
		"*daemon.busyboxManager",
		"*daemon.sysvinitManager",
		"*daemon.crontabManager",
	}
	names := []string{
		"systemd", "launchd", "openrc", "dinit", "finit", "runit", "s6", "procd", "busybox", "sysvinit", "crontab",
	}
	for i, want := range types {
		mgr := d[i].create()
		got := fmt.Sprintf("%T", mgr)
		if got != want {
			t.Errorf("detector[%d] creates %s, want %s", i, got, want)
		}
		if d[i].name != names[i] {
			t.Errorf("detector[%d].name = %q, want %q", i, d[i].name, names[i])
		}
	}
}

func TestDetectionPriorityByName(t *testing.T) {
	expectedOrder := []string{
		"systemd", "launchd", "openrc", "dinit", "finit", "runit", "s6", "procd", "busybox", "sysvinit", "crontab",
	}

	d := defaultDetectors()
	if len(d) != len(expectedOrder) {
		t.Fatalf("expected %d detectors, got %d", len(expectedOrder), len(d))
	}

	for i, wantName := range expectedOrder {
		mgr := d[i].create()
		gotName := mgr.Name()
		if gotName != wantName {
			t.Errorf("detector[%d].Name() = %q, want %q", i, gotName, wantName)
		}
	}
}

func TestDetectionPriorityFirstMatchWins(t *testing.T) {
	expectedOrder := []string{
		"systemd", "launchd", "openrc", "dinit", "finit", "runit", "s6", "procd", "busybox", "sysvinit", "crontab",
	}

	for targetIdx, targetName := range expectedOrder {
		t.Run(targetName+" wins when first to detect", func(t *testing.T) {
			entries := make([]detectorEntry, len(expectedOrder))
			for i := range expectedOrder {
				detected := i == targetIdx
				name := expectedOrder[i]
				entries[i] = detectorEntry{
					name:   name,
					detect: func() bool { return detected },
					create: createManagerByName(name),
				}
			}
			withDetectors(entries, func() {
				mgr, err := DetectServiceManager()
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if mgr.Name() != targetName {
					t.Errorf("got %q, want %q", mgr.Name(), targetName)
				}
			})
		})
	}
}

func TestDetectServiceManager_SysvinitDetectionOrder(t *testing.T) {
	d := defaultDetectors()
	var sysvinitIdx, openrcIdx, runitIdx, dinitIdx, finitIdx, busyboxIdx, crontabIdx int
	for i, entry := range d {
		switch entry.name {
		case "openrc":
			openrcIdx = i
		case "dinit":
			dinitIdx = i
		case "finit":
			finitIdx = i
		case "sysvinit":
			sysvinitIdx = i
		case "runit":
			runitIdx = i
		case "busybox":
			busyboxIdx = i
		case "crontab":
			crontabIdx = i
		}
	}
	if sysvinitIdx <= busyboxIdx {
		t.Errorf("sysvinit (index %d) should come after busybox (index %d)", sysvinitIdx, busyboxIdx)
	}
	if sysvinitIdx >= crontabIdx {
		t.Errorf("sysvinit (index %d) should come before crontab (index %d)", sysvinitIdx, crontabIdx)
	}
	if dinitIdx <= openrcIdx {
		t.Errorf("dinit (index %d) should come after openrc (index %d)", dinitIdx, openrcIdx)
	}
	if dinitIdx >= runitIdx {
		t.Errorf("dinit (index %d) should come before runit (index %d)", dinitIdx, runitIdx)
	}
	if finitIdx <= dinitIdx {
		t.Errorf("finit (index %d) should come after dinit (index %d)", finitIdx, dinitIdx)
	}
	if finitIdx >= runitIdx {
		t.Errorf("finit (index %d) should come before runit (index %d)", finitIdx, runitIdx)
	}
}

func TestDetectServiceManager_RunitBeforeSysvinit(t *testing.T) {
	withDetectors([]detectorEntry{
		{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
		{name: "launchd", detect: func() bool { return false }, create: func() ServiceManager { return &launchdManager{} }},
		{name: "openrc", detect: func() bool { return false }, create: func() ServiceManager { return &openrcManager{} }},
		{name: "dinit", detect: func() bool { return false }, create: func() ServiceManager { return newDinitManager() }},
		{name: "finit", detect: func() bool { return false }, create: func() ServiceManager { return newFinitManager() }},
		{name: "runit", detect: func() bool { return true }, create: func() ServiceManager { return &runitManager{} }},
		{name: "sysvinit", detect: func() bool { return true }, create: func() ServiceManager { return &sysvinitManager{} }},
		{name: "crontab", detect: func() bool { return true }, create: func() ServiceManager { return &crontabManager{} }},
	}, func() {
		mgr, err := DetectServiceManager()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := mgr.(*runitManager); !ok {
			t.Errorf("expected runitManager (before sysvinit), got %T", mgr)
		}
	})
}

func TestDetectServiceManager_NanokvmScenario(t *testing.T) {
	requireLinuxDetection(t)
	runitMgr := newTestRunitManager(func(m *runitManager) {
		m.lookPathFunc = func(name string) (string, error) {
			if name == "runsvdir" {
				return "/usr/bin/runsvdir", nil
			}
			return "", errors.New("not found")
		}
		m.statFunc = func(path string) (os.FileInfo, error) {
			return nil, os.ErrNotExist
		}
	})
	if runitMgr.detect() {
		t.Fatal("runit should not be detected when /var/service is absent")
	}

	busyboxMgr := newTestBusyboxManager(func(m *busyboxManager) {
		m.lookPathFunc = func(name string) (string, error) {
			if name == "runsvdir" {
				return "/usr/bin/runsvdir", nil
			}
			return "", errors.New("not found")
		}
		m.statFunc = func(path string) (os.FileInfo, error) {
			return nil, os.ErrNotExist
		}
		m.readlinkFunc = func(path string) (string, error) {
			if path == "/proc/1/exe" {
				return "/bin/busybox", nil
			}
			return "", os.ErrNotExist
		}
	})
	if !busyboxMgr.detect() {
		t.Fatal("busybox should be detected when runsvdir exists but /var/service is absent and PID 1 is busybox")
	}

	withDetectors([]detectorEntry{
		{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
		{name: "launchd", detect: func() bool { return false }, create: func() ServiceManager { return &launchdManager{} }},
		{name: "openrc", detect: func() bool { return false }, create: func() ServiceManager { return &openrcManager{} }},
		{name: "dinit", detect: func() bool { return false }, create: func() ServiceManager { return newDinitManager() }},
		{name: "finit", detect: func() bool { return false }, create: func() ServiceManager { return newFinitManager() }},
		{name: "sysvinit", detect: func() bool { return false }, create: func() ServiceManager { return &sysvinitManager{} }},
		{name: "runit", detect: runitMgr.detect, create: func() ServiceManager { return &runitManager{} }},
		{name: "s6", detect: func() bool { return false }, create: func() ServiceManager { return &s6Manager{} }},
		{name: "procd", detect: func() bool { return false }, create: func() ServiceManager { return &procdManager{} }},
		{name: "busybox", detect: busyboxMgr.detect, create: func() ServiceManager { return &busyboxManager{} }},
		{name: "crontab", detect: func() bool { return false }, create: func() ServiceManager { return &crontabManager{} }},
	}, func() {
		mgr, err := DetectServiceManager()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := mgr.(*busyboxManager); !ok {
			t.Errorf("expected busyboxManager, got %T", mgr)
		}
	})
}

func TestDetectServiceManager_SysvinitScenario(t *testing.T) {
	sysvinitMgr := newTestSysvinitManager(func(m *sysvinitManager) {
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/inittab" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
		m.readlinkFunc = func(path string) (string, error) {
			if path == "/proc/1/exe" {
				return "/usr/sbin/init", nil
			}
			return "", os.ErrNotExist
		}
	})

	withDetectionLogger(func(buf *bytes.Buffer) {
		withDetectors([]detectorEntry{
			{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
			{name: "launchd", detect: func() bool { return false }, create: func() ServiceManager { return &launchdManager{} }},
			{name: "openrc", detect: func() bool { return false }, create: func() ServiceManager { return &openrcManager{} }},
			{name: "dinit", detect: func() bool { return false }, create: func() ServiceManager { return newDinitManager() }},
			{name: "finit", detect: func() bool { return false }, create: func() ServiceManager { return newFinitManager() }},
			{name: "sysvinit", detect: sysvinitMgr.detect, create: func() ServiceManager { return &sysvinitManager{} }},
			{name: "runit", detect: func() bool { return false }, create: func() ServiceManager { return &runitManager{} }},
			{name: "s6", detect: func() bool { return false }, create: func() ServiceManager { return &s6Manager{} }},
			{name: "procd", detect: func() bool { return false }, create: func() ServiceManager { return &procdManager{} }},
			{name: "busybox", detect: func() bool { return false }, create: func() ServiceManager { return &busyboxManager{} }},
			{name: "crontab", detect: func() bool { return true }, create: func() ServiceManager { return &crontabManager{} }},
		}, func() {
			mgr, err := DetectServiceManager()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, ok := mgr.(*sysvinitManager); !ok {
				t.Errorf("expected sysvinitManager, got %T", mgr)
			}

			output := buf.String()
			if !strings.Contains(output, "selected manager=sysvinit") {
				t.Errorf("expected log to contain 'selected manager=sysvinit', got:\n%s", output)
			}
		})
	})
}

func TestDetectServiceManager_NanokvmSysvinitScenario(t *testing.T) {
	runitMgr := newTestRunitManager(func(m *runitManager) {
		m.lookPathFunc = func(name string) (string, error) {
			if name == "runsvdir" {
				return "/usr/bin/runsvdir", nil
			}
			return "", errors.New("not found")
		}
		m.statFunc = func(path string) (os.FileInfo, error) {
			return nil, os.ErrNotExist
		}
	})

	sysvinitMgr := newTestSysvinitManager(func(m *sysvinitManager) {
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/inittab" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
		m.readlinkFunc = func(path string) (string, error) {
			if path == "/proc/1/exe" {
				return "/usr/sbin/init", nil
			}
			return "", os.ErrNotExist
		}
	})

	withDetectors([]detectorEntry{
		{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
		{name: "launchd", detect: func() bool { return false }, create: func() ServiceManager { return &launchdManager{} }},
		{name: "openrc", detect: func() bool { return false }, create: func() ServiceManager { return &openrcManager{} }},
		{name: "dinit", detect: func() bool { return false }, create: func() ServiceManager { return newDinitManager() }},
		{name: "finit", detect: func() bool { return false }, create: func() ServiceManager { return newFinitManager() }},
		{name: "sysvinit", detect: sysvinitMgr.detect, create: func() ServiceManager { return &sysvinitManager{} }},
		{name: "runit", detect: runitMgr.detect, create: func() ServiceManager { return &runitManager{} }},
		{name: "s6", detect: func() bool { return false }, create: func() ServiceManager { return &s6Manager{} }},
		{name: "procd", detect: func() bool { return false }, create: func() ServiceManager { return &procdManager{} }},
		{name: "busybox", detect: func() bool { return false }, create: func() ServiceManager { return &busyboxManager{} }},
		{name: "crontab", detect: func() bool { return true }, create: func() ServiceManager { return &crontabManager{} }},
	}, func() {
		mgr, err := DetectServiceManager()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := mgr.(*sysvinitManager); !ok {
			t.Errorf("expected sysvinitManager on nanokvm (not crontab), got %T", mgr)
		}
	})
}

func createManagerByName(name string) func() ServiceManager {
	switch name {
	case "systemd":
		return func() ServiceManager { return &systemdManager{} }
	case "launchd":
		return func() ServiceManager { return &launchdManager{} }
	case "openrc":
		return func() ServiceManager { return &openrcManager{} }
	case "dinit":
		return func() ServiceManager { return newDinitManager() }
	case "finit":
		return func() ServiceManager { return newFinitManager() }
	case "sysvinit":
		return func() ServiceManager { return &sysvinitManager{} }
	case "runit":
		return func() ServiceManager { return &runitManager{} }
	case "s6":
		return func() ServiceManager { return &s6Manager{} }
	case "procd":
		return func() ServiceManager { return &procdManager{} }
	case "busybox":
		return func() ServiceManager { return &busyboxManager{} }
	case "crontab":
		return func() ServiceManager { return &crontabManager{} }
	default:
		panic("unknown manager: " + name)
	}
}

func withDetectionLogger(fn func(logOutput *bytes.Buffer)) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)
	fn(&buf)
}

func TestDetectServiceManager_LogsDetectionSuccess(t *testing.T) {
	withDetectionLogger(func(buf *bytes.Buffer) {
		withDetectors([]detectorEntry{
			{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
			{name: "launchd", detect: func() bool { return true }, create: func() ServiceManager { return &launchdManager{} }},
		}, func() {
			mgr, err := DetectServiceManager()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, ok := mgr.(*launchdManager); !ok {
				t.Errorf("expected launchdManager, got %T", mgr)
			}

			output := buf.String()
			expectedMessages := []string{
				"init detection: starting service manager detection",
				"init detection: checking systemd",
				"init detection: systemd: detected=false",
				"init detection: checking launchd",
				"init detection: launchd: detected=true",
				"init detection: selected manager=launchd",
			}
			for _, msg := range expectedMessages {
				if !strings.Contains(output, msg) {
					t.Errorf("expected log to contain %q, got:\n%s", msg, output)
				}
			}
		})
	})
}

func TestDetectServiceManager_LogsDetectionFallback(t *testing.T) {
	withDetectionLogger(func(buf *bytes.Buffer) {
		withDetectors([]detectorEntry{
			{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
			{name: "launchd", detect: func() bool { return false }, create: func() ServiceManager { return &launchdManager{} }},
			{name: "openrc", detect: func() bool { return false }, create: func() ServiceManager { return &openrcManager{} }},
		}, func() {
			_, err := DetectServiceManager()
			if err == nil {
				t.Fatal("expected error when no manager detected")
			}

			output := buf.String()
			expectedMessages := []string{
				"init detection: starting service manager detection",
				"init detection: checking systemd",
				"init detection: systemd: detected=false",
				"init detection: checking launchd",
				"init detection: launchd: detected=false",
				"init detection: checking openrc",
				"init detection: openrc: detected=false",
				"init detection: no supported service manager detected",
			}
			for _, msg := range expectedMessages {
				if !strings.Contains(output, msg) {
					t.Errorf("expected log to contain %q, got:\n%s", msg, output)
				}
			}

			if strings.Contains(output, "selected manager=") {
				t.Error("should not contain 'selected manager=' when no manager detected")
			}
		})
	})
}

func TestDetectServiceManager_LogsEntryPoint(t *testing.T) {
	withDetectionLogger(func(buf *bytes.Buffer) {
		withDetectors([]detectorEntry{
			{name: "test", detect: func() bool { return true }, create: func() ServiceManager { return &systemdManager{} }},
		}, func() {
			_, _ = DetectServiceManager()
			output := buf.String()
			if !strings.Contains(output, "init detection: starting service manager detection") {
				t.Errorf("expected entry log, got:\n%s", output)
			}
		})
	})
}

func TestDetectServiceManager_LogsEachDetector(t *testing.T) {
	withDetectionLogger(func(buf *bytes.Buffer) {
		names := []string{"alpha", "beta", "gamma"}
		entries := []detectorEntry{
			{name: "alpha", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
			{name: "beta", detect: func() bool { return false }, create: func() ServiceManager { return &launchdManager{} }},
			{name: "gamma", detect: func() bool { return true }, create: func() ServiceManager { return &openrcManager{} }},
		}
		withDetectors(entries, func() {
			_, _ = DetectServiceManager()
			output := buf.String()
			for _, name := range names {
				if !strings.Contains(output, "init detection: checking "+name) {
					t.Errorf("expected log for checking %q, got:\n%s", name, output)
				}
			}
		})
	})
}

func TestDetectServiceManager_NoLogWithNoopLogger(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	SetDetectionLogger(nil)
	withDetectors([]detectorEntry{
		{name: "test", detect: func() bool { return true }, create: func() ServiceManager { return &systemdManager{} }},
	}, func() {
		_, err := DetectServiceManager()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestDetectServiceManager_DinitScenario(t *testing.T) {
	dinitMgr := newTestDinitManager(func(m *dinitManager) {
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/dinit.d" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
		m.lookPathFunc = func(name string) (string, error) {
			if name == "dinitctl" {
				return "/usr/bin/dinitctl", nil
			}
			return "", errors.New("not found")
		}
		m.readlinkFunc = func(path string) (string, error) {
			if path == "/proc/1/exe" {
				return "/sbin/dinit", nil
			}
			return "", os.ErrNotExist
		}
	})

	withDetectionLogger(func(buf *bytes.Buffer) {
		withDetectors([]detectorEntry{
			{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
			{name: "launchd", detect: func() bool { return false }, create: func() ServiceManager { return &launchdManager{} }},
			{name: "openrc", detect: func() bool { return false }, create: func() ServiceManager { return &openrcManager{} }},
			{name: "dinit", detect: dinitMgr.detect, create: func() ServiceManager { return newDinitManager() }},
			{name: "finit", detect: func() bool { return false }, create: func() ServiceManager { return newFinitManager() }},
			{name: "sysvinit", detect: func() bool { return false }, create: func() ServiceManager { return &sysvinitManager{} }},
			{name: "runit", detect: func() bool { return false }, create: func() ServiceManager { return &runitManager{} }},
			{name: "s6", detect: func() bool { return false }, create: func() ServiceManager { return &s6Manager{} }},
			{name: "procd", detect: func() bool { return false }, create: func() ServiceManager { return &procdManager{} }},
			{name: "busybox", detect: func() bool { return false }, create: func() ServiceManager { return &busyboxManager{} }},
			{name: "crontab", detect: func() bool { return true }, create: func() ServiceManager { return &crontabManager{} }},
		}, func() {
			mgr, err := DetectServiceManager()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, ok := mgr.(*dinitManager); !ok {
				t.Errorf("expected dinitManager, got %T", mgr)
			}

			output := buf.String()
			if !strings.Contains(output, "selected manager=dinit") {
				t.Errorf("expected log to contain 'selected manager=dinit', got:\n%s", output)
			}
		})
	})
}

func TestDetectServiceManager_FinitScenario(t *testing.T) {
	finitMgr := newTestFinitManager(func(m *finitManager) {
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/finit.d" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
		m.lookPathFunc = func(name string) (string, error) {
			if name == "initctl" {
				return "/usr/bin/initctl", nil
			}
			return "", errors.New("not found")
		}
		m.readlinkFunc = func(path string) (string, error) {
			if path == "/proc/1/exe" {
				return "/sbin/finit", nil
			}
			return "", os.ErrNotExist
		}
	})

	withDetectionLogger(func(buf *bytes.Buffer) {
		withDetectors([]detectorEntry{
			{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
			{name: "launchd", detect: func() bool { return false }, create: func() ServiceManager { return &launchdManager{} }},
			{name: "openrc", detect: func() bool { return false }, create: func() ServiceManager { return &openrcManager{} }},
			{name: "dinit", detect: func() bool { return false }, create: func() ServiceManager { return newDinitManager() }},
			{name: "finit", detect: finitMgr.detect, create: func() ServiceManager { return newFinitManager() }},
			{name: "sysvinit", detect: func() bool { return false }, create: func() ServiceManager { return &sysvinitManager{} }},
			{name: "runit", detect: func() bool { return false }, create: func() ServiceManager { return &runitManager{} }},
			{name: "s6", detect: func() bool { return false }, create: func() ServiceManager { return &s6Manager{} }},
			{name: "procd", detect: func() bool { return false }, create: func() ServiceManager { return &procdManager{} }},
			{name: "busybox", detect: func() bool { return false }, create: func() ServiceManager { return &busyboxManager{} }},
			{name: "crontab", detect: func() bool { return true }, create: func() ServiceManager { return &crontabManager{} }},
		}, func() {
			mgr, err := DetectServiceManager()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, ok := mgr.(*finitManager); !ok {
				t.Errorf("expected finitManager, got %T", mgr)
			}

			output := buf.String()
			if !strings.Contains(output, "selected manager=finit") {
				t.Errorf("expected log to contain 'selected manager=finit', got:\n%s", output)
			}
		})
	})
}

func TestDetectServiceManager_DinitFinitPriority(t *testing.T) {
	dinitMgr := newTestDinitManager(func(m *dinitManager) {
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/dinit.d" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
		m.readlinkFunc = func(path string) (string, error) {
			if path == "/proc/1/exe" {
				return "/sbin/dinit", nil
			}
			return "", os.ErrNotExist
		}
	})

	finitMgr := newTestFinitManager(func(m *finitManager) {
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/finit.d" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
		m.readlinkFunc = func(path string) (string, error) {
			if path == "/proc/1/exe" {
				return "/sbin/finit", nil
			}
			return "", os.ErrNotExist
		}
	})

	withDetectors([]detectorEntry{
		{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
		{name: "launchd", detect: func() bool { return false }, create: func() ServiceManager { return &launchdManager{} }},
		{name: "openrc", detect: func() bool { return false }, create: func() ServiceManager { return &openrcManager{} }},
		{name: "dinit", detect: dinitMgr.detect, create: func() ServiceManager { return newDinitManager() }},
		{name: "finit", detect: finitMgr.detect, create: func() ServiceManager { return newFinitManager() }},
		{name: "sysvinit", detect: func() bool { return false }, create: func() ServiceManager { return &sysvinitManager{} }},
		{name: "runit", detect: func() bool { return false }, create: func() ServiceManager { return &runitManager{} }},
		{name: "s6", detect: func() bool { return false }, create: func() ServiceManager { return &s6Manager{} }},
		{name: "procd", detect: func() bool { return false }, create: func() ServiceManager { return &procdManager{} }},
		{name: "busybox", detect: func() bool { return false }, create: func() ServiceManager { return &busyboxManager{} }},
		{name: "crontab", detect: func() bool { return true }, create: func() ServiceManager { return &crontabManager{} }},
	}, func() {
		mgr, err := DetectServiceManager()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := mgr.(*dinitManager); !ok {
			t.Errorf("expected dinitManager (higher priority than finit), got %T", mgr)
		}
	})
}

type daemonpilotRunError struct{ msg string }

func (e *daemonpilotRunError) Error() string { return e.msg }

func TestRunErr(t *testing.T) {
	base := errors.New("exit status 1")

	tests := []struct {
		name         string
		action       string
		out          []byte
		err          error
		want         string
		wantContains string
	}{
		{
			name:   "no output keeps the bare message",
			action: "starting service",
			out:    nil,
			err:    base,
			want:   "starting service: exit status 1",
		},
		{
			name:   "whitespace-only output is treated as empty",
			action: "starting service",
			out:    []byte("  \n\t "),
			err:    base,
			want:   "starting service: exit status 1",
		},
		{
			name:   "output is appended and trimmed",
			action: "enabling service",
			out:    []byte("\nInteractive authentication required.\n"),
			err:    base,
			want:   "enabling service: exit status 1: Interactive authentication required.",
		},
		{
			name:   "multi-line output is preserved",
			action: "starting service",
			out:    []byte("Job failed.\nSee journalctl -xeu foo.service."),
			err:    base,
			want:   "starting service: exit status 1: Job failed.\nSee journalctl -xeu foo.service.",
		},
		{
			name:   "terminal escape sequences are stripped",
			action: "starting service",
			out:    []byte("\x1b[2J\x1b[1;31mfailed\x1b[0m\rnot really"),
			err:    base,
			want:   "starting service: exit status 1: [2J[1;31mfailed[0mnot really",
		},
		{
			name:   "delete and C1 control characters are stripped",
			action: "starting service",
			out:    []byte("fai\x7fled\u009bmore"),
			err:    base,
			want:   "starting service: exit status 1: failedmore",
		},
		{
			name:   "tabs and newlines survive stripping",
			action: "starting service",
			out:    []byte("line one\n\tindented"),
			err:    base,
			want:   "starting service: exit status 1: line one\n\tindented",
		},
		{
			name:         "long output is truncated",
			action:       "stopping service",
			out:          []byte(strings.Repeat("x", maxCommandOutputBytes+100)),
			err:          base,
			wantContains: strings.Repeat("x", maxCommandOutputBytes) + " (output truncated)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runErr(tt.action, tt.out, tt.err)
			if got == nil {
				t.Fatal("runErr() = nil, want error")
			}
			if tt.want != "" && got.Error() != tt.want {
				t.Errorf("runErr() = %q, want %q", got.Error(), tt.want)
			}
			if tt.wantContains != "" && !strings.Contains(got.Error(), tt.wantContains) {
				t.Errorf("runErr() = %q, want it to contain %q", got.Error(), tt.wantContains)
			}
			if !errors.Is(got, tt.err) {
				t.Errorf("errors.Is(runErr(), original) = false, want true")
			}
		})
	}
}

func TestRunErrTruncatesToExactLimit(t *testing.T) {
	out := []byte(strings.Repeat("y", maxCommandOutputBytes*3))
	got := runErr("starting service", out, errors.New("boom")).Error()

	if strings.Count(got, "y") != maxCommandOutputBytes {
		t.Errorf("kept %d output bytes, want %d", strings.Count(got, "y"), maxCommandOutputBytes)
	}
	if !strings.HasSuffix(got, "(output truncated)") {
		t.Errorf("runErr() = %q, want a truncation marker suffix", got)
	}
}

func TestRunErrTruncationDropsPartialRune(t *testing.T) {
	// A multi-byte rune straddling the cut point must not leave invalid UTF-8.
	out := []byte(strings.Repeat("a", maxCommandOutputBytes-1) + "état")
	got := runErr("starting service", out, errors.New("boom")).Error()

	if strings.ContainsRune(got, '�') {
		t.Errorf("runErr() = %q, want no replacement character", got)
	}
}

func TestRunErrUnwrapsToTypedError(t *testing.T) {
	daemonpilot := &daemonpilotRunError{msg: "permission denied"}
	got := runErr("enabling service", []byte("Access denied."), daemonpilot)

	var target *daemonpilotRunError
	if !errors.As(got, &target) {
		t.Fatal("errors.As(runErr(), *daemonpilotRunError) = false, want true")
	}
	if target.msg != "permission denied" {
		t.Errorf("unwrapped error = %q, want %q", target.msg, "permission denied")
	}
}

// Command output must survive the trip from the executor to the operator for
// every init system, not just the one whose manager happened to be tested.
func TestManagersIncludeCommandOutputInErrors(t *testing.T) {
	const stderr = "Interactive authentication required."
	failure := errors.New("exit status 1")

	// arrange overrides the default "make exactly this command fail" wiring for
	// managers whose failing command carries an argument the test cannot predict.
	tests := []struct {
		name    string
		command string
		arrange func(exec *mockExecutor)
		newMgr  func(exec commandExecutor) ServiceManager
		op      func(mgr ServiceManager) error
	}{
		{
			name:    "systemd start",
			command: "systemctl start test-svc",
			newMgr: func(exec commandExecutor) ServiceManager {
				return &systemdManager{executor: exec, fs: newMockFS(), serviceName: "test-svc"}
			},
			op: func(mgr ServiceManager) error { return mgr.Start() },
		},
		{
			name:    "systemd enable",
			command: "systemctl enable test-svc",
			newMgr: func(exec commandExecutor) ServiceManager {
				return &systemdManager{executor: exec, fs: newMockFS(), serviceName: "test-svc"}
			},
			op: func(mgr ServiceManager) error { return mgr.Enable() },
		},
		{
			name:    "launchd start",
			command: "launchctl kickstart -k system/com.test-svc",
			newMgr: func(exec commandExecutor) ServiceManager {
				return &launchdManager{executor: exec, fs: newMockFS(), label: "com.test-svc"}
			},
			op: func(mgr ServiceManager) error { return mgr.Start() },
		},
		{
			name:    "openrc stop",
			command: "rc-service test-svc stop",
			newMgr: func(exec commandExecutor) ServiceManager {
				return &openrcManager{executor: exec, fs: newMockFS(), serviceName: "test-svc"}
			},
			op: func(mgr ServiceManager) error { return mgr.Stop() },
		},
		{
			name:    "dinit disable",
			command: "dinitctl disable test-svc",
			newMgr: func(exec commandExecutor) ServiceManager {
				return &dinitManager{
					executor:     exec,
					fs:           newMockFS(),
					serviceName:  "test-svc",
					lookPathFunc: func(string) (string, error) { return "/usr/bin/dinitctl", nil },
				}
			},
			op: func(mgr ServiceManager) error { return mgr.Disable() },
		},
		{
			name:    "runit start",
			command: "sv up test-svc",
			newMgr: func(exec commandExecutor) ServiceManager {
				return &runitManager{
					executor:     exec,
					fs:           newMockFS(),
					serviceName:  "test-svc",
					lookPathFunc: func(string) (string, error) { return "/usr/bin/sv", nil },
					statFunc:     func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
				}
			},
			op: func(mgr ServiceManager) error { return mgr.Start() },
		},
		{
			name:    "finit start",
			command: "initctl start test-svc",
			newMgr: func(exec commandExecutor) ServiceManager {
				return &finitManager{
					executor:     exec,
					fs:           newMockFS(),
					serviceName:  "test-svc",
					lookPathFunc: func(string) (string, error) { return "/sbin/initctl", nil },
				}
			},
			op: func(mgr ServiceManager) error { return mgr.Start() },
		},
		{
			name:    "sysvinit stop via service command",
			command: "service test-svc stop",
			newMgr: func(exec commandExecutor) ServiceManager {
				return &sysvinitManager{
					executor:     exec,
					fs:           newMockFS(),
					serviceName:  "test-svc",
					lookPathFunc: func(string) (string, error) { return "/usr/sbin/service", nil },
					statFunc:     func(string) (os.FileInfo, error) { return nil, nil },
				}
			},
			op: func(mgr ServiceManager) error { return mgr.Stop() },
		},
		{
			name:    "sysvinit enable via update-rc.d",
			command: "update-rc.d test-svc defaults",
			newMgr: func(exec commandExecutor) ServiceManager {
				return &sysvinitManager{
					executor:    exec,
					fs:          newMockFS(),
					serviceName: "test-svc",
					lookPathFunc: func(name string) (string, error) {
						if name == "update-rc.d" {
							return "/usr/sbin/update-rc.d", nil
						}
						return "", os.ErrNotExist
					},
					statFunc: func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
				}
			},
			op: func(mgr ServiceManager) error { return mgr.Enable() },
		},
		{
			name:    "s6 stop",
			command: "s6-svc -wD -T 30000 -d /run/service/test-svc",
			newMgr: func(exec commandExecutor) ServiceManager {
				return &s6Manager{
					executor:    exec,
					fs:          newMockFS(),
					serviceName: "test-svc",
					scanDir:     "/run/service",
				}
			},
			op: func(mgr ServiceManager) error { return mgr.Stop() },
		},
		{
			name:    "procd enable",
			command: "/etc/init.d/test-svc enable",
			newMgr: func(exec commandExecutor) ServiceManager {
				return &procdManager{executor: exec, fs: newMockFS(), serviceName: "test-svc"}
			},
			op: func(mgr ServiceManager) error { return mgr.Enable() },
		},
		{
			name:    "busybox start",
			command: "/etc/init.d/S95test-svc start",
			newMgr: func(exec commandExecutor) ServiceManager {
				return &busyboxManager{
					executor:    exec,
					fs:          newMockFS(),
					serviceName: "test-svc",
					statFunc:    func(string) (os.FileInfo, error) { return nil, nil },
				}
			},
			op: func(mgr ServiceManager) error { return mgr.Start() },
		},
		{
			// writeCrontab shells out to "crontab <temp file>", so the failing
			// command cannot be named up front; fail everything but the read.
			name: "crontab install",
			arrange: func(exec *mockExecutor) {
				exec.setResult("crontab -l", []byte(""), nil)
				exec.fallbackOutput = []byte(stderr)
				exec.fallbackErr = failure
			},
			newMgr: func(exec commandExecutor) ServiceManager {
				return &crontabManager{
					executor:     exec,
					serviceName:  "test-svc",
					installDir:   "/opt/test-svc",
					lookPathFunc: func(string) (string, error) { return "/usr/bin/crontab", nil },
				}
			},
			op: func(mgr ServiceManager) error {
				return mgr.Install(ServiceConfig{Name: "test-svc", ExecPath: "/opt/test-svc/test-svc"})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			if tt.arrange != nil {
				tt.arrange(exec)
			} else {
				exec.setResult(tt.command, []byte(stderr), failure)
			}

			err := tt.op(tt.newMgr(exec))
			if err == nil {
				t.Fatal("operation succeeded, want error")
			}
			if !strings.Contains(err.Error(), stderr) {
				t.Errorf("error = %q, want it to contain the command output %q", err.Error(), stderr)
			}
			if !errors.Is(err, failure) {
				t.Errorf("errors.Is(err, failure) = false, want true")
			}
		})
	}
}

func TestManagerErrorOmitsEmptyCommandOutput(t *testing.T) {
	failure := errors.New("exit status 1")
	exec := newMockExecutor()
	exec.setResult("systemctl start test-svc", nil, failure)

	mgr := &systemdManager{executor: exec, fs: newMockFS(), serviceName: "test-svc"}
	err := mgr.Start()
	if err == nil {
		t.Fatal("Start() succeeded, want error")
	}
	if err.Error() != "starting service: exit status 1" {
		t.Errorf("Start() error = %q, want %q", err.Error(), "starting service: exit status 1")
	}
}

// systemdEntry builds a detector entry named "systemd" whose host predicate and
// user-manager reachability are controlled independently.
func systemdEntry(host, userManager bool) detectorEntry {
	return detectorEntry{
		name:   "systemd",
		detect: func() bool { return host },
		create: func() ServiceManager {
			return &systemdManager{
				statFunc: func(path string) (os.FileInfo, error) {
					if path == "/run/user/1000/systemd/private" && userManager {
						return mockFileInfo{name: path}, nil
					}
					return nil, os.ErrNotExist
				},
				getenvFunc:  func(string) string { return "" },
				geteuidFunc: func() int { return 1000 },
			}
		},
	}
}

func TestDetectForScope_SystemScopeUnchanged(t *testing.T) {
	withDetectors([]detectorEntry{
		{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
		{name: "openrc", detect: func() bool { return true }, create: func() ServiceManager { return &openrcManager{} }},
		{name: "crontab", detect: func() bool { return true }, create: func() ServiceManager { return &crontabManager{} }},
	}, func() {
		det, err := DetectForScope(ScopeSystem)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := det.Manager.(*openrcManager); !ok {
			t.Errorf("expected openrcManager, got %T", det.Manager)
		}
		if det.HostInitSystem != "openrc" {
			t.Errorf("HostInitSystem = %q, want %q", det.HostInitSystem, "openrc")
		}
		if len(det.Skipped) != 0 {
			t.Errorf("Skipped = %v, want none", det.Skipped)
		}
	})
}

func TestDetectForScope_SkipsUnsupportedManagers(t *testing.T) {
	withDetectors([]detectorEntry{
		{name: "openrc", detect: func() bool { return true }, create: func() ServiceManager { return &openrcManager{} }},
		systemdEntry(true, true),
	}, func() {
		det, err := DetectForScope(ScopeUser)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := det.Manager.(*systemdManager); !ok {
			t.Errorf("expected systemdManager, got %T", det.Manager)
		}
		if len(det.Skipped) != 1 || det.Skipped[0].Name != "openrc" {
			t.Fatalf("Skipped = %v, want openrc only", det.Skipped)
		}
		if !strings.Contains(det.Skipped[0].Reason, "user scope") {
			t.Errorf("skip reason = %q, want it to mention user scope", det.Skipped[0].Reason)
		}
	})
}

// TestDetectForScope_ReportsUnfilteredHostInit covers the embedded-log decision:
// the host is procd, but user scope selects systemd, and callers must still be
// able to see that the host is an embedded init system.
func TestDetectForScope_ReportsUnfilteredHostInit(t *testing.T) {
	withDetectors([]detectorEntry{
		{name: "procd", detect: func() bool { return true }, create: func() ServiceManager { return &procdManager{} }},
		systemdEntry(true, true),
	}, func() {
		det, err := DetectForScope(ScopeUser)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if det.HostInitSystem != "procd" {
			t.Errorf("HostInitSystem = %q, want %q", det.HostInitSystem, "procd")
		}
		if det.Manager.Name() != "systemd" {
			t.Errorf("selected manager = %q, want systemd", det.Manager.Name())
		}
	})
}

// A detection failure must not erase HostInitSystem: on an embedded board no
// manager supports user scope, so the error path is the only one callers reach,
// and the embedded log filter keys off exactly this field.
func TestDetectForScope_ReportsHostInitOnError(t *testing.T) {
	tests := []struct {
		name      string
		detectors []detectorEntry
		wantErr   error
	}{
		{
			name: "no manager supports the scope",
			detectors: []detectorEntry{
				{name: "procd", detect: func() bool { return true }, create: func() ServiceManager { return &procdManager{} }},
				{name: "sysvinit", detect: func() bool { return true }, create: func() ServiceManager { return &sysvinitManager{} }},
			},
			wantErr: ErrScopeNotSupported,
		},
		{
			name: "the user manager is unreachable",
			detectors: []detectorEntry{
				{name: "procd", detect: func() bool { return true }, create: func() ServiceManager { return &procdManager{} }},
				systemdEntry(true, false),
			},
			wantErr: ErrUserManagerUnreachable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withDetectors(tt.detectors, func() {
				det, err := DetectForScope(ScopeUser)
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				if det.HostInitSystem != "procd" {
					t.Errorf("HostInitSystem = %q, want %q", det.HostInitSystem, "procd")
				}
				if det.Manager != nil {
					t.Errorf("expected no manager on error, got %T", det.Manager)
				}
			})
		})
	}
}

func TestDetectForScope_UserManagerUnreachableAborts(t *testing.T) {
	withDetectors([]detectorEntry{
		systemdEntry(true, false),
		{name: "crontab", detect: func() bool { return true }, create: func() ServiceManager { return &crontabManager{} }},
	}, func() {
		det, err := DetectForScope(ScopeUser)
		if !errors.Is(err, ErrUserManagerUnreachable) {
			t.Fatalf("error = %v, want ErrUserManagerUnreachable", err)
		}
		if det.Manager != nil {
			t.Errorf("expected no manager, got %T", det.Manager)
		}
		for _, want := range []string{"loginctl enable-linger", "machinectl shell", "systemd-run --user"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention remedy %q", err, want)
			}
		}
	})
}

func TestDetectForScope_UserManagerUnreachableNamesCurrentUser(t *testing.T) {
	old := currentUserFunc
	currentUserFunc = func() (*user.User, error) { return &user.User{Username: "alice"}, nil }
	defer func() { currentUserFunc = old }()

	withDetectors([]detectorEntry{systemdEntry(true, false)}, func() {
		_, err := DetectForScope(ScopeUser)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "loginctl enable-linger alice") {
			t.Errorf("error %q does not contain %q", err, "loginctl enable-linger alice")
		}
	})
}

// TestDetectForScope_SystemScopeIgnoresUserManager guards the system path: an
// unreachable user manager must not affect a system-scope install.
func TestDetectForScope_SystemScopeIgnoresUserManager(t *testing.T) {
	withDetectors([]detectorEntry{systemdEntry(true, false)}, func() {
		det, err := DetectForScope(ScopeSystem)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := det.Manager.(*systemdManager); !ok {
			t.Errorf("expected systemdManager, got %T", det.Manager)
		}
	})
}

// TestDetectForScope_SelectsUserCapableManagers covers the detectors enabled in
// this phase: launchd and crontab must now be selected in user scope rather than
// filtered out.
func TestDetectForScope_SelectsUserCapableManagers(t *testing.T) {
	tests := []struct {
		name   string
		detect detectorEntry
	}{
		{name: "launchd", detect: detectorEntry{name: "launchd", detect: func() bool { return true }, create: func() ServiceManager { return &launchdManager{} }}},
		{name: "crontab", detect: detectorEntry{name: "crontab", detect: func() bool { return true }, create: func() ServiceManager { return &crontabManager{} }}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withDetectors([]detectorEntry{tt.detect}, func() {
				det, err := DetectForScope(ScopeUser)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if det.Manager == nil || det.Manager.Name() != tt.name {
					t.Fatalf("selected manager = %v, want %s", det.Manager, tt.name)
				}
				if len(det.Skipped) != 0 {
					t.Errorf("Skipped = %v, want none", det.Skipped)
				}
			})
		})
	}
}

func TestDetectForScope_NoManagerSupportsScope(t *testing.T) {
	withDetectors([]detectorEntry{
		{name: "openrc", detect: func() bool { return true }, create: func() ServiceManager { return &openrcManager{} }},
		{name: "sysvinit", detect: func() bool { return true }, create: func() ServiceManager { return &sysvinitManager{} }},
	}, func() {
		_, err := DetectForScope(ScopeUser)
		if !errors.Is(err, ErrScopeNotSupported) {
			t.Fatalf("error = %v, want ErrScopeNotSupported", err)
		}
		for _, want := range []string{"openrc", "sysvinit"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not list skipped detector %q", err, want)
			}
		}
	})
}

// Enabling user scope for crontab means a user-scope install on a host running
// OpenRC or runit no longer fails — it lands on the crontab fallback, which
// cannot restart the watchdog. That downgrade has to be said out loud.
func TestDetectForScope_WarnsWhenSelectionSkippedTheHostInit(t *testing.T) {
	tests := []struct {
		name      string
		detectors []detectorEntry
		wantWarn  bool
	}{
		{
			name: "crontab selected only after openrc was skipped",
			detectors: []detectorEntry{
				{name: "openrc", detect: func() bool { return true }, create: func() ServiceManager { return &openrcManager{} }},
				{name: "crontab", detect: func() bool { return true }, create: func() ServiceManager { return &crontabManager{} }},
			},
			wantWarn: true,
		},
		{
			name:      "host init system serves the scope itself",
			detectors: []detectorEntry{systemdEntry(true, true)},
			wantWarn:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldLog := getDetectionLog()
			defer SetDetectionLogger(oldLog)
			var buf bytes.Buffer
			SetDetectionLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

			withDetectors(tt.detectors, func() {
				if _, err := DetectForScope(ScopeUser); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			})

			warned := strings.Contains(buf.String(), "not supervising the service")
			if warned != tt.wantWarn {
				t.Errorf("downgrade warning emitted = %v, want %v; log:\n%s", warned, tt.wantWarn, buf.String())
			}
			if tt.wantWarn && !strings.Contains(buf.String(), "openrc") {
				t.Errorf("warning does not name the skipped init system:\n%s", buf.String())
			}
		})
	}
}

func TestDetectForScope_NoneDetected(t *testing.T) {
	withDetectors([]detectorEntry{
		{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
		{name: "crontab", detect: func() bool { return false }, create: func() ServiceManager { return &crontabManager{} }},
	}, func() {
		det, err := DetectForScope(ScopeSystem)
		if err == nil {
			t.Fatal("expected an error when nothing is detected")
		}
		if errors.Is(err, ErrScopeNotSupported) {
			t.Errorf("error = %v, want a plain no-manager error", err)
		}
		if det.HostInitSystem != "" {
			t.Errorf("HostInitSystem = %q, want empty", det.HostInitSystem)
		}
	})
}

func TestNewServiceManagerByName_AllRegisteredNames(t *testing.T) {
	for _, d := range defaultDetectors() {
		t.Run(d.name, func(t *testing.T) {
			mgr, err := NewServiceManagerByName(d.name)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mgr == nil {
				t.Fatal("expected a manager, got nil")
			}
			if mgr.Name() != d.name {
				t.Errorf("Name() = %q, want %q", mgr.Name(), d.name)
			}
			if fmt.Sprintf("%T", mgr) != fmt.Sprintf("%T", d.create()) {
				t.Errorf("type = %T, want %T", mgr, d.create())
			}
		})
	}
}

func TestNewServiceManagerByName_UnknownName(t *testing.T) {
	mgr, err := NewServiceManagerByName("upstart")
	if err == nil {
		t.Fatal("expected an error for an unknown init system")
	}
	if mgr != nil {
		t.Errorf("expected nil manager, got %T", mgr)
	}
	if !strings.Contains(err.Error(), "upstart") {
		t.Errorf("error %q does not name the unknown init system", err)
	}
}

// TestNewServiceManagerByName_IgnoresDetectorSubstitution keeps construction
// independent of whatever detection the current session would perform.
func TestNewServiceManagerByName_IgnoresDetectorSubstitution(t *testing.T) {
	withDetectors([]detectorEntry{
		{name: "crontab", detect: func() bool { return true }, create: func() ServiceManager { return &crontabManager{} }},
	}, func() {
		mgr, err := NewServiceManagerByName("systemd")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := mgr.(*systemdManager); !ok {
			t.Errorf("expected systemdManager, got %T", mgr)
		}
	})
}
