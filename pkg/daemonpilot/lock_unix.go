//go:build !windows

package daemonpilot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
)

var (
	openLockFile      = os.OpenFile
	flockLockFile     = syscall.Flock
	removeLockFile    = os.Remove
	lockProbeSequence atomic.Uint64
)

func platformLockStatus() LockStatus { return LockStatusHeld }

type suspectLockError struct{ cause error }

func (e *suspectLockError) Error() string { return e.cause.Error() }
func (e *suspectLockError) Unwrap() error { return e.cause }

// lockFileOwnerUID reports the owning uid of an already-open file. It is a
// package-level var because a test cannot create a file owned by another
// account without being root.
var lockFileOwnerUID = func(f *os.File) (uint32, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return 0, fmt.Errorf("fstat: %w", err)
	}
	return st.Uid, nil
}

// lockFileIsRegular reports whether an already-open file is a regular file.
// It is not a test seam: a test that needs a non-regular file can create one.
func lockFileIsRegular(f *os.File) (bool, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return false, fmt.Errorf("fstat: %w", err)
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFREG, nil
}

// lockFileHasExtraLinks reports whether an already-open file is reachable under
// more than one name. O_NOFOLLOW refuses a symlink, but nothing refuses a hard
// link, and the install directory is group-writable on the hosts this file was
// written for. A hard link from one service's lock path to another's makes both
// flock the same inode, so the second service reads a lock nobody took for it as
// ErrAlreadyRunning even though no peer holds this path. This validation stays
// fatal even when locking is unavailable. writeFileNoFollow refuses Nlink != 1
// for the state file for the same reason.
func lockFileHasExtraLinks(f *os.File) (bool, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return false, fmt.Errorf("fstat: %w", err)
	}
	return st.Nlink != 1, nil
}

// acquireInstanceLock takes an exclusive, non-blocking advisory lock (flock) on
// lockPath so a second "run" — e.g. a manual invocation racing the
// init-supervised service — refuses to start and corrupt shared on-disk state
// (the SQLite databases in InstallDir).
//
// owner is the account install handed the lock file to (Config.ServiceUser). It
// is accepted as an owner in addition to the current euid, so a root invocation
// does not treat the service account's own lock file as foreign.
//
// Returns ErrAlreadyRunning when another instance holds the lock and
// ErrLockUnavailable only when a probe proves the host cannot establish one in
// this directory. Path-specific and suspect-object failures remain distinct
// fatal errors. The returned release closes the lock file to drop the lock; the
// OS also drops it when the process exits. The lock fd is close-on-exec
// (os.OpenFile sets O_CLOEXEC), so a staged-update self-exec releases the lock
// and the re-exec'd image re-acquires it cleanly.
func acquireInstanceLock(lockPath string, owner lockOwner) (release func(), err error) {
	// A lock is only a lock while the file it is taken on is still the file at
	// the path. The root replacement path below unlinks the inode it locked, so
	// an instance that opened that inode first can end up flocking a detached
	// file cleanly while another flocks the replacement — two supervisors over
	// one InstallDir. Revalidate after the flock and start over on the file that
	// is actually there now. The bound is a backstop against a pathological
	// replace loop, not an expected outcome.
	for range lockRevalidateAttempts {
		f, err := openOwnedLockFile(lockPath, owner)
		if err != nil {
			return classifyLockFailure(lockPath, err)
		}
		if err := flockLockFile(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return func() {}, ErrAlreadyRunning
			}
			return classifyLockFailure(lockPath, fmt.Errorf("flock: %w", err))
		}
		current, err := lockedFileIsStillAtPath(f, lockPath)
		if err != nil {
			_ = f.Close()
			return classifyLockFailure(lockPath, err)
		}
		if current {
			return func() { _ = f.Close() }, nil
		}
		_ = f.Close()
	}
	return classifyLockFailure(lockPath, &suspectLockError{fmt.Errorf(
		"lock file %s was replaced on every one of %d attempts to lock it; refusing to run without a lock on the current file",
		lockPath, lockRevalidateAttempts)})
}

func classifyLockFailure(lockPath string, cause error) (func(), error) {
	probeErr := probeInstanceLock(filepath.Dir(lockPath), filepath.Base(lockPath))
	var suspect *suspectLockError
	if errors.As(cause, &suspect) {
		return func() {}, cause
	}
	if probeErr == nil {
		return func() {}, cause
	}
	return func() {}, &lockUnavailableError{cause: cause, probe: probeErr}
}

// probeInstanceLock measures the complete mechanism in the lock directory.
// If any operation fails, no instance could create and hold this lock there,
// so refusing one run would provide no exclusion while preventing all work.
func probeInstanceLock(dir, base string) error {
	probePath := filepath.Join(dir, "."+base+".lock-probe-"+strconv.Itoa(os.Getpid())+"-"+strconv.FormatUint(lockProbeSequence.Add(1), 10))
	f, err := openLockFile(probePath, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return fmt.Errorf("create probe file %s: %w", probePath, err)
	}
	defer func() {
		_ = removeLockFile(probePath)
		_ = f.Close()
	}()
	if err := flockLockFile(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("flock probe file %s: %w", probePath, err)
	}
	if err := removeLockFile(probePath); err != nil {
		return fmt.Errorf("remove probe file %s: %w", probePath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close probe file %s: %w", probePath, err)
	}
	return nil
}

// lockRevalidateAttempts bounds the open/flock/revalidate retry.
const lockRevalidateAttempts = 5

// lockedFileIsStillAtPath reports whether the already-locked f is the same inode
// the lock path resolves to now. The path is inspected without following a
// symlink: one planted after the open must read as "not the file we locked".
func lockedFileIsStillAtPath(f *os.File, lockPath string) (bool, error) {
	var locked syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &locked); err != nil {
		return false, fmt.Errorf("fstat lock file %s: %w", lockPath, err)
	}
	var current syscall.Stat_t
	if err := syscall.Lstat(lockPath, &current); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat lock path %s: %w", lockPath, err)
	}
	return locked.Dev == current.Dev && locked.Ino == current.Ino, nil
}

// openOwnedLockFile opens lockPath without following a symlink and refuses a
// lock file that belongs to neither the current euid nor owner, or that is
// reachable under a second name, so the file the lock is taken on is one only
// this install could have created and one nothing else can be locking.
func openOwnedLockFile(lockPath string, owner lockOwner) (*os.File, error) {
	f, err := openLockFileNoFollow(lockPath)
	if err != nil {
		var st syscall.Stat_t
		isSymlink := syscall.Lstat(lockPath, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFLNK
		if isSymlink && geteuid() == 0 {
			replacement, replaceErr := replaceLockFile(lockPath)
			if replaceErr != nil {
				return nil, &suspectLockError{replaceErr}
			}
			return replacement, nil
		}
		if isSymlink {
			return nil, &suspectLockError{err}
		}
		return nil, err
	}

	// A lock is only a lock on a regular file. O_NOFOLLOW refuses a symlink, but
	// nothing refuses a FIFO: it opens fine with O_RDWR, and flock on it either
	// fails outright (EOPNOTSUPP on Darwin) or locks an object no other instance
	// would ever open. A FIFO planted in a group-writable install directory must
	// remain a suspect object rather than being reclassified as unavailable.
	regular, err := lockFileIsRegular(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat lock file %s: %w", lockPath, err)
	}
	if !regular {
		_ = f.Close()
		// No daemonpilot instance can be holding this as its lock — every one of
		// them refuses a non-regular file here — so the flock-before-unlink the
		// ownership path below needs is neither possible nor necessary.
		// acquireInstanceLock revalidates the inode after locking the
		// replacement, so a racer that replaces it again is still caught.
		if geteuid() != 0 {
			return nil, &suspectLockError{fmt.Errorf("lock file %s is not a regular file; "+
				"refusing to take a single-instance lock on it; remove it and start again",
				lockPath)}
		}
		replacement, replaceErr := replaceLockFile(lockPath)
		if replaceErr != nil {
			return nil, &suspectLockError{replaceErr}
		}
		return replacement, nil
	}

	return checkLockFileOwnership(lockPath, f, owner)
}

func checkLockFileOwnership(lockPath string, f *os.File, owner lockOwner) (*os.File, error) {
	// Ownership alone does not make the file safe to lock: a hard link planted
	// from another service's lock path points at an inode this install created
	// and owns, and locking it would answer a question about the wrong service.
	linked, err := lockFileHasExtraLinks(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat lock file %s: %w", lockPath, err)
	}

	fileUID, err := lockFileOwnerUID(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat lock file %s: %w", lockPath, err)
	}
	euid := geteuid()
	// The service account's own lock file is not foreign: install created it and
	// chowned it to Config.ServiceUser precisely so the unprivileged worker can
	// take the lock, and a root invocation that replaced it would leave a
	// root-owned 0600 file the service account can never open again, turning a
	// recoverable ownership problem into an unbounded restart loop.
	owned := int(fileUID) == euid || (owner.uid >= 0 && int(fileUID) == owner.uid)
	if owned && !linked {
		return f, nil
	}

	if euid != 0 {
		_ = f.Close()
		if linked {
			return nil, &suspectLockError{fmt.Errorf("lock file %s is also reachable under another name; "+
				"refusing to take a single-instance lock on a file shared with something else; "+
				"remove it and start again", lockPath)}
		}
		return nil, &suspectLockError{fmt.Errorf("lock file %s is owned by uid %d, not by the current euid %d; "+
			"refusing to take a single-instance lock on a file another account controls; "+
			"remove it or fix its ownership", lockPath, fileUID, euid)}
	}

	// Root, but the configured service account could not be resolved, so there
	// is no way to tell its own lock file from a squatter's. Replacing it would
	// recreate a root-owned 0600 file the account can never open again — the
	// exact restart loop install's chown exists to prevent.
	// Refusing is recoverable; the operator is told which account to make
	// resolvable. A file this euid or the service account already owns is not
	// ambiguous that way, so an extra hard link on it is still replaced below.
	if !owned && owner.unresolved {
		_ = f.Close()
		return nil, &suspectLockError{fmt.Errorf("lock file %s is owned by uid %d and Config.ServiceUser %q could not be resolved; "+
			"refusing to replace a file that may belong to the service account itself; "+
			"make the account resolvable, or remove the file if it is stale",
			lockPath, fileUID, owner.name)}
	}

	// Running as root, replace it rather than refuse. The system-scope install
	// directory is group-writable on some hosts — Homebrew leaves /usr/local/bin
	// writable by admin on Intel Macs — so an unprivileged account could
	// otherwise pre-create the lock file — or hard-link another service's over it
	// — and permanently prevent a root daemon with Restart=always from ever
	// starting.
	//
	// Only a file nobody holds is replaced. Unlinking detaches the inode a live
	// instance is flocking, so the recreated file would lock cleanly and two
	// supervisors would run over one InstallDir — the exact corruption this lock
	// exists to prevent, and silently. A holder is reported as ErrAlreadyRunning
	// instead: refusing to start is recoverable, running twice is not.
	if flockErr := flockLockFile(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); flockErr != nil {
		_ = f.Close()
		if errors.Is(flockErr, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("flock lock file %s owned by uid %d: %w", lockPath, fileUID, flockErr)
	}
	// The lock just taken is held across the unlink and the recreate, and only
	// released once the replacement exists. Closing first would open a window in
	// which a racing instance flocks the old inode cleanly; this process would
	// then unlink it and recreate a file it locks cleanly too, and two
	// supervisors would run over one InstallDir. Holding it means a racer that
	// arrives now gets ErrAlreadyRunning and refuses to start, which is the
	// recoverable outcome. It does not close the window on its own: a racer that
	// opened the old inode before this flock can still lock it once this defer
	// runs, which is why acquireInstanceLock revalidates the locked inode
	// against the path afterwards and retries when they have diverged.
	defer func() { _ = f.Close() }()

	replacement, replaceErr := replaceLockFile(lockPath)
	if replaceErr != nil {
		return nil, &suspectLockError{replaceErr}
	}
	return replacement, nil
}

// replaceLockFile unlinks whatever is at lockPath and creates a fresh 0600
// regular file there. Callers must have established that replacing it is safe:
// either they hold an flock on it, or it is a file type no instance could be
// holding as its lock.
func replaceLockFile(lockPath string) (*os.File, error) {
	if err := removeLockFile(lockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("removing unusable lock file %s: %w", lockPath, err)
	}
	replacement, err := openLockFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("recreating lock file %s: %w", lockPath, err)
	}
	return replacement, nil
}

// openLockFileNoFollow opens the lock path without following a symlink. It is
// O_NONBLOCK so a FIFO planted at the path cannot stall the open before
// openOwnedLockFile gets a descriptor to refuse it on; the flag is a no-op on
// the regular file this is expected to find.
func openLockFileNoFollow(lockPath string) (*os.File, error) {
	f, err := openLockFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err == nil {
		return f, nil
	}
	if errors.Is(err, syscall.EROFS) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		readOnly, readErr := openLockFile(lockPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if readErr == nil {
			if errors.Is(err, syscall.EROFS) || !lockDirectoryAcceptsWrite(filepath.Dir(lockPath), filepath.Base(lockPath)) {
				// This descriptor can establish a genuine flock, but cannot repair a
				// suspect object. Any failed repair is classified by the later probe;
				// it is never accepted merely because the mount is read-only.
				return readOnly, nil
			}
			_ = readOnly.Close()
		}
	}
	if symlinkRefused(err) {
		return nil, fmt.Errorf("lock file %s is a symbolic link; refusing to follow it: %w", lockPath, err)
	}
	return nil, fmt.Errorf("open lock file: %w", err)
}

func lockDirectoryAcceptsWrite(dir, base string) bool {
	path := filepath.Join(dir, "."+base+".write-check-"+strconv.Itoa(os.Getpid())+"-"+strconv.FormatUint(lockProbeSequence.Add(1), 10))
	f, err := openLockFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = removeLockFile(path)
	return true
}
