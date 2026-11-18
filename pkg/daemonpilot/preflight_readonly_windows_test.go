//go:build windows

package daemonpilot

import (
	"os/exec"
	"testing"
)

const everyoneSID = "*S-1-1-0"

func makeReadOnlyDirectory(t *testing.T, dir string) {
	t.Helper()
	if output, err := exec.Command("icacls.exe", dir, "/deny", everyoneSID+":(W)").CombinedOutput(); err != nil {
		t.Fatalf("deny writes to test directory: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		if output, err := exec.Command("icacls.exe", dir, "/remove:d", everyoneSID).CombinedOutput(); err != nil {
			t.Errorf("restore test directory ACL: %v\n%s", err, output)
		}
	})
}
