package daemonpilot

import "os"

// sdNotifyReady sends the systemd readiness notification over NOTIFY_SOCKET.
// It is a no-op when the service was not started by systemd with Type=notify.
func sdNotifyReady() error {
	socketAddr := os.Getenv("NOTIFY_SOCKET")
	if socketAddr == "" {
		return nil
	}

	if socketAddr[0] == '@' {
		socketAddr = "\x00" + socketAddr[1:]
	}

	return sendReadyNotification(socketAddr)
}
