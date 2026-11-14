package daemonpilot

import (
	"fmt"
	"os"
)

// NotifyReady signals to the watchdog that the worker has finished
// initialization and is ready to serve. When the watchdog receives this
// signal it marks the worker as stable and resets the restart backoff
// counter.
//
// When the init system is systemd, NotifyReady additionally sends
// sd_notify("READY=1") to inform systemd the service is ready.
//
// If IPC is not available (e.g., running standalone), NotifyReady returns
// nil without error.
func NotifyReady() error {
	client, err := getOrCreateIPCClient()
	if err == nil {
		if err := client.NotifyReady(); err != nil {
			return fmt.Errorf("notifying watchdog: %w", err)
		}
	}

	if os.Getenv(EnvInitSystem) == "systemd" {
		if err := sdNotifyReady(); err != nil {
			return err
		}
	}

	return nil
}
