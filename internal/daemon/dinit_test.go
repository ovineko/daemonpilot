//go:build !windows

package daemon

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func newTestDinitManager(opts ...func(*dinitManager)) *dinitManager {
	m := &dinitManager{
		executor:     newMockExecutor(),
		fs:           newMockFS(),
		statFunc:     func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		lookPathFunc: func(string) (string, error) { return "", errors.New("not found") },
		readFileFunc: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		readlinkFunc: func(string) (string, error) { return "", os.ErrNotExist },
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func TestDinitDetect(t *testing.T) {
	tests := []struct {
		name        string
		statPaths   map[string]bool
		lookPaths   map[string]bool
		readlinkMap map[string]string
		want        bool
	}{
		{
			name:        "/etc/dinit.d directory exists and PID 1 is dinit",
			statPaths:   map[string]bool{"/etc/dinit.d": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/sbin/dinit"},
			want:        true,
		},
		{
			name:      "/etc/dinit.d exists but readlink fails",
			statPaths: map[string]bool{"/etc/dinit.d": true},
			want:      false,
		},
		{
			name:        "dinitctl in PATH and PID 1 is dinit",
			lookPaths:   map[string]bool{"dinitctl": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/sbin/dinit"},
			want:        true,
		},
		{
			name:      "dinitctl in PATH but readlink fails",
			lookPaths: map[string]bool{"dinitctl": true},
			want:      false,
		},
		{
			name:        "/sbin/dinit exists and running as init",
			statPaths:   map[string]bool{"/sbin/dinit": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/sbin/dinit"},
			want:        true,
		},
		{
			name:      "/sbin/dinit exists but not running as init",
			statPaths: map[string]bool{"/sbin/dinit": true},
			want:      false,
		},
		{
			name: "nothing detected",
			want: false,
		},
		{
			name:        "/etc/dinit.d takes priority over dinitctl",
			statPaths:   map[string]bool{"/etc/dinit.d": true},
			lookPaths:   map[string]bool{"dinitctl": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/sbin/dinit"},
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestDinitManager(func(m *dinitManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
				m.lookPathFunc = func(name string) (string, error) {
					if tt.lookPaths[name] {
						return "/usr/bin/" + name, nil
					}
					return "", errors.New("not found")
				}
				if tt.readlinkMap != nil {
					m.readlinkFunc = func(path string) (string, error) {
						if target, ok := tt.readlinkMap[path]; ok {
							return target, nil
						}
						return "", os.ErrNotExist
					}
				}
			})
			got := m.detect()
			if got != tt.want {
				t.Errorf("detect() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDinitDetectFalsePositives(t *testing.T) {
	tests := []struct {
		name        string
		statPaths   map[string]bool
		lookPaths   map[string]bool
		readlinkMap map[string]string
		want        bool
	}{
		{
			name:        "systemd system with /sbin/dinit binary but systemd running as init",
			statPaths:   map[string]bool{"/sbin/dinit": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/usr/lib/systemd/systemd"},
			want:        false,
		},
		{
			name:        "/sbin/dinit exists but readlink fails",
			statPaths:   map[string]bool{"/sbin/dinit": true},
			readlinkMap: map[string]string{},
			want:        false,
		},
		{
			name:        "/etc/dinit.d exists but PID 1 is systemd",
			statPaths:   map[string]bool{"/etc/dinit.d": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/usr/lib/systemd/systemd"},
			want:        false,
		},
		{
			name:        "dinitctl in PATH but PID 1 is runit",
			lookPaths:   map[string]bool{"dinitctl": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/usr/bin/runit"},
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestDinitManager(func(m *dinitManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
				m.lookPathFunc = func(name string) (string, error) {
					if tt.lookPaths != nil && tt.lookPaths[name] {
						return "/usr/bin/" + name, nil
					}
					return "", errors.New("not found")
				}
				if tt.readlinkMap != nil {
					m.readlinkFunc = func(path string) (string, error) {
						if target, ok := tt.readlinkMap[path]; ok {
							return target, nil
						}
						return "", os.ErrNotExist
					}
				}
			})
			got := m.detect()
			if got != tt.want {
				t.Errorf("detect() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDinitDetectLogging(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	SetDetectionLogger(logger)
	defer SetDetectionLogger(nil)

	m := newTestDinitManager()
	m.detect()

	logOutput := buf.String()
	if logOutput == "" {
		t.Error("expected detection log output, got empty")
	}
}

func TestDinitCanRestartWatchdog(t *testing.T) {
	m := newTestDinitManager()
	if !m.CanRestartWatchdog() {
		t.Error("CanRestartWatchdog() = false, want true")
	}
}

func TestDinitName(t *testing.T) {
	m := newTestDinitManager()
	if got := m.Name(); got != "dinit" {
		t.Errorf("Name() = %q, want %q", got, "dinit")
	}
}

func TestDinitImplementsInterface(t *testing.T) {
	var _ ServiceManager = (*dinitManager)(nil)
}

func TestDinitGetManifestPath(t *testing.T) {
	m := newTestDinitManager()
	m.serviceName = "myapp"
	want := "/etc/dinit.d/myapp"
	if got := m.GetManifestPath(); got != want {
		t.Errorf("GetManifestPath() = %q, want %q", got, want)
	}
}

func TestDinitExecPath(t *testing.T) {
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
			m := newTestDinitManager()
			if got := m.execPath(tt.config); got != tt.want {
				t.Errorf("execPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGenerateDinitServiceFile(t *testing.T) {
	tests := []struct {
		name     string
		config   ServiceConfig
		execPath string
		contains []string
	}{
		{
			name: "basic service",
			config: ServiceConfig{
				Name: "myapp",
			},
			execPath: "/opt/myapp/myapp",
			contains: []string{
				"type = process",
				"command = /opt/myapp/myapp",
				"logfile = /var/log/myapp.log",
				"restart = true",
				"smooth-recovery = true",
			},
		},
		{
			name: "with arguments",
			config: ServiceConfig{
				Name:      "myapp",
				Arguments: []string{"--config", "/etc/myapp.conf"},
			},
			execPath: "/usr/local/bin/myapp",
			contains: []string{
				"command = /usr/local/bin/myapp --config /etc/myapp.conf",
			},
		},
		{
			name: "with working directory",
			config: ServiceConfig{
				Name:       "myapp",
				WorkingDir: "/var/lib/myapp",
			},
			execPath: "/opt/myapp/myapp",
			contains: []string{
				"working-dir = /var/lib/myapp",
			},
		},
		{
			name: "with user",
			config: ServiceConfig{
				Name: "myapp",
				User: "appuser",
			},
			execPath: "/opt/myapp/myapp",
			contains: []string{
				"run-as = appuser",
			},
		},
		{
			name: "with environment variables",
			config: ServiceConfig{
				Name:        "myapp",
				Environment: map[string]string{"LOG_LEVEL": "info", "PORT": "8080"},
			},
			execPath: "/opt/myapp/myapp",
			contains: []string{
				"type = process",
				"command = /opt/myapp/myapp",
				"env-file = /etc/dinit.d/myapp.env",
			},
		},
		{
			name: "arguments with special characters",
			config: ServiceConfig{
				Name:      "myapp",
				Arguments: []string{"--name", "hello world", "--flag"},
			},
			execPath: "/opt/myapp/myapp",
			contains: []string{
				`command = /opt/myapp/myapp --name "hello world" --flag`,
			},
		},
		{
			name: "full config",
			config: ServiceConfig{
				Name:        "myapp",
				WorkingDir:  "/var/lib/myapp",
				User:        "appuser",
				Arguments:   []string{"run", "--port", "8080"},
				Environment: map[string]string{"LOG_LEVEL": "info"},
			},
			execPath: "/opt/myapp/myapp",
			contains: []string{
				"type = process",
				"command = /opt/myapp/myapp run --port 8080",
				"working-dir = /var/lib/myapp",
				"run-as = appuser",
				"logfile = /var/log/myapp.log",
				"restart = true",
				"smooth-recovery = true",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := GenerateDinitServiceFile(tt.config, tt.execPath)
			if err != nil {
				t.Fatalf("GenerateDinitServiceFile() error = %v", err)
			}
			for _, s := range tt.contains {
				if !strings.Contains(content, s) {
					t.Errorf("service file missing %q\ngot:\n%s", s, content)
				}
			}
		})
	}
}

func TestGenerateDinitServiceFileNoWorkingDir(t *testing.T) {
	config := ServiceConfig{Name: "myapp"}
	content, err := GenerateDinitServiceFile(config, "/opt/myapp/myapp")
	if err != nil {
		t.Fatalf("GenerateDinitServiceFile() error = %v", err)
	}
	if strings.Contains(content, "working-dir") {
		t.Errorf("service file should not contain working-dir when not set\ngot:\n%s", content)
	}
}

func TestGenerateDinitServiceFileNoUser(t *testing.T) {
	config := ServiceConfig{Name: "myapp"}
	content, err := GenerateDinitServiceFile(config, "/opt/myapp/myapp")
	if err != nil {
		t.Fatalf("GenerateDinitServiceFile() error = %v", err)
	}
	if strings.Contains(content, "run-as") {
		t.Errorf("service file should not contain run-as when not set\ngot:\n%s", content)
	}
}

func TestGenerateDinitServiceFileInvalidEnvKey(t *testing.T) {
	config := ServiceConfig{
		Name:        "myapp",
		Environment: map[string]string{"INVALID-KEY": "value"},
	}
	_, err := GenerateDinitServiceFile(config, "/opt/myapp/myapp")
	if err == nil {
		t.Fatal("expected error for invalid env key, got nil")
	}
	if !strings.Contains(err.Error(), "invalid environment variable key") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFormatDinitCommand(t *testing.T) {
	tests := []struct {
		name     string
		execPath string
		args     []string
		want     string
	}{
		{
			name:     "no args",
			execPath: "/opt/myapp/myapp",
			want:     "/opt/myapp/myapp",
		},
		{
			name:     "simple args",
			execPath: "/opt/myapp/myapp",
			args:     []string{"--port", "8080"},
			want:     "/opt/myapp/myapp --port 8080",
		},
		{
			name:     "args with spaces",
			execPath: "/opt/myapp/myapp",
			args:     []string{"--name", "hello world"},
			want:     `/opt/myapp/myapp --name "hello world"`,
		},
		{
			name:     "args with quotes",
			execPath: "/opt/myapp/myapp",
			args:     []string{"--msg", `say "hi"`},
			want:     `/opt/myapp/myapp --msg "say \"hi\""`,
		},
		{
			name:     "args with backslash",
			execPath: "/opt/myapp/myapp",
			args:     []string{"--path", `C:\Users`},
			want:     `/opt/myapp/myapp --path "C:\\Users"`,
		},
		{
			name:     "exec path with spaces",
			execPath: "/opt/my app/myapp",
			args:     []string{"--port", "8080"},
			want:     `"/opt/my app/myapp" --port 8080`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDinitCommand(tt.execPath, tt.args)
			if got != tt.want {
				t.Errorf("formatDinitCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatDinitEnvFile(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "single var",
			env:  map[string]string{"LOG_LEVEL": "info"},
			want: "LOG_LEVEL=info\n",
		},
		{
			name: "multiple vars sorted",
			env:  map[string]string{"PORT": "8080", "HOST": "0.0.0.0", "LOG_LEVEL": "debug"},
			want: "HOST=0.0.0.0\nLOG_LEVEL=debug\nPORT=8080\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatLineEnvironment(tt.env)
			if got != tt.want {
				t.Errorf("formatLineEnvironment() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDinitGenerateConfig(t *testing.T) {
	m := newTestDinitManager()
	config := ServiceConfig{
		Name:     "myapp",
		ExecPath: "/opt/myapp/myapp",
	}

	content, err := m.GenerateConfig(config)
	if err != nil {
		t.Fatalf("GenerateConfig() error = %v", err)
	}
	if !strings.Contains(content, "type = process") {
		t.Error("GenerateConfig() missing type = process")
	}
	if m.serviceName != "myapp" {
		t.Errorf("serviceName = %q, want %q", m.serviceName, "myapp")
	}
}

func TestDinitInstall(t *testing.T) {
	mockFS := newMockFS()
	m := newTestDinitManager(func(m *dinitManager) {
		m.fs = mockFS
	})

	config := ServiceConfig{
		Name:     "myapp",
		ExecPath: "/opt/myapp/myapp",
	}

	err := m.Install(config)
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	if m.serviceName != "myapp" {
		t.Errorf("serviceName = %q, want %q", m.serviceName, "myapp")
	}

	if !mockFS.dirs["/etc/dinit.d"] {
		t.Error("Install() did not create /etc/dinit.d directory")
	}

	content, ok := mockFS.files["/etc/dinit.d/myapp"]
	if !ok {
		t.Fatal("Install() did not write service file to /etc/dinit.d/myapp")
	}

	if !strings.Contains(string(content), "type = process") {
		t.Error("service file missing 'type = process'")
	}
	if !strings.Contains(string(content), "command = /opt/myapp/myapp") {
		t.Error("service file missing command")
	}
}

func TestDinitInstallWithFullConfig(t *testing.T) {
	mockFS := newMockFS()
	m := newTestDinitManager(func(m *dinitManager) {
		m.fs = mockFS
	})

	config := ServiceConfig{
		Name:       "myapp",
		ExecPath:   "/opt/myapp/myapp",
		WorkingDir: "/var/lib/myapp",
		User:       "appuser",
		Arguments:  []string{"--port", "8080"},
	}

	err := m.Install(config)
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	content := string(mockFS.files["/etc/dinit.d/myapp"])
	for _, want := range []string{
		"working-dir = /var/lib/myapp",
		"run-as = appuser",
		"command = /opt/myapp/myapp --port 8080",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("service file missing %q\ngot:\n%s", want, content)
		}
	}
}

func TestDinitInstallWithEnvVars(t *testing.T) {
	mockFS := newMockFS()
	m := newTestDinitManager(func(m *dinitManager) {
		m.fs = mockFS
	})

	config := ServiceConfig{
		Name:     "myapp",
		ExecPath: "/opt/myapp/myapp",
		Environment: map[string]string{
			"PORT":      "8080",
			"LOG_LEVEL": "info",
		},
	}

	err := m.Install(config)
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	serviceContent := string(mockFS.files["/etc/dinit.d/myapp"])
	if !strings.Contains(serviceContent, "env-file = /etc/dinit.d/myapp.env") {
		t.Errorf("service file missing env-file directive\ngot:\n%s", serviceContent)
	}

	envContent, ok := mockFS.files["/etc/dinit.d/myapp.env"]
	if !ok {
		t.Fatal("env file not written to /etc/dinit.d/myapp.env")
	}
	envStr := string(envContent)
	if !strings.Contains(envStr, "LOG_LEVEL=") {
		t.Errorf("env file missing LOG_LEVEL\ngot:\n%s", envStr)
	}
	if !strings.Contains(envStr, "PORT=") {
		t.Errorf("env file missing PORT\ngot:\n%s", envStr)
	}
}

func TestDinitInstallInvalidEnvKey(t *testing.T) {
	m := newTestDinitManager(func(m *dinitManager) {
		m.fs = newMockFS()
	})

	config := ServiceConfig{
		Name:        "myapp",
		ExecPath:    "/opt/myapp/myapp",
		Environment: map[string]string{"INVALID-KEY": "value"},
	}

	err := m.Install(config)
	if err == nil {
		t.Fatal("Install() expected error for invalid env key, got nil")
	}
}

func TestDinitInstallMkdirError(t *testing.T) {
	mockFS := newMockFS()
	mockFS.mkdirErr = errors.New("permission denied")
	m := newTestDinitManager(func(m *dinitManager) {
		m.fs = mockFS
	})

	config := ServiceConfig{
		Name:     "myapp",
		ExecPath: "/opt/myapp/myapp",
	}

	err := m.Install(config)
	if err == nil {
		t.Fatal("Install() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "creating /etc/dinit.d directory") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDinitInstallWriteError(t *testing.T) {
	mockFS := newMockFS()
	mockFS.writeErr = errors.New("permission denied")
	m := newTestDinitManager(func(m *dinitManager) {
		m.fs = mockFS
	})

	config := ServiceConfig{
		Name:     "myapp",
		ExecPath: "/opt/myapp/myapp",
	}

	err := m.Install(config)
	if err == nil {
		t.Fatal("Install() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "writing dinit service file") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDinitUninstall(t *testing.T) {
	mockFS := newMockFS()
	exec := newMockExecutor()
	m := newTestDinitManager(func(m *dinitManager) {
		m.fs = mockFS
		m.executor = exec
		m.serviceName = "myapp"
	})

	err := m.Uninstall()
	if err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}

	if !exec.called("dinitctl", "stop", "myapp") {
		t.Error("Uninstall() did not call dinitctl stop myapp")
	}

	if !exec.called("dinitctl", "disable", "myapp") {
		t.Error("Uninstall() did not call dinitctl disable myapp")
	}

	if !mockFS.removed["/etc/dinit.d/myapp"] {
		t.Error("Uninstall() did not remove /etc/dinit.d/myapp")
	}
}

func TestDinitUninstallNoServiceName(t *testing.T) {
	m := newTestDinitManager()

	err := m.Uninstall()
	if err == nil {
		t.Fatal("Uninstall() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "service name not set") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDinitUninstallRemoveError(t *testing.T) {
	mockFS := newMockFS()
	mockFS.removeErr = errors.New("device busy")
	m := newTestDinitManager(func(m *dinitManager) {
		m.fs = mockFS
		m.executor = newMockExecutor()
		m.serviceName = "myapp"
	})

	err := m.Uninstall()
	if err == nil {
		t.Fatal("Uninstall() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "removing dinit service file") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDinitWriteConfig(t *testing.T) {
	mockFS := newMockFS()
	m := newTestDinitManager(func(m *dinitManager) {
		m.fs = mockFS
		m.serviceName = "myapp"
	})

	err := m.WriteConfig("type = process\ncommand = /opt/myapp/myapp\n")
	if err != nil {
		t.Fatalf("WriteConfig() error = %v", err)
	}

	if !mockFS.dirs["/etc/dinit.d"] {
		t.Error("WriteConfig() did not create /etc/dinit.d directory")
	}

	content, ok := mockFS.files["/etc/dinit.d/myapp"]
	if !ok {
		t.Fatal("WriteConfig() did not write to /etc/dinit.d/myapp")
	}
	if string(content) != "type = process\ncommand = /opt/myapp/myapp\n" {
		t.Errorf("WriteConfig() wrote %q", string(content))
	}
}

func TestDinitWriteConfigNoServiceName(t *testing.T) {
	m := newTestDinitManager()

	err := m.WriteConfig("content")
	if err == nil {
		t.Fatal("WriteConfig() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "service name not set") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDinitReadInstalledConfig(t *testing.T) {
	m := newTestDinitManager(func(m *dinitManager) {
		m.serviceName = "myapp"
		m.readFileFunc = func(path string) ([]byte, error) {
			if path == "/etc/dinit.d/myapp" {
				return []byte("type = process\ncommand = /opt/myapp/myapp\n"), nil
			}
			return nil, os.ErrNotExist
		}
	})

	content, err := m.ReadInstalledConfig()
	if err != nil {
		t.Fatalf("ReadInstalledConfig() error = %v", err)
	}
	if content != "type = process\ncommand = /opt/myapp/myapp\n" {
		t.Errorf("ReadInstalledConfig() = %q", content)
	}
}

func TestDinitReadInstalledConfigNoServiceName(t *testing.T) {
	m := newTestDinitManager()

	_, err := m.ReadInstalledConfig()
	if err == nil {
		t.Fatal("ReadInstalledConfig() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "service name not set") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDinitReadInstalledConfigNotFound(t *testing.T) {
	m := newTestDinitManager(func(m *dinitManager) {
		m.serviceName = "myapp"
	})

	_, err := m.ReadInstalledConfig()
	if err == nil {
		t.Fatal("ReadInstalledConfig() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "reading dinit service file") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDinitHasDinitctl(t *testing.T) {
	tests := []struct {
		name      string
		lookPaths map[string]bool
		want      bool
	}{
		{
			name:      "dinitctl found",
			lookPaths: map[string]bool{"dinitctl": true},
			want:      true,
		},
		{
			name: "dinitctl not found",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestDinitManager(func(m *dinitManager) {
				m.lookPathFunc = func(name string) (string, error) {
					if tt.lookPaths[name] {
						return "/usr/bin/" + name, nil
					}
					return "", errors.New("not found")
				}
			})
			if got := m.hasDinitctl(); got != tt.want {
				t.Errorf("hasDinitctl() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDinitEnable(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		lookPaths   map[string]bool
		execResult  map[string]mockResult
		wantErr     string
	}{
		{
			name:        "successful enable",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"dinitctl": true},
		},
		{
			name:    "no service name",
			wantErr: "service name not set",
		},
		{
			name:        "dinitctl not found",
			serviceName: "myapp",
			wantErr:     "dinitctl command not found",
		},
		{
			name:        "command fails",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"dinitctl": true},
			execResult:  map[string]mockResult{"dinitctl enable myapp": {Err: errors.New("permission denied")}},
			wantErr:     "enabling service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			if tt.execResult != nil {
				exec.results = tt.execResult
			}
			m := newTestDinitManager(func(m *dinitManager) {
				m.serviceName = tt.serviceName
				m.executor = exec
				m.lookPathFunc = func(name string) (string, error) {
					if tt.lookPaths[name] {
						return "/usr/bin/" + name, nil
					}
					return "", errors.New("not found")
				}
			})

			err := m.Enable()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Enable() expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Enable() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Enable() error = %v", err)
			}
			if !exec.called("dinitctl", "enable", "myapp") {
				t.Error("Enable() did not call dinitctl enable myapp")
			}
		})
	}
}

func TestDinitStart(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		lookPaths   map[string]bool
		execResult  map[string]mockResult
		wantErr     string
	}{
		{
			name:        "successful start",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"dinitctl": true},
		},
		{
			name:    "no service name",
			wantErr: "service name not set",
		},
		{
			name:        "dinitctl not found",
			serviceName: "myapp",
			wantErr:     "dinitctl command not found",
		},
		{
			name:        "command fails",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"dinitctl": true},
			execResult:  map[string]mockResult{"dinitctl start myapp": {Err: errors.New("permission denied")}},
			wantErr:     "starting service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			if tt.execResult != nil {
				exec.results = tt.execResult
			}
			m := newTestDinitManager(func(m *dinitManager) {
				m.serviceName = tt.serviceName
				m.executor = exec
				m.lookPathFunc = func(name string) (string, error) {
					if tt.lookPaths[name] {
						return "/usr/bin/" + name, nil
					}
					return "", errors.New("not found")
				}
			})

			err := m.Start()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Start() expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Start() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			if !exec.called("dinitctl", "start", "myapp") {
				t.Error("Start() did not call dinitctl start myapp")
			}
		})
	}
}

func TestDinitStop(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		lookPaths   map[string]bool
		execResult  map[string]mockResult
		wantErr     string
	}{
		{
			name:        "successful stop",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"dinitctl": true},
		},
		{
			name:    "no service name",
			wantErr: "service name not set",
		},
		{
			name:        "dinitctl not found",
			serviceName: "myapp",
			wantErr:     "dinitctl command not found",
		},
		{
			name:        "command fails",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"dinitctl": true},
			execResult:  map[string]mockResult{"dinitctl stop myapp": {Err: errors.New("permission denied")}},
			wantErr:     "stopping service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			if tt.execResult != nil {
				exec.results = tt.execResult
			}
			m := newTestDinitManager(func(m *dinitManager) {
				m.serviceName = tt.serviceName
				m.executor = exec
				m.lookPathFunc = func(name string) (string, error) {
					if tt.lookPaths[name] {
						return "/usr/bin/" + name, nil
					}
					return "", errors.New("not found")
				}
			})

			err := m.Stop()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Stop() expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Stop() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Stop() error = %v", err)
			}
			if !exec.called("dinitctl", "stop", "myapp") {
				t.Error("Stop() did not call dinitctl stop myapp")
			}
		})
	}
}

func TestDinitStatus(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		lookPaths   map[string]bool
		execResult  map[string]mockResult
		wantStatus  ServiceStatus
		wantErr     string
	}{
		{
			name:        "started status",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"dinitctl": true},
			execResult:  map[string]mockResult{"dinitctl status myapp": {Output: []byte("Service: myapp\nState: STARTED\n")}},
			wantStatus:  ServiceStatusRunning,
		},
		{
			name:        "stopped status",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"dinitctl": true},
			execResult:  map[string]mockResult{"dinitctl status myapp": {Output: []byte("Service: myapp\nState: STOPPED\n")}},
			wantStatus:  ServiceStatusStopped,
		},
		{
			name:        "starting status",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"dinitctl": true},
			execResult:  map[string]mockResult{"dinitctl status myapp": {Output: []byte("Service: myapp\nState: STARTING\n")}},
			wantStatus:  ServiceStatusRunning,
		},
		{
			name:        "stopping status",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"dinitctl": true},
			execResult:  map[string]mockResult{"dinitctl status myapp": {Output: []byte("Service: myapp\nState: STOPPING\n")}},
			wantStatus:  ServiceStatusStopped,
		},
		{
			name:        "unknown output",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"dinitctl": true},
			execResult:  map[string]mockResult{"dinitctl status myapp": {Output: []byte("something unexpected")}},
			wantStatus:  ServiceStatusUnknown,
		},
		{
			name:        "command error returns unknown",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"dinitctl": true},
			execResult:  map[string]mockResult{"dinitctl status myapp": {Err: errors.New("exit code 1")}},
			wantStatus:  ServiceStatusUnknown,
		},
		{
			name:    "no service name",
			wantErr: "service name not set",
		},
		{
			name:        "dinitctl not found",
			serviceName: "myapp",
			wantErr:     "dinitctl command not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			if tt.execResult != nil {
				exec.results = tt.execResult
			}
			m := newTestDinitManager(func(m *dinitManager) {
				m.serviceName = tt.serviceName
				m.executor = exec
				m.lookPathFunc = func(name string) (string, error) {
					if tt.lookPaths[name] {
						return "/usr/bin/" + name, nil
					}
					return "", errors.New("not found")
				}
			})

			status, err := m.Status()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Status() expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Status() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Status() error = %v", err)
			}
			if status != tt.wantStatus {
				t.Errorf("Status() = %v, want %v", status, tt.wantStatus)
			}
		})
	}
}

func TestDinitDisable(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		lookPaths   map[string]bool
		execResult  map[string]mockResult
		wantErr     string
	}{
		{
			name:        "successful disable",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"dinitctl": true},
		},
		{
			name:    "no service name",
			wantErr: "service name not set",
		},
		{
			name:        "dinitctl not found",
			serviceName: "myapp",
			wantErr:     "dinitctl command not found",
		},
		{
			name:        "command fails",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"dinitctl": true},
			execResult:  map[string]mockResult{"dinitctl disable myapp": {Err: errors.New("permission denied")}},
			wantErr:     "disabling service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			if tt.execResult != nil {
				exec.results = tt.execResult
			}
			m := newTestDinitManager(func(m *dinitManager) {
				m.serviceName = tt.serviceName
				m.executor = exec
				m.lookPathFunc = func(name string) (string, error) {
					if tt.lookPaths[name] {
						return "/usr/bin/" + name, nil
					}
					return "", errors.New("not found")
				}
			})

			err := m.Disable()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Disable() expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Disable() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Disable() error = %v", err)
			}
			if !exec.called("dinitctl", "disable", "myapp") {
				t.Error("Disable() did not call dinitctl disable myapp")
			}
		})
	}
}

func TestDinitSupportsCapabilities(t *testing.T) {
	mgr := &dinitManager{}
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
