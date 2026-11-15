//go:build unix

package daemonpilot

import (
	"os"
	"testing"
)

func makeReadOnlyDirectory(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not deny writes")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod() error: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("restore directory permissions: %v", err)
		}
	})
}
