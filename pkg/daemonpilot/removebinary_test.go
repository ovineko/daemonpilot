package daemonpilot

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRemoveInstalledBinary covers the ordinary path. The Windows
// move-aside-and-schedule fallback only triggers for a RUNNING image, which
// cannot be produced from a test.
func TestRemoveInstalledBinary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent")
	if err := os.WriteFile(path, []byte("x"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := removeInstalledBinary(path, nil); err != nil {
		t.Fatalf("removeInstalledBinary: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the binary is still there")
	}
}

// TestRemoveInstalledBinaryReportsAbsence: uninstall treats fs.ErrNotExist as
// "already gone", so the error has to stay recognisable through the helper.
func TestRemoveInstalledBinaryReportsAbsence(t *testing.T) {
	err := removeInstalledBinary(filepath.Join(t.TempDir(), "absent"), nil)
	if err == nil {
		t.Fatal("removing an absent binary reported success")
	}
	if !os.IsNotExist(err) {
		t.Errorf("error = %v, want it to satisfy os.IsNotExist so uninstall reads it as already gone", err)
	}
}
