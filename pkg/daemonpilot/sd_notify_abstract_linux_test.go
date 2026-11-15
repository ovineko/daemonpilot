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

// Linux abstract Unix sockets have no filesystem path; other Unix targets use
// the filesystem-socket tests in sd_notify_test.go.
func TestSdNotifyAbstractSocket(t *testing.T) {
	addr := &net.UnixAddr{Name: "\x00daemonpilot-test-notify", Net: "unixgram"}
	type listenResult struct {
		conn *net.UnixConn
		err  error
	}
	listening := make(chan listenResult, 1)
	go func() {
		conn, err := net.ListenUnixgram("unixgram", addr)
		listening <- listenResult{conn: conn, err: err}
	}()
	var conn *net.UnixConn
	var err error
	select {
	case result := <-listening:
		conn, err = result.conn, result.err
	case <-time.After(testsupport.Scale(2 * time.Second)):
		t.Fatal("ListenUnixgram timed out for an abstract Unix socket")
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ENOSYS) {
		t.Skipf("host cannot provide Unix datagram sockets: %v", err)
	}
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	defer func() { _ = conn.Close() }()

	t.Setenv("NOTIFY_SOCKET", "@daemonpilot-test-notify")
	ready := make(chan error, 1)
	go func() { ready <- sdNotifyReady() }()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("sdNotify: %v", err)
		}
	case <-time.After(testsupport.Scale(2 * time.Second)):
		t.Fatal("sdNotify timed out for an abstract Unix socket")
	}

	type readResult struct {
		message string
		err     error
	}
	read := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 128)
		n, err := conn.Read(buf)
		read <- readResult{message: string(buf[:n]), err: err}
	}()
	select {
	case result := <-read:
		if result.err != nil {
			t.Fatalf("Read: %v", result.err)
		}
		if result.message != "READY=1" {
			t.Errorf("got %q, want %q", result.message, "READY=1")
		}
	case <-time.After(testsupport.Scale(2 * time.Second)):
		t.Fatal("timed out waiting for an abstract-socket readiness notification")
	}
}
