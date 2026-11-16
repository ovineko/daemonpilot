//go:build linux

package daemonpilot

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestSdNotifyAbstractSocket(t *testing.T) {
	runAbstractSocketCheck(t)
}

func runAbstractSocketCheck(t *testing.T) {
	t.Helper()
	addr := &net.UnixAddr{Name: "\x00daemonpilot-test-notify", Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ENOSYS) {
		t.Skipf("host cannot provide Unix datagram sockets: %v", err)
	}
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	defer func() { _ = conn.Close() }()

	t.Setenv("NOTIFY_SOCKET", "@daemonpilot-test-notify")
	if err := sdNotifyReady(); err != nil {
		t.Fatalf("sdNotify: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(testsupport.Scale(time.Second))); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := string(buf[:n]); got != "READY=1" {
		t.Errorf("got %q, want %q", got, "READY=1")
	}
}
