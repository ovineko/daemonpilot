//go:build !linux

package daemonpilot

import (
	"context"
	"fmt"
	"net"
	"time"
)

func sendReadyNotification(socketAddr string) error {
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
