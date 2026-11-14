//go:build !windows

package updater

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// linkFile is swapped in tests to exercise the copy fallback.
var linkFile = os.Link

// createRollbackBackup preserves the installed binary as the rollback copy
// without ever removing it from the executable path: a crash or kill after
// the backup but before the publication rename leaves the installed path
// intact, so the init system can still start the service. The hard link
// shares the inode with the running binary; on filesystems without hard-link
// support the copy fallback preserves the same property at the cost of a
// second copy.
func createRollbackBackup(currentPath, rollbackPath string) error {
	// A pre-existing rollback copy is replaced, matching the rename this
	// function supersedes: the current binary is about to become the new
	// rollback source regardless of what occupied the name.
	if err := os.Remove(rollbackPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("replace rollback copy: %w", err)
	}
	if linkErr := linkFile(currentPath, rollbackPath); linkErr == nil {
		return nil
	} else if copyErr := copyRollbackBackup(currentPath, rollbackPath); copyErr == nil {
		return nil
	} else {
		return errors.Join(fmt.Errorf("link rollback copy: %w", linkErr), fmt.Errorf("copy rollback copy: %w", copyErr))
	}
}

// discardRollbackBackup undoes a backup whose update never published: the
// installed path was never moved, so the spent copy is simply removed.
func discardRollbackBackup(rollbackPath, _ string) error {
	return os.Remove(rollbackPath)
}

// chmodFile is swapped in tests to exercise a refused mode change.
var chmodFile = func(f *os.File, mode os.FileMode) error {
	return f.Chmod(mode)
}

func copyRollbackBackup(currentPath, rollbackPath string) error {
	//nolint:gosec // the install directory holds the service binary by design
	src, err := os.Open(currentPath)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat rollback source: %w", err)
	}
	//nolint:gosec // an executable kept for rollback needs its exec bits
	dst, err := os.OpenFile(rollbackPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create rollback copy: %w", err)
	}
	cleanup := func(cause error) error {
		_ = dst.Close()
		_ = os.Remove(rollbackPath)
		return cause
	}
	if _, err := io.Copy(dst, src); err != nil {
		return cleanup(fmt.Errorf("write rollback copy: %w", err))
	}
	// OpenFile perms are a umask subject; the contract keeps the rollback
	// copy's exact permission bits, so they are reapplied through the
	// descriptor after the bytes land. Order: bytes, mode, sync, close.
	if err := chmodFile(dst, info.Mode().Perm()); err != nil {
		return cleanup(fmt.Errorf("set rollback copy mode: %w", err))
	}
	if err := syncFile(dst); err != nil {
		return cleanup(fmt.Errorf("sync rollback copy: %w", err))
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(rollbackPath)
		return fmt.Errorf("close rollback copy: %w", err)
	}
	return nil
}
