package daemonpilot

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/ovineko/daemonpilot/internal/daemon"
)

// readOnlyDir creates a directory that refuses new entries using the host's
// native permission mechanism.
func readOnlyDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir() error: %v", err)
	}
	makeReadOnlyDirectory(t, dir)
	return dir
}

func TestProbeDirWritable(t *testing.T) {
	writable := t.TempDir()
	readOnly := readOnlyDir(t)

	tests := []struct {
		name       string
		dir        string
		wantProbed string
		wantErr    bool
	}{
		{name: "writable directory", dir: writable, wantProbed: writable},
		{name: "read-only directory", dir: readOnly, wantProbed: readOnly, wantErr: true},
		{
			name:       "missing directory with a writable ancestor",
			dir:        filepath.Join(writable, "a", "b"),
			wantProbed: writable,
		},
		{
			name:       "missing directory with a read-only ancestor",
			dir:        filepath.Join(readOnly, "a", "b"),
			wantProbed: readOnly,
			wantErr:    true,
		},
		{name: "no directory configured", dir: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probed, err := probeDirWritable(tt.dir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("probeDirWritable(%q) error = %v, wantErr %v", tt.dir, err, tt.wantErr)
			}
			if tt.wantProbed != "" && probed != tt.wantProbed {
				t.Errorf("probed = %q, want %q", probed, tt.wantProbed)
			}
		})
	}
}

func TestProbeDirWritableLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	if _, err := probeDirWritable(dir); err != nil {
		t.Fatalf("probeDirWritable() error: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("probe left %d entries behind, want none", len(entries))
	}
}

func TestProbeDirWritableRejectsNonDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	if _, err := probeDirWritable(file); err == nil {
		t.Fatal("probeDirWritable() = nil, want error for a non-directory")
	}
}

func TestPreflightInstall(t *testing.T) {
	writable := t.TempDir()
	readOnly := readOnlyDir(t)

	tests := []struct {
		name         string
		installDir   string
		manifestPath string
		wantErr      bool
	}{
		{
			name:         "both writable",
			installDir:   writable,
			manifestPath: filepath.Join(writable, "myapp.service"),
		},
		{
			name:         "install directory missing but its ancestor is writable",
			installDir:   filepath.Join(writable, "opt", "myapp"),
			manifestPath: filepath.Join(writable, "myapp.service"),
		},
		{
			name:         "read-only install directory",
			installDir:   readOnly,
			manifestPath: filepath.Join(writable, "myapp.service"),
			wantErr:      true,
		},
		{
			name:         "read-only manifest directory",
			installDir:   writable,
			manifestPath: filepath.Join(readOnly, "myapp.service"),
			wantErr:      true,
		},
		{
			name:         "install directory missing under a read-only ancestor",
			installDir:   filepath.Join(readOnly, "opt", "myapp"),
			manifestPath: filepath.Join(writable, "myapp.service"),
			wantErr:      true,
		},
		{
			name:       "a manager with no manifest path probes only the install directory",
			installDir: writable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Name: "myapp", InstallDir: tt.installDir, Logger: NewDefaultLogger(LogLevelInfo)}

			err := preflightInstall(cfg, tt.manifestPath)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("preflightInstall() error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("preflightInstall() = nil, want error")
			}
			if !errors.Is(err, ErrNeedsPrivileges) {
				t.Errorf("errors.Is(err, ErrNeedsPrivileges) = false, err = %v", err)
			}

			msg := err.Error()
			for _, want := range []string{tt.installDir, filepath.Dir(tt.manifestPath), "euid"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not name %q", msg, want)
				}
			}
		})
	}
}

// crontabManager returns a bare relative manifest name when no install
// directory is bound to it. Resolving that against the CLI's working directory
// would refuse the install over a directory the install never writes to.
func TestPreflightInstallIgnoresRelativeManifestPath(t *testing.T) {
	readOnly := readOnlyDir(t)
	manifestPath := ".daemonpilot-crontab-myapp.entry"
	cfg := &Config{Name: "myapp", InstallDir: readOnly, Logger: NewDefaultLogger(LogLevelInfo)}
	err := preflightInstall(cfg, manifestPath)
	if err == nil {
		t.Fatal("preflightInstall() error = nil, want install-directory probe failure")
	}
	if !strings.Contains(err.Error(), "probed paths: install directory") || strings.Contains(err.Error(), "manifest directory") {
		t.Errorf("relative manifest was not ignored in probed paths: %v", err)
	}
}

func TestPreflightInstallRemedy(t *testing.T) {
	readOnly := readOnlyDir(t)

	t.Run("system scope offers elevation and the user-scope paths", func(t *testing.T) {
		home := t.TempDir()
		saved := userHomeDir
		defer func() { userHomeDir = saved }()
		userHomeDir = func() (string, error) { return home, nil }

		cfg := &Config{Name: "myapp", InstallDir: readOnly, Logger: NewDefaultLogger(LogLevelInfo)}
		err := preflightInstall(cfg, filepath.Join(readOnly, "myapp.service"))
		if err == nil {
			t.Fatal("preflightInstall() = nil, want error")
		}
		msg := err.Error()
		for _, want := range []string{"elevated privileges", "daemonpilot.ScopeUser", home} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q does not mention %q", msg, want)
			}
		}
	})

	t.Run("system scope without a home directory still offers elevation", func(t *testing.T) {
		saved := userHomeDir
		defer func() { userHomeDir = saved }()
		userHomeDir = func() (string, error) { return "", errors.New("no home") }

		cfg := &Config{Name: "myapp", InstallDir: readOnly, Logger: NewDefaultLogger(LogLevelInfo)}
		err := preflightInstall(cfg, "")
		if err == nil {
			t.Fatal("preflightInstall() = nil, want error")
		}
		if !strings.Contains(err.Error(), "elevated privileges") {
			t.Errorf("error %q does not offer elevation", err)
		}
	})

	t.Run("user scope never suggests elevation", func(t *testing.T) {
		cfg := &Config{
			Name: "myapp", InstallDir: readOnly, Scope: ScopeUser,
			Logger: NewDefaultLogger(LogLevelInfo),
		}
		err := preflightInstall(cfg, "")
		if err == nil {
			t.Fatal("preflightInstall() = nil, want error")
		}
		if strings.Contains(err.Error(), "sudo") {
			t.Errorf("user-scope error %q suggests elevation", err)
		}
	})
}

func TestPreflightExecClassifiesFailures(t *testing.T) {
	tests := []struct {
		name     string
		probeErr error
		wantErr  bool
		wantMsg  string
	}{
		{name: "the binary runs"},
		{
			name:     "permission denied",
			probeErr: &fs.PathError{Op: "fork/exec", Path: "/opt/myapp/myapp", Err: syscall.EACCES},
			wantErr:  true,
			wantMsg:  "noexec",
		},
		{
			name:     "not a valid executable",
			probeErr: &fs.PathError{Op: "fork/exec", Path: "/opt/myapp/myapp", Err: syscall.ENOEXEC},
			wantErr:  true,
			wantMsg:  "different architecture",
		},
		{
			name:     "a non-zero exit status is not the install's business",
			probeErr: errors.New("exit status 2"),
		},
		{
			name:     "an unrelated exec error does not fail the install",
			probeErr: &fs.PathError{Op: "fork/exec", Path: "/opt/myapp/myapp", Err: syscall.ENOENT},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := runVersionProbe
			defer func() { runVersionProbe = saved }()

			probed := ""
			runVersionProbe = func(path string, _ []string) error {
				probed = path
				return tt.probeErr
			}

			cfg := &Config{Name: "myapp", Logger: NewDefaultLogger(LogLevelInfo)}
			err := preflightExec(cfg, "/opt/myapp/myapp")
			if probed != "/opt/myapp/myapp" {
				t.Errorf("probed %q, want the destination path", probed)
			}
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("preflightExec() error: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrNotExecutable) {
				t.Fatalf("errors.Is(err, ErrNotExecutable) = false, err = %v", err)
			}
			msg := err.Error()
			for _, want := range []string{"/opt/myapp/myapp", tt.wantMsg} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not mention %q", msg, want)
				}
			}
		})
	}
}

// TestPreflightExecAgainstRealFiles pins the classification to what the host
// kernel actually returns, so the seam-driven table above cannot drift from the
// errors a real exec produces.
func TestPreflightExecAgainstRealFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute permission and ENOEXEC do not map to Windows")
	}
	dir := t.TempDir()
	cfg := &Config{Name: "myapp", Logger: NewDefaultLogger(LogLevelInfo)}

	t.Run("an executable file passes", func(t *testing.T) {
		path := filepath.Join(dir, "runnable")
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("WriteFile() error: %v", err)
		}
		if err := preflightExec(cfg, path); err != nil {
			t.Fatalf("preflightExec() error: %v", err)
		}
	})

	t.Run("a file without the execute bit is refused", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: the execute bit is not enforced")
		}
		path := filepath.Join(dir, "not-executable")
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
			t.Fatalf("WriteFile() error: %v", err)
		}
		err := preflightExec(cfg, path)
		if !errors.Is(err, ErrNotExecutable) {
			t.Fatalf("preflightExec() error = %v, want ErrNotExecutable", err)
		}
	})

	t.Run("a file that is not an executable is refused", func(t *testing.T) {
		path := filepath.Join(dir, "garbage")
		if err := os.WriteFile(path, []byte("\x00\x01not an executable\n"), 0o755); err != nil {
			t.Fatalf("WriteFile() error: %v", err)
		}
		err := preflightExec(cfg, path)
		if !errors.Is(err, ErrNotExecutable) {
			t.Fatalf("preflightExec() error = %v, want ErrNotExecutable", err)
		}
	})
}

// stubCrossScopeStat makes exactly the listed paths look present to the
// cross-scope probe. The system-scope manifest directories are not writable by
// a test, so the probe is driven through its seam rather than through real
// files under /etc and /Library.
func stubCrossScopeStat(t *testing.T, present ...string) {
	t.Helper()
	saved := crossScopeStat
	t.Cleanup(func() { crossScopeStat = saved })

	existing := make(map[string]bool, len(present))
	for _, p := range present {
		existing[p] = true
	}
	crossScopeStat = func(path string) (os.FileInfo, error) {
		if !existing[path] {
			return nil, fs.ErrNotExist
		}
		return nil, nil
	}
}

func TestPreflightCrossScope(t *testing.T) {
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH"} {
		t.Setenv(key, home)
	}

	systemUnit := filepath.Join("/etc/systemd/system", "myapp.service")
	userUnit := filepath.Join(home, ".config", "systemd", "user", "myapp.service")
	systemPlist := filepath.Join("/Library/LaunchDaemons", "com.myapp.plist")
	userPlist := filepath.Join(home, "Library", "LaunchAgents", "com.myapp.plist")

	tests := []struct {
		name       string
		initSystem string
		scope      InstallScope
		present    []string
		wantPath   string
		wantScope  string
	}{
		{
			name:       "systemd user install over a system unit",
			initSystem: "systemd",
			scope:      ScopeUser,
			present:    []string{systemUnit},
			wantPath:   systemUnit,
			wantScope:  "system",
		},
		{
			name:       "systemd system install over a user unit",
			initSystem: "systemd",
			scope:      ScopeSystem,
			present:    []string{userUnit},
			wantPath:   userUnit,
			wantScope:  "user",
		},
		{
			name:       "launchd user install over a LaunchDaemon",
			initSystem: "launchd",
			scope:      ScopeUser,
			present:    []string{systemPlist},
			wantPath:   systemPlist,
			wantScope:  "system",
		},
		{
			name:       "launchd system install over a LaunchAgent",
			initSystem: "launchd",
			scope:      ScopeSystem,
			present:    []string{userPlist},
			wantPath:   userPlist,
			wantScope:  "user",
		},
		{
			name:       "no collision when the other scope holds nothing",
			initSystem: "systemd",
			scope:      ScopeUser,
		},
		{
			name:       "a manifest in the scope being installed into is not a collision",
			initSystem: "systemd",
			scope:      ScopeSystem,
			present:    []string{systemUnit},
		},
		{
			name:       "an init system with only one scope has nothing to collide with",
			initSystem: "openrc",
			scope:      ScopeSystem,
			present:    []string{systemUnit, userUnit},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubCrossScopeStat(t, tt.present...)

			cfg := &Config{Name: "myapp", Scope: tt.scope, Logger: NewDefaultLogger(LogLevelInfo)}
			err := preflightCrossScope(cfg, tt.initSystem)
			if tt.wantPath == "" {
				if err != nil {
					t.Fatalf("preflightCrossScope() error: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrScopeCollision) {
				t.Fatalf("errors.Is(err, ErrScopeCollision) = false, err = %v", err)
			}
			msg := err.Error()
			for _, want := range []string{tt.wantPath, tt.wantScope, "myapp"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not mention %q", msg, want)
				}
			}
		})
	}
}

// TestCrossScopeManifestPathIsUnresolvable covers the paths that cannot be
// derived: an unknown init system, and a user path with no home directory.
func TestCrossScopeManifestPathIsUnresolvable(t *testing.T) {
	for _, key := range []string{"HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH"} {
		t.Setenv(key, "")
	}

	if got := crossScopeManifestPath("no-such-init", "myapp", ScopeUser); got != "" {
		t.Errorf("crossScopeManifestPath() = %q, want an empty path", got)
	}
	if got := crossScopeManifestPath("systemd", "myapp", ScopeUser); got != "" {
		t.Errorf("crossScopeManifestPath() without a home = %q, want an empty path", got)
	}
}

func TestHandleInstallFailsOnCrossScopeCollision(t *testing.T) {
	saved := saveCommandVars()
	defer restoreCommandVars2(saved)

	mock := &mockServiceManager{}
	detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
	getExecutable = func() (string, error) { return "/usr/bin/myapp", nil }

	copied := false
	copyFileFn = func(string, string) error {
		copied = true
		return nil
	}
	saveStateFn = func(string, *StateData) error { return nil }

	savedCross := preflightCrossScope
	defer func() { preflightCrossScope = savedCross }()
	preflightCrossScope = func(*Config, string) error { return ErrScopeCollision }

	cfg := &Config{Name: "myapp", InstallDir: "/opt/myapp", Logger: NewDefaultLogger(LogLevelInfo)}
	err := handleInstall(cfg)
	if !errors.Is(err, ErrScopeCollision) {
		t.Fatalf("handleInstall() error = %v, want ErrScopeCollision", err)
	}
	if copied {
		t.Error("binary was copied despite a cross-scope collision")
	}
	if mock.installCalled {
		t.Error("Install was called despite a cross-scope collision")
	}
}

func TestHandleInstallFailsOnExecProbe(t *testing.T) {
	saved := saveCommandVars()
	defer restoreCommandVars2(saved)

	mock := &mockServiceManager{}
	detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
	getExecutable = func() (string, error) { return "/usr/bin/myapp", nil }
	copyFileFn = func(string, string) error { return nil }
	mkdirAll = func(string, os.FileMode) error { return nil }
	saveStateFn = func(string, *StateData) error { return nil }

	probed := ""
	preflightExec = func(_ *Config, path string) error {
		probed = path
		return ErrNotExecutable
	}

	cfg := &Config{Name: "myapp", InstallDir: "/opt/myapp", Logger: NewDefaultLogger(LogLevelInfo)}
	err := handleInstall(cfg)
	if !errors.Is(err, ErrNotExecutable) {
		t.Fatalf("handleInstall() error = %v, want ErrNotExecutable", err)
	}
	if probed != filepath.Join("/opt/myapp", InstalledBinaryName("myapp")) {
		t.Errorf("probed %q, want the installed binary path", probed)
	}
	if mock.installCalled {
		t.Error("Install was called despite a failed exec probe")
	}
}

func TestHandleInstallRunsPreflightBeforeMutating(t *testing.T) {
	saved := saveCommandVars()
	defer restoreCommandVars2(saved)

	mock := &mockServiceManager{}
	detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
	getExecutable = func() (string, error) { return "/usr/bin/myapp", nil }

	copied := false
	copyFileFn = func(string, string) error {
		copied = true
		return nil
	}
	saveStateFn = func(string, *StateData) error { return nil }
	preflightInstall = func(*Config, string) error { return ErrNeedsPrivileges }

	cfg := &Config{Name: "myapp", InstallDir: "/opt/myapp", Logger: NewDefaultLogger(LogLevelInfo)}
	err := handleInstall(cfg)
	if !errors.Is(err, ErrNeedsPrivileges) {
		t.Fatalf("handleInstall() error = %v, want ErrNeedsPrivileges", err)
	}
	if copied {
		t.Error("binary was copied despite a failed preflight")
	}
	if mock.installCalled {
		t.Error("Install was called despite a failed preflight")
	}
}
