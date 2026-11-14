package daemonpilot

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ovineko/daemonpilot/internal/daemon"
)

// A user-scope install must fail when no init system on the host can register a
// per-user service. Detecting without the scope filter would write a
// machine-wide manifest and record it as user-scope, leaving lifecycle commands
// operating on a service the user never asked for.
func TestHandleInstall_ScopeAwareDetection(t *testing.T) {
	tests := []struct {
		name    string
		detErr  error
		wantErr string
	}{
		{
			name:    "host cannot serve user scope",
			detErr:  daemon.ErrScopeNotSupported,
			wantErr: "detecting service manager",
		},
		{
			name:    "per-user manager is unreachable",
			detErr:  daemon.ErrUserManagerUnreachable,
			wantErr: "detecting service manager",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := saveCommandVars()
			t.Cleanup(func() { restoreCommandVars2(saved) })

			var copied, stateWritten bool
			detectForScope = func(daemon.Scope) (daemon.Detection, error) {
				return daemon.Detection{}, tt.detErr
			}
			initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
			getExecutable = func() (string, error) { return "/usr/bin/testapp", nil }
			mkdirAll = func(string, os.FileMode) error { return nil }
			copyFileFn = func(string, string) error {
				copied = true
				return nil
			}
			saveStateFn = func(string, *StateData) error {
				stateWritten = true
				return nil
			}

			err := handleInstall(userScopeConfig())
			if err == nil {
				t.Fatal("handleInstall() = nil, want error")
			}
			if !errors.Is(err, tt.detErr) {
				t.Errorf("error %v does not wrap %v", err, tt.detErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
			if copied {
				t.Error("binary was copied despite the failed scope detection")
			}
			if stateWritten {
				t.Error("state file was written despite the failed scope detection")
			}
		})
	}
}

func TestHandleInstall_PassesScopeToDetection(t *testing.T) {
	saved := saveCommandVars()
	t.Cleanup(func() { restoreCommandVars2(saved) })

	var gotScope daemon.Scope
	detectForScope = func(scope daemon.Scope) (daemon.Detection, error) {
		gotScope = scope
		return daemon.Detection{Manager: newLingerManager(true), HostInitSystem: "systemd"}, nil
	}
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
	getExecutable = func() (string, error) { return "/usr/bin/testapp", nil }
	mkdirAll = func(string, os.FileMode) error { return nil }
	copyFileFn = func(string, string) error { return nil }
	saveStateFn = func(string, *StateData) error { return nil }

	if err := handleInstall(userScopeConfig()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotScope != daemon.ScopeUser {
		t.Errorf("detection scope = %v, want %v", gotScope, daemon.ScopeUser)
	}
}

// Drift detection must compare the recorded init system against a fresh host
// detection. Comparing it against the manager rebuilt from the state file can
// never differ, since that manager is constructed from the recorded name.
func TestInitSystemDriftUsesFreshDetection(t *testing.T) {
	tests := []struct {
		name       string
		hostInit   string
		detectErr  error
		wantErr    string
		wantNoDiff bool
	}{
		{
			name:     "host init system changed since install",
			hostInit: "openrc",
			wantErr:  "init system changed from \"systemd\" to \"openrc\"",
		},
		{
			name:       "unchanged host passes",
			hostInit:   "systemd",
			wantNoDiff: true,
		},
		{
			name:       "detection failure is not drift",
			detectErr:  errors.New("no supported service manager detected"),
			wantNoDiff: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := saveCommandVars()
			t.Cleanup(func() { restoreCommandVars2(saved) })

			detectForScope = func(daemon.Scope) (daemon.Detection, error) {
				if tt.detectErr != nil {
					return daemon.Detection{}, tt.detectErr
				}
				return daemon.Detection{
					Manager:        &mockServiceManager{nameVal: tt.hostInit},
					HostInitSystem: tt.hostInit,
				}, nil
			}

			cfg := &Config{Name: "testapp", InstallDir: "/opt/testapp", Logger: NewDefaultLogger(LogLevelError)}
			state := &StateData{ServiceName: "testapp", InitSystem: "systemd"}

			err := checkInitSystemDrift(cfg, state)

			if tt.wantNoDiff {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("checkInitSystemDrift() = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

// A user-scope install on a host whose own init system cannot register a
// per-user service is served by the crontab fallback, so the state file records
// "crontab" while HostInitSystem stays "openrc". Comparing those two directly
// would make validate and update-config fail permanently on exactly the
// platforms user-scope crontab was enabled for.
func TestInitSystemDriftAcceptsScopeFallbackManager(t *testing.T) {
	tests := []struct {
		name      string
		hostInit  string
		selected  string
		recorded  string
		wantError bool
	}{
		{
			name:     "crontab fallback on an openrc host is not drift",
			hostInit: "openrc",
			selected: "crontab",
			recorded: "crontab",
		},
		{
			name:     "crontab fallback on a runit host is not drift",
			hostInit: "runit",
			selected: "crontab",
			recorded: "crontab",
		},
		{
			name:      "a host that now serves the scope itself is drift",
			hostInit:  "systemd",
			selected:  "systemd",
			recorded:  "crontab",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := saveCommandVars()
			t.Cleanup(func() { restoreCommandVars2(saved) })

			detectForScope = func(daemon.Scope) (daemon.Detection, error) {
				return daemon.Detection{
					Manager:        &mockServiceManager{nameVal: tt.selected},
					HostInitSystem: tt.hostInit,
				}, nil
			}

			cfg := &Config{
				Name: "testapp", InstallDir: "/opt/testapp",
				Scope: ScopeUser, Logger: NewDefaultLogger(LogLevelError),
			}
			state := &StateData{ServiceName: "testapp", InitSystem: tt.recorded, Scope: ScopeUser}

			err := checkInitSystemDrift(cfg, state)

			if tt.wantError {
				if err == nil {
					t.Fatal("checkInitSystemDrift() = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("checkInitSystemDrift() = %v, want nil", err)
			}
		})
	}
}

// buildServiceConfig must carry the scope through to the manager: without it a
// user-scope install renders a system unit (multi-user.target, network-online
// ordering) into the per-user unit directory.
func TestBuildServiceConfigCarriesScope(t *testing.T) {
	tests := []struct {
		scope InstallScope
		want  daemon.Scope
	}{
		{ScopeUser, daemon.ScopeUser},
		{ScopeSystem, daemon.ScopeSystem},
		{InstallScope(""), daemon.ScopeSystem},
	}

	for _, tt := range tests {
		t.Run(string(tt.scope), func(t *testing.T) {
			cfg := &Config{Name: "testapp", InstallDir: "/opt/testapp", Scope: tt.scope}
			if got := buildServiceConfig(cfg).Scope; got != tt.want {
				t.Errorf("ServiceConfig.Scope = %v, want %v", got, tt.want)
			}
		})
	}
}

// status explains an inactive user-scope service that its init system tears
// down at logout, which is the reason it is not running after a logout or a
// reboot. The remedy differs per init system, so the note has to as well: only
// systemd has lingering and only systemd has loginctl.
func TestHandleStatusReportsLingering(t *testing.T) {
	tests := []struct {
		name       string
		state      *StateData
		status     daemon.ServiceStatus
		wantNote   string
		wantAbsent []string
	}{
		{
			name:       "stopped user systemd service without lingering explains why",
			state:      &StateData{ServiceName: "testapp", InitSystem: "systemd", Scope: ScopeUser},
			status:     daemon.ServiceStatusStopped,
			wantNote:   "installed without lingering",
			wantAbsent: []string{"launchd"},
		},
		{
			name:   "running user systemd service without lingering says nothing",
			state:  &StateData{ServiceName: "testapp", InitSystem: "systemd", Scope: ScopeUser},
			status: daemon.ServiceStatusRunning,
		},
		{
			name:   "user systemd service with lingering says nothing",
			state:  &StateData{ServiceName: "testapp", InitSystem: "systemd", Scope: ScopeUser, Lingering: true},
			status: daemon.ServiceStatusStopped,
		},
		{
			name:       "stopped user launchd agent gets launchd advice, not loginctl",
			state:      &StateData{ServiceName: "testapp", InitSystem: "launchd", Scope: ScopeUser},
			status:     daemon.ServiceStatusStopped,
			wantNote:   "user-scope launchd agents have no equivalent of lingering",
			wantAbsent: []string{"loginctl", "systemd", "installed without lingering"},
		},
		{
			name:       "stopped user crontab service says nothing about lingering",
			state:      &StateData{ServiceName: "testapp", InitSystem: "crontab", Scope: ScopeUser},
			status:     daemon.ServiceStatusStopped,
			wantAbsent: []string{"loginctl", "lingering"},
		},
		{
			name:       "system service says nothing",
			state:      &StateData{ServiceName: "testapp", InitSystem: "systemd"},
			status:     daemon.ServiceStatusStopped,
			wantAbsent: []string{"lingering"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockServiceManager{statusResult: tt.status}
			restore := setupMockManager(mock)
			t.Cleanup(restore)
			loadStateFn = func(string, string) (*StateData, error) { return tt.state, nil }

			savedOut := saveStdoutWriter()
			t.Cleanup(func() { restoreStdoutWriter(savedOut) })
			var buf bytes.Buffer
			stdoutWriter = &buf

			cfg := &Config{
				Name:       "testapp",
				InstallDir: "/opt/testapp",
				Scope:      tt.state.EffectiveScope(),
				Logger:     NewDefaultLogger(LogLevelError),
			}
			if err := handleStatus(cfg); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantNote != "" && !strings.Contains(buf.String(), tt.wantNote) {
				t.Errorf("output missing %q; output:\n%s", tt.wantNote, buf.String())
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(buf.String(), absent) {
					t.Errorf("output must not mention %q; output:\n%s", absent, buf.String())
				}
			}
		})
	}
}
