//go:build !windows

package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func newTestSysvinitManager(opts ...func(*sysvinitManager)) *sysvinitManager {
	m := &sysvinitManager{
		executor:     newMockExecutor(),
		fs:           newMockFS(),
		statFunc:     func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		lookPathFunc: func(string) (string, error) { return "", errors.New("not found") },
		readlinkFunc: func(string) (string, error) { return "", os.ErrNotExist },
		readFileFunc: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		renameFunc:   func(string, string) error { return errors.New("rename not configured") },
		symlinkFunc:  func(string, string) error { return errors.New("symlink not configured") },
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func TestSysvinitDetect(t *testing.T) {
	tests := []struct {
		name      string
		statPaths map[string]bool
		readlinks map[string]string
		want      bool
	}{
		{
			name:      "detected via /etc/inittab",
			statPaths: map[string]bool{"/etc/inittab": true},
			want:      true,
		},
		{
			name:      "detected via /sbin/init and /proc/1/exe pointing to init",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/proc/1/exe": "/usr/sbin/init"},
			want:      true,
		},
		{
			name:      "detected when /sbin/init is not a symlink",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/proc/1/exe": "/sbin/init"},
			want:      true,
		},
		{
			name:      "not detected when nothing present",
			statPaths: map[string]bool{},
			want:      false,
		},
		{
			name:      "not detected when /sbin/init is symlink to systemd",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/sbin/init": "/lib/systemd/systemd", "/proc/1/exe": "/lib/systemd/systemd"},
			want:      false,
		},
		{
			name:      "not detected when /sbin/init is symlink to busybox",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/sbin/init": "/bin/busybox", "/proc/1/exe": "/bin/busybox"},
			want:      false,
		},
		{
			name:      "not detected when /sbin/init is symlink to runit",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/sbin/init": "/usr/bin/runit-init", "/proc/1/exe": "/usr/bin/runit-init"},
			want:      false,
		},
		{
			name:      "not detected when /proc/1/exe points to systemd",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/proc/1/exe": "/lib/systemd/systemd"},
			want:      false,
		},
		{
			name:      "not detected when /proc/1/exe points to busybox",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      false,
		},
		{
			name:      "not detected when /proc/1/exe points to runit",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/proc/1/exe": "/sbin/runit"},
			want:      false,
		},
		{
			name:      "not detected when /proc/1/exe unreadable and no inittab",
			statPaths: map[string]bool{"/sbin/init": true},
			want:      false,
		},
		{
			name:      "nanokvm scenario - /etc/inittab present, /usr/sbin/init",
			statPaths: map[string]bool{"/etc/inittab": true},
			readlinks: map[string]string{"/proc/1/exe": "/usr/sbin/init"},
			want:      true,
		},
		{
			name:      "nanokvm scenario - /sbin/init exists, /proc/1/exe -> /usr/sbin/init",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/proc/1/exe": "/usr/sbin/init"},
			want:      true,
		},
		{
			name:      "case insensitive exclusion - Systemd",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/sbin/init": "/lib/Systemd/Systemd"},
			want:      false,
		},
		{
			name:      "not detected when /proc/1/exe points to s6",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/proc/1/exe": "/usr/bin/s6-svscan"},
			want:      false,
		},
		{
			name:      "not detected when /sbin/init is symlink to s6",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/sbin/init": "/usr/bin/s6-linux-init", "/proc/1/exe": "/usr/bin/s6-svscan"},
			want:      false,
		},
		{
			name:      "not detected when /proc/1/exe points to procd",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/proc/1/exe": "/sbin/procd"},
			want:      false,
		},
		{
			name:      "not detected when /etc/inittab exists but /proc/1/exe points to runit",
			statPaths: map[string]bool{"/etc/inittab": true},
			readlinks: map[string]string{"/proc/1/exe": "/usr/bin/runit-init"},
			want:      false,
		},
		{
			name:      "not detected when /etc/inittab exists but /proc/1/exe points to s6",
			statPaths: map[string]bool{"/etc/inittab": true},
			readlinks: map[string]string{"/proc/1/exe": "/usr/bin/s6-svscan"},
			want:      false,
		},
		{
			name:      "not detected when /etc/inittab exists but /proc/1/exe points to systemd",
			statPaths: map[string]bool{"/etc/inittab": true},
			readlinks: map[string]string{"/proc/1/exe": "/lib/systemd/systemd"},
			want:      false,
		},
		{
			name:      "not detected when /sbin/init is symlink to dinit",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/sbin/init": "/usr/bin/dinit", "/proc/1/exe": "/usr/bin/dinit"},
			want:      false,
		},
		{
			name:      "not detected when /proc/1/exe points to dinit",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/proc/1/exe": "/usr/bin/dinit"},
			want:      false,
		},
		{
			name:      "not detected when /sbin/init is symlink to finit",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/sbin/init": "/usr/bin/finit", "/proc/1/exe": "/usr/bin/finit"},
			want:      false,
		},
		{
			name:      "not detected when /proc/1/exe points to finit",
			statPaths: map[string]bool{"/sbin/init": true},
			readlinks: map[string]string{"/proc/1/exe": "/usr/bin/finit"},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			oldLog := getDetectionLog()
			defer SetDetectionLogger(oldLog)
			SetDetectionLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

			m := newTestSysvinitManager(func(m *sysvinitManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
				m.readlinkFunc = func(path string) (string, error) {
					if target, ok := tt.readlinks[path]; ok {
						return target, nil
					}
					return "", os.ErrNotExist
				}
			})

			got := m.detect()
			if got != tt.want {
				t.Errorf("detect() = %v, want %v\nlog output:\n%s", got, tt.want, buf.String())
			}
		})
	}
}

func TestSysvinitName(t *testing.T) {
	m := newTestSysvinitManager()
	if got := m.Name(); got != "sysvinit" {
		t.Errorf("Name() = %q, want %q", got, "sysvinit")
	}
}

func TestSysvinitCanRestartWatchdog(t *testing.T) {
	m := newTestSysvinitManager()
	if got := m.CanRestartWatchdog(); got != false {
		t.Errorf("CanRestartWatchdog() = %v, want false", got)
	}
}

func TestSysvinitGetManifestPath(t *testing.T) {
	m := newTestSysvinitManager()
	m.serviceName = "myservice"
	if got := m.GetManifestPath(); got != "/etc/init.d/myservice" {
		t.Errorf("GetManifestPath() = %q, want %q", got, "/etc/init.d/myservice")
	}
}

func TestSysvinitGenerateConfig(t *testing.T) {
	tests := []struct {
		name     string
		config   ServiceConfig
		contains []string
		excludes []string
	}{
		{
			name: "minimal config",
			config: ServiceConfig{
				Name:     "myapp",
				ExecPath: "/opt/myapp/myapp",
			},
			contains: []string{
				"#!/bin/sh",
				"### BEGIN INIT INFO",
				"# Provides:          myapp",
				"### END INIT INFO",
				`NAME="myapp"`,
				"DAEMON='/opt/myapp/myapp'",
				`PIDFILE="/var/run/$NAME.pid"`,
				"start)",
				"stop)",
				"restart)",
				"status)",
			},
			excludes: []string{
				"WORKDIR=",
				"USER=",
				"export ",
			},
		},
		{
			name: "full config with all fields",
			config: ServiceConfig{
				Name:        "http-server",
				Description: "HTTP Server Service",
				ExecPath:    "/opt/http-server/server",
				WorkingDir:  "/opt/http-server",
				User:        "www-data",
				Arguments:   []string{"--port", "8080"},
				Environment: map[string]string{
					"PORT":      "8080",
					"LOG_LEVEL": "info",
				},
			},
			contains: []string{
				"# Provides:          http-server",
				"# Short-Description: HTTP Server Service",
				"DAEMON='/opt/http-server/server'",
				"WORKDIR='/opt/http-server'",
				"USER='www-data'",
				"su -s /bin/sh \"$USER\" -c",
				shellQuote("'/opt/http-server/server' '--port' '8080' >/dev/null 2>&1 & echo $!"),
				"export LOG_LEVEL='info'",
				"export PORT='8080'",
			},
		},
		{
			name: "config with working dir but no user",
			config: ServiceConfig{
				Name:       "worker",
				ExecPath:   "/usr/local/bin/worker",
				WorkingDir: "/var/lib/worker",
			},
			contains: []string{
				"WORKDIR='/var/lib/worker'",
				`cd "$WORKDIR" || exit 1`,
			},
			excludes: []string{
				"USER=",
				"su -s /bin/sh",
			},
		},
		{
			name: "config with user but no working dir",
			config: ServiceConfig{
				Name:     "daemon",
				ExecPath: "/usr/bin/daemon",
				User:     "nobody",
			},
			contains: []string{
				"USER='nobody'",
				"su -s /bin/sh \"$USER\" -c",
			},
			excludes: []string{
				"WORKDIR=",
				`cd "$WORKDIR"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestSysvinitManager()
			content, err := m.GenerateConfig(tt.config)
			if err != nil {
				t.Fatalf("GenerateConfig() error = %v", err)
			}

			if m.serviceName != tt.config.Name {
				t.Errorf("serviceName = %q, want %q", m.serviceName, tt.config.Name)
			}

			for _, s := range tt.contains {
				if !strings.Contains(content, s) {
					t.Errorf("GenerateConfig() missing %q in output:\n%s", s, content)
				}
			}
			for _, s := range tt.excludes {
				if strings.Contains(content, s) {
					t.Errorf("GenerateConfig() should not contain %q in output:\n%s", s, content)
				}
			}
		})
	}
}

func TestSysvinitGenerateConfig_SpecialChars(t *testing.T) {
	tests := []struct {
		name     string
		config   ServiceConfig
		contains []string
	}{
		{
			name: "single quotes in arguments",
			config: ServiceConfig{
				Name:      "myapp",
				ExecPath:  "/opt/myapp/myapp",
				Arguments: []string{"--name=it's a test"},
			},
			contains: []string{
				"'--name=it'\\''s a test'",
			},
		},
		{
			name: "spaces in paths",
			config: ServiceConfig{
				Name:       "myapp",
				ExecPath:   "/opt/my app/myapp",
				WorkingDir: "/opt/my dir",
			},
			contains: []string{
				"DAEMON='/opt/my app/myapp'",
				"WORKDIR='/opt/my dir'",
			},
		},
		{
			name: "special chars in env values",
			config: ServiceConfig{
				Name:     "myapp",
				ExecPath: "/opt/myapp/myapp",
				Environment: map[string]string{
					"DSN": "postgres://user:p@ss$word/db",
				},
			},
			contains: []string{
				"export DSN='postgres://user:p@ss$word/db'",
			},
		},
		{
			name: "empty arguments list",
			config: ServiceConfig{
				Name:      "myapp",
				ExecPath:  "/opt/myapp/myapp",
				Arguments: []string{},
			},
			contains: []string{
				"DAEMON='/opt/myapp/myapp'",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestSysvinitManager()
			content, err := m.GenerateConfig(tt.config)
			if err != nil {
				t.Fatalf("GenerateConfig() error = %v", err)
			}
			for _, s := range tt.contains {
				if !strings.Contains(content, s) {
					t.Errorf("GenerateConfig() missing %q in output:\n%s", s, content)
				}
			}
		})
	}
}

func TestSysvinitGenerateConfig_InvalidEnvKey(t *testing.T) {
	m := newTestSysvinitManager()
	_, err := m.GenerateConfig(ServiceConfig{
		Name:     "myapp",
		ExecPath: "/opt/myapp/myapp",
		Environment: map[string]string{
			"INVALID-KEY": "value",
		},
	})
	if err == nil {
		t.Fatal("GenerateConfig() expected error for invalid env key")
	}
	if !strings.Contains(err.Error(), "sysvinit") {
		t.Errorf("error should mention sysvinit, got: %v", err)
	}
}

func TestGenerateSysvinitInitScript(t *testing.T) {
	config := ServiceConfig{
		Name:     "testservice",
		ExecPath: "/usr/bin/testservice",
	}
	content, err := GenerateSysvinitInitScript(config)
	if err != nil {
		t.Fatalf("GenerateSysvinitInitScript() error = %v", err)
	}
	if !strings.Contains(content, "#!/bin/sh") {
		t.Error("missing shebang")
	}
	if !strings.Contains(content, "# Provides:          testservice") {
		t.Error("missing LSB provides header")
	}
}

func TestSysvinitInstall(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := &sysvinitManager{
		executor: exec,
		fs:       fs,
		statFunc: func(path string) (os.FileInfo, error) {
			if path == "/etc/rc0.d" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		},
	}

	config := ServiceConfig{
		Name:        "test-svc",
		Description: "Test Service",
		ExecPath:    "/opt/test-svc/test-svc",
		WorkingDir:  "/opt/test-svc",
	}

	err := mgr.Install(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	scriptPath := "/etc/init.d/test-svc"
	data, ok := fs.files[scriptPath]
	if !ok {
		t.Fatal("init script was not written")
	}

	content := string(data)
	if !strings.Contains(content, "#!/bin/sh") {
		t.Error("missing shebang in written script")
	}
	if !strings.Contains(content, "# Provides:          test-svc") {
		t.Error("missing LSB header in written script")
	}

	if mgr.serviceName != "test-svc" {
		t.Errorf("serviceName = %q, want %q", mgr.serviceName, "test-svc")
	}
}

func TestSysvinitInstall_BusyBoxStyle(t *testing.T) {
	fs := newMockFS()
	mgr := newTestSysvinitManager(func(m *sysvinitManager) {
		m.fs = fs
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/init.d/rcS" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
	})

	config := ServiceConfig{
		Name:        "test-svc",
		Description: "Test Service",
		ExecPath:    "/opt/test-svc/test-svc",
	}

	err := mgr.Install(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	busyboxPath := "/etc/init.d/K95test-svc"
	data, ok := fs.files[busyboxPath]
	if !ok {
		t.Fatalf("expected script at %s, got files: %v", busyboxPath, keysOf(fs.files))
	}
	if !strings.Contains(string(data), "#!/bin/sh") {
		t.Error("missing shebang in written script")
	}

	if _, ok := fs.files["/etc/init.d/test-svc"]; ok {
		t.Error("should not write to initScriptPath on BusyBox systems")
	}
}

func TestSysvinitInstall_ClassicalSysV(t *testing.T) {
	fs := newMockFS()
	mgr := newTestSysvinitManager(func(m *sysvinitManager) {
		m.fs = fs
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/rc0.d" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
	})

	config := ServiceConfig{
		Name:        "test-svc",
		Description: "Test Service",
		ExecPath:    "/opt/test-svc/test-svc",
	}

	err := mgr.Install(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	classicalPath := "/etc/init.d/test-svc"
	if _, ok := fs.files[classicalPath]; !ok {
		t.Fatalf("expected script at %s, got files: %v", classicalPath, keysOf(fs.files))
	}

	if _, ok := fs.files["/etc/init.d/K95test-svc"]; ok {
		t.Error("should not write to disabledScriptPath on classical SysV systems")
	}
}

func keysOf(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestSysvinitInstall_WriteFileError(t *testing.T) {
	fs := newMockFS()
	fs.writeErr = errors.New("permission denied")
	mgr := newTestSysvinitManager(func(m *sysvinitManager) {
		m.fs = fs
	})

	err := mgr.Install(ServiceConfig{
		Name:     "test-svc",
		ExecPath: "/opt/test-svc/test-svc",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "writing init script") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSysvinitInstall_InvalidConfig(t *testing.T) {
	mgr := newTestSysvinitManager()

	err := mgr.Install(ServiceConfig{
		Name:     "test-svc",
		ExecPath: "/opt/test-svc/test-svc",
		Environment: map[string]string{
			"INVALID-KEY": "value",
		},
	})
	if err == nil {
		t.Fatal("expected error for invalid env key")
	}
	if !strings.Contains(err.Error(), "generating init script") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSysvinitUninstall(t *testing.T) {
	exec := newMockExecutor()
	exec.fallbackErr = errors.New("command not found")
	fs := newMockFS()
	mgr := &sysvinitManager{
		serviceName:  "test-svc",
		executor:     exec,
		fs:           fs,
		lookPathFunc: func(string) (string, error) { return "", errors.New("not found") },
		statFunc:     func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		renameFunc:   func(string, string) error { return nil },
		symlinkFunc:  func(string, string) error { return nil },
	}

	err := mgr.Uninstall()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !fs.removed["/etc/init.d/test-svc"] {
		t.Error("init script was not removed")
	}
}

func TestSysvinitUninstall_NoServiceName(t *testing.T) {
	mgr := &sysvinitManager{executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Uninstall()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "service name not set") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSysvinitUninstall_RemoveError(t *testing.T) {
	fs := newMockFS()
	fs.removeErr = errors.New("permission denied")
	mgr := &sysvinitManager{
		serviceName:  "test-svc",
		executor:     newMockExecutor(),
		fs:           fs,
		lookPathFunc: func(string) (string, error) { return "", errors.New("not found") },
		statFunc:     func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		renameFunc:   func(string, string) error { return nil },
		symlinkFunc:  func(string, string) error { return nil },
	}

	err := mgr.Uninstall()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "removing init script") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSysvinitWriteConfig(t *testing.T) {
	fs := newMockFS()
	mgr := newTestSysvinitManager(func(m *sysvinitManager) {
		m.serviceName = "test-svc"
		m.fs = fs
	})

	content := "#!/bin/sh\n# test script\n"
	err := mgr.WriteConfig(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, ok := fs.files["/etc/init.d/test-svc"]
	if !ok {
		t.Fatal("config was not written")
	}
	if string(data) != content {
		t.Errorf("written content = %q, want %q", string(data), content)
	}
}

func TestSysvinitWriteConfig_NoServiceName(t *testing.T) {
	mgr := newTestSysvinitManager()
	err := mgr.WriteConfig("content")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "service name not set") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSysvinitWriteConfig_WriteError(t *testing.T) {
	fs := newMockFS()
	fs.writeErr = errors.New("permission denied")
	mgr := newTestSysvinitManager(func(m *sysvinitManager) {
		m.serviceName = "test-svc"
		m.fs = fs
	})

	err := mgr.WriteConfig("content")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "writing init script") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSysvinitReadInstalledConfig(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		content := "#!/bin/sh\n# test init script\n"
		mgr := newTestSysvinitManager(func(m *sysvinitManager) {
			m.readFileFunc = func(path string) ([]byte, error) {
				if path == "/etc/init.d/test-svc" {
					return []byte(content), nil
				}
				return nil, os.ErrNotExist
			}
		})
		mgr.serviceName = "test-svc"

		got, err := mgr.ReadInstalledConfig()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != content {
			t.Errorf("ReadInstalledConfig() = %q, want %q", got, content)
		}
	})

	t.Run("file not found", func(t *testing.T) {
		mgr := newTestSysvinitManager()
		mgr.serviceName = "test-svc"

		_, err := mgr.ReadInstalledConfig()
		if err == nil {
			t.Fatal("expected error for non-existent file")
		}
		if !strings.Contains(err.Error(), "reading init script") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("no service name", func(t *testing.T) {
		mgr := newTestSysvinitManager()

		_, err := mgr.ReadInstalledConfig()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "service name not set") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestSysvinitHasUpdateRcD(t *testing.T) {
	tests := []struct {
		name         string
		lookPathFunc func(string) (string, error)
		want         bool
	}{
		{
			name:         "update-rc.d available",
			lookPathFunc: func(cmd string) (string, error) { return "/usr/sbin/update-rc.d", nil },
			want:         true,
		},
		{
			name:         "update-rc.d not found",
			lookPathFunc: func(cmd string) (string, error) { return "", errors.New("not found") },
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestSysvinitManager(func(m *sysvinitManager) {
				m.lookPathFunc = tt.lookPathFunc
			})
			if got := m.hasUpdateRcD(); got != tt.want {
				t.Errorf("hasUpdateRcD() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSysvinitEnableDebian(t *testing.T) {
	exec := newMockExecutor()
	mgr := &sysvinitManager{
		serviceName: "test-svc",
		executor:    exec,
		fs:          newMockFS(),
	}

	err := mgr.enableDebian()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exec.called("update-rc.d", "test-svc", "defaults") {
		t.Error("update-rc.d was not called with correct arguments")
	}
}

func TestSysvinitEnableDebian_Error(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("update-rc.d test-svc defaults", nil, errors.New("permission denied"))
	mgr := &sysvinitManager{
		serviceName: "test-svc",
		executor:    exec,
		fs:          newMockFS(),
	}

	err := mgr.enableDebian()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "enabling service with update-rc.d") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSysvinitDisableDebian(t *testing.T) {
	exec := newMockExecutor()
	mgr := &sysvinitManager{
		serviceName: "test-svc",
		executor:    exec,
		fs:          newMockFS(),
	}

	err := mgr.disableDebian()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exec.called("update-rc.d", "-f", "test-svc", "remove") {
		t.Error("update-rc.d was not called with correct arguments")
	}
}

func TestSysvinitDisableDebian_Error(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("update-rc.d -f test-svc remove", nil, errors.New("permission denied"))
	mgr := &sysvinitManager{
		serviceName: "test-svc",
		executor:    exec,
		fs:          newMockFS(),
	}

	err := mgr.disableDebian()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "disabling service with update-rc.d") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSysvinitHasChkconfig(t *testing.T) {
	tests := []struct {
		name         string
		lookPathFunc func(string) (string, error)
		want         bool
	}{
		{
			name: "chkconfig available",
			lookPathFunc: func(cmd string) (string, error) {
				if cmd == "chkconfig" {
					return "/sbin/chkconfig", nil
				}
				return "", errors.New("not found")
			},
			want: true,
		},
		{
			name:         "chkconfig not found",
			lookPathFunc: func(cmd string) (string, error) { return "", errors.New("not found") },
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestSysvinitManager(func(m *sysvinitManager) {
				m.lookPathFunc = tt.lookPathFunc
			})
			if got := m.hasChkconfig(); got != tt.want {
				t.Errorf("hasChkconfig() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSysvinitEnableRHEL(t *testing.T) {
	exec := newMockExecutor()
	mgr := &sysvinitManager{
		serviceName: "test-svc",
		executor:    exec,
		fs:          newMockFS(),
	}

	err := mgr.enableRHEL()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exec.called("chkconfig", "--add", "test-svc") {
		t.Error("chkconfig --add was not called with correct arguments")
	}
	if !exec.called("chkconfig", "test-svc", "on") {
		t.Error("chkconfig on was not called with correct arguments")
	}
}

func TestSysvinitEnableRHEL_AddError(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("chkconfig --add test-svc", nil, errors.New("permission denied"))
	mgr := &sysvinitManager{
		serviceName: "test-svc",
		executor:    exec,
		fs:          newMockFS(),
	}

	err := mgr.enableRHEL()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "registering service with chkconfig") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSysvinitEnableRHEL_OnError(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("chkconfig test-svc on", nil, errors.New("permission denied"))
	mgr := &sysvinitManager{
		serviceName: "test-svc",
		executor:    exec,
		fs:          newMockFS(),
	}

	err := mgr.enableRHEL()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "enabling service with chkconfig") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSysvinitDisableRHEL(t *testing.T) {
	exec := newMockExecutor()
	mgr := &sysvinitManager{
		serviceName: "test-svc",
		executor:    exec,
		fs:          newMockFS(),
	}

	err := mgr.disableRHEL()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exec.called("chkconfig", "test-svc", "off") {
		t.Error("chkconfig off was not called with correct arguments")
	}
	if !exec.called("chkconfig", "--del", "test-svc") {
		t.Error("chkconfig --del was not called with correct arguments")
	}
}

func TestSysvinitDisableRHEL_OffError(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("chkconfig test-svc off", nil, errors.New("permission denied"))
	mgr := &sysvinitManager{
		serviceName: "test-svc",
		executor:    exec,
		fs:          newMockFS(),
	}

	err := mgr.disableRHEL()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "disabling service with chkconfig") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSysvinitDisableRHEL_DelError(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("chkconfig --del test-svc", nil, errors.New("permission denied"))
	mgr := &sysvinitManager{
		serviceName: "test-svc",
		executor:    exec,
		fs:          newMockFS(),
	}

	err := mgr.disableRHEL()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unregistering service with chkconfig") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSysvinitEnable(t *testing.T) {
	tests := []struct {
		name             string
		lookPathFunc     func(string) (string, error)
		statFunc         func(string) (os.FileInfo, error)
		symlinkFunc      func(string, string) error
		renameFunc       func(string, string) error
		supportsKSPrefix bool
		wantErr          string
		wantCalled       []string
	}{
		{
			name: "uses update-rc.d when available",
			lookPathFunc: func(cmd string) (string, error) {
				if cmd == "update-rc.d" {
					return "/usr/sbin/update-rc.d", nil
				}
				return "", errors.New("not found")
			},
			wantCalled: []string{"update-rc.d test-svc defaults"},
		},
		{
			name: "uses chkconfig when update-rc.d not available",
			lookPathFunc: func(cmd string) (string, error) {
				if cmd == "chkconfig" {
					return "/sbin/chkconfig", nil
				}
				return "", errors.New("not found")
			},
			wantCalled: []string{"chkconfig --add test-svc", "chkconfig test-svc on"},
		},
		{
			name: "prefers update-rc.d over chkconfig when both available",
			lookPathFunc: func(cmd string) (string, error) {
				if cmd == "update-rc.d" {
					return "/usr/sbin/update-rc.d", nil
				}
				if cmd == "chkconfig" {
					return "/sbin/chkconfig", nil
				}
				return "", errors.New("not found")
			},
			wantCalled: []string{"update-rc.d test-svc defaults"},
		},
		{
			name:         "falls back to symlinks when runlevel dirs exist",
			lookPathFunc: func(cmd string) (string, error) { return "", errors.New("not found") },
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/rc0.d" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			symlinkFunc: func(string, string) error { return nil },
			wantCalled:  nil,
		},
		{
			name:             "falls back to busybox when no runlevel dirs",
			lookPathFunc:     func(cmd string) (string, error) { return "", errors.New("not found") },
			supportsKSPrefix: true,
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/K95test-svc" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			renameFunc: func(string, string) error { return nil },
			wantCalled: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			mgr := &sysvinitManager{
				serviceName:      "test-svc",
				supportsKSPrefix: tt.supportsKSPrefix,
				executor:         exec,
				fs:               newMockFS(),
				lookPathFunc:     tt.lookPathFunc,
				statFunc: func(string) (os.FileInfo, error) {
					return nil, os.ErrNotExist
				},
				renameFunc:  func(string, string) error { return errors.New("rename not configured") },
				symlinkFunc: func(string, string) error { return errors.New("symlink not configured") },
			}
			if tt.statFunc != nil {
				mgr.statFunc = tt.statFunc
			}
			if tt.symlinkFunc != nil {
				mgr.symlinkFunc = tt.symlinkFunc
			}
			if tt.renameFunc != nil {
				mgr.renameFunc = tt.renameFunc
			}

			err := mgr.Enable()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, key := range tt.wantCalled {
				args := strings.Split(key, " ")
				if !exec.called(args[0], args[1:]...) {
					t.Errorf("expected call %q", key)
				}
			}
		})
	}
}

func TestSysvinitEnableFallbackSymlinks(t *testing.T) {
	var createdSymlinks []string
	m := newTestSysvinitManager(func(m *sysvinitManager) {
		m.serviceName = "test-svc"
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/rc0.d" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
		m.symlinkFunc = func(target, link string) error {
			createdSymlinks = append(createdSymlinks, link)
			return nil
		}
	})

	err := m.Enable()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(createdSymlinks) != 7 {
		t.Errorf("expected 7 symlinks, got %d: %v", len(createdSymlinks), createdSymlinks)
	}
}

func TestSysvinitEnableFallbackBusyBox(t *testing.T) {
	var renamed [2]string
	m := newTestSysvinitManager(func(m *sysvinitManager) {
		m.serviceName = "test-svc"
		m.supportsKSPrefix = true
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/init.d/K95test-svc" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
		m.renameFunc = func(from, to string) error {
			renamed = [2]string{from, to}
			return nil
		}
	})

	err := m.Enable()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if renamed[0] != "/etc/init.d/K95test-svc" || renamed[1] != "/etc/init.d/S95test-svc" {
		t.Errorf("unexpected rename: %v -> %v", renamed[0], renamed[1])
	}
}

func TestSysvinitEnableFallbackDetectsRunlevelDirs(t *testing.T) {
	tests := []struct {
		name            string
		hasRunlevelDirs bool
		wantSymlink     bool
		wantRename      bool
	}{
		{
			name:            "with runlevel dirs uses symlinks",
			hasRunlevelDirs: true,
			wantSymlink:     true,
		},
		{
			name:            "without runlevel dirs uses busybox",
			hasRunlevelDirs: false,
			wantRename:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			symlinkCalled := false
			renameCalled := false
			m := newTestSysvinitManager(func(m *sysvinitManager) {
				m.serviceName = "test-svc"
				m.supportsKSPrefix = !tt.hasRunlevelDirs
				m.statFunc = func(path string) (os.FileInfo, error) {
					if path == "/etc/rc0.d" && tt.hasRunlevelDirs {
						return nil, nil
					}
					if path == "/etc/init.d/K95test-svc" && !tt.hasRunlevelDirs {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
				m.symlinkFunc = func(string, string) error {
					symlinkCalled = true
					return nil
				}
				m.renameFunc = func(string, string) error {
					renameCalled = true
					return nil
				}
			})

			err := m.Enable()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantSymlink && !symlinkCalled {
				t.Error("expected symlink to be called")
			}
			if tt.wantRename && !renameCalled {
				t.Error("expected rename to be called")
			}
			if !tt.wantSymlink && symlinkCalled {
				t.Error("did not expect symlink to be called")
			}
			if !tt.wantRename && renameCalled {
				t.Error("did not expect rename to be called")
			}
		})
	}
}

func TestSysvinitDisable(t *testing.T) {
	tests := []struct {
		name             string
		lookPathFunc     func(string) (string, error)
		statFunc         func(string) (os.FileInfo, error)
		renameFunc       func(string, string) error
		supportsKSPrefix bool
		wantErr          string
		wantCalled       []string
	}{
		{
			name: "uses update-rc.d when available",
			lookPathFunc: func(cmd string) (string, error) {
				if cmd == "update-rc.d" {
					return "/usr/sbin/update-rc.d", nil
				}
				return "", errors.New("not found")
			},
			wantCalled: []string{"update-rc.d -f test-svc remove"},
		},
		{
			name: "uses chkconfig when update-rc.d not available",
			lookPathFunc: func(cmd string) (string, error) {
				if cmd == "chkconfig" {
					return "/sbin/chkconfig", nil
				}
				return "", errors.New("not found")
			},
			wantCalled: []string{"chkconfig test-svc off", "chkconfig --del test-svc"},
		},
		{
			name: "prefers update-rc.d over chkconfig when both available",
			lookPathFunc: func(cmd string) (string, error) {
				if cmd == "update-rc.d" {
					return "/usr/sbin/update-rc.d", nil
				}
				if cmd == "chkconfig" {
					return "/sbin/chkconfig", nil
				}
				return "", errors.New("not found")
			},
			wantCalled: []string{"update-rc.d -f test-svc remove"},
		},
		{
			name:         "falls back to symlinks when runlevel dirs exist",
			lookPathFunc: func(cmd string) (string, error) { return "", errors.New("not found") },
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/rc0.d" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			wantCalled: nil,
		},
		{
			name:             "falls back to busybox when no runlevel dirs",
			lookPathFunc:     func(cmd string) (string, error) { return "", errors.New("not found") },
			supportsKSPrefix: true,
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/S95test-svc" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			renameFunc: func(string, string) error { return nil },
			wantCalled: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			mgr := &sysvinitManager{
				serviceName:      "test-svc",
				supportsKSPrefix: tt.supportsKSPrefix,
				executor:         exec,
				fs:               newMockFS(),
				lookPathFunc:     tt.lookPathFunc,
				statFunc: func(string) (os.FileInfo, error) {
					return nil, os.ErrNotExist
				},
				renameFunc:  func(string, string) error { return errors.New("rename not configured") },
				symlinkFunc: func(string, string) error { return errors.New("symlink not configured") },
			}
			if tt.statFunc != nil {
				mgr.statFunc = tt.statFunc
			}
			if tt.renameFunc != nil {
				mgr.renameFunc = tt.renameFunc
			}

			err := mgr.Disable()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, key := range tt.wantCalled {
				args := strings.Split(key, " ")
				if !exec.called(args[0], args[1:]...) {
					t.Errorf("expected call %q", key)
				}
			}
		})
	}
}

func TestSysvinitDisableFallbackSymlinks(t *testing.T) {
	fs := newMockFS()
	m := newTestSysvinitManager(func(m *sysvinitManager) {
		m.serviceName = "test-svc"
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/rc0.d" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
		m.fs = fs
	})

	err := m.Disable()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fs.removed) != 7 {
		t.Errorf("expected 7 symlink removals, got %d: %v", len(fs.removed), fs.removed)
	}
}

func TestSysvinitDisableFallbackBusyBox(t *testing.T) {
	var renamed [2]string
	m := newTestSysvinitManager(func(m *sysvinitManager) {
		m.serviceName = "test-svc"
		m.supportsKSPrefix = true
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/init.d/S95test-svc" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
		m.renameFunc = func(from, to string) error {
			renamed = [2]string{from, to}
			return nil
		}
	})

	err := m.Disable()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if renamed[0] != "/etc/init.d/S95test-svc" || renamed[1] != "/etc/init.d/K95test-svc" {
		t.Errorf("unexpected rename: %v -> %v", renamed[0], renamed[1])
	}
}

func TestSysvinitDisableFallbackDetectsRunlevelDirs(t *testing.T) {
	tests := []struct {
		name            string
		hasRunlevelDirs bool
		wantRemove      bool
		wantRename      bool
	}{
		{
			name:            "with runlevel dirs uses symlink removal",
			hasRunlevelDirs: true,
			wantRemove:      true,
		},
		{
			name:            "without runlevel dirs uses busybox",
			hasRunlevelDirs: false,
			wantRename:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			renameCalled := false
			fs := newMockFS()
			m := newTestSysvinitManager(func(m *sysvinitManager) {
				m.serviceName = "test-svc"
				m.supportsKSPrefix = !tt.hasRunlevelDirs
				m.statFunc = func(path string) (os.FileInfo, error) {
					if path == "/etc/rc0.d" && tt.hasRunlevelDirs {
						return nil, nil
					}
					if path == "/etc/init.d/S95test-svc" && !tt.hasRunlevelDirs {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
				m.fs = fs
				m.renameFunc = func(string, string) error {
					renameCalled = true
					return nil
				}
			})

			err := m.Disable()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			removeCalled := len(fs.removed) > 0
			if tt.wantRemove && !removeCalled {
				t.Error("expected remove to be called")
			}
			if tt.wantRename && !renameCalled {
				t.Error("expected rename to be called")
			}
			if !tt.wantRemove && removeCalled {
				t.Error("did not expect remove to be called")
			}
			if !tt.wantRename && renameCalled {
				t.Error("did not expect rename to be called")
			}
		})
	}
}

func TestSysvinitEnableEmptyServiceName(t *testing.T) {
	m := newTestSysvinitManager()
	err := m.Enable()
	if err == nil {
		t.Fatal("expected error for empty service name")
	}
	if !strings.Contains(err.Error(), "service name not set") {
		t.Errorf("error = %v, want containing %q", err, "service name not set")
	}
}

func TestSysvinitDisableEmptyServiceName(t *testing.T) {
	m := newTestSysvinitManager()
	err := m.Disable()
	if err == nil {
		t.Fatal("expected error for empty service name")
	}
	if !strings.Contains(err.Error(), "service name not set") {
		t.Errorf("error = %v, want containing %q", err, "service name not set")
	}
}

func TestSysvinitHasServiceCommand(t *testing.T) {
	tests := []struct {
		name         string
		lookPathFunc func(string) (string, error)
		want         bool
	}{
		{
			name: "service command available",
			lookPathFunc: func(cmd string) (string, error) {
				if cmd == "service" {
					return "/usr/sbin/service", nil
				}
				return "", errors.New("not found")
			},
			want: true,
		},
		{
			name:         "service command not found",
			lookPathFunc: func(cmd string) (string, error) { return "", errors.New("not found") },
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestSysvinitManager(func(m *sysvinitManager) {
				m.lookPathFunc = tt.lookPathFunc
			})
			if got := m.hasServiceCommand(); got != tt.want {
				t.Errorf("hasServiceCommand() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSysvinitStart(t *testing.T) {
	notFound := func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	tests := []struct {
		name         string
		serviceName  string
		lookPathFunc func(string) (string, error)
		statFunc     func(string) (os.FileInfo, error)
		execErr      map[string]error
		wantErr      string
		wantCalled   string
	}{
		{
			name:        "start with service command",
			serviceName: "test-svc",
			lookPathFunc: func(cmd string) (string, error) {
				if cmd == "service" {
					return "/usr/sbin/service", nil
				}
				return "", errors.New("not found")
			},
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/rc0.d" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			wantCalled: "service test-svc start",
		},
		{
			name:         "start with direct script execution",
			serviceName:  "test-svc",
			lookPathFunc: func(cmd string) (string, error) { return "", errors.New("not found") },
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/test-svc" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			wantCalled: "/etc/init.d/test-svc start",
		},
		{
			name:         "start with S95 script on BusyBox system",
			serviceName:  "test-svc",
			lookPathFunc: func(cmd string) (string, error) { return "", errors.New("not found") },
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/S95test-svc" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			wantCalled: "/etc/init.d/S95test-svc start",
		},
		{
			name:         "start with K95 script on BusyBox system",
			serviceName:  "test-svc",
			lookPathFunc: func(cmd string) (string, error) { return "", errors.New("not found") },
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/K95test-svc" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			wantCalled: "/etc/init.d/K95test-svc start",
		},
		{
			name:        "start error with service command",
			serviceName: "test-svc",
			lookPathFunc: func(cmd string) (string, error) {
				if cmd == "service" {
					return "/usr/sbin/service", nil
				}
				return "", errors.New("not found")
			},
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/rc0.d" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			execErr: map[string]error{"service test-svc start": errors.New("permission denied")},
			wantErr: "starting service",
		},
		{
			name:         "start error with direct script",
			serviceName:  "test-svc",
			lookPathFunc: func(cmd string) (string, error) { return "", errors.New("not found") },
			statFunc:     notFound,
			execErr:      map[string]error{"/etc/init.d/test-svc start": errors.New("script not found")},
			wantErr:      "starting service",
		},
		{
			name:         "error when service name not set",
			serviceName:  "",
			lookPathFunc: func(cmd string) (string, error) { return "", errors.New("not found") },
			wantErr:      "service name not set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			for key, err := range tt.execErr {
				exec.setResult(key, nil, err)
			}
			statFn := tt.statFunc
			if statFn == nil {
				statFn = notFound
			}
			mgr := &sysvinitManager{
				serviceName:  tt.serviceName,
				executor:     exec,
				fs:           newMockFS(),
				lookPathFunc: tt.lookPathFunc,
				statFunc:     statFn,
			}

			err := mgr.Start()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			args := strings.Split(tt.wantCalled, " ")
			if !exec.called(args[0], args[1:]...) {
				t.Errorf("expected call %q", tt.wantCalled)
			}
		})
	}
}

func TestSysvinitStop(t *testing.T) {
	notFound := func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	tests := []struct {
		name         string
		serviceName  string
		lookPathFunc func(string) (string, error)
		statFunc     func(string) (os.FileInfo, error)
		execErr      map[string]error
		wantErr      string
		wantCalled   string
	}{
		{
			name:        "stop with service command",
			serviceName: "test-svc",
			lookPathFunc: func(cmd string) (string, error) {
				if cmd == "service" {
					return "/usr/sbin/service", nil
				}
				return "", errors.New("not found")
			},
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/rc0.d" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			wantCalled: "service test-svc stop",
		},
		{
			name:         "stop with direct script execution",
			serviceName:  "test-svc",
			lookPathFunc: func(cmd string) (string, error) { return "", errors.New("not found") },
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/test-svc" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			wantCalled: "/etc/init.d/test-svc stop",
		},
		{
			name:         "stop with S95 script on BusyBox system",
			serviceName:  "test-svc",
			lookPathFunc: func(cmd string) (string, error) { return "", errors.New("not found") },
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/S95test-svc" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			wantCalled: "/etc/init.d/S95test-svc stop",
		},
		{
			name:         "stop with K95 script on BusyBox system",
			serviceName:  "test-svc",
			lookPathFunc: func(cmd string) (string, error) { return "", errors.New("not found") },
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/K95test-svc" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			wantCalled: "/etc/init.d/K95test-svc stop",
		},
		{
			name:        "stop error with service command",
			serviceName: "test-svc",
			lookPathFunc: func(cmd string) (string, error) {
				if cmd == "service" {
					return "/usr/sbin/service", nil
				}
				return "", errors.New("not found")
			},
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/rc0.d" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			execErr: map[string]error{"service test-svc stop": errors.New("permission denied")},
			wantErr: "stopping service",
		},
		{
			name:         "stop error with direct script",
			serviceName:  "test-svc",
			lookPathFunc: func(cmd string) (string, error) { return "", errors.New("not found") },
			statFunc:     notFound,
			execErr:      map[string]error{"/etc/init.d/test-svc stop": errors.New("script not found")},
			wantErr:      "stopping service",
		},
		{
			name:         "error when service name not set",
			serviceName:  "",
			lookPathFunc: func(cmd string) (string, error) { return "", errors.New("not found") },
			wantErr:      "service name not set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			for key, err := range tt.execErr {
				exec.setResult(key, nil, err)
			}
			statFn := tt.statFunc
			if statFn == nil {
				statFn = notFound
			}
			mgr := &sysvinitManager{
				serviceName:  tt.serviceName,
				executor:     exec,
				fs:           newMockFS(),
				lookPathFunc: tt.lookPathFunc,
				statFunc:     statFn,
			}

			err := mgr.Stop()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			args := strings.Split(tt.wantCalled, " ")
			if !exec.called(args[0], args[1:]...) {
				t.Errorf("expected call %q", tt.wantCalled)
			}
		})
	}
}

func TestSysvinitInstallUninstallRoundtrip(t *testing.T) {
	t.Run("classical SysV", func(t *testing.T) {
		exec := newMockExecutor()
		exec.fallbackErr = errors.New("command not found")
		fs := newMockFS()
		mgr := &sysvinitManager{
			executor:     exec,
			fs:           fs,
			lookPathFunc: func(string) (string, error) { return "", errors.New("not found") },
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/rc0.d" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			renameFunc:  func(string, string) error { return nil },
			symlinkFunc: func(string, string) error { return nil },
		}

		config := ServiceConfig{
			Name:        "roundtrip-svc",
			Description: "Roundtrip Test",
			ExecPath:    "/opt/roundtrip-svc/roundtrip-svc",
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
	})

	t.Run("BusyBox style", func(t *testing.T) {
		exec := newMockExecutor()
		exec.fallbackErr = errors.New("command not found")
		fs := newMockFS()
		written := make(map[string]bool)
		mgr := &sysvinitManager{
			executor:     exec,
			fs:           fs,
			lookPathFunc: func(string) (string, error) { return "", errors.New("not found") },
			statFunc: func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/rcS" {
					return nil, nil
				}
				if written[path] {
					return nil, nil
				}
				return nil, os.ErrNotExist
			},
			renameFunc:  func(string, string) error { return nil },
			symlinkFunc: func(string, string) error { return nil },
		}

		config := ServiceConfig{
			Name:        "roundtrip-svc",
			Description: "Roundtrip Test",
			ExecPath:    "/opt/roundtrip-svc/roundtrip-svc",
			WorkingDir:  "/opt/roundtrip-svc",
		}

		if err := mgr.Install(config); err != nil {
			t.Fatalf("install error: %v", err)
		}

		scriptPath := "/etc/init.d/K95roundtrip-svc"
		if _, ok := fs.files[scriptPath]; !ok {
			t.Fatal("init script not written after install")
		}
		written[scriptPath] = true

		if err := mgr.Uninstall(); err != nil {
			t.Fatalf("uninstall error: %v", err)
		}

		if !fs.removed[scriptPath] {
			t.Error("init script not removed after uninstall")
		}
	})
}

func TestSysvinitStatus(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		hasService  bool
		output      string
		execErr     error
		wantStatus  ServiceStatus
		wantErr     bool
		errSubstr   string
	}{
		{
			name:        "running via service command",
			serviceName: "myapp",
			hasService:  true,
			output:      "myapp is running (PID 1234)",
			wantStatus:  ServiceStatusRunning,
		},
		{
			name:        "running via direct script",
			serviceName: "myapp",
			hasService:  false,
			output:      "myapp is running (PID 5678)",
			wantStatus:  ServiceStatusRunning,
		},
		{
			name:        "stopped with not running message",
			serviceName: "myapp",
			hasService:  true,
			output:      "myapp is not running",
			execErr:     errors.New("exit status 3"),
			wantStatus:  ServiceStatusStopped,
		},
		{
			name:        "stopped with stopped keyword",
			serviceName: "myapp",
			hasService:  true,
			output:      "myapp is stopped",
			execErr:     errors.New("exit status 1"),
			wantStatus:  ServiceStatusStopped,
		},
		{
			name:        "stopped with dead keyword",
			serviceName: "myapp",
			hasService:  true,
			output:      "myapp dead but pid file exists",
			execErr:     errors.New("exit status 1"),
			wantStatus:  ServiceStatusStopped,
		},
		{
			name:        "unknown on error with no recognizable output",
			serviceName: "myapp",
			hasService:  true,
			output:      "",
			execErr:     errors.New("exit status 3"),
			wantStatus:  ServiceStatusUnknown,
			wantErr:     true,
			errSubstr:   "status command failed",
		},
		{
			name:        "unknown when no error and no recognizable output",
			serviceName: "myapp",
			hasService:  true,
			output:      "some unrecognized output",
			wantStatus:  ServiceStatusUnknown,
		},
		{
			name:        "error when service name not set",
			serviceName: "",
			wantStatus:  ServiceStatusUnknown,
			wantErr:     true,
			errSubstr:   "service name not set",
		},
		{
			name:        "direct script execution when service command absent",
			serviceName: "myapp",
			hasService:  false,
			output:      "myapp is not running",
			execErr:     errors.New("exit status 3"),
			wantStatus:  ServiceStatusStopped,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()

			var cmdKey string
			if tt.hasService {
				cmdKey = "service " + tt.serviceName + " status"
			} else {
				cmdKey = "/etc/init.d/" + tt.serviceName + " status"
			}
			if tt.serviceName != "" {
				exec.setResult(cmdKey, []byte(tt.output), tt.execErr)
			}

			m := newTestSysvinitManager(func(m *sysvinitManager) {
				m.executor = exec
				if tt.hasService {
					m.lookPathFunc = func(cmd string) (string, error) {
						if cmd == "service" {
							return "/usr/sbin/service", nil
						}
						return "", errors.New("not found")
					}
					m.statFunc = func(path string) (os.FileInfo, error) {
						if path == "/etc/rc0.d" {
							return nil, nil
						}
						return nil, os.ErrNotExist
					}
				}
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

func makeExitError(code int) error {
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	return err
}

func TestSysvinitStatusLSBExitCodes(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		exitCode   int
		wantStatus ServiceStatus
	}{
		{"exit code 1 no output", "", 1, ServiceStatusStopped},
		{"exit code 2 no output", "", 2, ServiceStatusStopped},
		{"exit code 3 no output", "", 3, ServiceStatusStopped},
		{"exit code 4 no output", "", 4, ServiceStatusUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exitErr := makeExitError(tt.exitCode)

			ex := newMockExecutor()
			ex.setResult("service myapp status", []byte(tt.output), exitErr)

			m := newTestSysvinitManager(func(m *sysvinitManager) {
				m.executor = ex
				m.lookPathFunc = func(cmd string) (string, error) {
					if cmd == "service" {
						return "/usr/sbin/service", nil
					}
					return "", errors.New("not found")
				}
				m.statFunc = func(path string) (os.FileInfo, error) {
					if path == "/etc/rc0.d" {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
			})
			m.serviceName = "myapp"

			status, err := m.Status()
			if tt.wantStatus == ServiceStatusUnknown {
				if err == nil {
					t.Fatal("expected error for unknown status")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
			if status != tt.wantStatus {
				t.Errorf("Status() = %v, want %v", status, tt.wantStatus)
			}
		})
	}
}

func TestSanitizeDescription(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple description", "simple description"},
		{"line1\nline2", "line1 line2"},
		{"line1\r\nline2", "line1  line2"},
		{"  leading/trailing  ", "leading/trailing"},
		{"", ""},
	}
	for _, tt := range tests {
		got := sanitizeDescription(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeDescription(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSysvinitFuncPointers(t *testing.T) {
	t.Run("default mock renameFunc returns error", func(t *testing.T) {
		m := newTestSysvinitManager()
		err := m.renameFunc("a", "b")
		if err == nil {
			t.Fatal("expected error from default mock renameFunc")
		}
	})

	t.Run("default mock symlinkFunc returns error", func(t *testing.T) {
		m := newTestSysvinitManager()
		err := m.symlinkFunc("a", "b")
		if err == nil {
			t.Fatal("expected error from default mock symlinkFunc")
		}
	})

	t.Run("renameFunc can be overridden via option", func(t *testing.T) {
		var calledOld, calledNew string
		m := newTestSysvinitManager(func(m *sysvinitManager) {
			m.renameFunc = func(oldpath, newpath string) error {
				calledOld = oldpath
				calledNew = newpath
				return nil
			}
		})
		if err := m.renameFunc("/old", "/new"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if calledOld != "/old" || calledNew != "/new" {
			t.Fatalf("expected /old, /new but got %s, %s", calledOld, calledNew)
		}
	})

	t.Run("symlinkFunc can be overridden via option", func(t *testing.T) {
		var calledTarget, calledLink string
		m := newTestSysvinitManager(func(m *sysvinitManager) {
			m.symlinkFunc = func(target, link string) error {
				calledTarget = target
				calledLink = link
				return nil
			}
		})
		if err := m.symlinkFunc("/target", "/link"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if calledTarget != "/target" || calledLink != "/link" {
			t.Fatalf("expected /target, /link but got %s, %s", calledTarget, calledLink)
		}
	})
}

func TestSysvinitHasRunlevelDirs(t *testing.T) {
	tests := []struct {
		name      string
		statPaths map[string]bool
		want      bool
	}{
		{
			name:      "returns true when /etc/rc0.d exists",
			statPaths: map[string]bool{"/etc/rc0.d": true},
			want:      true,
		},
		{
			name:      "returns false when /etc/rc0.d does not exist",
			statPaths: map[string]bool{},
			want:      false,
		},
		{
			name:      "returns false on stat error",
			statPaths: map[string]bool{},
			want:      false,
		},
		{
			name:      "returns true even with other paths missing",
			statPaths: map[string]bool{"/etc/rc0.d": true, "/etc/rc1.d": false},
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestSysvinitManager(func(m *sysvinitManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
			})

			got := m.hasRunlevelDirs()
			if got != tt.want {
				t.Errorf("hasRunlevelDirs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSysvinitHasRunlevelDirsPermissionDenied(t *testing.T) {
	m := newTestSysvinitManager(func(m *sysvinitManager) {
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/rc0.d" {
				return nil, os.ErrPermission
			}
			return nil, os.ErrNotExist
		}
	})

	if m.hasRunlevelDirs() {
		t.Error("hasRunlevelDirs() should return false on permission denied")
	}
}

func TestSysvinitScriptPaths(t *testing.T) {
	tests := []struct {
		name           string
		serviceName    string
		wantEnabled    string
		wantDisabled   string
		wantInitScript string
	}{
		{
			name:           "simple service name",
			serviceName:    "myapp",
			wantEnabled:    "/etc/init.d/S95myapp",
			wantDisabled:   "/etc/init.d/K95myapp",
			wantInitScript: "/etc/init.d/myapp",
		},
		{
			name:           "nanokvm service",
			serviceName:    "nanokvm",
			wantEnabled:    "/etc/init.d/S95nanokvm",
			wantDisabled:   "/etc/init.d/K95nanokvm",
			wantInitScript: "/etc/init.d/nanokvm",
		},
		{
			name:           "hyphenated service name",
			serviceName:    "my-daemon",
			wantEnabled:    "/etc/init.d/S95my-daemon",
			wantDisabled:   "/etc/init.d/K95my-daemon",
			wantInitScript: "/etc/init.d/my-daemon",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestSysvinitManager()
			m.serviceName = tt.serviceName

			if got := m.enabledScriptPath(); got != tt.wantEnabled {
				t.Errorf("enabledScriptPath() = %q, want %q", got, tt.wantEnabled)
			}
			if got := m.disabledScriptPath(); got != tt.wantDisabled {
				t.Errorf("disabledScriptPath() = %q, want %q", got, tt.wantDisabled)
			}
			if got := m.initScriptPath(); got != tt.wantInitScript {
				t.Errorf("initScriptPath() = %q, want %q", got, tt.wantInitScript)
			}
		})
	}
}

func TestSysvinitResolveScriptPath(t *testing.T) {
	tests := []struct {
		name     string
		existing map[string]bool
		wantPath string
	}{
		{
			name:     "initScriptPath exists",
			existing: map[string]bool{"/etc/init.d/myapp": true},
			wantPath: "/etc/init.d/myapp",
		},
		{
			name:     "enabledScriptPath exists",
			existing: map[string]bool{"/etc/init.d/S95myapp": true},
			wantPath: "/etc/init.d/S95myapp",
		},
		{
			name:     "disabledScriptPath exists",
			existing: map[string]bool{"/etc/init.d/K95myapp": true},
			wantPath: "/etc/init.d/K95myapp",
		},
		{
			name:     "initScriptPath preferred over others",
			existing: map[string]bool{"/etc/init.d/myapp": true, "/etc/init.d/S95myapp": true},
			wantPath: "/etc/init.d/myapp",
		},
		{
			name:     "enabledScriptPath preferred over disabled",
			existing: map[string]bool{"/etc/init.d/S95myapp": true, "/etc/init.d/K95myapp": true},
			wantPath: "/etc/init.d/S95myapp",
		},
		{
			name:     "falls back to initScriptPath when none exist",
			existing: map[string]bool{},
			wantPath: "/etc/init.d/myapp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestSysvinitManager(func(m *sysvinitManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.existing[path] {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
			})
			m.serviceName = "myapp"

			if got := m.resolveScriptPath(); got != tt.wantPath {
				t.Errorf("resolveScriptPath() = %q, want %q", got, tt.wantPath)
			}
		})
	}
}

func TestSysvinitGenerateConfigUserPIDFile(t *testing.T) {
	config := ServiceConfig{
		Name:     "myapp",
		ExecPath: "/opt/myapp/myapp",
		User:     "appuser",
	}
	content, err := GenerateSysvinitInitScript(config)
	if err != nil {
		t.Fatalf("GenerateSysvinitInitScript() error: %v", err)
	}
	if !strings.Contains(content, `echo "$DPID" > "$PIDFILE"`) {
		t.Error("PID file should be written as root outside the su subshell when User is set")
	}
	if !strings.Contains(content, `DPID=$(su -s /bin/sh`) {
		t.Error("PID should be captured from su subshell output")
	}
}

func TestSysvinitEnableBusyBox(t *testing.T) {
	tests := []struct {
		name       string
		statPaths  map[string]bool
		renameErr  error
		wantErr    bool
		wantErrMsg string
		wantFrom   string
		wantTo     string
		noRename   bool
	}{
		{
			name:      "already enabled - S95 exists",
			statPaths: map[string]bool{"/etc/init.d/S95myapp": true},
			noRename:  true,
		},
		{
			name:      "rename from K95 to S95",
			statPaths: map[string]bool{"/etc/init.d/K95myapp": true},
			wantFrom:  "/etc/init.d/K95myapp",
			wantTo:    "/etc/init.d/S95myapp",
		},
		{
			name:      "rename from init script to S95",
			statPaths: map[string]bool{"/etc/init.d/myapp": true},
			wantFrom:  "/etc/init.d/myapp",
			wantTo:    "/etc/init.d/S95myapp",
		},
		{
			name:       "no script found",
			statPaths:  map[string]bool{},
			wantErr:    true,
			wantErrMsg: "no init script found",
			noRename:   true,
		},
		{
			name:       "rename fails - permission denied",
			statPaths:  map[string]bool{"/etc/init.d/K95myapp": true},
			renameErr:  os.ErrPermission,
			wantErr:    true,
			wantErrMsg: "permission denied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var renamedFrom, renamedTo string
			m := newTestSysvinitManager(func(m *sysvinitManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
				m.renameFunc = func(from, to string) error {
					renamedFrom = from
					renamedTo = to
					if tt.renameErr != nil {
						return tt.renameErr
					}
					return nil
				}
			})
			m.serviceName = "myapp"
			m.supportsKSPrefix = true

			err := m.enableBusyBox()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.wantErrMsg != "" && !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("error %q should contain %q", err.Error(), tt.wantErrMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.noRename {
				if renamedFrom != "" {
					t.Errorf("expected no rename, but got rename from %q to %q", renamedFrom, renamedTo)
				}
				return
			}
			if renamedFrom != tt.wantFrom {
				t.Errorf("rename from = %q, want %q", renamedFrom, tt.wantFrom)
			}
			if renamedTo != tt.wantTo {
				t.Errorf("rename to = %q, want %q", renamedTo, tt.wantTo)
			}
		})
	}
}

func TestSysvinitDisableBusyBox(t *testing.T) {
	tests := []struct {
		name       string
		statPaths  map[string]bool
		renameErr  error
		wantErr    bool
		wantErrMsg string
		wantFrom   string
		wantTo     string
		noRename   bool
	}{
		{
			name:      "already disabled - K95 exists",
			statPaths: map[string]bool{"/etc/init.d/K95myapp": true},
			noRename:  true,
		},
		{
			name:      "rename from S95 to K95",
			statPaths: map[string]bool{"/etc/init.d/S95myapp": true},
			wantFrom:  "/etc/init.d/S95myapp",
			wantTo:    "/etc/init.d/K95myapp",
		},
		{
			name:      "rename from init script to K95",
			statPaths: map[string]bool{"/etc/init.d/myapp": true},
			wantFrom:  "/etc/init.d/myapp",
			wantTo:    "/etc/init.d/K95myapp",
		},
		{
			name:       "no script found",
			statPaths:  map[string]bool{},
			wantErr:    true,
			wantErrMsg: "no init script found",
			noRename:   true,
		},
		{
			name:       "rename fails - permission denied",
			statPaths:  map[string]bool{"/etc/init.d/S95myapp": true},
			renameErr:  os.ErrPermission,
			wantErr:    true,
			wantErrMsg: "permission denied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var renamedFrom, renamedTo string
			m := newTestSysvinitManager(func(m *sysvinitManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
				m.renameFunc = func(from, to string) error {
					renamedFrom = from
					renamedTo = to
					if tt.renameErr != nil {
						return tt.renameErr
					}
					return nil
				}
			})
			m.serviceName = "myapp"
			m.supportsKSPrefix = true

			err := m.disableBusyBox()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.wantErrMsg != "" && !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("error %q should contain %q", err.Error(), tt.wantErrMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.noRename {
				if renamedFrom != "" {
					t.Errorf("expected no rename, but got rename from %q to %q", renamedFrom, renamedTo)
				}
				return
			}
			if renamedFrom != tt.wantFrom {
				t.Errorf("rename from = %q, want %q", renamedFrom, tt.wantFrom)
			}
			if renamedTo != tt.wantTo {
				t.Errorf("rename to = %q, want %q", renamedTo, tt.wantTo)
			}
		})
	}
}

func TestSysvinitEnableSymlinks(t *testing.T) {
	tests := []struct {
		name         string
		statPaths    map[string]bool
		symlinkErr   error
		wantErr      bool
		wantErrMsg   string
		wantSymlinks map[string]string
		noSymlinks   bool
	}{
		{
			name:      "creates all symlinks",
			statPaths: map[string]bool{},
			wantSymlinks: map[string]string{
				"/etc/rc2.d/S20myapp": "/etc/init.d/myapp",
				"/etc/rc3.d/S20myapp": "/etc/init.d/myapp",
				"/etc/rc4.d/S20myapp": "/etc/init.d/myapp",
				"/etc/rc5.d/S20myapp": "/etc/init.d/myapp",
				"/etc/rc0.d/K20myapp": "/etc/init.d/myapp",
				"/etc/rc1.d/K20myapp": "/etc/init.d/myapp",
				"/etc/rc6.d/K20myapp": "/etc/init.d/myapp",
			},
		},
		{
			name: "skips existing symlinks",
			statPaths: map[string]bool{
				"/etc/rc2.d/S20myapp": true,
				"/etc/rc3.d/S20myapp": true,
				"/etc/rc0.d/K20myapp": true,
			},
			wantSymlinks: map[string]string{
				"/etc/rc4.d/S20myapp": "/etc/init.d/myapp",
				"/etc/rc5.d/S20myapp": "/etc/init.d/myapp",
				"/etc/rc1.d/K20myapp": "/etc/init.d/myapp",
				"/etc/rc6.d/K20myapp": "/etc/init.d/myapp",
			},
		},
		{
			name: "all symlinks already exist",
			statPaths: map[string]bool{
				"/etc/rc2.d/S20myapp": true,
				"/etc/rc3.d/S20myapp": true,
				"/etc/rc4.d/S20myapp": true,
				"/etc/rc5.d/S20myapp": true,
				"/etc/rc0.d/K20myapp": true,
				"/etc/rc1.d/K20myapp": true,
				"/etc/rc6.d/K20myapp": true,
			},
			noSymlinks: true,
		},
		{
			name:       "symlink creation fails",
			statPaths:  map[string]bool{},
			symlinkErr: os.ErrPermission,
			wantErr:    true,
			wantErrMsg: "creating symlink",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			createdSymlinks := make(map[string]string)
			m := newTestSysvinitManager(func(m *sysvinitManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
				m.symlinkFunc = func(target, link string) error {
					if tt.symlinkErr != nil {
						return tt.symlinkErr
					}
					createdSymlinks[link] = target
					return nil
				}
			})
			m.serviceName = "myapp"

			err := m.enableSymlinks()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.wantErrMsg != "" && !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("error %q should contain %q", err.Error(), tt.wantErrMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.noSymlinks {
				if len(createdSymlinks) != 0 {
					t.Errorf("expected no symlinks created, got %d", len(createdSymlinks))
				}
				return
			}
			if len(createdSymlinks) != len(tt.wantSymlinks) {
				t.Errorf("created %d symlinks, want %d", len(createdSymlinks), len(tt.wantSymlinks))
			}
			for link, target := range tt.wantSymlinks {
				if got, ok := createdSymlinks[link]; !ok {
					t.Errorf("expected symlink %s not created", link)
				} else if got != target {
					t.Errorf("symlink %s target = %q, want %q", link, got, target)
				}
			}
		})
	}
}

func TestSysvinitDisableSymlinks(t *testing.T) {
	tests := []struct {
		name        string
		removeErr   error
		wantErr     bool
		wantErrMsg  string
		wantRemoved []string
	}{
		{
			name: "removes all symlinks",
			wantRemoved: []string{
				"/etc/rc2.d/S20myapp",
				"/etc/rc3.d/S20myapp",
				"/etc/rc4.d/S20myapp",
				"/etc/rc5.d/S20myapp",
				"/etc/rc0.d/K20myapp",
				"/etc/rc1.d/K20myapp",
				"/etc/rc6.d/K20myapp",
			},
		},
		{
			name:      "already disabled - symlinks do not exist",
			removeErr: os.ErrNotExist,
		},
		{
			name:       "partial failure - remove returns permission error",
			removeErr:  os.ErrPermission,
			wantErr:    true,
			wantErrMsg: "removing symlink",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newMockFS()
			if tt.removeErr != nil {
				fs.removeErr = tt.removeErr
			}
			m := newTestSysvinitManager(func(m *sysvinitManager) {
				m.fs = fs
			})
			m.serviceName = "myapp"

			err := m.disableSymlinks()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.wantErrMsg != "" && !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("error %q should contain %q", err.Error(), tt.wantErrMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, path := range tt.wantRemoved {
				if !fs.removed[path] {
					t.Errorf("expected %s to be removed", path)
				}
			}
		})
	}
}

func TestSysvinitSupportsCapabilities(t *testing.T) {
	t.Run("with runlevel dirs", func(t *testing.T) {
		mgr := newTestSysvinitManager(func(m *sysvinitManager) {
			m.statFunc = func(path string) (os.FileInfo, error) {
				if path == "/etc/rc0.d" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}
		})
		if !mgr.SupportsEnable() {
			t.Error("SupportsEnable() = false, want true")
		}
		if !mgr.SupportsStart() {
			t.Error("SupportsStart() = false, want true")
		}
		if !mgr.SupportsStatus() {
			t.Error("SupportsStatus() = false, want true")
		}
	})

	t.Run("BusyBox mode with K/S support", func(t *testing.T) {
		mgr := newTestSysvinitManager(func(m *sysvinitManager) {
			m.supportsKSPrefix = true
		})
		if !mgr.SupportsEnable() {
			t.Error("SupportsEnable() = false, want true")
		}
	})

	t.Run("BusyBox mode without K/S support", func(t *testing.T) {
		mgr := newTestSysvinitManager()
		if !mgr.SupportsEnable() {
			t.Error("SupportsEnable() = false, want true (flag file fallback)")
		}
	})
}

func TestSysvinitDetectKSPrefixSupport(t *testing.T) {
	tests := []struct {
		name      string
		statPaths map[string]bool
		want      bool
	}{
		{
			name:      "rcS exists",
			statPaths: map[string]bool{"/etc/init.d/rcS": true},
			want:      true,
		},
		{
			name:      "rcK exists",
			statPaths: map[string]bool{"/etc/init.d/rcK": true},
			want:      true,
		},
		{
			name:      "rc.d exists",
			statPaths: map[string]bool{"/etc/rc.d": true},
			want:      true,
		},
		{
			name:      "none exist",
			statPaths: map[string]bool{},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestSysvinitManager(func(m *sysvinitManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
			})
			got := m.detectKSPrefixSupport()
			if got != tt.want {
				t.Errorf("detectKSPrefixSupport() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSysvinitInstallBusyBoxCleanup(t *testing.T) {
	t.Run("cleans up K file", func(t *testing.T) {
		fs := newMockFS()
		m := newTestSysvinitManager(func(m *sysvinitManager) {
			m.fs = fs
			m.statFunc = func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/rcS" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}
		})
		err := m.Install(ServiceConfig{
			Name:     "test-svc",
			ExecPath: "/opt/test-svc/test-svc",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !fs.removed["/etc/init.d/K95test-svc"] {
			t.Error("expected K95 file to be cleaned up")
		}
	})

	t.Run("cleans up S file", func(t *testing.T) {
		fs := newMockFS()
		m := newTestSysvinitManager(func(m *sysvinitManager) {
			m.fs = fs
			m.statFunc = func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/rcS" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}
		})
		err := m.Install(ServiceConfig{
			Name:     "test-svc",
			ExecPath: "/opt/test-svc/test-svc",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !fs.removed["/etc/init.d/S95test-svc"] {
			t.Error("expected S95 file to be cleaned up")
		}
	})

	t.Run("cleans up both K and S files", func(t *testing.T) {
		fs := newMockFS()
		m := newTestSysvinitManager(func(m *sysvinitManager) {
			m.fs = fs
			m.statFunc = func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/rcS" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}
		})
		err := m.Install(ServiceConfig{
			Name:     "test-svc",
			ExecPath: "/opt/test-svc/test-svc",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !fs.removed["/etc/init.d/K95test-svc"] {
			t.Error("expected K95 file to be cleaned up")
		}
		if !fs.removed["/etc/init.d/S95test-svc"] {
			t.Error("expected S95 file to be cleaned up")
		}
		if !fs.removed["/etc/init.d/test-svc"] {
			t.Error("expected plain script to be cleaned up")
		}
	})

	t.Run("writes to K95 with K/S support", func(t *testing.T) {
		fs := newMockFS()
		m := newTestSysvinitManager(func(m *sysvinitManager) {
			m.fs = fs
			m.statFunc = func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/rcS" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}
		})
		err := m.Install(ServiceConfig{
			Name:     "test-svc",
			ExecPath: "/opt/test-svc/test-svc",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := fs.files["/etc/init.d/K95test-svc"]; !ok {
			t.Error("expected script at K95 path")
		}
	})

	t.Run("writes to plain path with flag file when no K/S support", func(t *testing.T) {
		fs := newMockFS()
		m := newTestSysvinitManager(func(m *sysvinitManager) {
			m.fs = fs
		})
		err := m.Install(ServiceConfig{
			Name:     "test-svc",
			ExecPath: "/opt/test-svc/test-svc",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := fs.files["/etc/init.d/test-svc"]; !ok {
			t.Error("expected script at plain path")
		}
		if _, ok := fs.files["/etc/init.d/test-svc.disabled"]; !ok {
			t.Error("expected disabled flag file")
		}
	})
}

func TestSysvinitEnableBusyBoxFlagFile(t *testing.T) {
	t.Run("removes flag file when no K/S support", func(t *testing.T) {
		fs := newMockFS()
		m := newTestSysvinitManager(func(m *sysvinitManager) {
			m.serviceName = "test-svc"
			m.supportsKSPrefix = false
			m.fs = fs
		})
		err := m.enableBusyBox()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !fs.removed["/etc/init.d/test-svc.disabled"] {
			t.Error("expected flag file to be removed")
		}
	})

	t.Run("renames K95 to S95 with K/S support", func(t *testing.T) {
		var renamed [2]string
		m := newTestSysvinitManager(func(m *sysvinitManager) {
			m.serviceName = "test-svc"
			m.supportsKSPrefix = true
			m.statFunc = func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/K95test-svc" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}
			m.renameFunc = func(from, to string) error {
				renamed = [2]string{from, to}
				return nil
			}
		})
		err := m.enableBusyBox()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if renamed[0] != "/etc/init.d/K95test-svc" || renamed[1] != "/etc/init.d/S95test-svc" {
			t.Errorf("unexpected rename: %v -> %v", renamed[0], renamed[1])
		}
	})
}

func TestSysvinitDisableBusyBoxFlagFile(t *testing.T) {
	t.Run("creates flag file when no K/S support", func(t *testing.T) {
		fs := newMockFS()
		m := newTestSysvinitManager(func(m *sysvinitManager) {
			m.serviceName = "test-svc"
			m.supportsKSPrefix = false
			m.fs = fs
		})
		err := m.disableBusyBox()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		data, ok := fs.files["/etc/init.d/test-svc.disabled"]
		if !ok {
			t.Error("expected flag file to be created")
		}
		if string(data) != "disabled\n" {
			t.Errorf("unexpected flag file content: %q", string(data))
		}
	})

	t.Run("renames S95 to K95 with K/S support", func(t *testing.T) {
		var renamed [2]string
		m := newTestSysvinitManager(func(m *sysvinitManager) {
			m.serviceName = "test-svc"
			m.supportsKSPrefix = true
			m.statFunc = func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/S95test-svc" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}
			m.renameFunc = func(from, to string) error {
				renamed = [2]string{from, to}
				return nil
			}
		})
		err := m.disableBusyBox()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if renamed[0] != "/etc/init.d/S95test-svc" || renamed[1] != "/etc/init.d/K95test-svc" {
			t.Errorf("unexpected rename: %v -> %v", renamed[0], renamed[1])
		}
	})
}

func TestSysvinitInitScriptTemplateFlagFileCheck(t *testing.T) {
	config := ServiceConfig{
		Name:     "test-svc",
		ExecPath: "/opt/test-svc/test-svc",
	}
	script, err := GenerateSysvinitInitScript(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(script, `/etc/init.d/$NAME.disabled`) {
		t.Error("generated script missing flag file check")
	}
	if !strings.Contains(script, `[ -f "/etc/init.d/$NAME.disabled" ] && exit 0`) {
		t.Error("generated script missing correct flag file check syntax")
	}
}
