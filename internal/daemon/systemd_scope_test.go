package daemon

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func userScopeManager(home string, executor commandExecutor, fs fileSystem) *systemdManager {
	return &systemdManager{
		scope:       ScopeUser,
		executor:    executor,
		fs:          fs,
		getenvFunc:  func(string) string { return "" },
		geteuidFunc: func() int { return 1000 },
		homeFunc:    func() (string, error) { return home, nil },
	}
}

func TestSystemdUnitFilePathPerScope(t *testing.T) {
	tests := []struct {
		name    string
		scope   Scope
		home    string
		homeErr error
		want    string
		wantErr error
	}{
		{
			name:  "system scope keeps /etc/systemd/system",
			scope: ScopeSystem,
			home:  "/home/alice",
			want:  "/etc/systemd/system/my-service.service",
		},
		{
			name:  "system scope ignores an unresolvable home",
			scope: ScopeSystem,
			want:  "/etc/systemd/system/my-service.service",
		},
		{
			name:  "user scope resolves under $HOME",
			scope: ScopeUser,
			home:  "/home/alice",
			want:  "/home/alice/.config/systemd/user/my-service.service",
		},
		{
			name:    "user scope with empty home errors",
			scope:   ScopeUser,
			home:    "",
			wantErr: ErrNoHomeDir,
		},
		{
			name:    "user scope with failing home lookup errors",
			scope:   ScopeUser,
			homeErr: errors.New("$HOME is not defined"),
			wantErr: ErrNoHomeDir,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &systemdManager{
				serviceName: "my-service",
				scope:       tt.scope,
				homeFunc:    func() (string, error) { return tt.home, tt.homeErr },
			}

			got, err := mgr.unitFilePathErr()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("unitFilePathErr() error = %v, want %v", err, tt.wantErr)
				}
				if path := mgr.unitFilePath(); path != "" {
					t.Errorf("unitFilePath() = %q, want empty on error", path)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("unitFilePathErr() = %q, want %q", got, tt.want)
			}
			if path := mgr.GetManifestPath(); path != tt.want {
				t.Errorf("GetManifestPath() = %q, want %q", path, tt.want)
			}
		})
	}
}

func TestSystemdUserScopeNeverReadsXDGConfigHome(t *testing.T) {
	var requested []string
	mgr := &systemdManager{
		serviceName: "my-service",
		scope:       ScopeUser,
		getenvFunc: func(key string) string {
			requested = append(requested, key)
			return "/somewhere/else"
		},
		homeFunc: func() (string, error) { return "/home/alice", nil },
	}

	got, err := mgr.unitFilePathErr()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "/home/alice/.config/systemd/user/my-service.service"; got != want {
		t.Errorf("unitFilePathErr() = %q, want %q", got, want)
	}
	for _, key := range requested {
		if strings.HasPrefix(key, "XDG_") {
			t.Errorf("unit path resolution read %s; only $HOME is permitted", key)
		}
	}
}

// systemctlVerbs exercises every systemctl invocation the manager makes, so the
// --user insertion cannot regress for one verb while passing for the others.
var systemctlVerbs = []struct {
	name     string
	run      func(m *systemdManager) error
	wantArgs []string
}{
	{
		name: "install daemon-reload",
		run: func(m *systemdManager) error {
			return m.Install(ServiceConfig{Name: "test-svc", ExecPath: "/opt/test-svc/test-svc", Scope: m.scope})
		},
		wantArgs: []string{"daemon-reload"},
	},
	{
		name:     "uninstall disable",
		run:      func(m *systemdManager) error { return m.Uninstall() },
		wantArgs: []string{"disable", "test-svc"},
	},
	{
		name:     "uninstall daemon-reload",
		run:      func(m *systemdManager) error { return m.Uninstall() },
		wantArgs: []string{"daemon-reload"},
	},
	{
		name:     "start",
		run:      func(m *systemdManager) error { return m.Start() },
		wantArgs: []string{"start", "test-svc"},
	},
	{
		name:     "stop",
		run:      func(m *systemdManager) error { return m.Stop() },
		wantArgs: []string{"stop", "test-svc"},
	},
	{
		name:     "enable",
		run:      func(m *systemdManager) error { return m.Enable() },
		wantArgs: []string{"enable", "test-svc"},
	},
	{
		name:     "disable",
		run:      func(m *systemdManager) error { return m.Disable() },
		wantArgs: []string{"disable", "test-svc"},
	},
	{
		name: "status",
		run: func(m *systemdManager) error {
			_, err := m.Status()
			return err
		},
		wantArgs: []string{"is-active", "test-svc"},
	},
	{
		name:     "write config daemon-reload",
		run:      func(m *systemdManager) error { return m.WriteConfig("[Unit]\n") },
		wantArgs: []string{"daemon-reload"},
	},
}

func TestSystemdUserScopePassesUserFlag(t *testing.T) {
	for _, tt := range systemctlVerbs {
		t.Run(tt.name, func(t *testing.T) {
			ex := newMockExecutor()
			mgr := userScopeManager("/home/alice", ex, newMockFS())
			mgr.serviceName = "test-svc"

			if err := tt.run(mgr); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !ex.called("systemctl", append([]string{"--user"}, tt.wantArgs...)...) {
				t.Errorf("systemctl --user %v was not called; calls: %v", tt.wantArgs, ex.calls)
			}
		})
	}
}

func TestSystemdSystemScopeOmitsUserFlag(t *testing.T) {
	for _, tt := range systemctlVerbs {
		t.Run(tt.name, func(t *testing.T) {
			ex := newMockExecutor()
			mgr := &systemdManager{serviceName: "test-svc", executor: ex, fs: newMockFS()}

			if err := tt.run(mgr); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !ex.called("systemctl", tt.wantArgs...) {
				t.Errorf("systemctl %v was not called; calls: %v", tt.wantArgs, ex.calls)
			}
			for _, c := range ex.calls {
				for _, a := range c.Args {
					if a == "--user" {
						t.Errorf("system scope passed --user: %v", c.Args)
					}
				}
			}
		})
	}
}

func TestSystemdInstallUserScope(t *testing.T) {
	ex := newMockExecutor()
	fs := newMockFS()
	mgr := userScopeManager("/home/alice", ex, fs)

	err := mgr.Install(ServiceConfig{
		Name:        "test-svc",
		Description: "Test Service",
		ExecPath:    "/home/alice/.local/share/test-svc/test-svc",
		InstallDir:  "/home/alice/.local/share/test-svc",
		WorkingDir:  "/home/alice/.local/share/test-svc",
		Scope:       ScopeUser,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	unitPath := "/home/alice/.config/systemd/user/test-svc.service"
	content, ok := fs.files[unitPath]
	if !ok {
		t.Fatalf("unit file not written to %s; wrote: %v", unitPath, fs.files)
	}
	if !fs.dirs["/home/alice/.config/systemd/user"] {
		t.Error("unit directory was not created")
	}

	unit := string(content)
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("user unit missing WantedBy=default.target:\n%s", unit)
	}
	if strings.Contains(unit, "network-online.target") {
		t.Errorf("user unit orders against network-online.target, which the user manager lacks:\n%s", unit)
	}
}

// Install and GenerateConfig adopt the scope of the config they are handed: a
// manager built by detection carries no scope until the install tells it one,
// so dropping that assignment would write a user-scope unit into
// /etc/systemd/system and reload the system manager.
func TestSystemdAdoptsConfigScope(t *testing.T) {
	cfg := ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/home/alice/.local/share/test-svc/test-svc",
		InstallDir: "/home/alice/.local/share/test-svc",
		Scope:      ScopeUser,
	}

	t.Run("Install", func(t *testing.T) {
		ex := newMockExecutor()
		fs := newMockFS()
		mgr := userScopeManager("/home/alice", ex, fs)
		mgr.scope = ScopeSystem

		if err := mgr.Install(cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mgr.scope != ScopeUser {
			t.Errorf("manager scope = %v, want %v", mgr.scope, ScopeUser)
		}
		if _, ok := fs.files["/home/alice/.config/systemd/user/test-svc.service"]; !ok {
			t.Errorf("unit was not written to the user unit directory; wrote: %v", fs.files)
		}
		if !ex.called("systemctl", "--user", "daemon-reload") {
			t.Errorf("daemon-reload did not target the user manager; calls: %v", ex.calls)
		}
	})

	t.Run("GenerateConfig", func(t *testing.T) {
		mgr := userScopeManager("/home/alice", newMockExecutor(), newMockFS())
		mgr.scope = ScopeSystem

		content, err := mgr.GenerateConfig(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mgr.scope != ScopeUser {
			t.Errorf("manager scope = %v, want %v", mgr.scope, ScopeUser)
		}
		if !strings.Contains(content, "WantedBy=default.target") {
			t.Errorf("generated unit is not a user unit:\n%s", content)
		}
		if got := mgr.GetManifestPath(); got != "/home/alice/.config/systemd/user/test-svc.service" {
			t.Errorf("GetManifestPath() = %q, want the user unit path", got)
		}
	})
}

// systemctl must not leak the per-user bus environment into a later system-scope
// call on the same manager.
func TestSystemdSystemctlClearsUserEnvOnScopeChange(t *testing.T) {
	e := &execCommandExecutor{}
	mgr := userScopeManager("/home/alice", e, newMockFS())
	mgr.getenvFunc = func(string) string { return "" }

	_, _ = mgr.systemctl("--version")
	if len(e.extraEnv) == 0 {
		t.Fatal("user scope did not set the per-user manager environment")
	}

	mgr.scope = ScopeSystem
	_, _ = mgr.systemctl("--version")
	if len(e.extraEnv) != 0 {
		t.Errorf("system-scope call kept the user environment: %v", e.extraEnv)
	}
}

func TestSystemdInstallUserScope_NoHome(t *testing.T) {
	mgr := userScopeManager("", newMockExecutor(), newMockFS())

	err := mgr.Install(ServiceConfig{Name: "test-svc", ExecPath: "/x/test-svc", Scope: ScopeUser})
	if !errors.Is(err, ErrNoHomeDir) {
		t.Fatalf("Install() error = %v, want ErrNoHomeDir", err)
	}
}

func TestGenerateUnitFile_ScopeTemplateDifferences(t *testing.T) {
	base := ServiceConfig{
		Name:        "test-svc",
		Description: "Test Service",
		ExecPath:    "/opt/test-svc/test-svc",
		WorkingDir:  "/opt/test-svc",
	}

	system, err := GenerateUnitFile(base)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(system, "WantedBy=multi-user.target") {
		t.Errorf("system unit missing WantedBy=multi-user.target:\n%s", system)
	}
	if !strings.Contains(system, "After=network-online.target\nWants=network-online.target") {
		t.Errorf("system unit lost its network ordering:\n%s", system)
	}

	userCfg := base
	userCfg.Scope = ScopeUser
	userUnit, err := GenerateUnitFile(userCfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(userUnit, "WantedBy=default.target") {
		t.Errorf("user unit missing WantedBy=default.target:\n%s", userUnit)
	}
	if strings.Contains(userUnit, "network-online.target") {
		t.Errorf("user unit kept network-online.target ordering:\n%s", userUnit)
	}
	if !strings.HasPrefix(userUnit, "[Unit]\nDescription=Test Service\n# Never let systemd") {
		t.Errorf("user unit header is malformed:\n%s", userUnit)
	}
}

func TestGenerateUnitFile_RejectsUserAndGroupInUserScope(t *testing.T) {
	tests := []struct {
		name  string
		user  string
		group string
	}{
		{"user only", "svc", ""},
		{"group only", "", "svc"},
		{"both", "svc", "svc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := GenerateUnitFile(ServiceConfig{
				Name:     "test-svc",
				ExecPath: "/opt/test-svc/test-svc",
				Scope:    ScopeUser,
				User:     tt.user,
				Group:    tt.group,
			})
			if !errors.Is(err, ErrUserGroupInUserScope) {
				t.Fatalf("GenerateUnitFile() error = %v, want ErrUserGroupInUserScope", err)
			}
		})
	}

	if _, err := GenerateUnitFile(ServiceConfig{
		Name:     "test-svc",
		ExecPath: "/opt/test-svc/test-svc",
		User:     "svc",
		Group:    "svc",
	}); err != nil {
		t.Fatalf("system scope must still accept User/Group: %v", err)
	}
}

func TestGenerateUnitFile_RejectsUserAndGroupDirectivesInUserScope(t *testing.T) {
	tests := []struct {
		name       string
		directives map[string]string
		wantReject bool
	}{
		{"user directive", map[string]string{"User": "svc"}, true},
		{"group directive", map[string]string{"Group": "svc"}, true},
		{"key with surrounding whitespace", map[string]string{"  User ": "svc"}, true},
		{"unrelated directive", map[string]string{"MemoryMax": "1G"}, false},
		// systemd's directive lookup is case-sensitive, so `user=` is an unknown
		// key it ignores rather than an identity switch.
		{"lowercase key is not a directive", map[string]string{"user": "svc"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := GenerateUnitFile(ServiceConfig{
				Name:              "test-svc",
				ExecPath:          "/opt/test-svc/test-svc",
				Scope:             ScopeUser,
				SystemdDirectives: tt.directives,
			})
			if tt.wantReject {
				if !errors.Is(err, ErrUserGroupInUserScope) {
					t.Fatalf("GenerateUnitFile() error = %v, want ErrUserGroupInUserScope", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GenerateUnitFile() error = %v, want nil", err)
			}
		})
	}

	unit, err := GenerateUnitFile(ServiceConfig{
		Name:              "test-svc",
		ExecPath:          "/opt/test-svc/test-svc",
		SystemdDirectives: map[string]string{"User": "svc"},
	})
	if err != nil {
		t.Fatalf("system scope must still accept a User directive: %v", err)
	}
	if !strings.Contains(unit, "User=svc") {
		t.Errorf("system unit dropped the User directive:\n%s", unit)
	}
}

func TestSystemdUserManagerEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{
			name: "runtime dir derived from uid when unset",
			env:  map[string]string{},
			want: []string{"XDG_RUNTIME_DIR=/run/user/1000", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus"},
		},
		{
			name: "runtime dir from the environment",
			env:  map[string]string{"XDG_RUNTIME_DIR": "/run/user/4242"},
			want: []string{"XDG_RUNTIME_DIR=/run/user/4242", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/4242/bus"},
		},
		{
			name: "existing bus address is left alone",
			env: map[string]string{
				"XDG_RUNTIME_DIR":          "/run/user/1000",
				"DBUS_SESSION_BUS_ADDRESS": "unix:path=/custom/bus",
			},
			want: []string{"XDG_RUNTIME_DIR=/run/user/1000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &systemdManager{
				scope:       ScopeUser,
				getenvFunc:  func(k string) string { return tt.env[k] },
				geteuidFunc: func() int { return 1000 },
			}

			got := mgr.userManagerEnv()
			if strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Errorf("userManagerEnv() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSystemdUserScopeSetsExecutorExtraEnv(t *testing.T) {
	origExecCommand := execCommand
	t.Cleanup(func() { execCommand = origExecCommand })
	execCommand = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("true")
	}

	executor := &execCommandExecutor{}
	mgr := userScopeManager("/home/alice", executor, newMockFS())
	mgr.serviceName = "test-svc"

	if err := mgr.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(executor.extraEnv, " ") != "XDG_RUNTIME_DIR=/run/user/1000 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus" {
		t.Errorf("extraEnv = %v", executor.extraEnv)
	}
}

func TestExecCommandExecutorExtraEnv(t *testing.T) {
	origExecCommand := execCommand
	t.Cleanup(func() { execCommand = origExecCommand })

	var cmd *exec.Cmd
	execCommand = func(_ string, _ ...string) *exec.Cmd {
		cmd = exec.Command("true")
		return cmd
	}

	e := &execCommandExecutor{extraEnv: []string{"XDG_RUNTIME_DIR=/run/user/1000"}}
	if _, err := e.Run("systemctl", "--user", "start", "test-svc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, kv := range cmd.Env {
		if kv == "XDG_RUNTIME_DIR=/run/user/1000" {
			found = true
		}
	}
	if !found {
		t.Errorf("extraEnv was not passed to the child: %v", cmd.Env)
	}
	if len(cmd.Env) <= 1 {
		t.Error("extraEnv replaced the process environment instead of extending it")
	}
}

func TestExecCommandExecutorWithoutExtraEnvInheritsProcessEnv(t *testing.T) {
	origExecCommand := execCommand
	t.Cleanup(func() { execCommand = origExecCommand })

	var cmd *exec.Cmd
	execCommand = func(_ string, _ ...string) *exec.Cmd {
		cmd = exec.Command("true")
		return cmd
	}

	e := &execCommandExecutor{}
	if _, err := e.Run("systemctl", "daemon-reload"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd.Env != nil {
		t.Errorf("Env = %v, want nil so the child inherits the process environment", cmd.Env)
	}
}

func TestInitServiceManagerWithOptions_SetsSystemdScope(t *testing.T) {
	mgr := newSystemdManager()
	if err := InitServiceManagerWithOptions(mgr, ManagerOptions{ServiceName: "test-svc", Scope: ScopeUser}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mgr.scope != ScopeUser {
		t.Errorf("scope = %v, want ScopeUser", mgr.scope)
	}

	sysMgr := newSystemdManager()
	if err := InitServiceManagerWithOptions(sysMgr, ManagerOptions{ServiceName: "test-svc"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sysMgr.scope != ScopeSystem {
		t.Errorf("scope = %v, want ScopeSystem", sysMgr.scope)
	}
}
