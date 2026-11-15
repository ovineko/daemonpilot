//go:build windows

package daemonpilot

import "testing"

func TestSdNotifyNoSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := sdNotifyReady(); err != nil {
		t.Fatalf("expected nil when NOTIFY_SOCKET is unset, got: %v", err)
	}
}

func TestNotifyReadySkipsUnixgramOnWindows(t *testing.T) {
	setupNoIPC(t)
	t.Setenv("NOTIFY_SOCKET", `C:\notify.sock`)
	t.Setenv(EnvInitSystem, "windows")
	if err := NotifyReady(); err != nil {
		t.Fatalf("NotifyReady() should not use systemd Unixgram on Windows: %v", err)
	}
}
