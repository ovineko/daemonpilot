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

func newTestS6Manager(opts ...func(*s6Manager)) *s6Manager {
	m := &s6Manager{
		scanDir:      "/var/service",
		serviceBase:  "/etc/s6/sv",
		executor:     newMockExecutor(),
		fs:           newMockFS(),
		statFunc:     func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		lookPathFunc: func(string) (string, error) { return "", errors.New("not found") },
		readFileFunc: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		readlinkFunc: func(string) (string, error) { return "", os.ErrNotExist },
		symlinkFunc:  func(string, string) error { return nil },
		removeFunc:   func(string) error { return nil },
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func TestS6Detect(t *testing.T) {
	tests := []struct {
		name      string
		lookPaths map[string]bool
		statPaths map[string]bool
		want      bool
	}{
		{
			name:      "s6-svscan in PATH",
			lookPaths: map[string]bool{"s6-svscan": true},
			want:      true,
		},
		{
			name:      "/usr/bin/s6-svscan exists",
			statPaths: map[string]bool{"/usr/bin/s6-svscan": true},
			want:      true,
		},
		{
			name:      "/bin/s6-svscan exists",
			statPaths: map[string]bool{"/bin/s6-svscan": true},
			want:      true,
		},
		{
			name: "not detected when s6-svscan absent",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestS6Manager(func(m *s6Manager) {
				m.lookPathFunc = func(name string) (string, error) {
					if tt.lookPaths[name] {
						return "/usr/bin/" + name, nil
					}
					return "", errors.New("not found")
				}
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

func TestS6DetectFalsePositives(t *testing.T) {
	tests := []struct {
		name        string
		lookPaths   map[string]bool
		statPaths   map[string]bool
		readlinkMap map[string]string
		want        bool
	}{
		{
			name:        "s6-svscan in PATH but PID 1 is systemd",
			lookPaths:   map[string]bool{"s6-svscan": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/usr/lib/systemd/systemd"},
			want:        false,
		},
		{
			name:        "/usr/bin/s6-svscan exists but PID 1 is runit",
			statPaths:   map[string]bool{"/usr/bin/s6-svscan": true},
			readlinkMap: map[string]string{"/proc/1/exe": "/usr/bin/runit"},
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestS6Manager(func(m *s6Manager) {
				m.lookPathFunc = func(name string) (string, error) {
					if tt.lookPaths[name] {
						return "/usr/bin/" + name, nil
					}
					return "", errors.New("not found")
				}
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
				m.readlinkFunc = func(path string) (string, error) {
					if target, ok := tt.readlinkMap[path]; ok {
						return target, nil
					}
					return "", os.ErrNotExist
				}
			})
			got := m.detect()
			if got != tt.want {
				t.Errorf("detect() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestS6CanRestartWatchdog(t *testing.T) {
	m := newTestS6Manager()
	if !m.CanRestartWatchdog() {
		t.Error("CanRestartWatchdog() = false, want true")
	}
}

func TestS6Name(t *testing.T) {
	m := newTestS6Manager()
	if got := m.Name(); got != "s6" {
		t.Errorf("Name() = %q, want %q", got, "s6")
	}
}

func TestS6ImplementsInterface(t *testing.T) {
	var _ ServiceManager = (*s6Manager)(nil)
}

func TestS6GetManifestPath(t *testing.T) {
	m := newTestS6Manager()
	m.serviceName = "myapp"
	want := "/etc/s6/sv/myapp/run"
	if got := m.GetManifestPath(); got != want {
		t.Errorf("GetManifestPath() = %q, want %q", got, want)
	}
}

func TestGenerateS6RunScript(t *testing.T) {
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
			content, err := GenerateS6RunScript(tt.config, tt.execPath)
			if err != nil {
				t.Fatalf("GenerateS6RunScript() error = %v", err)
			}
			for _, s := range tt.contains {
				if !strings.Contains(content, s) {
					t.Errorf("script missing %q\ngot:\n%s", s, content)
				}
			}
		})
	}
}

func TestS6ExecPath(t *testing.T) {
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
			m := newTestS6Manager()
			if got := m.execPath(tt.config); got != tt.want {
				t.Errorf("execPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestS6Install(t *testing.T) {
	t.Run("successful install without s6-rc", func(t *testing.T) {
		mockFS := newMockFS()
		var symlinkCalled bool
		executor := newMockExecutor()

		m := newTestS6Manager(func(m *s6Manager) {
			m.fs = mockFS
			m.executor = executor
			m.symlinkFunc = func(target, link string) error {
				symlinkCalled = true
				return nil
			}
		})

		err := m.Install(ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp/myapp"})
		if err != nil {
			t.Fatalf("Install() unexpected error: %v", err)
		}
		if m.serviceName != "myapp" {
			t.Errorf("serviceName = %q, want %q", m.serviceName, "myapp")
		}
		if !mockFS.dirs["/etc/s6/sv/myapp"] {
			t.Error("service directory not created")
		}
		if _, ok := mockFS.files["/etc/s6/sv/myapp/run"]; !ok {
			t.Error("run script not written")
		}
		content := string(mockFS.files["/etc/s6/sv/myapp/run"])
		if !strings.Contains(content, "#!/bin/sh") {
			t.Error("run script missing shebang")
		}
		if !symlinkCalled {
			t.Error("symlink not created to activate service")
		}
	})

	t.Run("successful install with s6-rc", func(t *testing.T) {
		mockFS := newMockFS()
		executor := newMockExecutor()

		m := newTestS6Manager(func(m *s6Manager) {
			m.fs = mockFS
			m.executor = executor
			m.lookPathFunc = func(name string) (string, error) {
				if name == "s6-rc-update" {
					return "/usr/bin/s6-rc-update", nil
				}
				return "", errors.New("not found")
			}
			m.symlinkFunc = func(string, string) error { return nil }
		})

		err := m.Install(ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp/myapp"})
		if err != nil {
			t.Fatalf("Install() unexpected error: %v", err)
		}
		if _, ok := mockFS.files["/etc/s6/sv/myapp/type"]; !ok {
			t.Error("type file not written")
		}
		typeContent := string(mockFS.files["/etc/s6/sv/myapp/type"])
		if typeContent != "longrun\n" {
			t.Errorf("type file content = %q, want %q", typeContent, "longrun\n")
		}
		if !executor.called("s6-rc-update", "/var/service") {
			t.Error("s6-rc-update not called with scanDir argument")
		}
	})

	t.Run("mkdir failure", func(t *testing.T) {
		mockFS := newMockFS()
		mockFS.mkdirErr = errors.New("permission denied")
		m := newTestS6Manager(func(m *s6Manager) { m.fs = mockFS })
		err := m.Install(ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp/myapp"})
		if err == nil || !strings.Contains(err.Error(), "creating service directory") {
			t.Errorf("Install() error = %v, want 'creating service directory'", err)
		}
	})

	t.Run("write failure", func(t *testing.T) {
		mockFS := newMockFS()
		mockFS.writeErr = errors.New("permission denied")
		m := newTestS6Manager(func(m *s6Manager) { m.fs = mockFS })
		err := m.Install(ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp/myapp"})
		if err == nil || !strings.Contains(err.Error(), "writing run script") {
			t.Errorf("Install() error = %v, want 'writing run script'", err)
		}
	})

	t.Run("symlink failure", func(t *testing.T) {
		mockFS := newMockFS()
		m := newTestS6Manager(func(m *s6Manager) {
			m.fs = mockFS
			m.symlinkFunc = func(string, string) error { return errors.New("permission denied") }
		})
		err := m.Install(ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp/myapp"})
		if err == nil || !strings.Contains(err.Error(), "activating service") {
			t.Errorf("Install() error = %v, want 'activating service'", err)
		}
	})

	t.Run("s6-rc-update failure is best-effort", func(t *testing.T) {
		mockFS := newMockFS()
		executor := newMockExecutor()
		executor.fallbackErr = errors.New("s6-rc-update failed")
		m := newTestS6Manager(func(m *s6Manager) {
			m.fs = mockFS
			m.executor = executor
			m.lookPathFunc = func(name string) (string, error) {
				if name == "s6-rc-update" {
					return "/usr/bin/s6-rc-update", nil
				}
				return "", errors.New("not found")
			}
			m.symlinkFunc = func(string, string) error { return nil }
		})
		err := m.Install(ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp/myapp"})
		if err != nil {
			t.Errorf("Install() error = %v, want nil (s6-rc-update is best-effort)", err)
		}
	})
}

func TestS6Uninstall(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		hasS6RC     bool
		removeErr   error
		wantErr     bool
		errSubstr   string
	}{
		{
			name:        "successful uninstall",
			serviceName: "myapp",
		},
		{
			name:        "successful uninstall with s6-rc",
			serviceName: "myapp",
			hasS6RC:     true,
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

			m := newTestS6Manager(func(m *s6Manager) {
				m.fs = mockFS
				m.executor = executor
				m.lookPathFunc = func(name string) (string, error) {
					if tt.hasS6RC && name == "s6-rc-update" {
						return "/usr/bin/s6-rc-update", nil
					}
					return "", errors.New("not found")
				}
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

			if !executor.called("s6-svc", "-wD", "-T", "30000", "-d", "/var/service/myapp") {
				t.Error("service not stopped before uninstall")
			}

			if !symlinkRemoved {
				t.Error("active symlink not removed")
			}

			if !mockFS.removed["/etc/s6/sv/myapp/run"] {
				t.Error("run script not removed")
			}

			if !mockFS.removed["/etc/s6/sv/myapp"] {
				t.Error("service directory not removed")
			}

			if tt.hasS6RC {
				if !mockFS.removed["/etc/s6/sv/myapp/type"] {
					t.Error("type file not removed")
				}
			}
		})
	}
}

func TestS6Start(t *testing.T) {
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
				executor.setResult("s6-svc -u /var/service/myapp", nil, tt.execErr)
			}

			m := newTestS6Manager(func(m *s6Manager) {
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

			if !executor.called("s6-svc", "-u", "/var/service/myapp") {
				t.Error("s6-svc -u not called")
			}
		})
	}
}

func TestS6Stop(t *testing.T) {
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
				executor.setResult("s6-svc -wD -T 30000 -d /var/service/myapp", nil, tt.execErr)
			}

			m := newTestS6Manager(func(m *s6Manager) {
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

			if !executor.called("s6-svc", "-wD", "-T", "30000", "-d", "/var/service/myapp") {
				t.Error("waiting s6-svc -wD -d not called")
			}
		})
	}
}

func TestS6Enable(t *testing.T) {
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
			statPaths:   map[string]bool{"/etc/s6/sv/myapp": true},
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
			statPaths:   map[string]bool{"/etc/s6/sv/myapp": true},
			symlinkErr:  errors.New("permission denied"),
			wantErr:     true,
			errSubstr:   "enabling service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			symlinkCalled := false
			m := newTestS6Manager(func(m *s6Manager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
				m.symlinkFunc = func(target, link string) error {
					symlinkCalled = true
					if tt.symlinkErr != nil {
						return tt.symlinkErr
					}
					if target != "/etc/s6/sv/myapp" || link != "/var/service/myapp" {
						t.Errorf("symlink(%q, %q), want (/etc/s6/sv/myapp, /var/service/myapp)", target, link)
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

func TestS6Disable(t *testing.T) {
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
			m := newTestS6Manager(func(m *s6Manager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return nil, nil
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

func TestS6Status(t *testing.T) {
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
			output:      "up (pid 1234) 100 seconds",
			wantStatus:  ServiceStatusRunning,
		},
		{
			name:        "down",
			serviceName: "myapp",
			output:      "down 5 seconds, normally up, ready 0 seconds",
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
				executor.setResult("s6-svstat /var/service/myapp", []byte(tt.output), nil)
			}
			if tt.execErr != nil {
				executor.setResult("s6-svstat /var/service/myapp", nil, tt.execErr)
			}

			m := newTestS6Manager(func(m *s6Manager) {
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

func TestS6GenerateConfig(t *testing.T) {
	m := newTestS6Manager()
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

func TestS6WriteConfig(t *testing.T) {
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
			m := newTestS6Manager(func(m *s6Manager) {
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
			if _, ok := mockFS.files["/etc/s6/sv/myapp/run"]; !ok {
				t.Error("config not written to expected path")
			}
		})
	}
}

func TestS6ReadInstalledConfig(t *testing.T) {
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
			fileContent: "#!/bin/sh\nexec /opt/myapp/myapp run",
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
			errSubstr:   "reading run script",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestS6Manager(func(m *s6Manager) {
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

func TestS6ServiceDir(t *testing.T) {
	m := newTestS6Manager()
	m.serviceName = "myapp"
	if got, want := m.serviceDir(), "/etc/s6/sv/myapp"; got != want {
		t.Errorf("serviceDir() = %q, want %q", got, want)
	}
}

func TestS6ActiveSymlink(t *testing.T) {
	m := newTestS6Manager()
	m.serviceName = "myapp"
	if got, want := m.activeSymlink(), "/var/service/myapp"; got != want {
		t.Errorf("activeSymlink() = %q, want %q", got, want)
	}
}

func TestS6HasS6RC(t *testing.T) {
	tests := []struct {
		name      string
		lookPaths map[string]bool
		want      bool
	}{
		{
			name:      "s6-rc-update available",
			lookPaths: map[string]bool{"s6-rc-update": true},
			want:      true,
		},
		{
			name: "s6-rc-update not available",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestS6Manager(func(m *s6Manager) {
				m.lookPathFunc = func(name string) (string, error) {
					if tt.lookPaths[name] {
						return "/usr/bin/" + name, nil
					}
					return "", errors.New("not found")
				}
			})
			if got := m.hasS6RC(); got != tt.want {
				t.Errorf("hasS6RC() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestS6Detect_LogsWhenDetected(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := newTestS6Manager(func(m *s6Manager) {
		m.lookPathFunc = func(name string) (string, error) {
			if name == "s6-svscan" {
				return "/usr/bin/s6-svscan", nil
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
		"init detection: s6: checking s6 detection",
		"init detection: s6: lookpath s6-svscan -> /usr/bin/s6-svscan",
		"init detection: s6: detected=true reason=s6-svscan found in PATH",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}
}

func TestS6Detect_LogsWhenNotDetected(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := newTestS6Manager()

	result := m.detect()
	if result {
		t.Fatal("expected detect() to return false")
	}

	output := buf.String()
	expectedMessages := []string{
		"init detection: s6: checking s6 detection",
		"init detection: s6: detected=false reason=s6-svscan not found",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}

	if strings.Contains(output, "detected=true") {
		t.Errorf("should not contain detected=true when s6 not detected")
	}
}

func TestS6SupportsCapabilities(t *testing.T) {
	mgr := &s6Manager{}
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
