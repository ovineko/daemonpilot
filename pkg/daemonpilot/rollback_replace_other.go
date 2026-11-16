//go:build !windows

package daemonpilot

import "os"

func replaceRestoredBinary(tempPath, targetPath string) error {
	return os.Rename(tempPath, targetPath)
}
