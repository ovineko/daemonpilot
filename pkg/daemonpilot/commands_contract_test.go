package daemonpilot

import (
	"errors"
	"os"
	"testing"

	"github.com/ovineko/daemonpilot/internal/daemon"
)

func TestErrNotInstalled(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Config) error
		want string
	}{
		{"uninstall", handleUninstall, `service "testapp" is not installed (no state file found in /opt/testapp)`},
		{"validate", validateServiceConfig, `service "testapp" is not installed (no state file found)`},
		{"update-config", handleUpdateConfig, `service "testapp" is not installed (no state file found)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := saveCommandVars()
			defer restoreCommandVars2(saved)
			mock := &mockServiceManager{manifestPath: "absent"}
			detectManager = func() (daemon.ServiceManager, error) { return mock, nil }
			initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
			loadStateFn = func(string, string) (*StateData, error) { return nil, nil }
			statFileFn = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
			err := tc.call(&Config{Name: "testapp", InstallDir: "/opt/testapp", Logger: NewDefaultLogger(LogLevelInfo)})
			if !errors.Is(err, ErrNotInstalled) || err.Error() != tc.want {
				t.Fatalf("got %v; want wrapped ErrNotInstalled with %q", err, tc.want)
			}
		})
	}
}
