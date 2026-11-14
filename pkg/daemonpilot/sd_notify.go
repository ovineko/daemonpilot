package daemonpilot

import (
	"context"
	"fmt"
	"net"
	"os"
)

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

	// The address comes from systemd via NOTIFY_SOCKET, not from user input.
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "unixgram", socketAddr)
	if err != nil {
		return fmt.Errorf("dialing notify socket: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("READY=1")); err != nil {
		return fmt.Errorf("writing readiness notification: %w", err)
	}
	return nil
}
