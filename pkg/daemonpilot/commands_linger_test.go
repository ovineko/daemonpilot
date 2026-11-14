package daemonpilot

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ovineko/daemonpilot/internal/daemon"
)

// lingerMockManager adds the two optional interfaces a user-scope install
// consults. It wraps the shared mock rather than extending it, so the ~50 tests
// that install through mockServiceManager keep bypassing the gate.
type lingerMockManager struct {
	*mockServiceManager

	lingering       bool
	userName        string
	lingeringErr    error
	verifyErr       error
	verifyCalled    bool
	lingeringCalled bool
}

// Compile-time proof the mock still satisfies the optional interface: a stale
// signature would make the type assertion in requireLingering fail silently and
// every gate test below would pass without exercising the gate.
var _ daemon.LingerChecker = (*lingerMockManager)(nil)

func (m *lingerMockManager) LingeringEnabled() (bool, string, error) {
	m.lingeringCalled = true
	return m.lingering, m.userName, m.lingeringErr
}

func (m *lingerMockManager) VerifyRegistered() error {
	m.verifyCalled = true
	return m.verifyErr
}

func newLingerManager(lingering bool) *lingerMockManager {
	return &lingerMockManager{
		mockServiceManager: &mockServiceManager{},
		lingering:          lingering,
		userName:           "alice",
	}
}

// installWithManager runs handleInstall against mgr with every filesystem
// effect stubbed out, returning the state that would have been written.
func installWithManager(t *testing.T, cfg *Config, mgr daemon.ServiceManager) (*StateData, error) {
	t.Helper()

	saved := saveCommandVars()
	t.Cleanup(func() { restoreCommandVars2(saved) })

	detectManager = func() (daemon.ServiceManager, error) { return mgr, nil }
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
	getExecutable = func() (string, error) { return "/usr/bin/testapp", nil }
	mkdirAll = func(string, os.FileMode) error { return nil }
	copyFileFn = func(string, string) error { return nil }

	var written *StateData
	saveStateFn = func(_ string, s *StateData) error {
		written = s
		return nil
	}

	err := handleInstall(cfg)
	return written, err
}

func userScopeConfig() *Config {
	return &Config{
		Name:       "testapp",
		InstallDir: "/home/alice/.local/share/testapp",
		Scope:      ScopeUser,
		Logger:     NewDefaultLogger(LogLevelInfo),
	}
}

func TestHandleInstall_LingeringGate(t *testing.T) {
	tests := []struct {
		name          string
		scope         InstallScope
		lingering     bool
		allowNoLinger bool
		wantErr       bool
		wantMsgs      []string
		// wantIs is the error value a caller switches on.
		wantIs       error
		wantRecorded bool
	}{
		{
			name:         "user scope with lingering installs and records it",
			scope:        ScopeUser,
			lingering:    true,
			wantRecorded: true,
		},
		{
			name:      "user scope without lingering refuses with the exact remedy",
			scope:     ScopeUser,
			lingering: false,
			wantErr:   true,
			wantMsgs:  []string{"loginctl enable-linger alice"},
			wantIs:    ErrLingerRequired,
		},
		{
			name:          "AllowNoLinger installs anyway and records lingering as off",
			scope:         ScopeUser,
			lingering:     false,
			allowNoLinger: true,
			wantRecorded:  false,
		},
		{
			name:         "system scope never consults lingering",
			scope:        ScopeSystem,
			lingering:    false,
			wantRecorded: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := newLingerManager(tt.lingering)

			cfg := userScopeConfig()
			cfg.Scope = tt.scope
			cfg.AllowNoLinger = tt.allowNoLinger
			if tt.scope != ScopeUser {
				cfg.InstallDir = "/opt/testapp"
			}

			state, err := installWithManager(t, cfg, mgr)

			if tt.wantErr {
				if err == nil {
					t.Fatal("handleInstall() = nil, want error")
				}
				for _, want := range tt.wantMsgs {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err, want)
					}
				}

				// An application offering a session-scoped service switches
				// on the value, not on the words.
				if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
					t.Errorf("error %v is not %v", err, tt.wantIs)
				}
				// The gate must fire before anything is copied or registered.
				if mgr.installCalled {
					t.Error("service was registered despite the failed lingering gate")
				}
				if state != nil {
					t.Error("state file was written despite the failed lingering gate")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if state == nil {
				t.Fatal("no state was written")
			}
			if state.Lingering != tt.wantRecorded {
				t.Errorf("state lingering = %v, want %v", state.Lingering, tt.wantRecorded)
			}
			if wantConsulted := tt.scope == ScopeUser; mgr.lingeringCalled != wantConsulted {
				t.Errorf("lingering consulted = %v, want %v", mgr.lingeringCalled, wantConsulted)
			}
		})
	}
}

// A system-scope state file must stay free of the lingering field even when the
// manager reports lingering enabled, so it keeps encoding byte-identically.
func TestHandleInstall_SystemScopeOmitsLingering(t *testing.T) {
	mgr := newLingerManager(true)
	cfg := userScopeConfig()
	cfg.Scope = ScopeSystem
	cfg.InstallDir = "/opt/testapp"

	state, err := installWithManager(t, cfg, mgr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.Lingering {
		t.Error("system-scope state recorded lingering")
	}
}

// An unresolvable account name must fail the install outright: treating it as
// "lingering disabled" would print a remedy naming no real account, and
// AllowNoLinger would silently install a service that cannot be verified.
func TestHandleInstall_LingeringCheckErrorFailsInstall(t *testing.T) {
	for _, allowNoLinger := range []bool{false, true} {
		mgr := newLingerManager(false)
		mgr.lingeringErr = daemon.ErrUnknownUser

		cfg := userScopeConfig()
		cfg.AllowNoLinger = allowNoLinger

		state, err := installWithManager(t, cfg, mgr)
		if err == nil {
			t.Fatalf("AllowNoLinger=%v: handleInstall() = nil, want error", allowNoLinger)
		}
		if !errors.Is(err, daemon.ErrUnknownUser) {
			t.Errorf("AllowNoLinger=%v: error = %v, want ErrUnknownUser", allowNoLinger, err)
		}
		if mgr.installCalled || state != nil {
			t.Errorf("AllowNoLinger=%v: install continued past the failed lingering check", allowNoLinger)
		}
	}
}

func TestHandleInstall_FragmentPathMismatchFailsInstall(t *testing.T) {
	mgr := newLingerManager(true)
	mgr.verifyErr = errors.New("systemd loads unit testapp.service from /elsewhere/testapp.service, but it was written to /home/alice/.config/systemd/user/testapp.service")

	cfg := userScopeConfig()

	_, err := installWithManager(t, cfg, mgr)
	if err == nil {
		t.Fatal("handleInstall() = nil, want error")
	}
	if !mgr.verifyCalled {
		t.Error("registration was not verified")
	}
	for _, want := range []string{"testapp", "/elsewhere/testapp.service"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if mgr.enableCalled || mgr.startCalled {
		t.Error("install continued past the failed registration check")
	}
}

func TestHandleInstall_VerifiesRegistrationOnSuccess(t *testing.T) {
	mgr := newLingerManager(true)

	if _, err := installWithManager(t, userScopeConfig(), mgr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mgr.verifyCalled {
		t.Error("registration was not verified")
	}
}
