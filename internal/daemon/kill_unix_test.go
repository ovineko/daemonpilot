//go:build !windows

package daemon

import (
	"os"
	"syscall"
	"testing"
)

func TestDefaultKill(t *testing.T) {
	tests := []struct {
		name    string
		pid     int
		sig     syscall.Signal
		wantErr bool
	}{
		{
			name: "existence check on self",
			pid:  os.Getpid(),
			sig:  0,
		},
		{
			name:    "existence check on invalid pid",
			pid:     -1 << 30,
			sig:     0,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := defaultKill(tt.pid, tt.sig)
			if tt.wantErr && err == nil {
				t.Fatalf("defaultKill(%d, %d) = nil, want error", tt.pid, tt.sig)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("defaultKill(%d, %d) returned error: %v", tt.pid, tt.sig, err)
			}
		})
	}
}

func TestNewBusyboxManagerHasKillFunc(t *testing.T) {
	m := newBusyboxManager()
	if m.killFunc == nil {
		t.Fatal("newBusyboxManager did not set killFunc")
	}
	if err := m.killFunc(os.Getpid(), 0); err != nil {
		t.Fatalf("default killFunc failed on self: %v", err)
	}
}
