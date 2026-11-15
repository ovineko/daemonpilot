package daemonpilot

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"
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

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unixgram", socketAddr)
	if err != nil {
		return fmt.Errorf("dialing notify socket: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("setting notify socket deadline: %w", err)
		}
	}

	if _, err := conn.Write([]byte("READY=1")); err != nil {
		return fmt.Errorf("writing readiness notification: %w", err)
	}
	return nil
}
