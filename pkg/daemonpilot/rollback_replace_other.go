//go:build !windows

package daemonpilot

import "os"

func replaceRestoredBinary(tempPath, targetPath string, _, _, _ string, _ os.FileMode, _ string) error {
	return os.Rename(tempPath, targetPath)
}

func isRollbackRecoveryExecutable(*Config) bool { return false }
