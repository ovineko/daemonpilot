package daemonpilot

import "os"

// RespawnWatchdog exits with ExitRespawnWatchdog to request a watchdog restart.
// The caller must flush its own logs first; os.Exit does not run deferred calls.
func RespawnWatchdog() { os.Exit(ExitRespawnWatchdog) }

// StopPermanently exits with ExitPermanent to end supervision.
// The caller must flush its own logs first; os.Exit does not run deferred calls.
func StopPermanently() { os.Exit(ExitPermanent) }

// ResidentWatchdogStale reports a non-empty resident version differing from ownWatchdogVersion.
// An empty version identifies a watchdog too old to understand exit 101.
func ResidentWatchdogStale(ownWatchdogVersion string) bool {
	resident := ReadStaticEnv().WatchdogVersion
	return resident != "" && resident != ownWatchdogVersion
}
