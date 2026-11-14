//go:build !windows

package daemonpilot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestAcquireInstanceLock_SecondFails(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "svc.lock")

	release1, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}

	// Second acquire on the same path must report already-running.
	release2, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second acquire err = %v, want ErrAlreadyRunning", err)
	}
	release2() // no-op

	// After releasing the first, a new acquire succeeds.
	release1()
	release3, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	if err != nil {
		t.Fatalf("re-acquire after release failed: %v", err)
	}
	release3()
}

func TestAcquireInstanceLock_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("writing the symlink target: %v", err)
	}
	lockPath := filepath.Join(dir, "svc.lock")
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatalf("planting a symlink at the lock path: %v", err)
	}
	savedEuid := geteuid
	t.Cleanup(func() { geteuid = savedEuid })
	geteuid = func() int { return 1000 }

	release, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	if err == nil {
		t.Fatal("expected a symlink at the lock path to be refused")
	}
	if errors.Is(err, ErrAlreadyRunning) {
		t.Fatal("a symlinked lock path must not look like already-running")
	}
	if !strings.Contains(err.Error(), "symbolic link") || !strings.Contains(err.Error(), lockPath) {
		t.Errorf("error = %v, want it to name the path and the symlink refusal", err)
	}
	release() // must be safe to call
}

func TestAcquireInstanceLock_RootReplacesSymlink(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root replacement requires root")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "svc.lock")
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatal(err)
	}

	release, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	defer release()
	if err != nil {
		t.Fatalf("root acquire over a symlink failed: %v", err)
	}
	info, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatalf("stat replaced lock path: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("lock path was not replaced with a regular file: mode=%v", info.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "untouched" {
		t.Fatalf("symlink target was changed: data=%q err=%v", data, err)
	}
}

func TestAcquireInstanceLock_ReadOnlyFallbackTakesARealLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "svc.lock")
	if err := os.WriteFile(lockPath, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	savedOpen := openLockFile
	t.Cleanup(func() { openLockFile = savedOpen })
	openLockFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		if name == lockPath && flag&os.O_RDWR != 0 {
			return nil, &os.PathError{Op: "open", Path: name, Err: syscall.EROFS}
		}
		return savedOpen(name, flag, perm)
	}

	release, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	if err != nil {
		t.Fatalf("read-only fallback failed: %v", err)
	}
	defer release()
	secondRelease, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	defer secondRelease()
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second acquisition = %v, want ErrAlreadyRunning", err)
	}
}

func TestAcquireInstanceLock_MechanismUnavailable(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "svc.lock")
	savedFlock := flockLockFile
	t.Cleanup(func() { flockLockFile = savedFlock })
	flockLockFile = func(int, int) error { return syscall.ENOLCK }

	release, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	defer release()
	if !errors.Is(err, ErrLockUnavailable) {
		t.Fatalf("error = %v, want ErrLockUnavailable", err)
	}
	var unavailable *lockUnavailableError
	if !errors.As(err, &unavailable) || !errors.Is(unavailable.cause, syscall.ENOLCK) || !errors.Is(unavailable.probe, syscall.ENOLCK) {
		t.Fatalf("classification did not preserve both failures: %#v", err)
	}
	probes, globErr := filepath.Glob(filepath.Join(dir, ".svc.lock.lock-probe-*"))
	if globErr != nil || len(probes) != 0 {
		t.Fatalf("probe files left behind: %v (glob error: %v)", probes, globErr)
	}
}

func TestAcquireInstanceLock_UsableMechanismKeepsPathFailureFatal(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "svc.lock")
	savedOpen := openLockFile
	t.Cleanup(func() { openLockFile = savedOpen })
	pathErr := errors.New("broken lock path")
	openLockFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		if name == lockPath {
			return nil, pathErr
		}
		return savedOpen(name, flag, perm)
	}

	release, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	defer release()
	if !errors.Is(err, pathErr) {
		t.Fatalf("error = %v, want original path failure", err)
	}
	if errors.Is(err, ErrLockUnavailable) {
		t.Fatal("a usable probe laundered a path-specific failure")
	}
}

func TestAcquireInstanceLock_ForeignOwner(t *testing.T) {
	// A file owned by another account cannot be created without being root, so
	// the owner lookup is substituted. The euid is substituted alongside it to
	// exercise both the root and non-root branches on any host.
	const foreignUID = 4242

	tests := []struct {
		name        string
		euid        int
		wantErr     bool
		wantInError []string
	}{
		{
			name: "root recreates a foreign-owned lock file",
			euid: 0,
		},
		{
			name:        "non-root refuses a foreign-owned lock file",
			euid:        1000,
			wantErr:     true,
			wantInError: []string{"4242", "1000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lockPath := filepath.Join(t.TempDir(), "svc.lock")
			if err := os.WriteFile(lockPath, []byte("stale"), 0o600); err != nil {
				t.Fatalf("pre-creating the lock file: %v", err)
			}

			savedOwner, savedEuid := lockFileOwnerUID, geteuid
			t.Cleanup(func() { lockFileOwnerUID, geteuid = savedOwner, savedEuid })
			// Report the foreign owner only for the pre-existing file: a file this
			// call recreates is owned by the caller, exactly as on a real host.
			var reported bool
			lockFileOwnerUID = func(f *os.File) (uint32, error) {
				if reported {
					return savedOwner(f)
				}
				reported = true
				return foreignUID, nil
			}
			geteuid = func() int { return tt.euid }

			release, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
			defer release()

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected a foreign-owned lock file to be refused")
				}
				if errors.Is(err, ErrAlreadyRunning) {
					t.Fatal("a foreign-owned lock file must not look like already-running")
				}
				for _, want := range append(tt.wantInError, lockPath) {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %v, want it to contain %q", err, want)
					}
				}
				// The file the other account owns is left in place for the operator.
				if _, statErr := os.Lstat(lockPath); statErr != nil {
					t.Errorf("refused lock file was removed: %v", statErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("root acquire over a foreign-owned lock file failed: %v", err)
			}
			// Recreated, so the stale contents are gone.
			data, readErr := os.ReadFile(lockPath)
			if readErr != nil {
				t.Fatalf("reading the recreated lock file: %v", readErr)
			}
			if len(data) != 0 {
				t.Errorf("lock file contents = %q, want the file to have been recreated empty", data)
			}
		})
	}
}

func TestAcquireInstanceLock_RootDoesNotStompAHeldForeignLock(t *testing.T) {
	// install chowns the lock file to Config.ServiceUser, so a root invocation
	// meets a foreign-owned file on the normal path. Unlinking one a live
	// instance holds detaches the inode it is flocking, and the recreated file
	// locks cleanly — two supervisors over one InstallDir, silently.
	const foreignUID = 4242
	lockPath := filepath.Join(t.TempDir(), "svc.lock")

	release1, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	if err != nil {
		t.Fatalf("the first acquire failed: %v", err)
	}
	defer release1()

	savedOwner, savedEuid := lockFileOwnerUID, geteuid
	t.Cleanup(func() { lockFileOwnerUID, geteuid = savedOwner, savedEuid })
	lockFileOwnerUID = func(*os.File) (uint32, error) { return foreignUID, nil }
	geteuid = func() int { return 0 }

	release2, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	defer release2()
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("root acquire over a held foreign-owned lock = %v, want ErrAlreadyRunning", err)
	}
	if _, statErr := os.Lstat(lockPath); statErr != nil {
		t.Errorf("the held lock file was removed: %v", statErr)
	}
}

func TestAcquireInstanceLock_AcceptsTheServiceUserOwner(t *testing.T) {
	// The service account's own lock file is not foreign: root replacing it would
	// leave a root-owned 0600 file the account can never open again, which — now
	// that every lock error is fatal — is an unbounded restart loop.
	const serviceUID = 4242
	lockPath := filepath.Join(t.TempDir(), "svc.lock")
	if err := os.WriteFile(lockPath, []byte("held by the service account"), 0o600); err != nil {
		t.Fatalf("pre-creating the lock file: %v", err)
	}

	savedOwner, savedEuid := lockFileOwnerUID, geteuid
	t.Cleanup(func() { lockFileOwnerUID, geteuid = savedOwner, savedEuid })
	lockFileOwnerUID = func(*os.File) (uint32, error) { return serviceUID, nil }
	geteuid = func() int { return 0 }

	release, err := acquireInstanceLock(lockPath, lockOwner{uid: serviceUID, name: "svc"})
	defer release()
	if err != nil {
		t.Fatalf("acquire over the service account's own lock file failed: %v", err)
	}
	// Accepted, not recreated: the contents survive.
	data, readErr := os.ReadFile(lockPath)
	if readErr != nil {
		t.Fatalf("reading the lock file: %v", readErr)
	}
	if len(data) == 0 {
		t.Error("the service account's lock file was recreated instead of accepted")
	}
}

func TestAcquireInstanceLock_UnwritablePathErrors(t *testing.T) {
	// A lock path under a nonexistent directory can't be created; the caller
	// must get a non-ErrAlreadyRunning error — which handleRun treats as fatal —
	// and a usable no-op release.
	lockPath := filepath.Join(t.TempDir(), "no-such-dir", "svc.lock")
	release, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	if err == nil {
		t.Fatal("expected an error for an uncreatable lock path")
	}
	if errors.Is(err, ErrAlreadyRunning) {
		t.Fatal("uncreatable lock path must not look like already-running")
	}
	release() // must be safe to call
}

func TestAcquireInstanceLock_RefusesToReplaceWhenTheServiceUserIsUnresolvable(t *testing.T) {
	// Root, a foreign-owned lock file, and a Config.ServiceUser os/user cannot
	// resolve — a CGO_ENABLED=0 build against an NSS-provided account. The file
	// is most likely the service account's own; recreating it root-owned 0600
	// would be the crash loop install's chown exists to prevent.
	lockPath := filepath.Join(t.TempDir(), "svc.lock")
	if err := os.WriteFile(lockPath, []byte("held by the service account"), 0o600); err != nil {
		t.Fatalf("pre-creating the lock file: %v", err)
	}

	savedOwner, savedEuid := lockFileOwnerUID, geteuid
	t.Cleanup(func() { lockFileOwnerUID, geteuid = savedOwner, savedEuid })
	lockFileOwnerUID = func(*os.File) (uint32, error) { return 4242, nil }
	geteuid = func() int { return 0 }

	release, err := acquireInstanceLock(lockPath, lockOwner{uid: -1, unresolved: true, name: "svc"})
	defer release()
	if err == nil {
		t.Fatal("expected root to refuse a foreign lock file it cannot attribute")
	}
	if !strings.Contains(err.Error(), "could not be resolved") || !strings.Contains(err.Error(), "svc") {
		t.Errorf("error = %v, want it to name the unresolvable service account", err)
	}
	data, readErr := os.ReadFile(lockPath)
	if readErr != nil || len(data) == 0 {
		t.Errorf("the lock file was replaced rather than left alone (err=%v, len=%d)", readErr, len(data))
	}
}

func TestAcquireInstanceLock_RetriesWhenTheLockedInodeIsReplaced(t *testing.T) {
	// The root replacement path unlinks the inode it locked. A racer that opened
	// that inode first can flock the detached file cleanly, so the flock has to
	// be revalidated against the path — otherwise two supervisors run over one
	// InstallDir. Simulated by swapping the file out underneath the first
	// attempt, which the revalidation must notice and retry on.
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "svc.lock")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatalf("pre-creating the lock file: %v", err)
	}

	savedOwner, savedEuid := lockFileOwnerUID, geteuid
	t.Cleanup(func() { lockFileOwnerUID, geteuid = savedOwner, savedEuid })
	euid := os.Geteuid()
	geteuid = func() int { return euid }
	calls := 0
	lockFileOwnerUID = func(f *os.File) (uint32, error) {
		calls++
		if calls == 1 {
			// Replace the file between this open and the revalidation, exactly as
			// a concurrent root replacement would.
			replacement := filepath.Join(dir, "replacement")
			if err := os.WriteFile(replacement, nil, 0o600); err != nil {
				t.Errorf("staging the replacement: %v", err)
			}
			if err := os.Rename(replacement, lockPath); err != nil {
				t.Errorf("replacing the lock file: %v", err)
			}
		}
		return savedOwner(f)
	}

	release, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	defer release()
	if err != nil {
		t.Fatalf("acquire over a replaced lock file failed: %v", err)
	}
	if calls < 2 {
		t.Errorf("opened the lock file %d time(s), want a retry after the inode was replaced", calls)
	}
}

// A FIFO opens fine with O_RDWR and O_NOFOLLOW does not refuse it, but a lock
// on one is not a lock: flock either fails outright or locks an object no other
// instance would ever open. Since every lock error is fatal to run, accepting
// one planted in a group-writable install directory would be a permanent
// Restart=always crash loop.
func TestAcquireInstanceLock_RefusesAFifoWhenNotRoot(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "svc.lock")
	if err := syscall.Mkfifo(lockPath, 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	savedEuid := geteuid
	t.Cleanup(func() { geteuid = savedEuid })
	geteuid = func() int { return 1000 }

	type result struct {
		release func()
		err     error
	}
	done := make(chan result, 1)
	go func() {
		release, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
		done <- result{release, err}
	}()

	select {
	case got := <-done:
		got.release()
		if got.err == nil {
			t.Fatal("expected a FIFO at the lock path to be refused")
		}
		if !strings.Contains(got.err.Error(), "not a regular file") {
			t.Errorf("error %q does not refuse it as a non-regular file", got.err)
		}
		if errors.Is(got.err, ErrAlreadyRunning) {
			t.Error("a FIFO must not be reported as another running instance")
		}
	case <-time.After(testsupport.Scale(10 * time.Second)):
		t.Fatal("acquireInstanceLock blocked on a FIFO at the lock path")
	}
}

// Root replaces it instead of refusing, for the same reason it replaces a
// foreign-owned lock file: an unprivileged account with write access to the
// system install directory could otherwise permanently prevent a root daemon
// from starting. Nothing can be holding a FIFO as its lock, so no flock is
// taken before the unlink.
func TestAcquireInstanceLock_RootReplacesAFifo(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "svc.lock")
	if err := syscall.Mkfifo(lockPath, 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	savedEuid := geteuid
	t.Cleanup(func() { geteuid = savedEuid })
	geteuid = func() int { return 0 }

	release, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	defer release()
	if err != nil {
		t.Fatalf("root acquire over a FIFO failed: %v", err)
	}
	info, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatalf("stat after replacement: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("lock path is %s after replacement, want a regular file", info.Mode())
	}
}

// O_NOFOLLOW refuses a symlink but nothing refuses a hard link, and the system
// install directory is group-writable on some hosts. A hard link from one
// service's lock path to another's makes both flock the same inode, so the
// second reads a lock it never took as ErrAlreadyRunning — a permanent
// Restart=always crash loop, since every lock error is fatal.
func TestAcquireInstanceLock_RefusesAHardLinkWhenNotRoot(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "other.lock")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "svc.lock")
	if err := os.Link(other, lockPath); err != nil {
		t.Skipf("cannot create a hard link here: %v", err)
	}

	savedOwner, savedEuid := lockFileOwnerUID, geteuid
	t.Cleanup(func() { lockFileOwnerUID, geteuid = savedOwner, savedEuid })
	// Owned by this euid: the refusal is about the extra link alone.
	lockFileOwnerUID = func(*os.File) (uint32, error) { return 1000, nil }
	geteuid = func() int { return 1000 }

	release, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	release()
	if err == nil {
		t.Fatal("expected a hard-linked lock file to be refused")
	}
	if !strings.Contains(err.Error(), "another name") {
		t.Errorf("error %q does not refuse it as a shared file", err)
	}
	if errors.Is(err, ErrAlreadyRunning) {
		t.Error("a hard-linked lock file must not be reported as another running instance")
	}
}

// Root replaces it rather than refusing, for the same reason it replaces a
// foreign-owned lock file — but only after flocking it, since the inode may be
// the one a live instance of the other service is holding.
func TestAcquireInstanceLock_RootReplacesAHardLink(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "other.lock")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "svc.lock")
	if err := os.Link(other, lockPath); err != nil {
		t.Skipf("cannot create a hard link here: %v", err)
	}

	savedOwner, savedEuid := lockFileOwnerUID, geteuid
	t.Cleanup(func() { lockFileOwnerUID, geteuid = savedOwner, savedEuid })
	lockFileOwnerUID = func(*os.File) (uint32, error) { return 0, nil }
	geteuid = func() int { return 0 }

	release, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	defer release()
	if err != nil {
		t.Fatalf("root acquire over a hard-linked lock file failed: %v", err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(lockPath, &st); err != nil {
		t.Fatalf("stat after replacement: %v", err)
	}
	if st.Nlink != 1 {
		t.Errorf("replacement lock file has %d links, want 1", st.Nlink)
	}
}

// A held hard link is not stomped: the flock fails, and refusing to start is
// recoverable where two supervisors over one InstallDir are not.
func TestAcquireInstanceLock_RootDoesNotStompAHeldHardLink(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "other.lock")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "svc.lock")
	if err := os.Link(other, lockPath); err != nil {
		t.Skipf("cannot create a hard link here: %v", err)
	}

	savedOwner, savedEuid := lockFileOwnerUID, geteuid
	t.Cleanup(func() { lockFileOwnerUID, geteuid = savedOwner, savedEuid })
	lockFileOwnerUID = func(*os.File) (uint32, error) { return 0, nil }
	geteuid = func() int { return 0 }

	holder, err := os.OpenFile(other, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	release, err := acquireInstanceLock(lockPath, lockOwner{uid: -1})
	release()
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("error = %v, want ErrAlreadyRunning", err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(lockPath, &st); err != nil {
		t.Fatalf("stat after the refused replacement: %v", err)
	}
	if st.Nlink != 2 {
		t.Errorf("held hard link has %d links, want it left untouched at 2", st.Nlink)
	}
}
