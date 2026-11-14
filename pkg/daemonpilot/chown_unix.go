//go:build !windows

package daemonpilot

import (
	"fmt"
	"os"
	"syscall"
)

// chownFileNoFollow hands a file to uid/gid through a file descriptor rather
// than a path, and refuses anything that is not a lone regular file.
//
// os.Lchown does not follow a symlink, but a hard link is not a symlink: it is
// the file. In the group-writable system install directory this whole path
// exists for (/usr/local/bin is admin-writable on Intel Macs), an unprivileged
// account can pre-create <name>.lock or the state file as a hard link to a
// root-owned file elsewhere on the same filesystem, and a root install would
// then hand that inode to the service account. Requiring Nlink == 1 removes the
// alias, and doing the chown on the descriptor that was checked removes the
// window between the check and the chown.
//
// The open is O_NONBLOCK because the type check can only run once there is a
// descriptor: opening a FIFO read-only blocks until a writer arrives, so a FIFO
// planted at the lock path or the state file would hang the install forever
// before it ever reached the S_IFREG check below. O_NONBLOCK makes the open
// return so the check can refuse it, and it is harmless on a regular file.
func chownFileNoFollow(path string, uid, gid int) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // fixed path in our InstallDir
	if err != nil {
		return fmt.Errorf("opening %s to change its owner: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return fmt.Errorf("inspecting %s: %w", path, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return fmt.Errorf("%s is not a regular file; refusing to change its owner", path)
	}
	if st.Nlink != 1 {
		return fmt.Errorf("%s has %d hard links; refusing to change the owner of a file that is also reachable elsewhere",
			path, st.Nlink)
	}
	if err := syscall.Fchown(int(f.Fd()), uid, gid); err != nil {
		return fmt.Errorf("changing the owner of %s: %w", path, err)
	}
	return nil
}
