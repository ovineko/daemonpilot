package daemonpilot_test

import (
	"testing"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

// Deployed workers and watchdogs require these exact protocol values.
func TestExitCodeContract(t *testing.T) {
	if daemonpilot.ExitPermanent != 100 {
		t.Errorf("ExitPermanent = %d, contract requires %d", daemonpilot.ExitPermanent, 100)
	}
	if daemonpilot.ExitRespawnWatchdog != 101 {
		t.Errorf("ExitRespawnWatchdog = %d, contract requires %d", daemonpilot.ExitRespawnWatchdog, 101)
	}
	if daemonpilot.EnvWatchdogVersion != "DAEMONPILOT_WATCHDOG_VERSION" {
		t.Errorf("EnvWatchdogVersion = %q, contract requires %q", daemonpilot.EnvWatchdogVersion, "DAEMONPILOT_WATCHDOG_VERSION")
	}
}
