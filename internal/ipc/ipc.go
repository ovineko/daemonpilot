package ipc

import "time"

// EnvIPCFD names the environment variable carrying the inherited pipe file
// descriptors from the watchdog to the worker.
const EnvIPCFD = "DAEMONPILOT_IPC_FD"

// Message types exchanged over the IPC socket.
const (
	MsgTypeRequest byte = 1
	MsgTypeReady   byte = 2
	MsgTypeLogs    byte = 3
)

// LogEntry is one retained watchdog log line.
type LogEntry struct {
	Sequence uint64 `json:"sequence"`
	Line     string `json:"line"`
}

// LogBatch is the bounded response to a log collection request.
type LogBatch struct {
	Entries []LogEntry `json:"entries"`
	Dropped uint64     `json:"dropped"`
}

// DynamicInfo holds the dynamic runtime data sent from the watchdog to the
// worker via IPC. Fields use basic types to avoid import cycles with
// pkg/daemonpilot.
type DynamicInfo struct {
	WatchdogPID       int           `json:"watchdog_pid"`
	WorkerPID         int           `json:"worker_pid"`
	WatchdogStartTime time.Time     `json:"watchdog_start_time"`
	WorkerStartTime   time.Time     `json:"worker_start_time"`
	RestartCount      int           `json:"restart_count"`
	CurrentBackoff    time.Duration `json:"current_backoff"`

	HealthTimestamp time.Time `json:"health_timestamp,omitzero"`
	HealthError     string    `json:"health_error,omitempty"`
	HealthFailures  int       `json:"health_failures,omitempty"`

	UpdateLastCheck    time.Time `json:"update_last_check,omitzero"`
	UpdateResult       string    `json:"update_result,omitempty"`
	UpdateAvailableVer string    `json:"update_available_ver,omitempty"`
	UpdateStatus       string    `json:"update_status,omitempty"`
}
