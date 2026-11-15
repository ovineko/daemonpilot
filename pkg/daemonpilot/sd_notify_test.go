//go:build unix

package daemonpilot

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

// notifySocketPath returns a socket path short enough for sun_path, which is
// 104 bytes on darwin/BSD. t.TempDir() embeds the test name, so long test names
// overflow it and ListenUnixgram fails with EINVAL.
func notifySocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sn") //nolint:usetesting // t.TempDir() embeds the test name and overflows sun_path
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "notify.sock")
}

func TestSdNotifyNoSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	err := sdNotifyReady()
	if err != nil {
		t.Fatalf("expected nil when NOTIFY_SOCKET unset, got: %v", err)
	}
}

func TestSdNotifySendsMessage(t *testing.T) {
	sockPath := notifySocketPath(t)

	addr := &net.UnixAddr{Name: sockPath, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ENOSYS) {
		t.Skipf("host cannot provide Unix datagram sockets: %v", err)
	}
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	defer func() { _ = conn.Close() }()

	t.Setenv("NOTIFY_SOCKET", sockPath)

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
	got := string(buf[:n])
	if got != "READY=1" {
		t.Errorf("got %q, want %q", got, "READY=1")
	}
}

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

func TestSdNotifyInvalidSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "/nonexistent/path/notify.sock")
	err := sdNotifyReady()
	if err == nil {
		t.Fatal("expected error for invalid socket path")
	}
}

func TestNotifyReadyCallsSdNotifyOnSystemd(t *testing.T) {
	sockPath := notifySocketPath(t)

	addr := &net.UnixAddr{Name: sockPath, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ENOSYS) {
		t.Skipf("host cannot provide Unix datagram sockets: %v", err)
	}
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	defer func() { _ = conn.Close() }()

	t.Setenv("NOTIFY_SOCKET", sockPath)
	t.Setenv(EnvInitSystem, "systemd")

	setupNoIPC(t)

	if err := NotifyReady(); err != nil {
		t.Fatalf("NotifyReady: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(testsupport.Scale(time.Second))); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got := string(buf[:n])
	if got != "READY=1" {
		t.Errorf("got %q, want %q", got, "READY=1")
	}
}

func TestNotifyReadySkipsSdNotifyOnNonSystemd(t *testing.T) {
	sockPath := notifySocketPath(t)

	addr := &net.UnixAddr{Name: sockPath, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ENOSYS) {
		t.Skipf("host cannot provide Unix datagram sockets: %v", err)
	}
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	defer func() { _ = conn.Close() }()

	t.Setenv("NOTIFY_SOCKET", sockPath)
	t.Setenv(EnvInitSystem, "launchd")

	setupNoIPC(t)

	if err := NotifyReady(); err != nil {
		t.Fatalf("NotifyReady: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(testsupport.Scale(100 * time.Millisecond)))
	buf := make([]byte, 128)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected no message on non-systemd init system")
	}
	if !os.IsTimeout(err) {
		t.Fatalf("expected timeout, got: %v", err)
	}
}
