package updater

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReplaceBinaryRefusesMissingDestination is the negative control for the
// fleet bug: removing the destination Lstat in replaceBinaryFor makes the
// rename silently create an extensionless sibling and makes this test fail.
func TestReplaceBinaryRefusesMissingDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "agent.new")
	dest := filepath.Join(dir, "agent.exe")
	if err := os.WriteFile(src, []byte("new"), 0o750); err != nil {
		t.Fatal(err)
	}

	err := replaceBinaryFor(src, dest, "windows")
	if err == nil || !strings.Contains(err.Error(), dest) {
		t.Fatalf("replaceBinaryFor() error = %v, want the missing destination path", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("missing destination was created: %v", statErr)
	}
	if _, statErr := os.Stat(src); statErr != nil {
		t.Fatalf("source was consumed after refusing the replacement: %v", statErr)
	}
}

func TestReplaceBinaryWindowsMoveAsideAndRollback(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "agent.exe")
	src := filepath.Join(dir, "agent.new.exe")
	if err := os.WriteFile(dest, []byte("old"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new"), 0o750); err != nil {
		t.Fatal(err)
	}

	originalRename := renameFile
	t.Cleanup(func() { renameFile = originalRename })
	calls := 0
	renameFile = func(oldPath, newPath string) error {
		calls++
		if calls == 1 || calls == 3 {
			return errors.New("simulated sharing violation")
		}
		return os.Rename(oldPath, newPath)
	}

	err := replaceBinaryFor(src, dest, "windows")
	if err == nil {
		t.Fatal("replaceBinaryFor() succeeded after the replacement rename failed")
	}
	got, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("read restored destination: %v", readErr)
	}
	if string(got) != "old" {
		t.Fatalf("restored destination = %q, want old", got)
	}
	if _, statErr := os.Stat(src); statErr != nil {
		t.Fatalf("failed update source should remain available: %v", statErr)
	}
}

func TestReplaceBinaryWindowsMoveAsideSucceeds(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "agent.exe")
	src := filepath.Join(dir, "agent.new.exe")
	if err := os.WriteFile(dest, []byte("old"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new"), 0o750); err != nil {
		t.Fatal(err)
	}

	originalRename := renameFile
	t.Cleanup(func() { renameFile = originalRename })
	calls := 0
	renameFile = func(oldPath, newPath string) error {
		calls++
		if calls == 1 {
			return errors.New("simulated sharing violation")
		}
		return os.Rename(oldPath, newPath)
	}

	if err := replaceBinaryFor(src, dest, "windows"); err != nil {
		t.Fatalf("replaceBinaryFor(): %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("replacement content = %q, want new", got)
	}
}

// TestReplaceBinaryOverwrites is the ordinary path — all POSIX ever needs. The
// Windows aside-and-rename fallback only triggers when the target is a RUNNING
// image, which cannot be produced here.
func TestReplaceBinaryOverwrites(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "agent")
	if err := os.WriteFile(dest, []byte("old"), 0o750); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "agent.new")
	if err := os.WriteFile(src, []byte("new"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := replaceBinary(src, dest); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("installed content = %q, want %q", got, "new")
	}
	if _, err := os.Stat(src); err == nil {
		t.Error("the staged .new file survived the replacement")
	}
}

// TestReplaceBinaryReportsAFailure: the update path must not report success when
// the binary did not move. A missing source is the cheap way to reach that.
func TestReplaceBinaryReportsAFailure(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "agent")
	if err := os.WriteFile(dest, []byte("old"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(filepath.Join(dir, "absent.new"), dest); err == nil {
		t.Fatal("replacing from a missing source reported success")
	}
	// And it must leave the incumbent alone.
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("the incumbent binary is gone after a failed replacement: %v", err)
	}
	if string(got) != "old" {
		t.Errorf("incumbent content = %q, want it untouched", got)
	}
}

// TestSweepSupersededRemovesOnlyLeftovers: the sweep runs at every start, so a
// pattern that matched a real artifact would delete the working binary — or the
// .rollback copy the staged path depends on.
func TestSweepSupersededRemovesOnlyLeftovers(t *testing.T) {
	dir := t.TempDir()
	const name = "sample-agent.exe"
	keep := []string{name, name + ".rollback", name + ".new", "config.json", "other.exe" + supersededSuffix + "zz"}
	drop := []string{name + supersededSuffix + "abc", name + supersededSuffix + "def"}
	for _, n := range append(append([]string{}, keep...), drop...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	sweepSuperseded(dir, name)

	for _, n := range keep {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("sweep deleted %s, which it must not touch: %v", n, err)
		}
	}
	for _, n := range drop {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			t.Errorf("sweep left %s behind", n)
		}
	}
}

// TestSupersededSuffixIsDistinctive guards the sweep's pattern against matching
// the artifacts the update machinery itself depends on.
func TestSupersededSuffixIsDistinctive(t *testing.T) {
	for _, real := range []string{".new", ".rollback", ".rollback.invalid", ".restore"} {
		if strings.HasPrefix(real, supersededSuffix) || strings.HasPrefix(supersededSuffix, real) {
			t.Errorf("superseded suffix %q collides with the real artifact suffix %q", supersededSuffix, real)
		}
	}
}
