package daemonpilot

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ovineko/daemonpilot/internal/daemon"
)

// domainMockManager reports a resolved domain, the optional interface a
// user-scope install reads to pin the launchd domain in the state file.
type domainMockManager struct {
	*lingerMockManager

	domain string
}

// Compile-time proof the mock still satisfies the optional interface: a stale
// signature would make the type assertion in handleInstall fail silently and the
// domain would go unrecorded with every test below still passing.
var _ daemon.DomainReporter = (*domainMockManager)(nil)

func (m *domainMockManager) ResolvedDomain() string { return m.domain }

func newDomainManager(domain string) *domainMockManager {
	return &domainMockManager{lingerMockManager: newLingerManager(true), domain: domain}
}

func TestHandleInstall_RecordsLaunchdDomain(t *testing.T) {
	tests := []struct {
		name   string
		scope  InstallScope
		domain string
		want   string
	}{
		{name: "user scope records the resolved gui domain", scope: ScopeUser, domain: "gui/501", want: "gui/501"},
		{name: "user scope records the user domain fallback", scope: ScopeUser, domain: "user/501", want: "user/501"},
		// A system-scope state file must stay byte-identical to one written
		// before scopes existed, so the field is omitted even though the
		// manager reports the system domain.
		{name: "system scope omits the domain", scope: ScopeSystem, domain: "system", want: ""},
		{name: "unresolved domain is recorded as empty", scope: ScopeUser, domain: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := userScopeConfig()
			cfg.Scope = tt.scope
			if tt.scope != ScopeUser {
				cfg.InstallDir = "/opt/testapp"
			}

			state, err := installWithManager(t, cfg, newDomainManager(tt.domain))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if state == nil {
				t.Fatal("no state was written")
			}
			if state.LaunchdDomain != tt.want {
				t.Errorf("state launchd domain = %q, want %q", state.LaunchdDomain, tt.want)
			}
		})
	}
}

// A manager that does not report a domain — every init system other than
// launchd — must leave the field empty rather than fail the install.
func TestHandleInstall_ManagerWithoutDomainRecordsNone(t *testing.T) {
	cfg := userScopeConfig()

	state, err := installWithManager(t, cfg, newLingerManager(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.LaunchdDomain != "" {
		t.Errorf("state launchd domain = %q, want empty", state.LaunchdDomain)
	}
}

// Lifecycle commands must address the domain the install bootstrapped into,
// which only the state file knows.
func TestManagerForState_PinsLaunchdDomain(t *testing.T) {
	tests := []struct {
		name  string
		state *StateData
		want  string
	}{
		{
			name:  "pinned domain from state",
			state: &StateData{ServiceName: "testapp", InitSystem: "mock", Scope: ScopeUser, LaunchdDomain: "user/501"},
			want:  "user/501",
		},
		{
			name:  "no domain recorded",
			state: &StateData{ServiceName: "testapp", InitSystem: "mock", Scope: ScopeUser},
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := saveCommandVars()
			t.Cleanup(func() { restoreCommandVars2(saved) })

			detectManager = func() (daemon.ServiceManager, error) { return &mockServiceManager{}, nil }

			var gotOpts daemon.ManagerOptions
			initManager = func(_ daemon.ServiceManager, opts daemon.ManagerOptions) error {
				gotOpts = opts
				return nil
			}

			cfg := userScopeConfig()
			if _, err := managerForState(cfg, tt.state, daemon.ManagerOptions{ServiceName: cfg.Name}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotOpts.LaunchdDomain != tt.want {
				t.Errorf("ManagerOptions.LaunchdDomain = %q, want %q", gotOpts.LaunchdDomain, tt.want)
			}
			if gotOpts.Scope != daemon.ScopeUser {
				t.Errorf("ManagerOptions.Scope = %v, want user", gotOpts.Scope)
			}
			// WriteConfig and the update-config rollback rewrite a manifest with
			// no ServiceConfig in hand, so the log directory has to travel on
			// the manager or they cannot create what the manifest redirects into.
			if gotOpts.LogDir != cfg.LogDir {
				t.Errorf("ManagerOptions.LogDir = %q, want %q", gotOpts.LogDir, cfg.LogDir)
			}
		})
	}
}

// A re-install resolves its own domain, but it has to be told which one the
// previous install bootstrapped into: a session with different login state
// resolves a different domain and would leave the earlier agent loaded, with the
// state file it just overwrote no longer naming the domain that could remove it.
func TestHandleInstall_PassesPreviousLaunchdDomainToManager(t *testing.T) {
	tests := []struct {
		name  string
		state *StateData
		want  string
	}{
		{
			name:  "domain recorded by the previous install",
			state: &StateData{ServiceName: "testapp", InitSystem: "launchd", Scope: ScopeUser, LaunchdDomain: "gui/501"},
			want:  "gui/501",
		},
		{name: "no previous install"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := saveCommandVars()
			t.Cleanup(func() { restoreCommandVars2(saved) })

			var gotOpts daemon.ManagerOptions
			initManager = func(_ daemon.ServiceManager, opts daemon.ManagerOptions) error {
				gotOpts = opts
				return nil
			}
			detectManager = func() (daemon.ServiceManager, error) { return &mockServiceManager{}, nil }
			getExecutable = func() (string, error) { return "/tmp/fake-binary", nil }
			copyFileFn = func(string, string) error { return nil }
			saveStateFn = func(string, *StateData) error { return nil }
			statFileFn = func(string) (os.FileInfo, error) { return nil, nil }
			loadStateFn = func(string, string) (*StateData, error) { return tt.state, nil }

			cfg := userScopeConfig()
			cfg.InstallDir = t.TempDir()
			if err := handleInstall(cfg); err != nil {
				t.Fatalf("handleInstall() error: %v", err)
			}
			if gotOpts.PreviousLaunchdDomain != tt.want {
				t.Errorf("ManagerOptions.PreviousLaunchdDomain = %q, want %q", gotOpts.PreviousLaunchdDomain, tt.want)
			}
		})
	}
}

// handleInstall builds its manager from detection rather than from state, so the
// LogDir wiring there is a second, independent path.
func TestHandleInstall_PassesLogDirToManager(t *testing.T) {
	saved := saveCommandVars()
	t.Cleanup(func() { restoreCommandVars2(saved) })

	var gotOpts daemon.ManagerOptions
	initManager = func(_ daemon.ServiceManager, opts daemon.ManagerOptions) error {
		gotOpts = opts
		return nil
	}
	detectManager = func() (daemon.ServiceManager, error) { return &mockServiceManager{}, nil }
	getExecutable = func() (string, error) { return "/tmp/fake-binary", nil }
	copyFileFn = func(string, string) error { return nil }
	saveStateFn = func(string, *StateData) error { return nil }
	statFileFn = func(string) (os.FileInfo, error) { return nil, nil }

	cfg := userScopeConfig()
	cfg.InstallDir = t.TempDir()
	cfg.LogDir = filepath.Join(cfg.InstallDir, "logs")
	if err := handleInstall(cfg); err != nil {
		t.Fatalf("handleInstall() error: %v", err)
	}
	if gotOpts.LogDir != cfg.LogDir {
		t.Errorf("ManagerOptions.LogDir = %q, want %q", gotOpts.LogDir, cfg.LogDir)
	}
}
