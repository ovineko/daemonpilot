package daemon

import "testing"

func TestScopeString(t *testing.T) {
	tests := []struct {
		name  string
		scope Scope
		want  string
	}{
		{name: "system", scope: ScopeSystem, want: "system"},
		{name: "user", scope: ScopeUser, want: "user"},
		{name: "zero value is system", scope: Scope(0), want: "system"},
		{name: "unknown falls back to system", scope: Scope(99), want: "system"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.scope.String(); got != tt.want {
				t.Errorf("Scope(%d).String() = %q, want %q", tt.scope, got, tt.want)
			}
		})
	}
}

func TestServiceConfigZeroScopeIsSystem(t *testing.T) {
	var cfg ServiceConfig
	if cfg.Scope != ScopeSystem {
		t.Errorf("zero ServiceConfig.Scope = %v, want ScopeSystem", cfg.Scope)
	}
}

func TestManagerOptionsZeroScopeIsSystem(t *testing.T) {
	var opts ManagerOptions
	if opts.Scope != ScopeSystem {
		t.Errorf("zero ManagerOptions.Scope = %v, want ScopeSystem", opts.Scope)
	}
}

// scopeSupport lists, per registered init system, whether it can register a
// service in user scope. Every init system supports system scope.
// The three that support it are the ones with a genuine per-user model:
// systemd's user manager, launchd's LaunchAgents and the crontab manager, which
// already edits the invoking user's crontab. The rest fail closed — openrc,
// finit, sysvinit, busybox and procd have no per-user notion at all, and dinit,
// runit and s6 are deferred because they would require bootstrapping a
// supervisor for the supervisor.
var scopeSupport = map[string]bool{
	"systemd":  true,
	"launchd":  true,
	"crontab":  true,
	"openrc":   false,
	"dinit":    false,
	"finit":    false,
	"runit":    false,
	"s6":       false,
	"procd":    false,
	"busybox":  false,
	"sysvinit": false,
}

func TestSupportsScopeAllManagers(t *testing.T) {
	for _, d := range defaultDetectors() {
		wantUser, ok := scopeSupport[d.name]
		if !ok {
			t.Fatalf("detector %q has no entry in scopeSupport; add one", d.name)
		}

		t.Run(d.name, func(t *testing.T) {
			mgr := d.create()
			if !mgr.SupportsScope(ScopeSystem) {
				t.Errorf("%s: SupportsScope(ScopeSystem) = false, want true", d.name)
			}
			if got := mgr.SupportsScope(ScopeUser); got != wantUser {
				t.Errorf("%s: SupportsScope(ScopeUser) = %v, want %v", d.name, got, wantUser)
			}
		})
	}
}

// TestUserScopeFailsClosed pins the two groups that must keep rejecting user
// scope, separately from the table above so that flipping an entry there does
// not silently flip the intent recorded here.
func TestUserScopeFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		reason string
	}{
		{name: "openrc", reason: "no per-user notion"},
		{name: "finit", reason: "no per-user notion"},
		{name: "sysvinit", reason: "no per-user notion"},
		{name: "busybox", reason: "no per-user notion"},
		{name: "procd", reason: "no per-user notion"},
		{name: "dinit", reason: "deferred: would require bootstrapping dinit --user"},
		{name: "runit", reason: "deferred: would require bootstrapping runsvdir"},
		{name: "s6", reason: "deferred: would require bootstrapping s6-svscan"},
	}

	byName := make(map[string]func() ServiceManager, len(defaultDetectors()))
	for _, d := range defaultDetectors() {
		byName[d.name] = d.create
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			create, ok := byName[tt.name]
			if !ok {
				t.Fatalf("detector %q is not registered", tt.name)
			}
			if create().SupportsScope(ScopeUser) {
				t.Errorf("%s: SupportsScope(ScopeUser) = true, want false (%s)", tt.name, tt.reason)
			}
		})
	}
}

// TestScopeSupportTableMatchesDetectors guards against drift in the other
// direction: a table entry naming an init system that is no longer registered.
func TestScopeSupportTableMatchesDetectors(t *testing.T) {
	registered := make(map[string]bool, len(defaultDetectors()))
	for _, d := range defaultDetectors() {
		registered[d.name] = true
	}

	for name := range scopeSupport {
		if !registered[name] {
			t.Errorf("scopeSupport lists %q, which is not a registered detector", name)
		}
	}

	if len(scopeSupport) != len(registered) {
		t.Errorf("scopeSupport has %d entries, detectors has %d", len(scopeSupport), len(registered))
	}
}
