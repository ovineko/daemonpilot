//go:build windows

package updater

import "os"

// createRollbackBackup preserves the installed binary as the rollback copy.
// Windows refuses to rename a new image over the file a running process is
// executing, so the current binary must move aside before the staged
// executable can take its place; the crash window this creates has no
// hard-link equivalent here and is a documented platform limitation.
func createRollbackBackup(currentPath, rollbackPath string) error {
	return os.Rename(currentPath, rollbackPath)
}

// discardRollbackBackup undoes a backup whose update never published: the
// installed path must be restored by moving the copy back, because on
// Windows it was renamed away from the executable path.
func discardRollbackBackup(rollbackPath, currentPath string) error {
	return os.Rename(rollbackPath, currentPath)
}
