//go:build !windows

package updater

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The hard-link backup keeps the installed path in place throughout the
// backup — the property that closes the staged-update crash window.
func TestCreateRollbackBackup_KeepsInstalledPathInPlace(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, "myapp")
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := createRollbackBackup(currentPath, currentPath+".rollback"); err != nil {
		t.Fatalf("createRollbackBackup() error = %v", err)
	}
	if _, err := os.Stat(currentPath); err != nil {
		t.Fatalf("installed path must remain: %v", err)
	}
	info, err := os.Stat(currentPath + ".rollback")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("rollback mode = %v, want %v", info.Mode().Perm(), os.FileMode(0o700))
	}
}

// Filesystems without hard-link support fall back to a verified copy with
// the executable bits preserved.
func TestCreateRollbackBackup_CopiesWhenLinksUnsupported(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, "myapp")
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	origLink := linkFile
	linkFile = func(string, string) error { return errors.New("unsupported") }
	defer func() { linkFile = origLink }()

	if err := createRollbackBackup(currentPath, currentPath+".rollback"); err != nil {
		t.Fatalf("createRollbackBackup() error = %v", err)
	}
	data, err := os.ReadFile(currentPath + ".rollback")
	if err != nil {
		t.Fatalf("read rollback copy: %v", err)
	}
	if string(data) != "old-binary" {
		t.Errorf("rollback content = %q, want %q", string(data), "old-binary")
	}
	info, err := os.Stat(currentPath + ".rollback")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("rollback mode = %v, want %v", info.Mode().Perm(), os.FileMode(0o755))
	}
}
