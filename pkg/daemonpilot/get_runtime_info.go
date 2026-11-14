package daemonpilot

import (
	"sync"
	"time"

	"github.com/ovineko/daemonpilot/internal/ipc"
)

var (
	ipcClientOnce sync.Once
	ipcClient     *ipc.Client
	errIPCClient  error
)

// ipcClientFactory is the function used to create an IPC client.
// It can be replaced in tests.
var ipcClientFactory = ipc.NewClientFromEnv

func getOrCreateIPCClient() (*ipc.Client, error) {
	ipcClientOnce.Do(func() {
		ipcClient, errIPCClient = ipcClientFactory()
	})
	return ipcClient, errIPCClient
}

// resetIPCClient resets the singleton IPC client. Only for testing.
func resetIPCClient() {
	ipcClientOnce = sync.Once{}
	ipcClient = nil
	errIPCClient = nil
}

// GetRuntimeInfo returns a snapshot of the current daemonpilot runtime state.
// It reads static data from environment variables and dynamic data from the
// watchdog via IPC. If IPC is not available (e.g., running standalone), the
// returned RuntimeInfo contains only static fields.
// It is safe for concurrent use from multiple goroutines.
func GetRuntimeInfo() (*RuntimeInfo, error) {
	info := ReadStaticEnv()

	client, err := getOrCreateIPCClient()
	if err != nil {
		return &info, nil //nolint:nilerr // IPC is optional: fall back to the static environment snapshot
	}

	dyn, err := client.Request()
	if err != nil {
		return &info, nil //nolint:nilerr // IPC is optional: fall back to the static environment snapshot
	}

	now := time.Now()
	info.WatchdogPID = dyn.WatchdogPID
	info.WorkerPID = dyn.WorkerPID
	if !dyn.WatchdogStartTime.IsZero() {
		info.WatchdogUptime = now.Sub(dyn.WatchdogStartTime)
	}
	if !dyn.WorkerStartTime.IsZero() {
		info.WorkerUptime = now.Sub(dyn.WorkerStartTime)
	}
	info.RestartCount = dyn.RestartCount
	info.CurrentBackoff = dyn.CurrentBackoff

	info.LastHealthCheck = HealthCheckInfo{
		Timestamp:           dyn.HealthTimestamp,
		Error:               dyn.HealthError,
		ConsecutiveFailures: dyn.HealthFailures,
	}

	info.UpdateStatus = UpdateStatusInfo{
		LastCheckTime:    dyn.UpdateLastCheck,
		Result:           dyn.UpdateResult,
		AvailableVersion: dyn.UpdateAvailableVer,
		CurrentStatus:    dyn.UpdateStatus,
	}

	return &info, nil
}
