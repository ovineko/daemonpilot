//go:build windows

package daemonpilot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

func replaceRestoredBinary(tempPath, targetPath string) error {
	if _, err := os.Lstat(targetPath); errors.Is(err, fs.ErrNotExist) {
		return os.Rename(tempPath, targetPath)
	} else if err != nil {
		return fmt.Errorf("checking current binary %s: %w", targetPath, err)
	}

	asidePath := StagedNewPath(targetPath)
	if _, err := os.Lstat(asidePath); err == nil {
		return fmt.Errorf("moving running binary aside: staging path %s already exists", asidePath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checking aside path %s: %w", asidePath, err)
	}
	if err := os.Rename(targetPath, asidePath); err != nil {
		return fmt.Errorf("moving running binary aside: %w", err)
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		if restoreErr := os.Rename(asidePath, targetPath); restoreErr != nil {
			return fmt.Errorf("restoring %s: %w (putting the current binary back from %s failed: %w)", targetPath, err, asidePath, restoreErr)
		}
		return fmt.Errorf("restoring %s: %w", targetPath, err)
	}

	// The running image may remain locked at .new until this service exits. The
	// successor's stale-file sweep removes it once the old process has stopped.
	_ = os.Remove(asidePath)
	return nil
}
