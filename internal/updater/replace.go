package updater

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// supersededSuffix marks a binary that was moved aside because it could not be
// overwritten in place. Distinctive enough that the sweep can never match a real
// artifact, and it carries a timestamp so two updates cannot collide.
const supersededSuffix = ".superseded-"

var renameFile = os.Rename

// replaceBinary puts newPath at destPath, handling the one rule that exists only
// on Windows: an image that is CURRENTLY RUNNING may not be deleted or
// overwritten, but it MAY be renamed.
//
// The fast update path replaces the binary the WATCHDOG IS EXECUTING — that is
// its whole design, since the next worker respawn then picks the new one up with
// no watchdog restart. On POSIX the rename simply detaches the old inode, which
// the running process keeps. On Windows it fails outright with "Access is
// denied", so a Windows node downloaded every update, verified it, failed at the
// last step and stayed on its old version — retrying forever, reporting healthy,
// and never moving. The staged path already does this correctly by accident: it
// renames the current binary to .rollback FIRST, which Windows permits.
//
// The superseded copy is deleted on the spot when nothing holds it, and swept by
// CleanupStaleFiles on the next start when something does — which, for the
// running watchdog, is always.
func replaceBinary(newPath, destPath string) error {
	return replaceBinaryFor(newPath, destPath, runtime.GOOS)
}

func replaceBinaryFor(newPath, destPath, goos string) error {
	if _, err := os.Lstat(destPath); err != nil {
		return fmt.Errorf("replace binary: expected destination %s: %w", destPath, err)
	}
	if err := renameFile(newPath, destPath); err == nil {
		return nil
	} else if goos != "windows" {
		return fmt.Errorf("rename binary: %w", err)
	}

	aside := destPath + supersededSuffix + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := renameFile(destPath, aside); err != nil {
		return fmt.Errorf("move the running binary aside: %w", err)
	}
	if err := renameFile(newPath, destPath); err != nil {
		// Put the incumbent back: a failed update must leave a working install.
		if rbErr := renameFile(aside, destPath); rbErr != nil {
			return fmt.Errorf("rename binary: %w (and restoring %s failed: %w)", err, destPath, rbErr)
		}
		return fmt.Errorf("rename binary: %w", err)
	}
	// Held by the process still executing it; swept on the next start.
	_ = os.Remove(aside)
	return nil
}

// sweepSuperseded removes binaries left behind by an in-place replacement. Best
// effort by construction: one still held simply fails and is tried again next
// time.
func sweepSuperseded(dir, binaryName string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := binaryName + supersededSuffix
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
