//go:build !windows

package daemon

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// requireLinuxDetection skips tests that exercise busyboxManager.detect, which
// returns false immediately on any non-linux host.
func requireLinuxDetection(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("BusyBox init detection is linux-only")
	}
}

func newTestBusyboxManager(opts ...func(*busyboxManager)) *busyboxManager {
	m := &busyboxManager{
		executor:        newMockExecutor(),
		fs:              newMockFS(),
		statFunc:        func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		readlinkFunc:    func(string) (string, error) { return "", os.ErrNotExist },
		lookPathFunc:    func(string) (string, error) { return "", errors.New("not found") },
		readFileFunc:    func(string) ([]byte, error) { return nil, os.ErrNotExist },
		killFunc:        func(int, syscall.Signal) error { return nil },
		readDirFunc:     func(string) ([]os.DirEntry, error) { return nil, os.ErrNotExist },
		sleepFunc:       func(time.Duration) {},
		renameFunc:      func(string, string) error { return nil },
		gracefulTimeout: 100 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func TestBusyboxDetect(t *testing.T) {
	requireLinuxDetection(t)
	tests := []struct {
		name      string
		statPaths map[string]bool
		readlinks map[string]string
		lookPaths map[string]bool
		initHelp  string
		readFiles map[string][]byte
		want      bool
	}{
		{
			name:      "proc/1/exe links to busybox",
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      true,
		},
		{
			name:      "sbin/init links to busybox",
			readlinks: map[string]string{"/sbin/init": "/bin/busybox"},
			want:      true,
		},
		{
			name:      "proc/1/exe links to busybox with path variant",
			readlinks: map[string]string{"/proc/1/exe": "/usr/bin/busybox"},
			want:      true,
		},
		{
			name:      "excluded when systemd present",
			statPaths: map[string]bool{"/run/systemd/system": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      false,
		},
		{
			name:      "excluded when openrc rc-service available",
			lookPaths: map[string]bool{"rc-service": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      false,
		},
		{
			name:      "excluded when /sbin/openrc exists",
			statPaths: map[string]bool{"/sbin/openrc": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      false,
		},
		{
			name:      "excluded when /sbin/openrc-run exists",
			statPaths: map[string]bool{"/sbin/openrc-run": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      false,
		},
		{
			name:      "excluded when /bin/openrc exists",
			statPaths: map[string]bool{"/bin/openrc": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      false,
		},
		{
			name:      "excluded when runit runsvdir in path and /var/service exists",
			lookPaths: map[string]bool{"runsvdir": true},
			statPaths: map[string]bool{"/var/service": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      false,
		},
		{
			name:      "excluded when runit /sbin/runsvdir exists and /var/service exists",
			statPaths: map[string]bool{"/sbin/runsvdir": true, "/var/service": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      false,
		},
		{
			name:      "excluded when runit /usr/bin/runsvdir exists and /var/service exists",
			statPaths: map[string]bool{"/usr/bin/runsvdir": true, "/var/service": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      false,
		},
		{
			name:      "not excluded when /usr/bin/runsvdir exists but /var/service absent",
			statPaths: map[string]bool{"/usr/bin/runsvdir": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      true,
		},
		{
			name:      "not excluded when runsvdir in path but /var/service absent",
			lookPaths: map[string]bool{"runsvdir": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      true,
		},
		{
			name:      "not excluded when /sbin/runsvdir exists but /var/service absent",
			statPaths: map[string]bool{"/sbin/runsvdir": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      true,
		},
		{
			name:      "excluded when s6 s6-svscan in path",
			lookPaths: map[string]bool{"s6-svscan": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      false,
		},
		{
			name:      "excluded when s6 /usr/bin/s6-svscan exists",
			statPaths: map[string]bool{"/usr/bin/s6-svscan": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      false,
		},
		{
			name:      "excluded when procd present",
			statPaths: map[string]bool{"/sbin/procd": true, "/lib/functions/procd.sh": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      false,
		},
		{
			name:      "not excluded when only /sbin/procd without procd.sh",
			statPaths: map[string]bool{"/sbin/procd": true},
			readlinks: map[string]string{"/proc/1/exe": "/bin/busybox"},
			want:      true,
		},
		{
			name:      "symlink chain from proc/1/exe target resolves to busybox",
			readlinks: map[string]string{"/proc/1/exe": "/sbin/init", "/sbin/init": "/bin/busybox"},
			want:      true,
		},
		{
			name:      "symlink chain from proc/1/exe target resolves to non-busybox",
			readlinks: map[string]string{"/proc/1/exe": "/sbin/init", "/sbin/init": "/usr/sbin/init"},
			want:      false,
		},
		{
			name:      "nanokvm scenario - binary content contains BusyBox",
			readlinks: map[string]string{"/proc/1/exe": "/usr/sbin/init"},
			readFiles: map[string][]byte{"/usr/sbin/init": []byte("\x7fELF\x00\x00BusyBox v1.36.1")},
			want:      true,
		},
		{
			name:      "nanokvm scenario - binary content does not contain BusyBox",
			readlinks: map[string]string{"/proc/1/exe": "/usr/sbin/init"},
			readFiles: map[string][]byte{"/usr/sbin/init": []byte("\x7fELF\x00\x00some other init")},
			want:      false,
		},
		{
			name:      "binary content fallback - file not readable",
			readlinks: map[string]string{"/proc/1/exe": "/usr/sbin/init"},
			want:      false,
		},
		{
			name: "not detected when init is not busybox",
			want: false,
		},
		{
			name:      "proc/1/exe links to non-busybox",
			readlinks: map[string]string{"/proc/1/exe": "/sbin/init"},
			want:      false,
		},
		{
			name:      "fallback to /sbin/init --help containing busybox",
			statPaths: map[string]bool{"/sbin/init": true},
			initHelp:  "BusyBox v1.36.1 (2024-01-01) multi-call binary",
			want:      true,
		},
		{
			name:      "fallback /sbin/init --help not busybox",
			statPaths: map[string]bool{"/sbin/init": true},
			initHelp:  "some other init system",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := newMockExecutor()
			if tt.initHelp != "" {
				executor.setResult("/sbin/init --help", []byte(tt.initHelp), nil)
			}

			m := newTestBusyboxManager(func(m *busyboxManager) {
				m.executor = executor
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
				m.lookPathFunc = func(name string) (string, error) {
					if tt.lookPaths[name] {
						return "/usr/bin/" + name, nil
					}
					return "", errors.New("not found")
				}
				if tt.readFiles != nil {
					m.readFileFunc = func(path string) ([]byte, error) {
						if data, ok := tt.readFiles[path]; ok {
							return data, nil
						}
						return nil, os.ErrNotExist
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

func TestIsBusyboxPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "direct busybox binary", path: "/bin/busybox", want: true},
		{name: "usr bin busybox", path: "/usr/bin/busybox", want: true},
		{name: "busybox in directory path", path: "/opt/busybox/init", want: true},
		{name: "case insensitive BusyBox", path: "/bin/BusyBox", want: true},
		{name: "case insensitive BUSYBOX", path: "/sbin/BUSYBOX", want: true},
		{name: "busybox with version suffix", path: "/bin/busybox-1.36", want: true},
		{name: "regular init path", path: "/sbin/init", want: false},
		{name: "usr sbin init", path: "/usr/sbin/init", want: false},
		{name: "systemd", path: "/lib/systemd/systemd", want: false},
		{name: "empty path", path: "", want: false},
		{name: "just slash", path: "/", want: false},
		{name: "special characters", path: "/sbin/init!@#$%", want: false},
		{name: "path with spaces", path: "/opt/my app/init", want: false},
		{name: "partial match busy", path: "/bin/busy", want: false},
		{name: "partial match box", path: "/bin/box", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isBusyboxPath(tt.path)
			if got != tt.want {
				t.Errorf("isBusyboxPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestResolveSymlinks(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		links    map[string]string
		maxDepth int
		want     string
		wantErr  bool
	}{
		{
			name:     "not a symlink",
			path:     "/usr/sbin/init",
			links:    map[string]string{},
			maxDepth: 10,
			want:     "/usr/sbin/init",
		},
		{
			name:     "single symlink",
			path:     "/sbin/init",
			links:    map[string]string{"/sbin/init": "/bin/busybox"},
			maxDepth: 10,
			want:     "/bin/busybox",
		},
		{
			name: "two level chain",
			path: "/sbin/init",
			links: map[string]string{
				"/sbin/init":     "/usr/sbin/init",
				"/usr/sbin/init": "/bin/busybox",
			},
			maxDepth: 10,
			want:     "/bin/busybox",
		},
		{
			name: "three level chain",
			path: "/a",
			links: map[string]string{
				"/a": "/b",
				"/b": "/c",
				"/c": "/bin/busybox",
			},
			maxDepth: 10,
			want:     "/bin/busybox",
		},
		{
			name: "relative symlink resolved",
			path: "/sbin/init",
			links: map[string]string{
				"/sbin/init": "../bin/busybox",
			},
			maxDepth: 10,
			want:     "/bin/busybox",
		},
		{
			name: "circular symlinks hit max depth",
			path: "/a",
			links: map[string]string{
				"/a": "/b",
				"/b": "/a",
			},
			maxDepth: 10,
			wantErr:  true,
		},
		{
			name: "self-referencing symlink",
			path: "/a",
			links: map[string]string{
				"/a": "/a",
			},
			maxDepth: 10,
			wantErr:  true,
		},
		{
			name: "chain resolved within depth limit",
			path: "/a",
			links: map[string]string{
				"/a": "/b",
				"/b": "/c",
			},
			maxDepth: 3,
			want:     "/c",
		},
		{
			name: "chain exceeds depth limit",
			path: "/a",
			links: map[string]string{
				"/a": "/b",
				"/b": "/c",
				"/c": "/d",
			},
			maxDepth: 2,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestBusyboxManager(func(m *busyboxManager) {
				m.readlinkFunc = func(path string) (string, error) {
					if target, ok := tt.links[path]; ok {
						return target, nil
					}
					return "", os.ErrNotExist
				}
			})

			got, err := m.resolveSymlinks(tt.path, tt.maxDepth)
			if tt.wantErr {
				if err == nil {
					t.Errorf("resolveSymlinks() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Errorf("resolveSymlinks() error = %v, want nil", err)
				return
			}
			if got != tt.want {
				t.Errorf("resolveSymlinks() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckBinaryForBusyBox(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		fileData []byte
		fileErr  error
		want     bool
	}{
		{
			name:     "contains BusyBox uppercase",
			path:     "/usr/sbin/init",
			fileData: []byte("\x7fELF\x00\x00BusyBox v1.36.1 (2024-01-01)"),
			want:     true,
		},
		{
			name:     "contains busybox lowercase",
			path:     "/usr/sbin/init",
			fileData: []byte("\x7fELF\x00\x00busybox multi-call binary"),
			want:     true,
		},
		{
			name:     "no busybox string",
			path:     "/usr/sbin/init",
			fileData: []byte("\x7fELF\x00\x00systemd version 252"),
			want:     false,
		},
		{
			name:    "file not found",
			path:    "/usr/sbin/init",
			fileErr: os.ErrNotExist,
			want:    false,
		},
		{
			name:    "permission denied",
			path:    "/usr/sbin/init",
			fileErr: os.ErrPermission,
			want:    false,
		},
		{
			name:     "empty file",
			path:     "/usr/sbin/init",
			fileData: []byte{},
			want:     false,
		},
		{
			name:     "BusyBox at byte 500 within 512 limit",
			path:     "/usr/sbin/init",
			fileData: append(make([]byte, 500), []byte("BusyBox")...),
			want:     true,
		},
		{
			name:     "busybox beyond 512 byte limit",
			path:     "/usr/sbin/init",
			fileData: append(make([]byte, 510), []byte("BusyBox")...),
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestBusyboxManager(func(m *busyboxManager) {
				m.readFileFunc = func(path string) ([]byte, error) {
					if tt.fileErr != nil {
						return nil, tt.fileErr
					}
					return tt.fileData, nil
				}
			})

			got := m.checkBinaryForBusyBox(tt.path)
			if got != tt.want {
				t.Errorf("checkBinaryForBusyBox(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestBusyboxCanRestartWatchdog(t *testing.T) {
	m := newTestBusyboxManager()
	if m.CanRestartWatchdog() {
		t.Error("CanRestartWatchdog() = true, want false")
	}
}

func TestBusyboxDetectKSPrefixSupport(t *testing.T) {
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
			name:      "rcS and rc.d both exist",
			statPaths: map[string]bool{"/etc/init.d/rcS": true, "/etc/rc.d": true},
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
			m := newTestBusyboxManager(func(m *busyboxManager) {
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

func TestBusyboxSupportsCapabilities(t *testing.T) {
	m := newTestBusyboxManager()
	m.supportsKSPrefix = false
	if !m.SupportsEnable() {
		t.Error("SupportsEnable() = false, want true (flag file fallback)")
	}

	m.supportsKSPrefix = true
	if !m.SupportsEnable() {
		t.Error("SupportsEnable() = false, want true (K/S rename)")
	}

	if !m.SupportsStart() {
		t.Error("SupportsStart() = false, want true")
	}
	if !m.SupportsStatus() {
		t.Error("SupportsStatus() = false, want true")
	}
}

func TestBusyboxInstallSetsKSPrefix(t *testing.T) {
	tests := []struct {
		name      string
		statPaths map[string]bool
		wantKS    bool
	}{
		{
			name:      "with rcS support",
			statPaths: map[string]bool{"/etc/init.d/rcS": true},
			wantKS:    true,
		},
		{
			name:      "without K/S support",
			statPaths: map[string]bool{},
			wantKS:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockFS := newMockFS()
			m := newTestBusyboxManager(func(m *busyboxManager) {
				m.fs = mockFS
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
			})

			err := m.Install(ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp/myapp"})
			if err != nil {
				t.Fatalf("Install() error = %v", err)
			}

			if m.supportsKSPrefix != tt.wantKS {
				t.Errorf("supportsKSPrefix = %v, want %v", m.supportsKSPrefix, tt.wantKS)
			}
		})
	}
}

func TestBusyboxName(t *testing.T) {
	m := newTestBusyboxManager()
	if got := m.Name(); got != "busybox" {
		t.Errorf("Name() = %q, want %q", got, "busybox")
	}
}

func TestBusyboxImplementsInterface(t *testing.T) {
	var _ ServiceManager = (*busyboxManager)(nil)
}

func TestBusyboxGetManifestPath(t *testing.T) {
	m := newTestBusyboxManager()
	m.serviceName = "myapp"
	want := "/etc/init.d/S95myapp"
	if got := m.GetManifestPath(); got != want {
		t.Errorf("GetManifestPath() = %q, want %q", got, want)
	}
}

func TestGenerateBusyboxInitScript(t *testing.T) {
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
				`DAEMON='/opt/myapp/myapp'`,
				`NAME="myapp"`,
				`PIDFILE="/var/run/${NAME}.pid"`,
				`"${DAEMON}" 'run' &`,
				`echo "${DPID}" > "${PIDFILE}"`,
				`kill $(cat "${PIDFILE}")`,
				`rm -f "${PIDFILE}"`,
				`(supervise) </dev/null >/dev/null 2>&1 &`,
				"start)",
				"stop)",
				"restart)",
			},
		},
		{
			name: "script contains flag file check",
			config: ServiceConfig{
				Name: "myapp",
			},
			execPath: "/opt/myapp/myapp",
			contains: []string{
				`if [ -f "/etc/init.d/myapp.disabled" ]; then exit 0; fi`,
			},
		},
		{
			name: "flag file check uses service name",
			config: ServiceConfig{
				Name: "daemonpilot",
			},
			execPath: "/opt/daemonpilot/daemonpilot",
			contains: []string{
				`if [ -f "/etc/init.d/daemonpilot.disabled" ]; then exit 0; fi`,
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
				`DAEMON='/usr/local/bin/myapp'`,
				`"${DAEMON}" 'run' '--config' '/etc/myapp.conf' &`,
			},
		},
		{
			name: "with environment variables",
			config: ServiceConfig{
				Name:        "myapp",
				Environment: map[string]string{"LOG_LEVEL": "info"},
			},
			execPath: "/opt/myapp/myapp",
			contains: []string{
				`export LOG_LEVEL='info'`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := GenerateBusyboxInitScript(tt.config, tt.execPath)
			if err != nil {
				t.Fatalf("GenerateBusyboxInitScript() error = %v", err)
			}
			for _, s := range tt.contains {
				if !strings.Contains(content, s) {
					t.Errorf("script missing %q\ngot:\n%s", s, content)
				}
			}
		})
	}
}

func TestBusyboxInitScriptFlagCheckPosition(t *testing.T) {
	config := ServiceConfig{Name: "myapp"}
	content, err := GenerateBusyboxInitScript(config, "/opt/myapp/myapp")
	if err != nil {
		t.Fatalf("GenerateBusyboxInitScript() error = %v", err)
	}

	flagCheck := `if [ -f "/etc/init.d/myapp.disabled" ]; then exit 0; fi`
	flagIdx := strings.Index(content, flagCheck)
	if flagIdx == -1 {
		t.Fatalf("script missing flag file check\ngot:\n%s", content)
	}

	shebangIdx := strings.Index(content, "#!/bin/sh")
	caseIdx := strings.Index(content, `case "$1" in`)

	if flagIdx <= shebangIdx {
		t.Errorf("flag check should appear after shebang")
	}
	if flagIdx >= caseIdx {
		t.Errorf("flag check should appear before case statement")
	}
}

func TestBusyboxExecPath(t *testing.T) {
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
			m := newTestBusyboxManager()
			if got := m.execPath(tt.config); got != tt.want {
				t.Errorf("execPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBusyboxInstall(t *testing.T) {
	tests := []struct {
		name      string
		config    ServiceConfig
		writeErr  error
		wantErr   bool
		errSubstr string
	}{
		{
			name: "successful install",
			config: ServiceConfig{
				Name:     "myapp",
				ExecPath: "/opt/myapp/myapp",
			},
		},
		{
			name: "write failure",
			config: ServiceConfig{
				Name:     "myapp",
				ExecPath: "/opt/myapp/myapp",
			},
			writeErr:  errors.New("permission denied"),
			wantErr:   true,
			errSubstr: "writing init script",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockFS := newMockFS()
			mockFS.writeErr = tt.writeErr
			executor := newMockExecutor()

			m := newTestBusyboxManager(func(m *busyboxManager) {
				m.fs = mockFS
				m.executor = executor
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

			scriptPath := "/etc/init.d/myapp"
			if _, ok := mockFS.files[scriptPath]; !ok {
				t.Error("init script not written")
			}

			content := string(mockFS.files[scriptPath])
			if !strings.Contains(content, "#!/bin/sh") {
				t.Error("init script missing shebang")
			}
		})
	}
}

func TestBusyboxInstallCleansUpKFile(t *testing.T) {
	mockFS := newMockFS()
	mockFS.files["/etc/init.d/K95myapp"] = []byte("old script")

	m := newTestBusyboxManager(func(m *busyboxManager) {
		m.fs = mockFS
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/init.d/rcS" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
	})

	err := m.Install(ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp/myapp"})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	if !mockFS.removed["/etc/init.d/K95myapp"] {
		t.Error("Install() did not clean up existing K95 file")
	}
}

func TestBusyboxInstallCleansUpSFile(t *testing.T) {
	mockFS := newMockFS()
	mockFS.files["/etc/init.d/S95myapp"] = []byte("old script")

	m := newTestBusyboxManager(func(m *busyboxManager) {
		m.fs = mockFS
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/init.d/rcS" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
	})

	err := m.Install(ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp/myapp"})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	if !mockFS.removed["/etc/init.d/S95myapp"] {
		t.Error("Install() did not clean up existing S95 file")
	}
}

func TestBusyboxInstallCleansUpBothFiles(t *testing.T) {
	mockFS := newMockFS()
	mockFS.files["/etc/init.d/K95myapp"] = []byte("old K script")
	mockFS.files["/etc/init.d/S95myapp"] = []byte("old S script")

	m := newTestBusyboxManager(func(m *busyboxManager) {
		m.fs = mockFS
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/init.d/rcS" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
	})

	err := m.Install(ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp/myapp"})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	if !mockFS.removed["/etc/init.d/K95myapp"] {
		t.Error("Install() did not clean up existing K95 file")
	}
	if !mockFS.removed["/etc/init.d/S95myapp"] {
		t.Error("Install() did not clean up existing S95 file")
	}
	if !mockFS.removed["/etc/init.d/myapp"] {
		t.Error("Install() did not clean up existing plain script file")
	}
}

func TestBusyboxInstallWritesToKSPath(t *testing.T) {
	mockFS := newMockFS()

	m := newTestBusyboxManager(func(m *busyboxManager) {
		m.fs = mockFS
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/init.d/rcS" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
	})

	err := m.Install(ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp/myapp"})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	if _, ok := mockFS.files["/etc/init.d/K95myapp"]; !ok {
		t.Error("Install() with K/S support should write to K95 path (disabled by default)")
	}
	if _, ok := mockFS.files["/etc/init.d/S95myapp"]; ok {
		t.Error("Install() with K/S support should not write to S95 path")
	}
}

func TestBusyboxInstallWritesToPlainPath(t *testing.T) {
	mockFS := newMockFS()

	m := newTestBusyboxManager(func(m *busyboxManager) {
		m.fs = mockFS
	})

	err := m.Install(ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp/myapp"})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	if _, ok := mockFS.files["/etc/init.d/myapp"]; !ok {
		t.Error("Install() without K/S support should write to plain path")
	}
	if _, ok := mockFS.files["/etc/init.d/myapp.disabled"]; !ok {
		t.Error("Install() without K/S support should create disabled flag file")
	}
}

func TestBusyboxUninstall(t *testing.T) {
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

			m := newTestBusyboxManager(func(m *busyboxManager) {
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

			if !executor.called("/etc/init.d/S95myapp", "stop") {
				t.Error("service not stopped before uninstall")
			}

			if !mockFS.removed["/etc/init.d/S95myapp"] {
				t.Error("init script not removed")
			}

			if !mockFS.removed["/etc/init.d/K95myapp"] {
				t.Error("disabled init script not removed")
			}

			if !mockFS.removed["/var/run/myapp.pid"] {
				t.Error("PID file not removed")
			}
		})
	}
}

func TestBusyboxGenerateConfig(t *testing.T) {
	m := newTestBusyboxManager()
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

func TestBusyboxWriteConfig(t *testing.T) {
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
			m := newTestBusyboxManager(func(m *busyboxManager) {
				m.fs = mockFS
			})
			m.serviceName = tt.serviceName

			err := m.WriteConfig("#!/bin/sh\n# test")
			if tt.wantErr {
				if err == nil {
					t.Fatal("WriteConfig() expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("WriteConfig() unexpected error: %v", err)
			}
			if _, ok := mockFS.files["/etc/init.d/S95myapp"]; !ok {
				t.Error("config not written to expected path")
			}
		})
	}
}

func TestBusyboxPidFilePath(t *testing.T) {
	m := newTestBusyboxManager()
	m.serviceName = "myapp"
	want := "/var/run/myapp.pid"
	if got := m.pidFilePath(); got != want {
		t.Errorf("pidFilePath() = %q, want %q", got, want)
	}
}

func TestBusyboxStart(t *testing.T) {
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
				executor.setResult("/etc/init.d/S95myapp start", nil, tt.execErr)
			}

			m := newTestBusyboxManager(func(m *busyboxManager) {
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

			if !executor.called("/etc/init.d/S95myapp", "start") {
				t.Error("init script not called with start")
			}
		})
	}
}

type mockDirEntry struct {
	name  string
	isDir bool
}

func (e mockDirEntry) Name() string               { return e.name }
func (e mockDirEntry) IsDir() bool                { return e.isDir }
func (e mockDirEntry) Type() fs.FileMode          { return 0 }
func (e mockDirEntry) Info() (fs.FileInfo, error) { return nil, nil }

func TestBusyboxStop(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		pidContent  string
		pidErr      error
		killResults map[int]map[syscall.Signal]error
		procEntries []os.DirEntry
		readlinks   map[string]string
		wantErr     bool
		errSubstr   string
	}{
		{
			name:        "successful stop via PID file - process exits after SIGTERM",
			serviceName: "myapp",
			pidContent:  "1234\n",
			readlinks: map[string]string{
				"/proc/1234/exe": "/opt/myapp/myapp",
			},
			killResults: map[int]map[syscall.Signal]error{
				1234: {
					syscall.SIGTERM: nil,
					0:               syscall.ESRCH,
				},
			},
		},
		{
			name:        "successful stop - binary replaced, exe shows deleted suffix",
			serviceName: "myapp",
			pidContent:  "1234\n",
			readlinks: map[string]string{
				"/proc/1234/exe": "/opt/myapp/myapp (deleted)",
			},
			killResults: map[int]map[syscall.Signal]error{
				1234: {
					syscall.SIGTERM: nil,
					0:               syscall.ESRCH,
				},
			},
		},
		{
			name:      "no service name",
			wantErr:   true,
			errSubstr: "service name not set",
		},
		{
			name:        "pid file missing - fallback to /proc search",
			serviceName: "myapp",
			pidErr:      os.ErrNotExist,
			procEntries: []os.DirEntry{
				mockDirEntry{name: "1"},
				mockDirEntry{name: "567"},
				mockDirEntry{name: "self"},
			},
			readlinks: map[string]string{
				"/proc/567/exe": "/opt/myapp/myapp",
			},
			killResults: map[int]map[syscall.Signal]error{
				567: {
					syscall.SIGTERM: nil,
					0:               syscall.ESRCH,
				},
			},
		},
		{
			name:        "pid file missing - no process found",
			serviceName: "myapp",
			pidErr:      os.ErrNotExist,
			procEntries: []os.DirEntry{
				mockDirEntry{name: "1"},
			},
			readlinks: map[string]string{
				"/proc/1/exe": "/sbin/init",
			},
			wantErr:   true,
			errSubstr: "finding process",
		},
		{
			name:        "sigterm fails",
			serviceName: "myapp",
			pidContent:  "1234\n",
			readlinks: map[string]string{
				"/proc/1234/exe": "/opt/myapp/myapp",
			},
			killResults: map[int]map[syscall.Signal]error{
				1234: {
					syscall.SIGTERM: errors.New("operation not permitted"),
				},
			},
			wantErr:   true,
			errSubstr: "sending SIGTERM",
		},
		{
			name:        "process needs SIGKILL after timeout",
			serviceName: "myapp",
			pidContent:  "1234\n",
			readlinks: map[string]string{
				"/proc/1234/exe": "/opt/myapp/myapp",
			},
			killResults: map[int]map[syscall.Signal]error{
				1234: {
					syscall.SIGTERM: nil,
					0:               nil,
					syscall.SIGKILL: nil,
				},
			},
		},
		{
			name:        "invalid pid in file",
			serviceName: "myapp",
			pidContent:  "not-a-number\n",
			procEntries: []os.DirEntry{},
			wantErr:     true,
			errSubstr:   "finding process",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestBusyboxManager(func(m *busyboxManager) {
				m.readFileFunc = func(path string) ([]byte, error) {
					if tt.pidErr != nil {
						return nil, tt.pidErr
					}
					return []byte(tt.pidContent), nil
				}
				m.killFunc = func(pid int, sig syscall.Signal) error {
					if pidResults, ok := tt.killResults[pid]; ok {
						if err, ok := pidResults[sig]; ok {
							return err
						}
					}
					return nil
				}
				m.readDirFunc = func(path string) ([]os.DirEntry, error) {
					return tt.procEntries, nil
				}
				if tt.readlinks != nil {
					m.readlinkFunc = func(path string) (string, error) {
						if target, ok := tt.readlinks[path]; ok {
							return target, nil
						}
						return "", os.ErrNotExist
					}
				}
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
		})
	}
}

func TestBusyboxStopSIGKILLAfterTimeout(t *testing.T) {
	killCalls := []struct {
		pid int
		sig syscall.Signal
	}{}

	m := newTestBusyboxManager(func(m *busyboxManager) {
		m.readFileFunc = func(string) ([]byte, error) {
			return []byte("999\n"), nil
		}
		m.readlinkFunc = func(path string) (string, error) {
			if path == "/proc/999/exe" {
				return "/opt/myapp/myapp", nil
			}
			return "", os.ErrNotExist
		}
		m.killFunc = func(pid int, sig syscall.Signal) error {
			killCalls = append(killCalls, struct {
				pid int
				sig syscall.Signal
			}{pid, sig})
			if sig == syscall.SIGKILL {
				return nil
			}
			return nil
		}
		m.gracefulTimeout = 0
	})
	m.serviceName = "myapp"

	err := m.Stop()
	if err != nil {
		t.Fatalf("Stop() unexpected error: %v", err)
	}

	hasSIGTERM := false
	hasSIGKILL := false
	for _, c := range killCalls {
		if c.sig == syscall.SIGTERM {
			hasSIGTERM = true
		}
		if c.sig == syscall.SIGKILL {
			hasSIGKILL = true
		}
	}
	if !hasSIGTERM {
		t.Error("expected SIGTERM to be sent")
	}
	if !hasSIGKILL {
		t.Error("expected SIGKILL to be sent after timeout")
	}
}

func TestBusyboxStatus(t *testing.T) {
	tests := []struct {
		name          string
		serviceName   string
		pidContent    string
		pidErr        error
		pidFileExists bool
		procExists    bool
		exeTarget     string
		wantStatus    ServiceStatus
		wantErr       bool
		errSubstr     string
	}{
		{
			name:          "running - valid PID and /proc entry",
			serviceName:   "myapp",
			pidContent:    "1234\n",
			pidFileExists: true,
			procExists:    true,
			wantStatus:    ServiceStatusRunning,
		},
		{
			name:          "stopped - no PID file",
			serviceName:   "myapp",
			pidErr:        os.ErrNotExist,
			pidFileExists: false,
			wantStatus:    ServiceStatusStopped,
		},
		{
			name:          "stopped - PID file exists but process gone",
			serviceName:   "myapp",
			pidContent:    "1234\n",
			pidFileExists: true,
			procExists:    false,
			wantStatus:    ServiceStatusStopped,
		},
		{
			name:          "running - binary replaced, exe shows deleted suffix",
			serviceName:   "myapp",
			pidContent:    "1234\n",
			pidFileExists: true,
			procExists:    true,
			exeTarget:     "/opt/myapp/myapp (deleted)",
			wantStatus:    ServiceStatusRunning,
		},
		{
			name:          "unknown - PID file exists but unreadable content",
			serviceName:   "myapp",
			pidContent:    "garbage",
			pidFileExists: true,
			wantStatus:    ServiceStatusUnknown,
		},
		{
			name:      "no service name",
			wantErr:   true,
			errSubstr: "service name not set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestBusyboxManager(func(m *busyboxManager) {
				m.readFileFunc = func(path string) ([]byte, error) {
					if tt.pidErr != nil {
						return nil, tt.pidErr
					}
					return []byte(tt.pidContent), nil
				}
				m.statFunc = func(path string) (os.FileInfo, error) {
					if path == "/var/run/myapp.pid" {
						if tt.pidFileExists {
							return nil, nil
						}
						return nil, os.ErrNotExist
					}
					if strings.HasPrefix(path, "/proc/") && tt.procExists {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
				m.readlinkFunc = func(path string) (string, error) {
					if path == "/proc/1234/exe" && tt.procExists {
						target := tt.exeTarget
						if target == "" {
							target = "/opt/myapp/myapp"
						}
						return target, nil
					}
					return "", os.ErrNotExist
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

func TestBusyboxReadPID(t *testing.T) {
	tests := []struct {
		name    string
		content string
		readErr error
		wantPID int
		wantErr bool
	}{
		{
			name:    "valid PID",
			content: "1234\n",
			wantPID: 1234,
		},
		{
			name:    "valid PID no newline",
			content: "5678",
			wantPID: 5678,
		},
		{
			name:    "file read error",
			readErr: os.ErrNotExist,
			wantErr: true,
		},
		{
			name:    "non-numeric content",
			content: "abc",
			wantErr: true,
		},
		{
			name:    "zero PID",
			content: "0",
			wantErr: true,
		},
		{
			name:    "negative PID",
			content: "-1",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestBusyboxManager(func(m *busyboxManager) {
				m.readFileFunc = func(string) ([]byte, error) {
					if tt.readErr != nil {
						return nil, tt.readErr
					}
					return []byte(tt.content), nil
				}
			})
			m.serviceName = "myapp"

			pid, err := m.readPID()
			if tt.wantErr {
				if err == nil {
					t.Fatal("readPID() expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("readPID() unexpected error: %v", err)
			}
			if pid != tt.wantPID {
				t.Errorf("readPID() = %d, want %d", pid, tt.wantPID)
			}
		})
	}
}

func TestBusyboxFindPIDByBinary(t *testing.T) {
	tests := []struct {
		name      string
		entries   []os.DirEntry
		readlinks map[string]string
		wantPID   int
		wantErr   bool
	}{
		{
			name: "finds matching process",
			entries: []os.DirEntry{
				mockDirEntry{name: "1"},
				mockDirEntry{name: "42"},
				mockDirEntry{name: "100"},
			},
			readlinks: map[string]string{
				"/proc/1/exe":   "/sbin/init",
				"/proc/42/exe":  "/opt/myapp/myapp",
				"/proc/100/exe": "/usr/bin/bash",
			},
			wantPID: 42,
		},
		{
			name: "no matching process",
			entries: []os.DirEntry{
				mockDirEntry{name: "1"},
			},
			readlinks: map[string]string{
				"/proc/1/exe": "/sbin/init",
			},
			wantErr: true,
		},
		{
			name:    "empty /proc",
			entries: []os.DirEntry{},
			wantErr: true,
		},
		{
			name: "skips non-numeric entries",
			entries: []os.DirEntry{
				mockDirEntry{name: "self"},
				mockDirEntry{name: "42"},
			},
			readlinks: map[string]string{
				"/proc/42/exe": "/opt/myapp/myapp",
			},
			wantPID: 42,
		},
		{
			name: "finds process with deleted suffix after binary replace",
			entries: []os.DirEntry{
				mockDirEntry{name: "42"},
			},
			readlinks: map[string]string{
				"/proc/42/exe": "/opt/myapp/myapp (deleted)",
			},
			wantPID: 42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestBusyboxManager(func(m *busyboxManager) {
				m.readDirFunc = func(string) ([]os.DirEntry, error) {
					return tt.entries, nil
				}
				m.readlinkFunc = func(path string) (string, error) {
					if target, ok := tt.readlinks[path]; ok {
						return target, nil
					}
					return "", os.ErrNotExist
				}
			})
			m.serviceName = "myapp"

			pid, err := m.findPIDByBinary()
			if tt.wantErr {
				if err == nil {
					t.Fatal("findPIDByBinary() expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("findPIDByBinary() unexpected error: %v", err)
			}
			if pid != tt.wantPID {
				t.Errorf("findPIDByBinary() = %d, want %d", pid, tt.wantPID)
			}
		})
	}
}

func TestBusyboxEnable(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		statPaths   map[string]bool
		renameErr   error
		wantErr     bool
		errSubstr   string
		wantRename  bool
	}{
		{
			name:        "already enabled - S95 script exists",
			serviceName: "myapp",
			statPaths:   map[string]bool{"/etc/init.d/S95myapp": true},
		},
		{
			name:        "rename K95 to S95",
			serviceName: "myapp",
			statPaths:   map[string]bool{"/etc/init.d/K95myapp": true},
			wantRename:  true,
		},
		{
			name:        "no script found",
			serviceName: "myapp",
			statPaths:   map[string]bool{},
			wantErr:     true,
			errSubstr:   "no init script found",
		},
		{
			name:      "no service name",
			wantErr:   true,
			errSubstr: "service name not set",
		},
		{
			name:        "rename fails",
			serviceName: "myapp",
			statPaths:   map[string]bool{"/etc/init.d/K95myapp": true},
			renameErr:   errors.New("permission denied"),
			wantErr:     true,
			errSubstr:   "enabling service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			renamed := false
			m := newTestBusyboxManager(func(m *busyboxManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
				m.renameFunc = func(oldpath, newpath string) error {
					renamed = true
					if tt.renameErr != nil {
						return tt.renameErr
					}
					if oldpath != "/etc/init.d/K95myapp" || newpath != "/etc/init.d/S95myapp" {
						t.Errorf("rename(%q, %q), want (/etc/init.d/K95myapp, /etc/init.d/S95myapp)", oldpath, newpath)
					}
					return nil
				}
			})
			m.serviceName = tt.serviceName
			m.supportsKSPrefix = true

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
			if tt.wantRename && !renamed {
				t.Error("expected rename to be called")
			}
			if !tt.wantRename && renamed {
				t.Error("unexpected rename call")
			}
		})
	}
}

func TestBusyboxDisable(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		statPaths   map[string]bool
		renameErr   error
		wantErr     bool
		errSubstr   string
		wantRename  bool
	}{
		{
			name:        "already disabled - K95 script exists",
			serviceName: "myapp",
			statPaths:   map[string]bool{"/etc/init.d/K95myapp": true},
		},
		{
			name:        "rename S95 to K95",
			serviceName: "myapp",
			statPaths:   map[string]bool{"/etc/init.d/S95myapp": true},
			wantRename:  true,
		},
		{
			name:        "no script found",
			serviceName: "myapp",
			statPaths:   map[string]bool{},
			wantErr:     true,
			errSubstr:   "no init script found",
		},
		{
			name:      "no service name",
			wantErr:   true,
			errSubstr: "service name not set",
		},
		{
			name:        "rename fails",
			serviceName: "myapp",
			statPaths:   map[string]bool{"/etc/init.d/S95myapp": true},
			renameErr:   errors.New("permission denied"),
			wantErr:     true,
			errSubstr:   "disabling service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			renamed := false
			m := newTestBusyboxManager(func(m *busyboxManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return nil, nil
					}
					return nil, os.ErrNotExist
				}
				m.renameFunc = func(oldpath, newpath string) error {
					renamed = true
					if tt.renameErr != nil {
						return tt.renameErr
					}
					if oldpath != "/etc/init.d/S95myapp" || newpath != "/etc/init.d/K95myapp" {
						t.Errorf("rename(%q, %q), want (/etc/init.d/S95myapp, /etc/init.d/K95myapp)", oldpath, newpath)
					}
					return nil
				}
			})
			m.serviceName = tt.serviceName
			m.supportsKSPrefix = true

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
			if tt.wantRename && !renamed {
				t.Error("expected rename to be called")
			}
			if !tt.wantRename && renamed {
				t.Error("unexpected rename call")
			}
		})
	}
}

func TestBusyboxEnableWithFlagFile(t *testing.T) {
	t.Run("removes disabled flag file", func(t *testing.T) {
		mockfs := newMockFS()
		m := newTestBusyboxManager(func(m *busyboxManager) {
			m.fs = mockfs
		})
		m.serviceName = "myapp"
		m.supportsKSPrefix = false

		err := m.Enable()
		if err != nil {
			t.Fatalf("Enable() unexpected error: %v", err)
		}
		if !mockfs.removed["/etc/init.d/myapp.disabled"] {
			t.Error("Enable() should remove /etc/init.d/myapp.disabled flag file")
		}
	})

	t.Run("succeeds even if flag file does not exist", func(t *testing.T) {
		mockfs := newMockFS()
		mockfs.removeErr = os.ErrNotExist
		m := newTestBusyboxManager(func(m *busyboxManager) {
			m.fs = mockfs
		})
		m.serviceName = "myapp"
		m.supportsKSPrefix = false

		err := m.Enable()
		if err != nil {
			t.Fatalf("Enable() unexpected error: %v", err)
		}
	})

	t.Run("returns error on remove failure", func(t *testing.T) {
		mockfs := newMockFS()
		mockfs.removeErr = errors.New("permission denied")
		m := newTestBusyboxManager(func(m *busyboxManager) {
			m.fs = mockfs
		})
		m.serviceName = "myapp"
		m.supportsKSPrefix = false

		err := m.Enable()
		if err == nil {
			t.Fatal("Enable() expected error, got nil")
		}
		if !strings.Contains(err.Error(), "removing disabled flag") {
			t.Errorf("Enable() error = %v, want substring 'removing disabled flag'", err)
		}
	})
}

func TestBusyboxDisableWithFlagFile(t *testing.T) {
	t.Run("creates disabled flag file", func(t *testing.T) {
		mockfs := newMockFS()
		m := newTestBusyboxManager(func(m *busyboxManager) {
			m.fs = mockfs
		})
		m.serviceName = "myapp"
		m.supportsKSPrefix = false

		err := m.Disable()
		if err != nil {
			t.Fatalf("Disable() unexpected error: %v", err)
		}
		data, ok := mockfs.files["/etc/init.d/myapp.disabled"]
		if !ok {
			t.Fatal("Disable() should create /etc/init.d/myapp.disabled flag file")
		}
		if string(data) != "disabled\n" {
			t.Errorf("flag file content = %q, want %q", string(data), "disabled\n")
		}
	})

	t.Run("returns error on write failure", func(t *testing.T) {
		mockfs := newMockFS()
		mockfs.writeErr = errors.New("no space left")
		m := newTestBusyboxManager(func(m *busyboxManager) {
			m.fs = mockfs
		})
		m.serviceName = "myapp"
		m.supportsKSPrefix = false

		err := m.Disable()
		if err == nil {
			t.Fatal("Disable() expected error, got nil")
		}
		if !strings.Contains(err.Error(), "creating disabled flag") {
			t.Errorf("Disable() error = %v, want substring 'creating disabled flag'", err)
		}
	})
}

func TestBusyboxEnableWithKSPrefix(t *testing.T) {
	t.Run("renames K95 to S95 when supportsKSPrefix is true", func(t *testing.T) {
		renamed := false
		m := newTestBusyboxManager(func(m *busyboxManager) {
			m.statFunc = func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/K95myapp" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}
			m.renameFunc = func(oldpath, newpath string) error {
				renamed = true
				if oldpath != "/etc/init.d/K95myapp" || newpath != "/etc/init.d/S95myapp" {
					t.Errorf("rename(%q, %q), want (/etc/init.d/K95myapp, /etc/init.d/S95myapp)", oldpath, newpath)
				}
				return nil
			}
		})
		m.serviceName = "myapp"
		m.supportsKSPrefix = true

		err := m.Enable()
		if err != nil {
			t.Fatalf("Enable() unexpected error: %v", err)
		}
		if !renamed {
			t.Error("expected rename to be called")
		}
	})

	t.Run("does not use flag file when supportsKSPrefix is true", func(t *testing.T) {
		mockfs := newMockFS()
		m := newTestBusyboxManager(func(m *busyboxManager) {
			m.fs = mockfs
			m.statFunc = func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/S95myapp" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}
		})
		m.serviceName = "myapp"
		m.supportsKSPrefix = true

		err := m.Enable()
		if err != nil {
			t.Fatalf("Enable() unexpected error: %v", err)
		}
		if mockfs.removed["/etc/init.d/myapp.disabled"] {
			t.Error("should not remove flag file when supportsKSPrefix is true")
		}
	})
}

func TestBusyboxDisableWithKSPrefix(t *testing.T) {
	t.Run("renames S95 to K95 when supportsKSPrefix is true", func(t *testing.T) {
		renamed := false
		m := newTestBusyboxManager(func(m *busyboxManager) {
			m.statFunc = func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/S95myapp" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}
			m.renameFunc = func(oldpath, newpath string) error {
				renamed = true
				if oldpath != "/etc/init.d/S95myapp" || newpath != "/etc/init.d/K95myapp" {
					t.Errorf("rename(%q, %q), want (/etc/init.d/S95myapp, /etc/init.d/K95myapp)", oldpath, newpath)
				}
				return nil
			}
		})
		m.serviceName = "myapp"
		m.supportsKSPrefix = true

		err := m.Disable()
		if err != nil {
			t.Fatalf("Disable() unexpected error: %v", err)
		}
		if !renamed {
			t.Error("expected rename to be called")
		}
	})

	t.Run("does not use flag file when supportsKSPrefix is true", func(t *testing.T) {
		mockfs := newMockFS()
		m := newTestBusyboxManager(func(m *busyboxManager) {
			m.fs = mockfs
			m.statFunc = func(path string) (os.FileInfo, error) {
				if path == "/etc/init.d/K95myapp" {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}
		})
		m.serviceName = "myapp"
		m.supportsKSPrefix = true

		err := m.Disable()
		if err != nil {
			t.Fatalf("Disable() unexpected error: %v", err)
		}
		if _, ok := mockfs.files["/etc/init.d/myapp.disabled"]; ok {
			t.Error("should not create flag file when supportsKSPrefix is true")
		}
	})
}

func TestBusyboxDisabledScriptPath(t *testing.T) {
	m := newTestBusyboxManager()
	m.serviceName = "myapp"
	want := "/etc/init.d/K95myapp"
	if got := m.disabledScriptPath(); got != want {
		t.Errorf("disabledScriptPath() = %q, want %q", got, want)
	}
}

func TestBusyboxDetect_LogsNanokvmScenario(t *testing.T) {
	requireLinuxDetection(t)
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := newTestBusyboxManager(func(m *busyboxManager) {
		m.statFunc = func(path string) (os.FileInfo, error) {
			return nil, os.ErrNotExist
		}
		m.lookPathFunc = func(name string) (string, error) {
			if name == "runsvdir" {
				return "/usr/bin/runsvdir", nil
			}
			return "", errors.New("not found")
		}
		m.readlinkFunc = func(path string) (string, error) {
			if path == "/proc/1/exe" {
				return "/bin/busybox", nil
			}
			return "", os.ErrNotExist
		}
	})

	result := m.detect()
	if !result {
		t.Fatal("expected detect() to return true for nanokvm scenario")
	}

	output := buf.String()
	expectedMessages := []string{
		"init detection: busybox: checking BusyBox detection",
		"init detection: busybox: readlink /proc/1/exe -> /bin/busybox",
		"init detection: busybox: found 'busybox' in path",
		"init detection: busybox: detected=true reason=PID 1 is busybox",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}
}

func TestBusyBoxDetect_NanokvmScenario(t *testing.T) {
	requireLinuxDetection(t)
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := newTestBusyboxManager(func(m *busyboxManager) {
		m.statFunc = func(path string) (os.FileInfo, error) {
			return nil, os.ErrNotExist
		}
		m.lookPathFunc = func(name string) (string, error) {
			return "", errors.New("not found")
		}
		m.readlinkFunc = func(path string) (string, error) {
			if path == "/proc/1/exe" {
				return "/usr/sbin/init", nil
			}
			return "", os.ErrNotExist
		}
		m.readFileFunc = func(path string) ([]byte, error) {
			if path == "/usr/sbin/init" {
				return []byte("\x7fELF\x00\x00BusyBox v1.36.1 (2024-06-10)"), nil
			}
			return nil, os.ErrNotExist
		}
	})

	result := m.detect()
	if !result {
		t.Fatal("expected detect() to return true for nanokvm scenario with binary content detection")
	}

	output := buf.String()

	expectedMessages := []string{
		"init detection: busybox: checking BusyBox detection",
		"init detection: busybox: readlink /proc/1/exe -> /usr/sbin/init",
		"init detection: busybox: checking binary content",
		"init detection: busybox: found BusyBox in binary content",
		"init detection: busybox: detected=true reason=binary content contains BusyBox",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}

	if strings.Contains(output, "detected=true reason=PID 1 is busybox") {
		t.Error("should not detect via path check - nanokvm path /usr/sbin/init does not contain 'busybox'")
	}
}

func TestBusyboxDetect_LogsWhenNotDetected(t *testing.T) {
	requireLinuxDetection(t)
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := newTestBusyboxManager()

	result := m.detect()
	if result {
		t.Fatal("expected detect() to return false")
	}

	output := buf.String()
	expectedMessages := []string{
		"init detection: busybox: checking BusyBox detection",
		"init detection: busybox: detected=false reason=no busybox indicators found",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}

	if strings.Contains(output, "detected=true") {
		t.Errorf("should not contain detected=true when busybox not detected")
	}
}
