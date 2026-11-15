//go:build !windows

package daemon

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

type mockFileInfo struct {
	name  string
	isDir bool
}

func (m mockFileInfo) Name() string       { return m.name }
func (m mockFileInfo) Size() int64        { return 0 }
func (m mockFileInfo) Mode() fs.FileMode  { return 0o755 }
func (m mockFileInfo) ModTime() time.Time { return time.Time{} }
func (m mockFileInfo) IsDir() bool        { return m.isDir }
func (m mockFileInfo) Sys() any           { return nil }

func newTestRunitManager(opts ...func(*runitManager)) *runitManager {
	m := &runitManager{
		executor:     newMockExecutor(),
		fs:           newMockFS(),
		statFunc:     func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		lookPathFunc: func(string) (string, error) { return "", errors.New("not found") },
		readFileFunc: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		symlinkFunc:  func(string, string) error { return nil },
		removeFunc:   func(string) error { return nil },
		readlinkFunc: func(string) (string, error) { return "", os.ErrNotExist },
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func TestRunitDetect(t *testing.T) {
	tests := []struct {
		name      string
		lookPaths map[string]bool
		statPaths map[string]bool
		want      bool
	}{
		{
			name:      "runsvdir in PATH and /var/service exists",
			lookPaths: map[string]bool{"runsvdir": true},
			statPaths: map[string]bool{"/var/service": true},
			want:      true,
		},
		{
			name:      "/sbin/runsvdir exists and /var/service exists",
			statPaths: map[string]bool{"/sbin/runsvdir": true, "/var/service": true},
			want:      true,
		},
		{
			name:      "/usr/bin/runsvdir exists and /var/service exists",
			statPaths: map[string]bool{"/usr/bin/runsvdir": true, "/var/service": true},
			want:      true,
		},
		{
			name: "not detected when runsvdir absent",
			want: false,
		},
		{
			name:      "not detected when runsvdir in PATH but /var/service absent",
			lookPaths: map[string]bool{"runsvdir": true},
			want:      false,
		},
		{
			name:      "not detected when /sbin/runsvdir exists but /var/service absent",
			statPaths: map[string]bool{"/sbin/runsvdir": true},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestRunitManager(func(m *runitManager) {
				m.lookPathFunc = func(name string) (string, error) {
					if tt.lookPaths[name] {
						return "/usr/bin/" + name, nil
					}
					return "", errors.New("not found")
				}
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						isDir := path == "/var/service"
						return mockFileInfo{name: path, isDir: isDir}, nil
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

func TestRunitDetectVarServiceNotDirectory(t *testing.T) {
	m := newTestRunitManager(func(m *runitManager) {
		m.lookPathFunc = func(name string) (string, error) {
			if name == "runsvdir" {
				return "/usr/bin/runsvdir", nil
			}
			return "", errors.New("not found")
		}
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/var/service" {
				return mockFileInfo{name: "service", isDir: false}, nil
			}
			return nil, os.ErrNotExist
		}
	})
	if m.detect() {
		t.Error("detect() = true, want false when /var/service is a file not a directory")
	}
}

func TestRunitCanRestartWatchdog(t *testing.T) {
	m := newTestRunitManager()
	if !m.CanRestartWatchdog() {
		t.Error("CanRestartWatchdog() = false, want true")
	}
}

func TestRunitName(t *testing.T) {
	m := newTestRunitManager()
	if got := m.Name(); got != "runit" {
		t.Errorf("Name() = %q, want %q", got, "runit")
	}
}

func TestRunitImplementsInterface(t *testing.T) {
	var _ ServiceManager = (*runitManager)(nil)
}

func TestRunitGetManifestPath(t *testing.T) {
	m := newTestRunitManager()
	m.serviceName = "myapp"
	want := "/etc/sv/myapp/run"
	if got := m.GetManifestPath(); got != want {
		t.Errorf("GetManifestPath() = %q, want %q", got, want)
	}
}

func TestGenerateRunitRunScript(t *testing.T) {
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
				"#!/bin/sh",
				"exec '/opt/myapp/myapp' 'run'",
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
				"exec '/usr/local/bin/myapp' 'run' '--config' '/etc/myapp.conf'",
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
				`export LOG_LEVEL='info'`,
				"exec '/opt/myapp/myapp' 'run'",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := GenerateRunitRunScript(tt.config, tt.execPath)
			if err != nil {
				t.Fatalf("GenerateRunitRunScript() error = %v", err)
			}
			for _, s := range tt.contains {
				if !strings.Contains(content, s) {
					t.Errorf("script missing %q\ngot:\n%s", s, content)
				}
			}
		})
	}
}

func TestRunitExecPath(t *testing.T) {
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
			m := newTestRunitManager()
			if got := m.execPath(tt.config); got != tt.want {
				t.Errorf("execPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunitInstall(t *testing.T) {
	tests := []struct {
		name       string
		config     ServiceConfig
		mkdirErr   error
		writeErr   error
		symlinkErr error
		wantErr    bool
		errSubstr  string
	}{
		{
			name: "successful install",
			config: ServiceConfig{
				Name:     "myapp",
				ExecPath: "/opt/myapp/myapp",
			},
		},
		{
			name: "mkdir failure",
			config: ServiceConfig{
				Name:     "myapp",
				ExecPath: "/opt/myapp/myapp",
			},
			mkdirErr:  errors.New("permission denied"),
			wantErr:   true,
			errSubstr: "creating service directory",
		},
		{
			name: "write failure",
			config: ServiceConfig{
				Name:     "myapp",
				ExecPath: "/opt/myapp/myapp",
			},
			writeErr:  errors.New("permission denied"),
			wantErr:   true,
			errSubstr: "writing run script",
		},
		{
			name: "symlink failure",
			config: ServiceConfig{
				Name:     "myapp",
				ExecPath: "/opt/myapp/myapp",
			},
			symlinkErr: errors.New("permission denied"),
			wantErr:    true,
			errSubstr:  "activating service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockFS := newMockFS()
			mockFS.writeErr = tt.writeErr
			mockFS.mkdirErr = tt.mkdirErr
			var symlinkCalled bool

			m := newTestRunitManager(func(m *runitManager) {
				m.fs = mockFS
				m.symlinkFunc = func(target, link string) error {
					symlinkCalled = true
					if tt.symlinkErr != nil {
						return tt.symlinkErr
					}
					return nil
				}
			})

			err := m.Install(tt.config)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Install() expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("Install() error = %v, want substring %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Install() unexpected error: %v", err)
			}

			if m.serviceName != tt.config.Name {
				t.Errorf("serviceName = %q, want %q", m.serviceName, tt.config.Name)
			}

			if !mockFS.dirs["/etc/sv/myapp"] {
				t.Error("service directory not created")
			}

			scriptPath := "/etc/sv/myapp/run"
			if _, ok := mockFS.files[scriptPath]; !ok {
				t.Error("run script not written")
			}

			content := string(mockFS.files[scriptPath])
			if !strings.Contains(content, "#!/bin/sh") {
				t.Error("run script missing shebang")
			}

			if !symlinkCalled {
				t.Error("symlink not created to activate service")
			}
		})
	}
}

func TestRunitUninstall(t *testing.T) {
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
			name:        "remove script failure",
			serviceName: "myapp",
			removeErr:   errors.New("permission denied"),
			wantErr:     true,
			errSubstr:   "removing run script",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockFS := newMockFS()
			mockFS.removeErr = tt.removeErr
			executor := newMockExecutor()
			var symlinkRemoved bool

			m := newTestRunitManager(func(m *runitManager) {
				m.fs = mockFS
				m.executor = executor
				m.removeFunc = func(path string) error {
					if path == "/var/service/myapp" {
						symlinkRemoved = true
					}
					return nil
				}
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

			if !executor.called("sv", "-w", "30", "stop", "myapp") {
				t.Error("service not stopped before uninstall")
			}

			if !symlinkRemoved {
				t.Error("active symlink not removed")
			}

			if !mockFS.removed["/etc/sv/myapp/run"] {
				t.Error("run script not removed")
			}

			if !mockFS.removed["/etc/sv/myapp"] {
				t.Error("service directory not removed")
			}
		})
	}
}

func TestRunitStart(t *testing.T) {
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
				executor.setResult("sv up myapp", nil, tt.execErr)
			}

			m := newTestRunitManager(func(m *runitManager) {
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

			if !executor.called("sv", "up", "myapp") {
				t.Error("sv up not called")
			}
		})
	}
}

func TestRunitStop(t *testing.T) {
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
				executor.setResult("sv -w 30 stop myapp", nil, tt.execErr)
			}

			m := newTestRunitManager(func(m *runitManager) {
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

			if !executor.called("sv", "-w", "30", "stop", "myapp") {
				t.Error("bounded sv stop not called")
			}
		})
	}
}

func TestRunitEnable(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		statPaths   map[string]bool
		symlinkErr  error
		wantErr     bool
		errSubstr   string
		wantSymlink bool
	}{
		{
			name:        "already enabled - symlink exists",
			serviceName: "myapp",
			statPaths:   map[string]bool{"/var/service/myapp": true},
		},
		{
			name:        "create symlink",
			serviceName: "myapp",
			statPaths:   map[string]bool{"/etc/sv/myapp": true},
			wantSymlink: true,
		},
		{
			name:        "service directory not found",
			serviceName: "myapp",
			statPaths:   map[string]bool{},
			wantErr:     true,
			errSubstr:   "service directory not found",
		},
		{
			name:      "no service name",
			wantErr:   true,
			errSubstr: "service name not set",
		},
		{
			name:        "symlink fails",
			serviceName: "myapp",
			statPaths:   map[string]bool{"/etc/sv/myapp": true},
			symlinkErr:  errors.New("permission denied"),
			wantErr:     true,
			errSubstr:   "enabling service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			symlinkCalled := false
			m := newTestRunitManager(func(m *runitManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return mockFileInfo{name: path}, nil
					}
					return nil, os.ErrNotExist
				}
				m.symlinkFunc = func(target, link string) error {
					symlinkCalled = true
					if tt.symlinkErr != nil {
						return tt.symlinkErr
					}
					if target != "/etc/sv/myapp" || link != "/var/service/myapp" {
						t.Errorf("symlink(%q, %q), want (/etc/sv/myapp, /var/service/myapp)", target, link)
					}
					return nil
				}
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
			if tt.wantSymlink && !symlinkCalled {
				t.Error("expected symlink to be created")
			}
			if !tt.wantSymlink && symlinkCalled {
				t.Error("unexpected symlink creation")
			}
		})
	}
}

func TestRunitDisable(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		statPaths   map[string]bool
		removeErr   error
		wantErr     bool
		errSubstr   string
		wantRemove  bool
	}{
		{
			name:        "already disabled - no symlink",
			serviceName: "myapp",
			statPaths:   map[string]bool{},
		},
		{
			name:        "remove symlink",
			serviceName: "myapp",
			statPaths:   map[string]bool{"/var/service/myapp": true},
			wantRemove:  true,
		},
		{
			name:      "no service name",
			wantErr:   true,
			errSubstr: "service name not set",
		},
		{
			name:        "remove fails",
			serviceName: "myapp",
			statPaths:   map[string]bool{"/var/service/myapp": true},
			removeErr:   errors.New("permission denied"),
			wantErr:     true,
			errSubstr:   "disabling service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			removeCalled := false
			m := newTestRunitManager(func(m *runitManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return mockFileInfo{name: path}, nil
					}
					return nil, os.ErrNotExist
				}
				m.removeFunc = func(path string) error {
					removeCalled = true
					if tt.removeErr != nil {
						return tt.removeErr
					}
					return nil
				}
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
			if tt.wantRemove && !removeCalled {
				t.Error("expected symlink to be removed")
			}
		})
	}
}

func TestRunitStatus(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		output      string
		execErr     error
		wantStatus  ServiceStatus
		wantErr     bool
		errSubstr   string
	}{
		{
			name:        "running",
			serviceName: "myapp",
			output:      "run: myapp: (pid 1234) 100s",
			wantStatus:  ServiceStatusRunning,
		},
		{
			name:        "down",
			serviceName: "myapp",
			output:      "down: myapp: 5s, normally up",
			wantStatus:  ServiceStatusStopped,
		},
		{
			name:        "unknown output",
			serviceName: "myapp",
			output:      "something unexpected",
			wantStatus:  ServiceStatusUnknown,
		},
		{
			name:        "executor error",
			serviceName: "myapp",
			execErr:     errors.New("exec failed"),
			wantStatus:  ServiceStatusUnknown,
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
			if tt.output != "" {
				executor.setResult("sv status myapp", []byte(tt.output), nil)
			}
			if tt.execErr != nil {
				executor.setResult("sv status myapp", nil, tt.execErr)
			}

			m := newTestRunitManager(func(m *runitManager) {
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

func TestRunitGenerateConfig(t *testing.T) {
	m := newTestRunitManager()
	config := ServiceConfig{
		Name:     "myapp",
		ExecPath: "/opt/myapp/myapp",
	}

	content, err := m.GenerateConfig(config)
	if err != nil {
		t.Fatalf("GenerateConfig() error = %v", err)
	}
	if !strings.Contains(content, "#!/bin/sh") {
		t.Error("GenerateConfig() missing shebang")
	}
	if m.serviceName != "myapp" {
		t.Errorf("serviceName = %q, want %q", m.serviceName, "myapp")
	}
}

func TestRunitWriteConfig(t *testing.T) {
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
			m := newTestRunitManager(func(m *runitManager) {
				m.fs = mockFS
			})
			m.serviceName = tt.serviceName

			err := m.WriteConfig("#!/bin/sh\nexec /opt/myapp/myapp run")
			if tt.wantErr {
				if err == nil {
					t.Fatal("WriteConfig() expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("WriteConfig() unexpected error: %v", err)
			}
			if _, ok := mockFS.files["/etc/sv/myapp/run"]; !ok {
				t.Error("config not written to expected path")
			}
		})
	}
}

func TestRunitServiceDir(t *testing.T) {
	m := newTestRunitManager()
	m.serviceName = "myapp"
	if got, want := m.serviceDir(), "/etc/sv/myapp"; got != want {
		t.Errorf("serviceDir() = %q, want %q", got, want)
	}
}

func TestRunitActiveSymlink(t *testing.T) {
	m := newTestRunitManager()
	m.serviceName = "myapp"
	if got, want := m.activeSymlink(), "/var/service/myapp"; got != want {
		t.Errorf("activeSymlink() = %q, want %q", got, want)
	}
}

func TestRunitDetect_LogsWhenDetected(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := newTestRunitManager(func(m *runitManager) {
		m.lookPathFunc = func(name string) (string, error) {
			if name == "runsvdir" {
				return "/usr/bin/runsvdir", nil
			}
			return "", errors.New("not found")
		}
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/var/service" {
				return mockFileInfo{name: "service", isDir: true}, nil
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
		"init detection: runit: checking runit detection",
		"init detection: runit: lookpath runsvdir -> /usr/bin/runsvdir",
		"init detection: runit: stat /var/service -> found",
		"init detection: runit: detected=true reason=runsvdir and /var/service directory found",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}
}

func TestRunitDetect_LogsWhenNotDetected(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := newTestRunitManager()

	result := m.detect()
	if result {
		t.Fatal("expected detect() to return false")
	}

	output := buf.String()
	expectedMessages := []string{
		"init detection: runit: checking runit detection",
		"init detection: runit: detected=false reason=runsvdir not found",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}

	if strings.Contains(output, "detected=true") {
		t.Errorf("should not contain detected=true when runit not detected")
	}
}

func TestRunitDetect_LogsNanokvmScenario(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := newTestRunitManager(func(m *runitManager) {
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

	result := m.detect()
	if result {
		t.Fatal("expected detect() to return false for nanokvm scenario")
	}

	output := buf.String()
	expectedMessages := []string{
		"init detection: runit: checking runit detection",
		"init detection: runit: lookpath runsvdir -> /usr/bin/runsvdir",
		"init detection: runit: stat /var/service ->",
		"init detection: runit: detected=false reason=/var/service directory not found",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}
}

func TestRunitSupportsCapabilities(t *testing.T) {
	mgr := &runitManager{}
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
