//go:build windows

package daemonpilot

import "os"

// chownFileNoFollow falls back to os.Lchown on Windows, which reports
// syscall.EWINDOWS. Ownership is not part of the Windows service model here and
// Config.ServiceUser is only consumed by the Unix init systems.
func chownFileNoFollow(path string, uid, gid int) error {
	//nolint:wrapcheck // the platform stub's error is the whole message
	return os.Lchown(path, uid, gid)
}
