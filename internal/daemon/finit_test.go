package daemon

import (
	"bytes"
	"errors"
	"log/slog"
	"maps"
	"os"
	"strings"
	"testing"
)

func newTestFinitManager(opts ...func(*finitManager)) *finitManager {
	m := &finitManager{
		executor:     newMockExecutor(),
		fs:           newMockFS(),
		statFunc:     func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		lookPathFunc: func(string) (string, error) { return "", errors.New("not found") },
		readFileFunc: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		readlinkFunc: func(string) (string, error) { return "", os.ErrNotExist },
		symlinkFunc:  func(string, string) error { return nil },
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func TestFinitDetect(t *testing.T) {
	tests := []struct {
		name        string
		statPaths   map[string]bool
		lookPaths   map[string]bool
		readlinkMap map[string]string
		want        bool
	}{
		{
			name:        "/etc/finit.d directory exists and PID 1 is finit",
			statPaths:   map[string]bool{"/etc/finit.d": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/sbin/finit"},
			want:        true,
		},
		{
			name:      "/etc/finit.d exists but readlink fails",
			statPaths: map[string]bool{"/etc/finit.d": true},
			want:      false,
		},
		{
			name:        "/etc/finit.conf file exists and PID 1 is finit",
			statPaths:   map[string]bool{"/etc/finit.conf": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/sbin/finit"},
			want:        true,
		},
		{
			name:      "/etc/finit.conf exists but readlink fails",
			statPaths: map[string]bool{"/etc/finit.conf": true},
			want:      false,
		},
		{
			name:        "/sbin/finit exists and running as init",
			statPaths:   map[string]bool{"/sbin/finit": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/sbin/finit"},
			want:        true,
		},
		{
			name:      "/sbin/finit exists but not running as init",
			statPaths: map[string]bool{"/sbin/finit": true},
			want:      false,
		},
		{
			name: "nothing detected",
			want: false,
		},
		{
			name:        "/etc/finit.d takes priority over /etc/finit.conf",
			statPaths:   map[string]bool{"/etc/finit.d": true, "/etc/finit.conf": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/sbin/finit"},
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestFinitManager(func(m *finitManager) {
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

func TestFinitDetectFalsePositives(t *testing.T) {
	tests := []struct {
		name        string
		statPaths   map[string]bool
		lookPaths   map[string]bool
		readlinkMap map[string]string
		want        bool
	}{
		{
			name:        "upstart system with initctl but no finit indicators",
			statPaths:   map[string]bool{},
			lookPaths:   map[string]bool{"initctl": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/sbin/upstart"},
			want:        false,
		},
		{
			name:        "systemd system with /sbin/finit binary but systemd running as init",
			statPaths:   map[string]bool{"/sbin/finit": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/usr/lib/systemd/systemd"},
			want:        false,
		},
		{
			name:        "/sbin/finit exists but readlink fails",
			statPaths:   map[string]bool{"/sbin/finit": true},
			readlinkMap: map[string]string{},
			want:        false,
		},
		{
			name:        "/etc/finit.d exists but PID 1 is systemd",
			statPaths:   map[string]bool{"/etc/finit.d": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/usr/lib/systemd/systemd"},
			want:        false,
		},
		{
			name:        "/etc/finit.conf exists but PID 1 is runit",
			statPaths:   map[string]bool{"/etc/finit.conf": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/usr/bin/runit"},
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestFinitManager(func(m *finitManager) {
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

func TestFinitDetectInitctlDisambiguation(t *testing.T) {
	tests := []struct {
		name        string
		statPaths   map[string]bool
		lookPaths   map[string]bool
		readlinkMap map[string]string
		want        bool
	}{
		{
			name:        "finit.d exists regardless of initctl presence",
			statPaths:   map[string]bool{"/etc/finit.d": true},
			lookPaths:   map[string]bool{"initctl": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/sbin/finit"},
			want:        true,
		},
		{
			name:        "finit.conf exists regardless of initctl presence",
			statPaths:   map[string]bool{"/etc/finit.conf": true},
			lookPaths:   map[string]bool{"initctl": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/sbin/finit"},
			want:        true,
		},
		{
			name:      "only initctl in PATH is not sufficient for finit detection",
			lookPaths: map[string]bool{"initctl": true},
			want:      false,
		},
		{
			name:        "initctl present with /sbin/finit and finit running as init",
			statPaths:   map[string]bool{"/sbin/finit": true},
			lookPaths:   map[string]bool{"initctl": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/sbin/finit"},
			want:        true,
		},
		{
			name:        "initctl present with /sbin/finit but upstart running as init",
			statPaths:   map[string]bool{"/sbin/finit": true},
			lookPaths:   map[string]bool{"initctl": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/sbin/upstart"},
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestFinitManager(func(m *finitManager) {
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

func TestFinitDetectLogging(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	SetDetectionLogger(logger)
	defer SetDetectionLogger(nil)

	m := newTestFinitManager()
	m.detect()

	logOutput := buf.String()
	if logOutput == "" {
		t.Error("expected detection log output, got empty")
	}
}

func TestFinitCanRestartWatchdog(t *testing.T) {
	m := newTestFinitManager()
	if !m.CanRestartWatchdog() {
		t.Error("CanRestartWatchdog() = false, want true")
	}
}

func TestFinitName(t *testing.T) {
	m := newTestFinitManager()
	if got := m.Name(); got != "finit" {
		t.Errorf("Name() = %q, want %q", got, "finit")
	}
}

func TestFinitImplementsInterface(t *testing.T) {
	var _ ServiceManager = (*finitManager)(nil)
}

func TestFinitGetManifestPath(t *testing.T) {
	m := newTestFinitManager()
	m.serviceName = "myapp"
	want := "/etc/finit.d/available/myapp.conf"
	if got := m.GetManifestPath(); got != want {
		t.Errorf("GetManifestPath() = %q, want %q", got, want)
	}
}

func TestFinitExecPath(t *testing.T) {
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
			m := newTestFinitManager()
			if got := m.execPath(tt.config); got != tt.want {
				t.Errorf("execPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGenerateFinitServiceFile(t *testing.T) {
	tests := []struct {
		name     string
		config   ServiceConfig
		execPath string
		contains []string
	}{
		{
			name:     "basic service",
			config:   ServiceConfig{Name: "myapp"},
			execPath: "/opt/myapp/myapp",
			contains: []string{
				"# Service configuration for myapp",
				"service [234] /opt/myapp/myapp -- myapp service",
			},
		},
		{
			name: "with description",
			config: ServiceConfig{
				Name:        "myapp",
				Description: "My application daemon",
			},
			execPath: "/opt/myapp/myapp",
			contains: []string{
				"service [234] /opt/myapp/myapp -- My application daemon",
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
				"service [234] /usr/local/bin/myapp --config /etc/myapp.conf -- myapp service",
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
				"/opt/myapp/myapp -- myapp service",
				". /etc/finit.d/available/myapp.env&&exec",
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
				`service [234] /opt/myapp/myapp --name "hello world" --flag -- myapp service`,
			},
		},
		{
			name: "full config",
			config: ServiceConfig{
				Name:        "myapp",
				Description: "My full service",
				Arguments:   []string{"run", "--port", "8080"},
				Environment: map[string]string{"LOG_LEVEL": "info"},
			},
			execPath: "/opt/myapp/myapp",
			contains: []string{
				"# Service configuration for myapp",
				`service [234] name:myapp /bin/sh -c ". /etc/finit.d/available/myapp.env&&exec \"\$@\"" daemonpilot /opt/myapp/myapp run --port 8080 -- My full service`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := GenerateFinitServiceFile(tt.config, tt.execPath)
			if err != nil {
				t.Fatalf("GenerateFinitServiceFile() error = %v", err)
			}
			for _, s := range tt.contains {
				if !strings.Contains(content, s) {
					t.Errorf("service file missing %q\ngot:\n%s", s, content)
				}
			}
		})
	}
}

func TestGenerateFinitServiceFileNoEnv(t *testing.T) {
	config := ServiceConfig{Name: "myapp"}
	content, err := GenerateFinitServiceFile(config, "/opt/myapp/myapp")
	if err != nil {
		t.Fatalf("GenerateFinitServiceFile() error = %v", err)
	}
	if strings.Contains(content, "env ") {
		t.Errorf("service file should not contain env lines when no env vars set\ngot:\n%s", content)
	}
}

func TestGenerateFinitServiceFileInvalidEnvKey(t *testing.T) {
	config := ServiceConfig{
		Name:        "myapp",
		Environment: map[string]string{"INVALID-KEY": "value"},
	}
	_, err := GenerateFinitServiceFile(config, "/opt/myapp/myapp")
	if err == nil {
		t.Fatal("expected error for invalid env key, got nil")
	}
	if !strings.Contains(err.Error(), "invalid environment variable key") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFormatFinitCommand(t *testing.T) {
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
			got := formatFinitCommand(tt.execPath, tt.args)
			if got != tt.want {
				t.Errorf("formatFinitCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFinitGenerateConfig(t *testing.T) {
	m := newTestFinitManager()
	config := ServiceConfig{
		Name:      "myapp",
		ExecPath:  "/usr/bin/myapp",
		Arguments: []string{"--port", "8080"},
	}
	content, err := m.GenerateConfig(config)
	if err != nil {
		t.Fatalf("GenerateConfig() error = %v", err)
	}
	if !strings.Contains(content, "service [234] /usr/bin/myapp --port 8080 -- myapp service") {
		t.Errorf("unexpected config output:\n%s", content)
	}
	if m.serviceName != "myapp" {
		t.Errorf("serviceName not set, got %q", m.serviceName)
	}
}

func TestFinitGenerateConfigDefaultExecPath(t *testing.T) {
	m := newTestFinitManager()
	config := ServiceConfig{Name: "myapp"}
	content, err := m.GenerateConfig(config)
	if err != nil {
		t.Fatalf("GenerateConfig() error = %v", err)
	}
	if !strings.Contains(content, "/opt/myapp/myapp") {
		t.Errorf("expected default exec path /opt/myapp/myapp in output:\n%s", content)
	}
}

func TestFinitInstall(t *testing.T) {
	mockFS := newMockFS()
	m := newTestFinitManager(func(m *finitManager) {
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

	if !mockFS.dirs["/etc/finit.d/available"] {
		t.Error("Install() did not create /etc/finit.d/available directory")
	}

	content, ok := mockFS.files["/etc/finit.d/available/myapp.conf"]
	if !ok {
		t.Fatal("Install() did not write service file to /etc/finit.d/available/myapp.conf")
	}

	if !strings.Contains(string(content), "service [234] /opt/myapp/myapp") {
		t.Error("service file missing service directive")
	}
}

func TestFinitInstallWithFullConfig(t *testing.T) {
	mockFS := newMockFS()
	m := newTestFinitManager(func(m *finitManager) {
		m.fs = mockFS
	})

	config := ServiceConfig{
		Name:        "myapp",
		ExecPath:    "/opt/myapp/myapp",
		Description: "My application daemon",
		Arguments:   []string{"--port", "8080"},
		Environment: map[string]string{"LOG_LEVEL": "info"},
	}

	err := m.Install(config)
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	content := string(mockFS.files["/etc/finit.d/available/myapp.conf"])
	for _, want := range []string{
		`service [234] name:myapp /bin/sh -c ". /etc/finit.d/available/myapp.env&&exec \"\$@\"" daemonpilot /opt/myapp/myapp --port 8080 -- My application daemon`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("service file missing %q\ngot:\n%s", want, content)
		}
	}
}

func TestFinitInstallInvalidEnvKey(t *testing.T) {
	m := newTestFinitManager(func(m *finitManager) {
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

func TestFinitInstallMkdirError(t *testing.T) {
	mockFS := newMockFS()
	mockFS.mkdirErr = errors.New("permission denied")
	m := newTestFinitManager(func(m *finitManager) {
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
	if !strings.Contains(err.Error(), "creating /etc/finit.d/available directory") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFinitInstallWriteError(t *testing.T) {
	mockFS := newMockFS()
	mockFS.writeErr = errors.New("permission denied")
	m := newTestFinitManager(func(m *finitManager) {
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
	if !strings.Contains(err.Error(), "writing finit service file") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFinitUninstall(t *testing.T) {
	mockFS := newMockFS()
	m := newTestFinitManager(func(m *finitManager) {
		m.fs = mockFS
		m.serviceName = "myapp"
	})

	err := m.Uninstall()
	if err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}

	if !mockFS.removed["/etc/finit.d/enabled/myapp.conf"] {
		t.Error("Uninstall() did not remove enabled symlink")
	}

	if !mockFS.removed["/etc/finit.d/available/myapp.conf"] {
		t.Error("Uninstall() did not remove available service file")
	}
}

func TestFinitUninstallReloadsConfig(t *testing.T) {
	mockFS := newMockFS()
	exec := newMockExecutor()
	m := newTestFinitManager(func(m *finitManager) {
		m.fs = mockFS
		m.executor = exec
		m.serviceName = "myapp"
		m.lookPathFunc = func(cmd string) (string, error) {
			if cmd == "initctl" {
				return "/sbin/initctl", nil
			}
			return "", errors.New("not found")
		}
	})

	err := m.Uninstall()
	if err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}

	stopFound := false
	reloadFound := false
	for _, c := range exec.calls {
		if c.Name == "initctl" && len(c.Args) == 2 && c.Args[0] == "stop" && c.Args[1] == "myapp" {
			stopFound = true
		}
		if c.Name == "initctl" && len(c.Args) == 1 && c.Args[0] == "reload" {
			reloadFound = true
		}
	}
	if !stopFound {
		t.Error("Uninstall() did not call initctl stop myapp")
	}
	if !reloadFound {
		t.Error("Uninstall() did not call initctl reload")
	}
}

func TestFinitUninstallNoServiceName(t *testing.T) {
	m := newTestFinitManager()

	err := m.Uninstall()
	if err == nil {
		t.Fatal("Uninstall() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "service name not set") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFinitUninstallRemoveError(t *testing.T) {
	mockFS := newMockFS()
	mockFS.removeErr = errors.New("device busy")
	m := newTestFinitManager(func(m *finitManager) {
		m.fs = mockFS
		m.serviceName = "myapp"
	})

	err := m.Uninstall()
	if err == nil {
		t.Fatal("Uninstall() expected error, got nil")
	}
}

func TestFinitWriteConfig(t *testing.T) {
	mockFS := newMockFS()
	m := newTestFinitManager(func(m *finitManager) {
		m.fs = mockFS
		m.serviceName = "myapp"
	})

	err := m.WriteConfig("service [234] /opt/myapp/myapp -- myapp service\n")
	if err != nil {
		t.Fatalf("WriteConfig() error = %v", err)
	}

	if !mockFS.dirs["/etc/finit.d/available"] {
		t.Error("WriteConfig() did not create /etc/finit.d/available directory")
	}

	content, ok := mockFS.files["/etc/finit.d/available/myapp.conf"]
	if !ok {
		t.Fatal("WriteConfig() did not write to /etc/finit.d/available/myapp.conf")
	}
	if string(content) != "service [234] /opt/myapp/myapp -- myapp service\n" {
		t.Errorf("WriteConfig() wrote %q", string(content))
	}
}

func TestFinitWriteConfigNoServiceName(t *testing.T) {
	m := newTestFinitManager()

	err := m.WriteConfig("content")
	if err == nil {
		t.Fatal("WriteConfig() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "service name not set") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFinitReadInstalledConfig(t *testing.T) {
	m := newTestFinitManager(func(m *finitManager) {
		m.serviceName = "myapp"
		m.readFileFunc = func(path string) ([]byte, error) {
			if path == "/etc/finit.d/available/myapp.conf" {
				return []byte("service [234] /opt/myapp/myapp -- myapp service\n"), nil
			}
			return nil, os.ErrNotExist
		}
	})

	content, err := m.ReadInstalledConfig()
	if err != nil {
		t.Fatalf("ReadInstalledConfig() error = %v", err)
	}
	if content != "service [234] /opt/myapp/myapp -- myapp service\n" {
		t.Errorf("ReadInstalledConfig() = %q", content)
	}
}

func TestFinitReadInstalledConfigNoServiceName(t *testing.T) {
	m := newTestFinitManager()

	_, err := m.ReadInstalledConfig()
	if err == nil {
		t.Fatal("ReadInstalledConfig() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "service name not set") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFinitReadInstalledConfigNotFound(t *testing.T) {
	m := newTestFinitManager(func(m *finitManager) {
		m.serviceName = "myapp"
	})

	_, err := m.ReadInstalledConfig()
	if err == nil {
		t.Fatal("ReadInstalledConfig() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "reading finit service file") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFinitHasInitctlFinit(t *testing.T) {
	t.Run("initctl found", func(t *testing.T) {
		m := newTestFinitManager(func(m *finitManager) {
			m.lookPathFunc = func(name string) (string, error) {
				if name == "initctl" {
					return "/usr/bin/initctl", nil
				}
				return "", errors.New("not found")
			}
		})
		if !m.hasInitctlFinit() {
			t.Error("hasInitctlFinit() = false, want true")
		}
	})

	t.Run("initctl not found", func(t *testing.T) {
		m := newTestFinitManager()
		if m.hasInitctlFinit() {
			t.Error("hasInitctlFinit() = true, want false")
		}
	})
}

func TestFinitEnabledSymlink(t *testing.T) {
	m := newTestFinitManager()
	m.serviceName = "myapp"
	want := "/etc/finit.d/enabled/myapp.conf"
	if got := m.enabledSymlink(); got != want {
		t.Errorf("enabledSymlink() = %q, want %q", got, want)
	}
}

func TestFinitEnable(t *testing.T) {
	tests := []struct {
		name        string
		statPaths   map[string]bool
		lookPaths   map[string]bool
		symlinkErr  error
		execResults map[string]mockResult
		wantSymlink bool
		wantErr     bool
		errContains string
	}{
		{
			name:        "already enabled - symlink exists, recreates",
			statPaths:   map[string]bool{"/etc/finit.d/enabled/myapp.conf": true, "/etc/finit.d/available/myapp.conf": true},
			wantSymlink: true,
		},
		{
			name:        "create symlink with initctl reload",
			statPaths:   map[string]bool{"/etc/finit.d/available/myapp.conf": true},
			lookPaths:   map[string]bool{"initctl": true},
			wantSymlink: true,
		},
		{
			name:        "create symlink without initctl",
			statPaths:   map[string]bool{"/etc/finit.d/available/myapp.conf": true},
			wantSymlink: true,
		},
		{
			name:        "service file not found",
			wantErr:     true,
			errContains: "service file not found",
		},
		{
			name:        "symlink fails",
			statPaths:   map[string]bool{"/etc/finit.d/available/myapp.conf": true},
			symlinkErr:  errors.New("permission denied"),
			wantErr:     true,
			errContains: "enabling service",
		},
		{
			name:      "reload fails",
			statPaths: map[string]bool{"/etc/finit.d/available/myapp.conf": true},
			lookPaths: map[string]bool{"initctl": true},
			execResults: map[string]mockResult{
				"initctl reload": {Err: errors.New("reload failed")},
			},
			wantSymlink: true,
			wantErr:     true,
			errContains: "reloading finit configuration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			symlinkCalled := false
			mockFS := newMockFS()
			exec := newMockExecutor()
			maps.Copy(exec.results, tt.execResults)

			m := newTestFinitManager(func(m *finitManager) {
				m.serviceName = "myapp"
				m.fs = mockFS
				m.executor = exec
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
				m.symlinkFunc = func(target, link string) error {
					symlinkCalled = true
					if tt.symlinkErr != nil {
						return tt.symlinkErr
					}
					if target != "/etc/finit.d/available/myapp.conf" || link != "/etc/finit.d/enabled/myapp.conf" {
						t.Errorf("symlink(%q, %q), want (/etc/finit.d/available/myapp.conf, /etc/finit.d/enabled/myapp.conf)", target, link)
					}
					return nil
				}
			})

			err := m.Enable()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Enable() expected error, got nil")
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error = %q, want containing %q", err.Error(), tt.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("Enable() error = %v", err)
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

func TestFinitEnableNoServiceName(t *testing.T) {
	m := newTestFinitManager()
	err := m.Enable()
	if err == nil {
		t.Fatal("Enable() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "service name not set") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFinitDisable(t *testing.T) {
	tests := []struct {
		name        string
		statPaths   map[string]bool
		lookPaths   map[string]bool
		removeErr   error
		execResults map[string]mockResult
		wantRemove  bool
		wantErr     bool
		errContains string
	}{
		{
			name:       "already disabled - no symlink",
			statPaths:  map[string]bool{},
			wantRemove: true,
		},
		{
			name:       "remove symlink with initctl reload",
			statPaths:  map[string]bool{"/etc/finit.d/enabled/myapp.conf": true},
			lookPaths:  map[string]bool{"initctl": true},
			wantRemove: true,
		},
		{
			name:       "remove symlink without initctl",
			statPaths:  map[string]bool{"/etc/finit.d/enabled/myapp.conf": true},
			wantRemove: true,
		},
		{
			name:        "remove fails",
			statPaths:   map[string]bool{"/etc/finit.d/enabled/myapp.conf": true},
			removeErr:   errors.New("permission denied"),
			wantErr:     true,
			errContains: "disabling service",
		},
		{
			name:      "reload fails",
			statPaths: map[string]bool{"/etc/finit.d/enabled/myapp.conf": true},
			lookPaths: map[string]bool{"initctl": true},
			execResults: map[string]mockResult{
				"initctl reload": {Err: errors.New("reload failed")},
			},
			wantRemove:  true,
			wantErr:     true,
			errContains: "reloading finit configuration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			maps.Copy(exec.results, tt.execResults)

			fs := newMockFS()
			if tt.removeErr != nil {
				fs.removeErr = tt.removeErr
			}

			m := newTestFinitManager(func(m *finitManager) {
				m.serviceName = "myapp"
				m.executor = exec
				m.fs = fs
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
			})

			err := m.Disable()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Disable() expected error, got nil")
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error = %q, want containing %q", err.Error(), tt.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("Disable() error = %v", err)
			}
			removeCalled := fs.removed["/etc/finit.d/enabled/myapp.conf"]
			if tt.wantRemove && !removeCalled {
				t.Error("expected symlink to be removed")
			}
			if !tt.wantRemove && removeCalled {
				t.Error("unexpected symlink removal")
			}
		})
	}
}

func TestFinitDisableNoServiceName(t *testing.T) {
	m := newTestFinitManager()
	err := m.Disable()
	if err == nil {
		t.Fatal("Disable() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "service name not set") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFinitStart(t *testing.T) {
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
			lookPaths:   map[string]bool{"initctl": true},
		},
		{
			name:    "no service name",
			wantErr: "service name not set",
		},
		{
			name:        "initctl not found",
			serviceName: "myapp",
			wantErr:     "initctl command not found",
		},
		{
			name:        "command fails",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"initctl": true},
			execResult:  map[string]mockResult{"initctl start myapp": {Err: errors.New("permission denied")}},
			wantErr:     "starting service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			if tt.execResult != nil {
				exec.results = tt.execResult
			}
			m := newTestFinitManager(func(m *finitManager) {
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
			if !exec.called("initctl", "start", "myapp") {
				t.Error("Start() did not call initctl start myapp")
			}
		})
	}
}

func TestFinitStop(t *testing.T) {
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
			lookPaths:   map[string]bool{"initctl": true},
		},
		{
			name:    "no service name",
			wantErr: "service name not set",
		},
		{
			name:        "initctl not found",
			serviceName: "myapp",
			wantErr:     "initctl command not found",
		},
		{
			name:        "command fails",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"initctl": true},
			execResult:  map[string]mockResult{"initctl stop myapp": {Err: errors.New("permission denied")}},
			wantErr:     "stopping service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			if tt.execResult != nil {
				exec.results = tt.execResult
			}
			m := newTestFinitManager(func(m *finitManager) {
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
			if !exec.called("initctl", "stop", "myapp") {
				t.Error("Stop() did not call initctl stop myapp")
			}
		})
	}
}

func TestFinitStatus(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		lookPaths   map[string]bool
		execResult  map[string]mockResult
		wantStatus  ServiceStatus
		wantErr     string
	}{
		{
			name:        "running status",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"initctl": true},
			execResult:  map[string]mockResult{"initctl status myapp": {Output: []byte("myapp running\n")}},
			wantStatus:  ServiceStatusRunning,
		},
		{
			name:        "stopped status",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"initctl": true},
			execResult:  map[string]mockResult{"initctl status myapp": {Output: []byte("myapp stopped\n")}},
			wantStatus:  ServiceStatusStopped,
		},
		{
			name:        "unknown output",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"initctl": true},
			execResult:  map[string]mockResult{"initctl status myapp": {Output: []byte("something unexpected")}},
			wantStatus:  ServiceStatusUnknown,
		},
		{
			name:        "command error returns unknown",
			serviceName: "myapp",
			lookPaths:   map[string]bool{"initctl": true},
			execResult:  map[string]mockResult{"initctl status myapp": {Err: errors.New("exit code 1")}},
			wantStatus:  ServiceStatusUnknown,
		},
		{
			name:    "no service name",
			wantErr: "service name not set",
		},
		{
			name:        "initctl not found",
			serviceName: "myapp",
			wantErr:     "initctl command not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			if tt.execResult != nil {
				exec.results = tt.execResult
			}
			m := newTestFinitManager(func(m *finitManager) {
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

func TestFinitEnableMkdirError(t *testing.T) {
	mockFS := newMockFS()
	mockFS.mkdirErr = errors.New("permission denied")
	m := newTestFinitManager(func(m *finitManager) {
		m.serviceName = "myapp"
		m.fs = mockFS
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/finit.d/available/myapp.conf" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
	})

	err := m.Enable()
	if err == nil {
		t.Fatal("Enable() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "creating /etc/finit.d/enabled directory") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFinitSupportsCapabilities(t *testing.T) {
	mgr := &finitManager{}
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
