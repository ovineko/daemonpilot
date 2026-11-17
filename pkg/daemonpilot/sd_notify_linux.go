//go:build linux

package daemonpilot

import (
	"fmt"
	"syscall"
)

func sendReadyNotification(socketAddr string) error {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("creating notify socket: %w", err)
	}
	defer syscall.Close(fd)

	// A notify datagram must not stall service startup if the socket is blocked.
	addr := &syscall.SockaddrUnix{Name: socketAddr}
	if err := syscall.Sendto(fd, []byte("READY=1"), syscall.MSG_DONTWAIT, addr); err != nil {
		return fmt.Errorf("writing readiness notification: %w", err)
	}
	return nil
}
