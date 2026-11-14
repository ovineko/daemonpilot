package daemonpilot

import (
	"os"
	"time"
)

// Environment variables the watchdog sets for its worker, carrying the static
// half of RuntimeInfo.
const (
	EnvMode                 = "DAEMONPILOT_MODE"
	EnvVersion              = "DAEMONPILOT_VERSION"
	EnvWatchdogVersion      = "DAEMONPILOT_WATCHDOG_VERSION"
	EnvBinaryPath           = "DAEMONPILOT_BINARY_PATH"
	EnvInitSystem           = "DAEMONPILOT_INIT_SYSTEM"
	EnvCanAutoRestart       = "DAEMONPILOT_CAN_AUTO_RESTART"
	EnvIsExecThrough        = "DAEMONPILOT_IS_EXEC_THROUGH"
	EnvMaxMemoryBytes       = "DAEMONPILOT_MAX_MEMORY_BYTES"
	EnvOOMScoreAdj          = "DAEMONPILOT_OOM_SCORE_ADJ"
	EnvParentPID            = "DAEMONPILOT_PARENT_PID"
	EnvDisableParentMonitor = "DAEMONPILOT_DISABLE_PARENT_MONITOR"
	EnvLockStatus           = "DAEMONPILOT_LOCK_STATUS"
)

var envLookup = os.Getenv

// ReadStaticEnv returns the RuntimeInfo fields the watchdog passes through the
// environment, without contacting the watchdog over IPC.
func ReadStaticEnv() RuntimeInfo {
	initSys := envLookup(EnvInitSystem)
	return RuntimeInfo{
		Mode:            RuntimeMode(envLookup(EnvMode)),
		Version:         envLookup(EnvVersion),
		WatchdogVersion: envLookup(EnvWatchdogVersion),
		BinaryPath:      envLookup(EnvBinaryPath),
		InitSystem:      initSys,
		CanAutoRestart:  envLookup(EnvCanAutoRestart) == "true",
		IsExecThrough:   envLookup(EnvIsExecThrough) == "true",
		IsEmbedded:      IsEmbeddedSystem(initSys),
		LockStatus:      LockStatus(envLookup(EnvLockStatus)),
	}
}

// LockStatus reports whether this process has the single-instance guarantee.
type LockStatus string

// Stable values exported to supervised worker processes.
const (
	LockStatusHeld     LockStatus = "ok"
	LockStatusUnlocked LockStatus = "unlocked"
	// LockStatusUnsupported means this platform has NO single-instance lock to
	// take — not that taking one was attempted and failed.
	//
	// The distinction is the whole point. "unlocked" is a measured failure on a
	// host that does have the mechanism, and a consumer is right to treat it as
	// degraded. Reporting it where no mechanism exists paints every node on that
	// platform permanently degraded for a reason nobody can act on, and an alarm
	// that is always on is an alarm nobody reads — so the next real one is missed
	// too. Same rule as everywhere else here: not measured is never measured bad.
	LockStatusUnsupported LockStatus = "unsupported"
)

// RuntimeMode describes the operating mode of the current process.
type RuntimeMode string

// Modes a daemonpilot process can run in.
const (
	ModeWatchdog    RuntimeMode = "watchdog"
	ModeWorker      RuntimeMode = "worker"
	ModeExecThrough RuntimeMode = "exec-through"
	ModeStandalone  RuntimeMode = "standalone"
)

// HealthCheckInfo holds the state of the most recent health check.
type HealthCheckInfo struct {
	Timestamp           time.Time `json:"timestamp,omitzero"`
	Error               string    `json:"error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures,omitempty"`
}

// UpdateStatusInfo holds the state of the most recent update check.
type UpdateStatusInfo struct {
	LastCheckTime    time.Time `json:"last_check_time,omitzero"`
	Result           string    `json:"result,omitempty"`
	AvailableVersion string    `json:"available_version,omitempty"`
	CurrentStatus    string    `json:"current_status,omitempty"`
}

// RuntimeInfo provides a snapshot of the daemonpilot runtime state.
// Static fields (Mode, Version, BinaryPath, InitSystem, etc.) are populated
// from environment variables set by the watchdog when spawning the worker.
// Dynamic fields (RestartCount, CurrentBackoff, Uptimes, etc.) are populated
// via IPC from the watchdog.
type RuntimeInfo struct {
	Mode            RuntimeMode `json:"mode"`
	IsExecThrough   bool        `json:"is_exec_through"`
	Version         string      `json:"version"`
	WatchdogVersion string      `json:"watchdog_version"`
	BinaryPath      string      `json:"binary_path"`

	WatchdogPID    int           `json:"watchdog_pid,omitempty"`
	WorkerPID      int           `json:"worker_pid,omitempty"`
	WatchdogUptime time.Duration `json:"watchdog_uptime,omitempty"`
	WorkerUptime   time.Duration `json:"worker_uptime,omitempty"`

	InitSystem     string     `json:"init_system"`
	CanAutoRestart bool       `json:"can_auto_restart"`
	IsEmbedded     bool       `json:"is_embedded"`
	LockStatus     LockStatus `json:"lock_status"`

	RestartCount   int           `json:"restart_count,omitempty"`
	CurrentBackoff time.Duration `json:"current_backoff,omitempty"`

	LastHealthCheck HealthCheckInfo  `json:"last_health_check,omitzero"`
	UpdateStatus    UpdateStatusInfo `json:"update_status,omitzero"`
}
