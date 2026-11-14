package daemon

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func newTestProcdManager(opts ...func(*procdManager)) *procdManager {
	m := &procdManager{
		executor:     newMockExecutor(),
		fs:           newMockFS(),
		statFunc:     func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		lookPathFunc: func(string) (string, error) { return "", errors.New("not found") },
		readFileFunc: func(string) ([]byte, error) { return nil, os.ErrNotExist },
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func TestProcdDetect(t *testing.T) {
	tests := []struct {
		name      string
		statPaths map[string]bool
		want      bool
	}{
		{
			name:      "procd and procd.sh present",
			statPaths: map[string]bool{"/sbin/procd": true, "/lib/functions/procd.sh": true},
			want:      true,
		},
		{
			name:      "only procd binary, no procd.sh",
			statPaths: map[string]bool{"/sbin/procd": true},
			want:      false,
		},
		{
			name:      "only procd.sh, no procd binary",
			statPaths: map[string]bool{"/lib/functions/procd.sh": true},
			want:      false,
		},
		{
			name: "neither present",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestProcdManager(func(m *procdManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return nil, nil
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

func TestProcdCanRestartWatchdog(t *testing.T) {
	m := newTestProcdManager()
	if !m.CanRestartWatchdog() {
		t.Error("CanRestartWatchdog() = false, want true")
	}
}

func TestProcdName(t *testing.T) {
	m := newTestProcdManager()
	if got := m.Name(); got != "procd" {
		t.Errorf("Name() = %q, want %q", got, "procd")
	}
}

func TestProcdImplementsInterface(t *testing.T) {
	var _ ServiceManager = (*procdManager)(nil)
}

func TestProcdGetManifestPath(t *testing.T) {
	m := newTestProcdManager()
	m.serviceName = "myapp"
	want := "/etc/init.d/myapp"
	if got := m.GetManifestPath(); got != want {
		t.Errorf("GetManifestPath() = %q, want %q", got, want)
	}
}

func TestGenerateProcdInitScript(t *testing.T) {
	tests := []struct {
		name     string
		config   ServiceConfig
		execPath string
		contains []string
	}{
		{
			name: "basic script",
			config: ServiceConfig{
				Name:      "myapp",
				Arguments: []string{"run"},
			},
			execPath: "/opt/myapp/myapp",
			contains: []string{
				"#!/bin/sh /etc/rc.common",
				"USE_PROCD=1",
				"procd_open_instance",
				"procd_set_param command '/opt/myapp/myapp' 'run'",
				"procd_set_param respawn",
				"procd_close_instance",
				"start_service()",
				"stop_service()",
			},
		},
		{
			name: "with arguments",
			config: ServiceConfig{
				Name:      "myapp",
				Arguments: []string{"run", "--config", "/etc/myapp.conf"},
			},
			execPath: "/usr/local/bin/myapp",
			contains: []string{
				"procd_set_param command '/usr/local/bin/myapp' 'run' '--config' '/etc/myapp.conf'",
			},
		},
		{
			name: "with environment variables",
			config: ServiceConfig{
				Name:        "myapp",
				Arguments:   []string{"run"},
				Environment: map[string]string{"LOG_LEVEL": "info"},
			},
			execPath: "/opt/myapp/myapp",
			contains: []string{
				`procd_set_param env LOG_LEVEL='info'`,
				"procd_set_param command '/opt/myapp/myapp' 'run'",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := GenerateProcdInitScript(tt.config, tt.execPath)
			if err != nil {
				t.Fatalf("GenerateProcdInitScript() error = %v", err)
			}
			for _, s := range tt.contains {
				if !strings.Contains(content, s) {
					t.Errorf("script missing %q\ngot:\n%s", s, content)
				}
			}
		})
	}
}

func TestProcdExecPath(t *testing.T) {
	tests := []struct {
		name   string
		config ServiceConfig
		want   string
	}{
		{
			name:   "explicit exec path",
			config: ServiceConfig{Name: "myapp", ExecPath: "/usr/bin/myapp"},
			want:   "/usr/bin/myapp",
		},
		{
			name:   "explicit install dir",
			config: ServiceConfig{Name: "myapp", InstallDir: "/usr/local"},
			want:   "/usr/local/myapp",
		},
		{
			name:   "default install dir",
			config: ServiceConfig{Name: "myapp"},
			want:   "/opt/myapp/myapp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestProcdManager()
			if got := m.execPath(tt.config); got != tt.want {
				t.Errorf("execPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProcdInstall(t *testing.T) {
	t.Run("successful install", func(t *testing.T) {
		mockFS := newMockFS()
		executor := newMockExecutor()

		m := newTestProcdManager(func(m *procdManager) {
			m.fs = mockFS
			m.executor = executor
		})

		err := m.Install(ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp/myapp"})
		if err != nil {
			t.Fatalf("Install() unexpected error: %v", err)
		}
		if m.serviceName != "myapp" {
			t.Errorf("serviceName = %q, want %q", m.serviceName, "myapp")
		}
		if _, ok := mockFS.files["/etc/init.d/myapp"]; !ok {
			t.Error("init script not written")
		}
		content := string(mockFS.files["/etc/init.d/myapp"])
		if !strings.Contains(content, "#!/bin/sh /etc/rc.common") {
			t.Error("init script missing rc.common header")
		}
		if !executor.called("/etc/init.d/myapp", "enable") {
			t.Error("enable not called after install")
		}
	})

	t.Run("write failure", func(t *testing.T) {
		mockFS := newMockFS()
		mockFS.writeErr = errors.New("permission denied")
		m := newTestProcdManager(func(m *procdManager) { m.fs = mockFS })
		err := m.Install(ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp/myapp"})
		if err == nil || !strings.Contains(err.Error(), "writing init script") {
			t.Errorf("Install() error = %v, want 'writing init script'", err)
		}
	})

	t.Run("enable failure", func(t *testing.T) {
		mockFS := newMockFS()
		executor := newMockExecutor()
		executor.setResult("/etc/init.d/myapp enable", nil, errors.New("enable failed"))
		m := newTestProcdManager(func(m *procdManager) {
			m.fs = mockFS
			m.executor = executor
		})
		err := m.Install(ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp/myapp"})
		if err == nil || !strings.Contains(err.Error(), "enabling service") {
			t.Errorf("Install() error = %v, want 'enabling service'", err)
		}
	})
}

func TestProcdUninstall(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		removeErr   error
		wantErr     bool
		errSubstr   string
	}{
		{
			name:        "successful uninstall",
			serviceName: "myapp",
		},
		{
			name:      "no service name",
			wantErr:   true,
			errSubstr: "service name not set",
		},
		{
			name:        "remove failure",
			serviceName: "myapp",
			removeErr:   errors.New("permission denied"),
			wantErr:     true,
			errSubstr:   "removing init script",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockFS := newMockFS()
			mockFS.removeErr = tt.removeErr
			executor := newMockExecutor()

			m := newTestProcdManager(func(m *procdManager) {
				m.fs = mockFS
				m.executor = executor
			})
			m.serviceName = tt.serviceName

			err := m.Uninstall()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Uninstall() expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("Uninstall() error = %v, want substring %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Uninstall() unexpected error: %v", err)
			}

			if !executor.called("/etc/init.d/myapp", "stop") {
				t.Error("stop not called before uninstall")
			}
			if !executor.called("/etc/init.d/myapp", "disable") {
				t.Error("disable not called before uninstall")
			}
			if !mockFS.removed["/etc/init.d/myapp"] {
				t.Error("init script not removed")
			}
		})
	}
}

func TestProcdStart(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		execErr     error
		wantErr     bool
		errSubstr   string
	}{
		{
			name:        "successful start",
			serviceName: "myapp",
		},
		{
			name:      "no service name",
			wantErr:   true,
			errSubstr: "service name not set",
		},
		{
			name:        "executor error",
			serviceName: "myapp",
			execErr:     errors.New("exec failed"),
			wantErr:     true,
			errSubstr:   "starting service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := newMockExecutor()
			if tt.execErr != nil {
				executor.setResult("/etc/init.d/myapp start", nil, tt.execErr)
			}

			m := newTestProcdManager(func(m *procdManager) {
				m.executor = executor
			})
			m.serviceName = tt.serviceName

			err := m.Start()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Start() expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("Start() error = %v, want substring %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Start() unexpected error: %v", err)
			}
			if !executor.called("/etc/init.d/myapp", "start") {
				t.Error("init.d start not called")
			}
		})
	}
}

func TestProcdStop(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		execErr     error
		wantErr     bool
		errSubstr   string
	}{
		{
			name:        "successful stop",
			serviceName: "myapp",
		},
		{
			name:      "no service name",
			wantErr:   true,
			errSubstr: "service name not set",
		},
		{
			name:        "executor error",
			serviceName: "myapp",
			execErr:     errors.New("exec failed"),
			wantErr:     true,
			errSubstr:   "stopping service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := newMockExecutor()
			if tt.execErr != nil {
				executor.setResult("/etc/init.d/myapp stop", nil, tt.execErr)
			}

			m := newTestProcdManager(func(m *procdManager) {
				m.executor = executor
			})
			m.serviceName = tt.serviceName

			err := m.Stop()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Stop() expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("Stop() error = %v, want substring %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Stop() unexpected error: %v", err)
			}
			if !executor.called("/etc/init.d/myapp", "stop") {
				t.Error("init.d stop not called")
			}
		})
	}
}

func TestProcdEnable(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		execErr     error
		wantErr     bool
		errSubstr   string
	}{
		{
			name:        "successful enable",
			serviceName: "myapp",
		},
		{
			name:      "no service name",
			wantErr:   true,
			errSubstr: "service name not set",
		},
		{
			name:        "executor error",
			serviceName: "myapp",
			execErr:     errors.New("exec failed"),
			wantErr:     true,
			errSubstr:   "enabling service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := newMockExecutor()
			if tt.execErr != nil {
				executor.setResult("/etc/init.d/myapp enable", nil, tt.execErr)
			}

			m := newTestProcdManager(func(m *procdManager) {
				m.executor = executor
			})
			m.serviceName = tt.serviceName

			err := m.Enable()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Enable() expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("Enable() error = %v, want substring %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Enable() unexpected error: %v", err)
			}
			if !executor.called("/etc/init.d/myapp", "enable") {
				t.Error("init.d enable not called")
			}
		})
	}
}

func TestProcdDisable(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		execErr     error
		wantErr     bool
		errSubstr   string
	}{
		{
			name:        "successful disable",
			serviceName: "myapp",
		},
		{
			name:      "no service name",
			wantErr:   true,
			errSubstr: "service name not set",
		},
		{
			name:        "executor error",
			serviceName: "myapp",
			execErr:     errors.New("exec failed"),
			wantErr:     true,
			errSubstr:   "disabling service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := newMockExecutor()
			if tt.execErr != nil {
				executor.setResult("/etc/init.d/myapp disable", nil, tt.execErr)
			}

			m := newTestProcdManager(func(m *procdManager) {
				m.executor = executor
			})
			m.serviceName = tt.serviceName

			err := m.Disable()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Disable() expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("Disable() error = %v, want substring %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Disable() unexpected error: %v", err)
			}
			if !executor.called("/etc/init.d/myapp", "disable") {
				t.Error("init.d disable not called")
			}
		})
	}
}

func TestProcdStatus(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		infoOutput  string
		infoErr     error
		pgrepOutput string
		pgrepErr    error
		wantStatus  ServiceStatus
		wantErr     bool
		errSubstr   string
	}{
		{name: "empty service list after stop", serviceName: "myapp", infoOutput: `{}`, wantStatus: ServiceStatusStopped},
		{name: "false running flag", serviceName: "myapp", infoOutput: `{"myapp":{"instances":{"instance1":{"running":false}}}}`, wantStatus: ServiceStatusStopped},
		{name: "true running flag", serviceName: "myapp", infoOutput: `{"myapp":{"instances":{"instance1":{"running":true}}}}`, wantStatus: ServiceStatusRunning},
		{name: "other service running", serviceName: "myapp", infoOutput: `{"other":{"instances":{"instance1":{"running":true}}}}`, wantStatus: ServiceStatusStopped},

		{
			name:        "running via service info",
			serviceName: "myapp",
			infoOutput:  `"myapp" running`,
			wantStatus:  ServiceStatusRunning,
		},
		{
			name:        "inactive via service info",
			serviceName: "myapp",
			infoOutput:  `"myapp" inactive`,
			wantStatus:  ServiceStatusStopped,
		},
		{
			name:        "stopped via service info",
			serviceName: "myapp",
			infoOutput:  `"myapp" stopped`,
			wantStatus:  ServiceStatusStopped,
		},
		{
			name:        "unknown output from service info",
			serviceName: "myapp",
			infoOutput:  `something unexpected`,
			wantStatus:  ServiceStatusUnknown,
		},
		{
			name:        "service info fails, pgrep finds process",
			serviceName: "myapp",
			infoErr:     errors.New("command not found"),
			pgrepOutput: "1234",
			wantStatus:  ServiceStatusRunning,
		},
		{
			name:        "service info fails, pgrep finds nothing",
			serviceName: "myapp",
			infoErr:     errors.New("command not found"),
			pgrepErr:    errors.New("no process"),
			wantStatus:  ServiceStatusStopped,
		},
		{
			name:        "service info fails, pgrep returns empty",
			serviceName: "myapp",
			infoErr:     errors.New("command not found"),
			pgrepOutput: "",
			wantStatus:  ServiceStatusStopped,
		},
		{
			name:      "no service name",
			wantErr:   true,
			errSubstr: "service name not set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := newMockExecutor()
			if tt.infoOutput != "" {
				executor.setResult("service myapp info", []byte(tt.infoOutput), nil)
			}
			if tt.infoErr != nil {
				executor.setResult("service myapp info", nil, tt.infoErr)
			}
			if tt.pgrepOutput != "" {
				executor.setResult("pgrep -f myapp", []byte(tt.pgrepOutput), nil)
			}
			if tt.pgrepErr != nil {
				executor.setResult("pgrep -f myapp", nil, tt.pgrepErr)
			}

			m := newTestProcdManager(func(m *procdManager) {
				m.executor = executor
			})
			m.serviceName = tt.serviceName

			status, err := m.Status()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Status() expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("Status() error = %v, want substring %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Status() unexpected error: %v", err)
			}
			if status != tt.wantStatus {
				t.Errorf("Status() = %v, want %v", status, tt.wantStatus)
			}
		})
	}
}

func TestProcdGenerateConfig(t *testing.T) {
	m := newTestProcdManager()
	config := ServiceConfig{
		Name:     "myapp",
		ExecPath: "/opt/myapp/myapp",
	}

	content, err := m.GenerateConfig(config)
	if err != nil {
		t.Fatalf("GenerateConfig() error = %v", err)
	}
	if !strings.Contains(content, "#!/bin/sh /etc/rc.common") {
		t.Error("GenerateConfig() missing rc.common header")
	}
	if m.serviceName != "myapp" {
		t.Errorf("serviceName = %q, want %q", m.serviceName, "myapp")
	}
}

func TestProcdWriteConfig(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		wantErr     bool
	}{
		{
			name:        "successful write",
			serviceName: "myapp",
		},
		{
			name:    "no service name",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockFS := newMockFS()
			m := newTestProcdManager(func(m *procdManager) {
				m.fs = mockFS
			})
			m.serviceName = tt.serviceName

			err := m.WriteConfig("#!/bin/sh /etc/rc.common\n")
			if tt.wantErr {
				if err == nil {
					t.Fatal("WriteConfig() expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("WriteConfig() unexpected error: %v", err)
			}
			if _, ok := mockFS.files["/etc/init.d/myapp"]; !ok {
				t.Error("config not written to expected path")
			}
		})
	}
}

func TestProcdReadInstalledConfig(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		fileContent string
		readErr     error
		wantErr     bool
		errSubstr   string
	}{
		{
			name:        "successful read",
			serviceName: "myapp",
			fileContent: "#!/bin/sh /etc/rc.common\nUSE_PROCD=1\n",
		},
		{
			name:      "no service name",
			wantErr:   true,
			errSubstr: "service name not set",
		},
		{
			name:        "read error",
			serviceName: "myapp",
			readErr:     errors.New("file not found"),
			wantErr:     true,
			errSubstr:   "reading init script",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestProcdManager(func(m *procdManager) {
				m.readFileFunc = func(path string) ([]byte, error) {
					if tt.readErr != nil {
						return nil, tt.readErr
					}
					return []byte(tt.fileContent), nil
				}
			})
			m.serviceName = tt.serviceName

			content, err := m.ReadInstalledConfig()
			if tt.wantErr {
				if err == nil {
					t.Fatal("ReadInstalledConfig() expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("ReadInstalledConfig() error = %v, want substring %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadInstalledConfig() unexpected error: %v", err)
			}
			if content != tt.fileContent {
				t.Errorf("ReadInstalledConfig() = %q, want %q", content, tt.fileContent)
			}
		})
	}
}

func TestProcdInitScript(t *testing.T) {
	m := newTestProcdManager()
	m.serviceName = "myapp"
	if got, want := m.initScript(), "/etc/init.d/myapp"; got != want {
		t.Errorf("initScript() = %q, want %q", got, want)
	}
}

func TestProcdDetect_LogsWhenDetected(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := newTestProcdManager(func(m *procdManager) {
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/sbin/procd" || path == "/lib/functions/procd.sh" {
				return nil, nil
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
		"init detection: procd: checking procd detection",
		"init detection: procd: stat /sbin/procd -> found",
		"init detection: procd: stat /lib/functions/procd.sh -> found",
		"init detection: procd: detected=true reason=/sbin/procd and /lib/functions/procd.sh found",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}
}

func TestProcdDetect_LogsWhenNotDetected(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := newTestProcdManager()

	result := m.detect()
	if result {
		t.Fatal("expected detect() to return false")
	}

	output := buf.String()
	expectedMessages := []string{
		"init detection: procd: checking procd detection",
		"init detection: procd: detected=false reason=procd detection criteria not met",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}

	if strings.Contains(output, "detected=true") {
		t.Errorf("should not contain detected=true when procd not detected")
	}
}

func TestProcdSupportsCapabilities(t *testing.T) {
	mgr := &procdManager{}
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
