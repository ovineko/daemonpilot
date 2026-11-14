//go:build !windows

package daemonpilot

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

// chownFileNoFollow hands a file to the service account through a descriptor it
// has already vetted. os.Lchown would not follow a symlink, but a hard link is
// not a symlink — in the group-writable system install directory this path
// exists for, an unprivileged account can plant one and have a root install
// hand over an inode that is also reachable elsewhere.
func TestChownFileNoFollow(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    func(t *testing.T) string
		wantErr string
	}{
		{
			name: "a lone regular file is accepted",
			path: func(t *testing.T) string {
				t.Helper()
				p := filepath.Join(t.TempDir(), "lock")
				if err := os.WriteFile(p, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			name: "a hard link is refused",
			path: func(t *testing.T) string {
				t.Helper()
				p := filepath.Join(t.TempDir(), "lock")
				if err := os.Link(regular, p); err != nil {
					t.Fatal(err)
				}
				return p
			},
			wantErr: "hard links",
		},
		{
			name: "a symlink is refused",
			path: func(t *testing.T) string {
				t.Helper()
				p := filepath.Join(t.TempDir(), "lock")
				if err := os.Symlink(regular, p); err != nil {
					t.Fatal(err)
				}
				return p
			},
			wantErr: "to change its owner",
		},
		{
			name: "a directory is refused",
			path: func(t *testing.T) string { t.Helper(); return t.TempDir() },
			// Some platforms refuse the O_RDONLY open of a directory outright,
			// others reach the regular-file check; either is a refusal.
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.path(t)
			// -1/-1 leaves the ownership unchanged, so the test needs no privileges:
			// what is under test is which files are refused before the chown.
			err := chownFileNoFollow(path, -1, -1)
			if tt.name == "a lone regular file is accepted" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %s to be refused", path)
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

// A FIFO is the one refused file type that cannot be tested through the table
// above: opening it read-only blocks until a writer arrives, so a regression
// here hangs the install rather than failing it. The call runs on its own
// goroutine so that regression surfaces as a test failure instead of a
// ten-minute package timeout.
func TestChownFileNoFollowRefusesAFifoWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- chownFileNoFollow(path, -1, -1) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a FIFO at the lock path to be refused")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("error %q does not refuse it as a non-regular file", err)
		}
	case <-time.After(testsupport.Scale(10 * time.Second)):
		t.Fatal("chownFileNoFollow blocked on a FIFO; the open is missing O_NONBLOCK")
	}
}
