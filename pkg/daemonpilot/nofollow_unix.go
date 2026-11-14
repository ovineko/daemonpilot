//go:build !windows

package daemonpilot

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openRegularNoFollow opens a path for reading and returns a descriptor only
// when what it opened is a regular file. The type check runs on the descriptor,
// not on the path, so nothing can be substituted between the check and the
// reads that follow: a caller that hashes this descriptor has hashed the file it
// keeps reading from, whatever later appears at the path.
//
// O_NONBLOCK for the same reason chownFileNoFollow uses it: opening a FIFO
// read-only blocks until a writer arrives, so a FIFO planted at the path would
// hang before the S_IFREG check could refuse it.
func openRegularNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // a caller-named install artifact is the whole point
	if err != nil {
		// O_NOFOLLOW refuses a symlink (symlinkRefused: the errno is per kernel) and
		// O_NONBLOCK refuses a device with no reader with ENXIO, both before the
		// S_IFREG check below can run.
		// Those are the same verdict that check makes — something that is not a
		// regular file sits at the path — so they must be reported as one, not as
		// a read that happened to fail. See errNotRegularFile.
		if symlinkRefused(err) || errors.Is(err, syscall.ENXIO) {
			return nil, fmt.Errorf("opening %s: %w: %w", path, errNotRegularFile, err)
		}
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	if err := checkRegularFile(f, path); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// writeFileNoFollow writes data to path the way os.WriteFile does, except that
// it refuses to write through a symlink or into a file that is also reachable
// under another name.
//
// The state file lives in the install directory, which is group-writable on the
// hosts chownFileNoFollow was written for (/usr/local/bin is admin-writable on
// Intel Macs). An unprivileged account can pre-create .daemonpilot-<name>.json as a
// symlink to any path root can write, or as a hard link to a root-owned file on
// the same filesystem, and a root install would truncate and rewrite the target.
// The post-write chown refuses both, but only after the write has happened.
//
// O_TRUNC is deliberately absent from the open flags: truncation is the
// destructive half, and it must not happen until the descriptor has been proven
// to be a lone regular file.
// The write is fsynced before the descriptor is closed, and the close error is
// reported. Truncate(0) has already destroyed the previous contents by the time
// the data is written, so a failure that surfaces only at flush — ENOSPC, EIO —
// must not be reported as a successful write: the caller that recorded a
// rollback digest would go on to replace the binary the digest describes. The
// state file is also the durability partner of the two renames the staged
// updater makes durable through syncInstallDir; a digest lost to a power cut
// while the update it describes survives quarantines the only good binary.
func writeFileNoFollow(path string, data []byte, perm os.FileMode) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, perm) //nolint:gosec // fixed name in our InstallDir
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("closing %s: %w", path, closeErr)
		}
	}()

	if err := checkLoneRegularFile(f, path); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncating %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", path, err)
	}
	return nil
}

func statDescriptor(f *os.File, path string) (syscall.Stat_t, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return st, fmt.Errorf("inspecting %s: %w", path, err)
	}
	return st, nil
}

func checkRegularFile(f *os.File, path string) error {
	st, err := statDescriptor(f, path)
	if err != nil {
		return err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return fmt.Errorf("%s is %w", path, errNotRegularFile)
	}
	return nil
}

func checkLoneRegularFile(f *os.File, path string) error {
	if err := checkRegularFile(f, path); err != nil {
		return err
	}
	st, err := statDescriptor(f, path)
	if err != nil {
		return err
	}
	if st.Nlink != 1 {
		return fmt.Errorf("%s has %d hard links; refusing to write to a file that is also reachable elsewhere",
			path, st.Nlink)
	}
	return nil
}
