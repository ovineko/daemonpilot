package daemonpilot

import (
	"errors"
	"strings"
	"testing"

	"github.com/ovineko/daemonpilot/internal/daemon"
)

// runWithDetection drives handleRun with the watchdog and signal handling
// stubbed out, returning the logger that recorded the run.
func runWithDetection(
	t *testing.T,
	cfg *Config,
	state *StateData,
	stateErr error,
	det func(daemon.Scope) (daemon.Detection, error),
) *recordingLogger {
	t.Helper()

	sh, nu, nw, es, pm := saveRunVars()
	t.Cleanup(func() { restoreRunVars(sh, nu, nw, es, pm) })
	saved := saveCommandVars()
	t.Cleanup(func() { restoreCommandVars2(saved) })

	setupSignalHandler = fakeSignalHandler
	newWatchdogFn = func(*Config) Runner { return &mockRunner{} }
	loadStateFn = func(string, string) (*StateData, error) { return state, stateErr }
	if det != nil {
		detectForScope = det
	}
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }

	logger := &recordingLogger{}
	cfg.Logger = logger

	if err := handleRun(cfg); err != nil {
		t.Fatalf("handleRun returned an error, want none: %v", err)
	}
	return logger
}

func hasCall(logger *recordingLogger, level, substr string) bool {
	for _, c := range logger.calls {
		if c.level == level && strings.Contains(c.msg, substr) {
			return true
		}
	}
	return false
}

// A divergence between the state file and the running configuration must never
// be fatal: the generated unit restarts always with no rate limit, so exiting
// would turn a diagnosis into an unbounded crash loop.
func TestHandleRunStateDivergenceIsNotFatal(t *testing.T) {
	tests := []struct {
		name      string
		cfg       Config
		state     *StateData
		stateErr  error
		wantLevel string
		wantMsg   string
		wantQuiet bool
	}{
		{
			name: "scope divergence logs at error level",
			cfg:  Config{Name: "testapp", InstallDir: "/opt/testapp"},
			state: &StateData{
				ServiceName: "testapp", InitSystem: "systemd", Scope: ScopeUser, InstallDir: "/opt/testapp",
			},
			wantLevel: "error",
			wantMsg:   "install scope differs",
		},
		{
			name: "install dir divergence logs at error level",
			cfg:  Config{Name: "testapp", InstallDir: "/home/bob/.local/share/testapp", Scope: ScopeUser},
			state: &StateData{
				ServiceName: "testapp", InitSystem: "systemd", Scope: ScopeUser,
				InstallDir: "/home/alice/.local/share/testapp",
			},
			wantLevel: "error",
			wantMsg:   "install dir differs",
		},
		{
			// RecordRollbackDigest writes a state file for a service that was
			// never installed through daemonpilot. It has no scope to diverge from,
			// and reading its absent one as system scope would report a bogus
			// divergence on every start of a user-scope service.
			name:      "digest-only state file reports no divergence",
			cfg:       Config{Name: "testapp", InstallDir: "/home/alice/.local/share/testapp", Scope: ScopeUser},
			state:     &StateData{ServiceName: "testapp", RollbackDigest: "abc123"},
			wantQuiet: true,
		},
		{
			name:      "unreadable state file warns only",
			cfg:       Config{Name: "testapp", InstallDir: "/opt/testapp"},
			stateErr:  errors.New("permission denied"),
			wantLevel: "warn",
			wantMsg:   "failed to load state file",
		},
		{
			name:      "state file without a scope reads as system",
			cfg:       Config{Name: "testapp", InstallDir: "/opt/testapp"},
			state:     &StateData{ServiceName: "testapp", InitSystem: "systemd"},
			wantQuiet: true,
		},
		{
			name:      "no state file is silent",
			cfg:       Config{Name: "testapp", InstallDir: "/opt/testapp"},
			wantQuiet: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			logger := runWithDetection(t, &cfg, tt.state, tt.stateErr, nil)

			if tt.wantQuiet {
				if hasCall(logger, "error", "differs") {
					t.Errorf("unexpected divergence report: %v", logger.calls)
				}
				return
			}
			if !hasCall(logger, tt.wantLevel, tt.wantMsg) {
				t.Errorf("missing %s log containing %q, got %v", tt.wantLevel, tt.wantMsg, logger.calls)
			}
		})
	}
}

// Scope filtering may select a manager that is not the host's init system. The
// error-only log filter is a property of the host — a flash-backed board must
// not start writing debug logs because the install runs under crontab.
func TestHandleRunEmbeddedLogFilterKeysOffHostInit(t *testing.T) {
	tests := []struct {
		name           string
		hostInit       string
		managerName    string
		wantInitSystem string
		wantFiltered   bool
	}{
		{
			name:           "embedded host with a fallback manager stays filtered",
			hostInit:       "procd",
			managerName:    "crontab",
			wantInitSystem: "procd",
			wantFiltered:   true,
		},
		{
			name:           "non-embedded host is unfiltered",
			hostInit:       "systemd",
			managerName:    "systemd",
			wantInitSystem: "systemd",
		},
		{
			name:           "missing host init falls back to the manager name",
			managerName:    "busybox",
			wantInitSystem: "busybox",
			wantFiltered:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Name: "testapp", InstallDir: "/opt/testapp"}
			runWithDetection(t, cfg, nil, nil, func(daemon.Scope) (daemon.Detection, error) {
				return daemon.Detection{
					Manager:        &mockServiceManager{nameVal: tt.managerName},
					HostInitSystem: tt.hostInit,
				}, nil
			})

			if cfg.InitSystemName != tt.wantInitSystem {
				t.Errorf("InitSystemName = %q, want %q", cfg.InitSystemName, tt.wantInitSystem)
			}
			_, filtered := cfg.Logger.(embeddedFilterLogger)
			if filtered != tt.wantFiltered {
				t.Errorf("embedded log filter applied = %v, want %v", filtered, tt.wantFiltered)
			}
		})
	}
}

// A busybox/procd board can never resolve a user-scope manager — every embedded
// manager returns false from SupportsScope(ScopeUser) — so the detection error
// path is the only one that runs there. The host init system must still reach
// the log filter, or the board writes full Debug logging to flash.
func TestHandleRunEmbeddedLogFilterSurvivesDetectionFailure(t *testing.T) {
	tests := []struct {
		name         string
		hostInit     string
		detectErr    error
		wantFiltered bool
	}{
		{
			name:         "embedded host with an unsupported scope stays filtered",
			hostInit:     "procd",
			detectErr:    daemon.ErrScopeNotSupported,
			wantFiltered: true,
		},
		{
			name:      "non-embedded host with an unreachable user manager is unfiltered",
			hostInit:  "systemd",
			detectErr: daemon.ErrUserManagerUnreachable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Name: "testapp", InstallDir: "/opt/testapp", Scope: ScopeUser}
			runWithDetection(t, cfg, nil, nil, func(daemon.Scope) (daemon.Detection, error) {
				return daemon.Detection{HostInitSystem: tt.hostInit}, tt.detectErr
			})

			if cfg.InitSystemName != tt.hostInit {
				t.Errorf("InitSystemName = %q, want %q", cfg.InitSystemName, tt.hostInit)
			}
			if _, filtered := cfg.Logger.(embeddedFilterLogger); filtered != tt.wantFiltered {
				t.Errorf("embedded log filter applied = %v, want %v", filtered, tt.wantFiltered)
			}
			if cfg.CanInitRestartWatchdog {
				t.Error("CanInitRestartWatchdog = true, want false when detection failed")
			}
		})
	}
}

func TestHandleRunDetectionFailureIsNotFatal(t *testing.T) {
	cfg := &Config{Name: "testapp", InstallDir: "/opt/testapp", Scope: ScopeUser}
	logger := runWithDetection(t, cfg, nil, nil, func(daemon.Scope) (daemon.Detection, error) {
		return daemon.Detection{}, daemon.ErrUserManagerUnreachable
	})

	if !hasCall(logger, "warn", "failed to detect init system") {
		t.Errorf("missing detection warning, got %v", logger.calls)
	}
	if cfg.CanInitRestartWatchdog {
		t.Error("CanInitRestartWatchdog = true, want false when detection failed")
	}
}

func TestHandleRunPassesScopeToDetection(t *testing.T) {
	cfg := &Config{Name: "testapp", InstallDir: "/home/alice/.local/share/testapp", Scope: ScopeUser}

	var gotScope daemon.Scope
	runWithDetection(t, cfg, nil, nil, func(scope daemon.Scope) (daemon.Detection, error) {
		gotScope = scope
		return daemon.Detection{
			Manager:        &mockServiceManager{nameVal: "systemd"},
			HostInitSystem: "systemd",
		}, nil
	})

	if gotScope != daemon.ScopeUser {
		t.Errorf("detection scope = %v, want %v", gotScope, daemon.ScopeUser)
	}
}
