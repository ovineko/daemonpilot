//go:build !windows

package daemonpilot

import "os"

// removeInstalledBinary deletes the installed binary. On POSIX an unlink works
// even while the file is executing — the inode simply outlives the name — so
// there is nothing platform-specific to do.
func removeInstalledBinary(path string, _ Logger) error {
	//nolint:wrapcheck // the caller decides how to report this
	return os.Remove(path)
}
