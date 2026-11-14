package daemon

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestGenerateOpenRCInitScript(t *testing.T) {
	config := ServiceConfig{
		Name:        "test-agent",
		Description: "Test Agent Service",
		ExecPath:    "/opt/test-agent/test-agent",
		InstallDir:  "/opt/test-agent",
		WorkingDir:  "/opt/test-agent",
		Arguments:   []string{"run"},
	}

	content, err := GenerateOpenRCInitScript(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedParts := []string{
		"#!/sbin/openrc-run",
		`name="test-agent"`,
		`description="Test Agent Service"`,
		`command="/opt/test-agent/test-agent"`,
		`command_args="run"`,
		"command_background=true",
		"pidfile=",
		`directory="/opt/test-agent"`,
		"output_log=",
		"error_log=",
		"depend()",
		"need net",
		"after firewall",
	}

	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("init script missing %q\ngot:\n%s", part, content)
		}
	}
}

func TestGenerateOpenRCInitScript_Shebang(t *testing.T) {
	config := ServiceConfig{
		Name:       "myapp",
		ExecPath:   "/usr/local/bin/myapp",
		WorkingDir: "/tmp",
	}

	content, err := GenerateOpenRCInitScript(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasPrefix(content, "#!/sbin/openrc-run") {
		t.Error("init script missing shebang line")
	}
}

func TestOpenrcInstall(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := newTestOpenrcManager(func(m *openrcManager) {
		m.executor = exec
		m.fs = fs
	})

	config := ServiceConfig{
		Name:        "test-svc",
		Description: "Test Service",
		ExecPath:    "/opt/test-svc/test-svc",
		InstallDir:  "/opt/test-svc",
		WorkingDir:  "/opt/test-svc",
	}

	err := mgr.Install(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	scriptPath := "/etc/init.d/test-svc"
	if _, ok := fs.files[scriptPath]; !ok {
		t.Error("init script was not written")
	}

	if mgr.serviceName != "test-svc" {
		t.Errorf("serviceName = %q, want %q", mgr.serviceName, "test-svc")
	}
}

func TestOpenrcInstall_WriteFileError(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	fs.writeErr = errors.New("permission denied")
	mgr := newTestOpenrcManager(func(m *openrcManager) {
		m.executor = exec
		m.fs = fs
	})

	err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "writing init script") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOpenrcUninstall(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := &openrcManager{serviceName: "test-svc", executor: exec, fs: fs}

	err := mgr.Uninstall()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !exec.called("rc-update", "del", "test-svc", "default") {
		t.Error("rc-update del was not called")
	}
	if !fs.removed["/etc/init.d/test-svc"] {
		t.Error("init script was not removed")
	}
}

func TestOpenrcUninstall_NoServiceName(t *testing.T) {
	mgr := &openrcManager{executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Uninstall()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenrcUninstall_RcUpdateDelError(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("rc-update del test-svc default", nil, errors.New("del failed"))
	fs := newMockFS()
	mgr := &openrcManager{serviceName: "test-svc", executor: exec, fs: fs}

	err := mgr.Uninstall()
	if err != nil {
		t.Fatalf("rc-update del error should be ignored, got: %v", err)
	}
	if !fs.removed["/etc/init.d/test-svc"] {
		t.Error("init script should have been removed")
	}
}

func TestOpenrcStart(t *testing.T) {
	exec := newMockExecutor()
	mgr := &openrcManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Start()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exec.called("rc-service", "test-svc", "start") {
		t.Error("rc-service start was not called")
	}
}

func TestOpenrcStart_NoServiceName(t *testing.T) {
	mgr := &openrcManager{executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Start()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenrcStart_Error(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("rc-service test-svc start", nil, errors.New("start failed"))
	mgr := &openrcManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Start()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "starting service") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOpenrcStop(t *testing.T) {
	exec := newMockExecutor()
	mgr := &openrcManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Stop()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exec.called("rc-service", "test-svc", "stop") {
		t.Error("rc-service stop was not called")
	}
}

func TestOpenrcStop_NoServiceName(t *testing.T) {
	mgr := &openrcManager{executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Stop()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenrcStop_Error(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("rc-service test-svc stop", nil, errors.New("stop failed"))
	mgr := &openrcManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Stop()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "stopping service") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOpenrcEnable(t *testing.T) {
	exec := newMockExecutor()
	mgr := &openrcManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Enable()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exec.called("rc-update", "add", "test-svc", "default") {
		t.Error("rc-update add was not called")
	}
}

func TestOpenrcEnable_NoServiceName(t *testing.T) {
	mgr := &openrcManager{executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Enable()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenrcEnable_Error(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("rc-update add test-svc default", nil, errors.New("enable failed"))
	mgr := &openrcManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Enable()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "enabling service") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOpenrcDisable(t *testing.T) {
	exec := newMockExecutor()
	mgr := &openrcManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Disable()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exec.called("rc-update", "del", "test-svc", "default") {
		t.Error("rc-update del was not called")
	}
}

func TestOpenrcDisable_NoServiceName(t *testing.T) {
	mgr := &openrcManager{executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Disable()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenrcDisable_Error(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("rc-update del test-svc default", nil, errors.New("disable failed"))
	mgr := &openrcManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Disable()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "disabling service") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOpenrcStatus(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		execErr    error
		wantStatus ServiceStatus
		wantErr    bool
	}{
		{"started", " * status: started\n", nil, ServiceStatusRunning, false},
		{"stopped", " * status: stopped\n", nil, ServiceStatusStopped, false},
		{"crashed", " * status: crashed\n", nil, ServiceStatusFailed, false},
		{"unknown output", "something else\n", nil, ServiceStatusUnknown, false},
		{"error with unknown", "something\n", errors.New("exit 1"), ServiceStatusUnknown, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			exec.setResult("rc-service test-svc status", []byte(tt.output), tt.execErr)
			mgr := &openrcManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

			status, err := mgr.Status()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != tt.wantStatus {
				t.Errorf("status = %v, want %v", status, tt.wantStatus)
			}
		})
	}
}

func TestOpenrcStatus_NoServiceName(t *testing.T) {
	mgr := &openrcManager{executor: newMockExecutor(), fs: newMockFS()}
	_, err := mgr.Status()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenrcInitScriptPath(t *testing.T) {
	mgr := &openrcManager{serviceName: "my-service"}
	got := mgr.initScriptPath()
	want := "/etc/init.d/my-service"
	if got != want {
		t.Errorf("initScriptPath() = %q, want %q", got, want)
	}
}

func TestOpenrcInstallUninstallRoundtrip(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := newTestOpenrcManager(func(m *openrcManager) {
		m.executor = exec
		m.fs = fs
	})

	config := ServiceConfig{
		Name:        "roundtrip-svc",
		Description: "Roundtrip Test",
		ExecPath:    "/opt/roundtrip-svc/roundtrip-svc",
		InstallDir:  "/opt/roundtrip-svc",
		WorkingDir:  "/opt/roundtrip-svc",
	}

	if err := mgr.Install(config); err != nil {
		t.Fatalf("install error: %v", err)
	}

	scriptPath := "/etc/init.d/roundtrip-svc"
	if _, ok := fs.files[scriptPath]; !ok {
		t.Fatal("init script not written after install")
	}

	if err := mgr.Uninstall(); err != nil {
		t.Fatalf("uninstall error: %v", err)
	}

	if !fs.removed[scriptPath] {
		t.Error("init script not removed after uninstall")
	}
}

func TestOpenrcGetManifestPath(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		want        string
	}{
		{"standard", "myapp", "/etc/init.d/myapp"},
		{"with-dash", "my-service", "/etc/init.d/my-service"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &openrcManager{serviceName: tt.serviceName}
			got := mgr.GetManifestPath()
			if got != tt.want {
				t.Errorf("GetManifestPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGenerateOpenRCInitScript_WithUserGroup(t *testing.T) {
	tests := []struct {
		name     string
		user     string
		group    string
		wantUser string
		notWant  string
	}{
		{
			name:     "user only",
			user:     "appuser",
			wantUser: `command_user="appuser"`,
		},
		{
			name:     "user and group",
			user:     "appuser",
			group:    "appgroup",
			wantUser: `command_user="appuser:appgroup"`,
		},
		{
			name:    "no user or group",
			notWant: "command_user=",
		},
		{
			name:    "group only without user",
			group:   "appgroup",
			notWant: "command_user=",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := ServiceConfig{
				Name:       "test-svc",
				ExecPath:   "/opt/test-svc/test-svc",
				WorkingDir: "/opt/test-svc",
				User:       tt.user,
				Group:      tt.group,
			}

			content, err := GenerateOpenRCInitScript(config)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantUser != "" && !strings.Contains(content, tt.wantUser) {
				t.Errorf("init script missing %q\ngot:\n%s", tt.wantUser, content)
			}
			if tt.notWant != "" && strings.Contains(content, tt.notWant) {
				t.Errorf("init script should not contain %q\ngot:\n%s", tt.notWant, content)
			}
		})
	}
}

func TestGenerateOpenRCInitScript_WithArguments(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantArgs string
	}{
		{
			name:     "no arguments",
			args:     []string{"run"},
			wantArgs: `command_args="run"`,
		},
		{
			name:     "single argument",
			args:     []string{"run", "--verbose"},
			wantArgs: `command_args="run --verbose"`,
		},
		{
			name:     "multiple arguments",
			args:     []string{"run", "--port", "8080", "--log-level", "debug"},
			wantArgs: `command_args="run --port 8080 --log-level debug"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := ServiceConfig{
				Name:       "test-svc",
				ExecPath:   "/opt/test-svc/test-svc",
				WorkingDir: "/opt/test-svc",
				Arguments:  tt.args,
			}

			content, err := GenerateOpenRCInitScript(config)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !strings.Contains(content, tt.wantArgs) {
				t.Errorf("init script missing %q\ngot:\n%s", tt.wantArgs, content)
			}
		})
	}
}

func TestGenerateOpenRCInitScript_WithEnvironment(t *testing.T) {
	config := ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
		Environment: map[string]string{
			"APP_ENV":  "production",
			"APP_PORT": "8080",
		},
	}

	content, err := GenerateOpenRCInitScript(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedParts := []string{
		`export APP_ENV='production'`,
		`export APP_PORT='8080'`,
	}
	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("init script missing %q\ngot:\n%s", part, content)
		}
	}
}

func TestGenerateOpenRCInitScript_WithOpenRCDirectives(t *testing.T) {
	config := ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
		OpenRCDirectives: map[string]string{
			"nice":                   "10",
			"start_stop_daemon_args": "--nicelevel 10",
		},
	}

	content, err := GenerateOpenRCInitScript(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedParts := []string{
		`nice="10"`,
		`start_stop_daemon_args="--nicelevel 10"`,
	}
	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("init script missing %q\ngot:\n%s", part, content)
		}
	}
}

func TestGenerateOpenRCInitScript_AllOptionalFields(t *testing.T) {
	config := ServiceConfig{
		Name:        "full-svc",
		Description: "Full Service",
		ExecPath:    "/opt/full-svc/full-svc",
		WorkingDir:  "/opt/full-svc",
		User:        "svcuser",
		Group:       "svcgroup",
		Arguments:   []string{"run", "--config", "/etc/full-svc.conf"},
		Environment: map[string]string{
			"LOG_LEVEL": "info",
		},
		OpenRCDirectives: map[string]string{
			"nice": "5",
		},
	}

	content, err := GenerateOpenRCInitScript(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedParts := []string{
		`command_args="run --config /etc/full-svc.conf"`,
		`command_user="svcuser:svcgroup"`,
		`export LOG_LEVEL='info'`,
		`nice="5"`,
	}
	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("init script missing %q\ngot:\n%s", part, content)
		}
	}
}

func TestGenerateOpenRCInitScript_NoOptionalFields(t *testing.T) {
	config := ServiceConfig{
		Name:        "basic-svc",
		Description: "Basic Service",
		ExecPath:    "/opt/basic-svc/basic-svc",
		WorkingDir:  "/opt/basic-svc",
		Arguments:   []string{"run"},
	}

	content, err := GenerateOpenRCInitScript(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(content, "command_user=") {
		t.Error("should not contain command_user when no user set")
	}
	if strings.Contains(content, "export ") {
		t.Error("should not contain export lines when no environment set")
	}
	if !strings.Contains(content, `command_args="run"`) {
		t.Errorf("should have default command_args\ngot:\n%s", content)
	}
}

func TestOpenrcGenerateConfig(t *testing.T) {
	mgr := newTestOpenrcManager()
	config := ServiceConfig{
		Name:        "test-svc",
		Description: "Test Service",
		ExecPath:    "/opt/test-svc/test-svc",
		InstallDir:  "/opt/test-svc",
		WorkingDir:  "/opt/test-svc",
	}

	content, err := mgr.GenerateConfig(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mgr.serviceName != "test-svc" {
		t.Errorf("serviceName = %q, want %q", mgr.serviceName, "test-svc")
	}

	expectedParts := []string{
		"#!/sbin/openrc-run",
		`name="test-svc"`,
		`command="/opt/test-svc/test-svc"`,
	}
	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("generated config missing %q", part)
		}
	}
}

func TestOpenrcWriteConfig(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		fs := newMockFS()
		mgr := &openrcManager{serviceName: "test-svc", executor: newMockExecutor(), fs: fs}

		err := mgr.WriteConfig("#!/sbin/openrc-run\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		scriptPath := "/etc/init.d/test-svc"
		if _, ok := fs.files[scriptPath]; !ok {
			t.Error("init script was not written")
		}
	})

	t.Run("no service name", func(t *testing.T) {
		mgr := &openrcManager{executor: newMockExecutor(), fs: newMockFS()}
		err := mgr.WriteConfig("content")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "service name not set") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("write error", func(t *testing.T) {
		fs := newMockFS()
		fs.writeErr = errors.New("permission denied")
		mgr := &openrcManager{serviceName: "test-svc", executor: newMockExecutor(), fs: fs}

		err := mgr.WriteConfig("content")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "writing init script") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestOpenRCCanRestartWatchdog_SupervisorDetection(t *testing.T) {
	tests := []struct {
		name      string
		lookupErr error
		want      bool
	}{
		{name: "available even when help requires service context", want: true},
		{name: "missing executable", lookupErr: os.ErrNotExist},
		{name: "not executable", lookupErr: os.ErrPermission},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := newMockExecutor()
			executor.fallbackErr = errors.New("RC_SVCNAME environment variable is not set")
			files := newMockFS()
			mgr := newTestOpenrcManager(func(m *openrcManager) {
				m.executor = executor
				m.fs = files
				m.lookPathFunc = func(name string) (string, error) {
					if name != "supervise-daemon" {
						t.Fatalf("unexpected executable lookup: %s", name)
					}
					return "/sbin/supervise-daemon", tt.lookupErr
				}
			})
			if got := mgr.CanRestartWatchdog(); got != tt.want {
				t.Fatalf("CanRestartWatchdog() = %v, want %v", got, tt.want)
			}
			cfg := ServiceConfig{Name: "test-svc", ExecPath: "/opt/test-svc", Arguments: []string{"run"}}
			content, err := mgr.GenerateConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := mgr.Install(cfg); err != nil {
				t.Fatal(err)
			}
			if string(files.files[mgr.initScriptPath()]) != content {
				t.Fatal("install and config generation disagree")
			}
			if strings.Contains(content, "supervisor=supervise-daemon") != tt.want ||
				strings.Contains(content, "command_background=true") == tt.want {
				t.Fatalf("script does not match supervision capability: %s", content)
			}
			if executor.called("supervise-daemon", "--help") {
				t.Fatal("capability detection must not run the context-dependent help command")
			}
		})
	}
}

func newTestOpenrcManager(opts ...func(*openrcManager)) *openrcManager {
	m := &openrcManager{
		executor:     newMockExecutor(),
		fs:           newMockFS(),
		lookPathFunc: func(string) (string, error) { return "", errors.New("not found") },
		statFunc:     func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func TestOpenrcDetect(t *testing.T) {
	tests := []struct {
		name      string
		lookPaths map[string]bool
		statPaths map[string]bool
		want      bool
	}{
		{
			name:      "rc-service in PATH",
			lookPaths: map[string]bool{"rc-service": true},
			want:      true,
		},
		{
			name:      "/sbin/openrc exists",
			statPaths: map[string]bool{"/sbin/openrc": true},
			want:      true,
		},
		{
			name:      "both rc-service and /sbin/openrc present",
			lookPaths: map[string]bool{"rc-service": true},
			statPaths: map[string]bool{"/sbin/openrc": true},
			want:      true,
		},
		{
			name: "neither rc-service nor /sbin/openrc present",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestOpenrcManager(func(m *openrcManager) {
				m.lookPathFunc = func(name string) (string, error) {
					if tt.lookPaths[name] {
						return "/usr/bin/" + name, nil
					}
					return "", errors.New("not found")
				}
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return mockFileInfo{name: path, isDir: false}, nil
					}
					return nil, os.ErrNotExist
				}
			})
			got := m.detect()
			if got != tt.want {
				t.Errorf("detect() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenrcDetect_LogsWhenDetected(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := newTestOpenrcManager(func(m *openrcManager) {
		m.lookPathFunc = func(name string) (string, error) {
			if name == "rc-service" {
				return "/usr/bin/rc-service", nil
			}
			return "", errors.New("not found")
		}
	})

	result := m.detect()
	if !result {
		t.Fatal("expected detect() to return true")
	}

	output := buf.String()
	expectedMessages := []string{
		"init detection: openrc: checking OpenRC detection",
		"init detection: openrc: lookpath rc-service -> /usr/bin/rc-service",
		"init detection: openrc: detected=true reason=rc-service found in PATH",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}
}

func TestOpenrcDetect_LogsWhenDetectedViaStat(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := newTestOpenrcManager(func(m *openrcManager) {
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/sbin/openrc" {
				return mockFileInfo{name: "openrc", isDir: false}, nil
			}
			return nil, os.ErrNotExist
		}
	})

	result := m.detect()
	if !result {
		t.Fatal("expected detect() to return true")
	}

	output := buf.String()
	expectedMessages := []string{
		"init detection: openrc: checking OpenRC detection",
		"init detection: openrc: lookpath rc-service ->",
		"init detection: openrc: stat /sbin/openrc -> found",
		"init detection: openrc: detected=true reason=/sbin/openrc found",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}
}

func TestOpenrcDetect_LogsWhenNotDetected(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := newTestOpenrcManager()

	result := m.detect()
	if result {
		t.Fatal("expected detect() to return false")
	}

	output := buf.String()
	expectedMessages := []string{
		"init detection: openrc: checking OpenRC detection",
		"init detection: openrc: lookpath rc-service ->",
		"init detection: openrc: stat /sbin/openrc ->",
		"init detection: openrc: detected=false reason=rc-service and /sbin/openrc not found",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}

	if strings.Contains(output, "detected=true") {
		t.Errorf("should not contain detected=true when openrc not detected")
	}
}

func TestOpenRCSupportsCapabilities(t *testing.T) {
	mgr := &openrcManager{}
	if !mgr.SupportsEnable() {
		t.Error("SupportsEnable() = false, want true")
	}
	if !mgr.SupportsStart() {
		t.Error("SupportsStart() = false, want true")
	}
	if !mgr.SupportsStatus() {
		t.Error("SupportsStatus() = false, want true")
	}
}
