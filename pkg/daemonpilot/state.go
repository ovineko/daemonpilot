package daemonpilot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// StateData holds information about an installed service for drift detection
// and configuration validation.
type StateData struct {
	ServiceName  string    `json:"service_name"`
	InitSystem   string    `json:"init_system"`
	ManifestPath string    `json:"manifest_path"`
	BinaryPath   string    `json:"binary_path"`
	InstalledAt  time.Time `json:"installed_at"`
	Version      string    `json:"version"`

	// Scope is the install scope the service was registered in. It is omitted
	// for system scope, so a state file written for a system install stays
	// byte-identical to one written before scopes existed and an absent value
	// read back from an older file means system scope.
	Scope InstallScope `json:"scope,omitempty"`
	// InstallDir is the install directory the install resolved, recorded only
	// for user scope where it depends on $HOME.
	InstallDir string `json:"install_dir,omitempty"`
	// Lingering records whether the invoking user had lingering enabled at
	// install time, so status can explain a user-scope service that is not
	// running after a logout.
	Lingering bool `json:"lingering,omitempty"`
	// LaunchdDomain is the launchd domain target the agent was bootstrapped
	// into ("gui/501", "user/501"). Every later launchctl call must name the
	// same domain: re-resolving it on a host whose session state has changed
	// would address a different domain and leave the agent orphaned.
	LaunchdDomain string `json:"launchd_domain,omitempty"`
	// RollbackDigest is the hex SHA-256 of the .rollback copy, recorded at the
	// moment the staged update displaced the binary. A rollback is only ever
	// restored when the file on disk still hashes to this — the file itself
	// lives in a directory writable by the service's own uid in user scope, so
	// its presence proves nothing about its provenance.
	RollbackDigest string `json:"rollback_digest,omitempty"`
}

// EffectiveScope returns the recorded scope, reading an absent value as system
// scope so state files written before scopes existed still load.
func (s *StateData) EffectiveScope() InstallScope {
	if s.Scope == "" {
		return ScopeSystem
	}
	return s.Scope
}

// recordsAnInstall reports whether this state file was written by an install
// rather than by the updater. RecordRollbackDigest creates a minimal state file
// for a service that was never installed through daemonpilot, and a handler that
// read that as "installed" would act on empty ManifestPath and BinaryPath — the
// stray ".backup" update-config would write next to the CLI's working directory
// is the same class of bug the crontab manifest-path guard exists to prevent.
func (s *StateData) recordsAnInstall() bool {
	return s != nil && s.InitSystem != ""
}

// StateFilePath returns the path to the state file for a given service name
// within the specified install directory.
func StateFilePath(installDir, serviceName string) string {
	return filepath.Join(installDir, fmt.Sprintf(".daemonpilot-%s.json", serviceName))
}

// checkedStateFilePath is StateFilePath with the service name proven to be one
// Config.Validate would accept.
//
// The name is not always the caller's own: install chowns the state file to
// Config.ServiceUser so the staged updater can record a rollback digest, so its
// service_name field is under an unprivileged account's control, and every
// handler that round-trips a loaded StateData back through SaveState would let
// it choose the destination. filepath.Join cleans ".." out of the formatted
// name, so a name of "/../../../etc/cron.d/x" escapes InstallDir entirely and
// root truncates and rewrites a file of the attacker's choosing. The pinning
// RecordRollbackDigest does is the same defence one layer up; this one cannot
// be forgotten by a new call site.
func checkedStateFilePath(installDir, serviceName string) (string, error) {
	if !validServiceNameRe.MatchString(serviceName) {
		return "", fmt.Errorf("refusing to address a state file for service name %q: must match [a-z0-9_-]+", serviceName)
	}
	return StateFilePath(installDir, serviceName), nil
}

var (
	writeFileFn = os.WriteFile
	// writeStateFileFn is deliberately not writeFileFn: the state file sits in
	// the install directory, which the lock-file hardening already treats as
	// group-writable, so its write must refuse a symlink or a hard link planted
	// at the path. The manifest backups writeFileFn writes live next to the
	// manifest, in a directory only root can write.
	writeStateFileFn = writeFileNoFollow
	readFileFn       = os.ReadFile
	removeFileFn     = os.Remove
)

// SaveState persists the state data to a JSON file in the install directory.
func SaveState(installDir string, state *StateData) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding state: %w", err)
	}
	path, err := checkedStateFilePath(installDir, state.ServiceName)
	if err != nil {
		return err
	}
	if err := writeStateFileFn(path, data, 0o600); err != nil {
		return fmt.Errorf("writing state file: %w", err)
	}
	return nil
}

// LoadState reads the state data from a JSON file. Returns nil, nil if the
// state file does not exist.
func LoadState(installDir, serviceName string) (*StateData, error) {
	path, err := checkedStateFilePath(installDir, serviceName)
	if err != nil {
		return nil, err
	}
	data, err := readFileFn(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil //nolint:nilnil // documented contract: no state file means no state
		}
		return nil, fmt.Errorf("reading state file: %w", err)
	}
	var state *StateData
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decoding state file: %w", err)
	}
	if state == nil {
		return nil, errors.New("decoding state file: expected an object")
	}
	return state, nil
}

// fileDigest returns the hex-encoded SHA-256 of a regular file. Anything that
// is not a regular file — a symlink above all — is refused rather than
// followed: hashing through a link would attest to one file while the rename
// that follows moves another into place.
// The type check runs on the descriptor rather than on the path, so a file
// swapped in after the open cannot be the one that gets hashed.
func fileDigest(path string) (string, error) {
	f, err := openRegularNoFollow(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return digestReader(f, path)
}

func digestReader(f *os.File, path string) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RecordRollbackDigest hashes the rollback copy and stores the digest in the
// state file, so a later restore can prove the file is the one this update
// displaced. A service with no state file gets a minimal one: without a
// recorded digest its rollback copy would be refused, silently removing the
// safety net the staged update just created.
func RecordRollbackDigest(installDir, serviceName, rollbackPath string) error {
	digest, err := fileDigest(rollbackPath)
	if err != nil {
		return fmt.Errorf("hashing rollback binary: %w", err)
	}
	state, err := LoadState(installDir, serviceName)
	if err != nil {
		return err
	}
	if state == nil {
		state = &StateData{}
	}
	// SaveState derives the path from state.ServiceName, which came out of the
	// file itself. Pin it back to the name the caller asked for, so a state file
	// naming something else cannot redirect the write away from the file it was
	// just read from.
	state.ServiceName = serviceName
	state.RollbackDigest = digest
	return SaveState(installDir, state)
}

// ClearRollbackDigest drops a recorded digest once the rollback copy it
// describes is gone, so a later file at the same path cannot inherit it.
func ClearRollbackDigest(installDir, serviceName string) error {
	state, err := LoadState(installDir, serviceName)
	if err != nil || state == nil || state.RollbackDigest == "" {
		return err
	}
	state.ServiceName = serviceName // same pinning as RecordRollbackDigest
	state.RollbackDigest = ""
	return SaveState(installDir, state)
}

// VerifyRollbackBinary reports whether the file at rollbackPath is the binary
// the staged update displaced, matching it against the digest recorded then.
// A copy that does not match, that has no recorded digest at all, or that is
// not a regular file wraps ErrRollbackUnverified: a rollback copy nobody
// recorded is one nobody created through the verified update path. A state file
// or a copy that could not be *read* wraps ErrRollbackDigestUnreadable instead —
// the copy is refused just the same, but it must not be quarantined over a read
// that may succeed next time.
func VerifyRollbackBinary(installDir, serviceName, rollbackPath string) error {
	recorded, err := loadRecordedRollbackDigest(installDir, serviceName, rollbackPath)
	if err != nil {
		return err
	}
	digest, err := fileDigest(rollbackPath)
	if err != nil {
		return classifyRollbackReadError(rollbackPath, err)
	}
	if digest != recorded {
		return fmt.Errorf("%w: %s hashes to %s, recorded %s",
			ErrRollbackUnverified, rollbackPath, digest, recorded)
	}
	return nil
}

// classifyRollbackReadError decides whether a failure to open or hash the
// rollback copy is a verdict about the copy or merely a failed read.
//
// Only the type refusal is a verdict: something that is not a regular file at
// the rollback path was planted there, and ErrRollbackUnverified tells the
// caller to quarantine it. An open or read that failed for any other reason —
// fd exhaustion, EIO, a mode the service account cannot open — says nothing
// about the copy's provenance, and quarantining on it would destroy a valid
// rollback binary over a condition that may not repeat. That case is refused
// like an unreadable recorded digest: still refused, but still refusable.
func classifyRollbackReadError(rollbackPath string, err error) error {
	if errors.Is(err, errNotRegularFile) {
		return fmt.Errorf("%w: %w", ErrRollbackUnverified, err)
	}
	return fmt.Errorf("%w: hashing %s: %w", ErrRollbackDigestUnreadable, rollbackPath, err)
}

func loadRecordedRollbackDigest(installDir, serviceName, rollbackPath string) (string, error) {
	state, err := LoadState(installDir, serviceName)
	if err != nil {
		// Not ErrRollbackUnverified: a state file that cannot be read is not
		// evidence about the rollback copy, and callers quarantine on that
		// daemonpilot. See ErrRollbackDigestUnreadable.
		return "", fmt.Errorf("%w: reading recorded digest for %s: %w", ErrRollbackDigestUnreadable, rollbackPath, err)
	}
	if state == nil || state.RollbackDigest == "" {
		return "", fmt.Errorf("%w: no digest recorded for %s", ErrRollbackUnverified, rollbackPath)
	}
	return state.RollbackDigest, nil
}

// RestoreRollbackBinary installs the rollback copy at targetPath, verifying it
// first. Verification and restore run against a single descriptor: the file is
// opened once and *copied* from that descriptor into a fresh temporary file,
// hashed as the bytes go by, and renamed into place only once that digest
// matches the recorded one.
//
// The copy is what makes the digest mean anything. Verifying a path and then
// renaming it leaves a window in which the rollback path can be replaced — with
// a symlink above all — between the two, and the rename would then move
// whatever now sits at the path into the executable position. Hashing the
// descriptor in a first pass and copying it in a second closes that window but
// not the one behind it: a descriptor pins an inode, not its contents, and in
// user scope anything running as the service's own uid can rewrite the file
// between the two passes. So there is only one pass — the digest is computed
// over the bytes actually written to the staging file, and a mismatch removes
// that file instead of renaming it.
//
// It reports whether the restore happened alongside the error, following the
// convention QuarantineRollbackFile uses: true with a non-nil error means the
// new binary is in place but the spent rollback copy or its recorded digest
// could not be cleaned up. Callers must branch on the bool, not on the error —
// a leftover copy re-arms staged probation but does not undo the restore.
//
// A verification failure wraps ErrRollbackUnverified, so callers can tell it
// apart from a restore that failed on I/O and quarantine the copy. A digest
// that could not be read — on either side, the state file or the copy itself —
// wraps ErrRollbackDigestUnreadable and must not be quarantined; see that
// error.
func RestoreRollbackBinary(installDir, serviceName, rollbackPath, targetPath string) (bool, error) {
	recorded, err := loadRecordedRollbackDigest(installDir, serviceName, rollbackPath)
	if err != nil {
		return false, err
	}
	src, err := openRegularNoFollow(rollbackPath)
	if err != nil {
		return false, classifyRollbackReadError(rollbackPath, err)
	}
	defer func() { _ = src.Close() }()

	mode, err := executableMode(src, rollbackPath)
	if err != nil {
		return false, err
	}
	if err := copyIntoPlace(src, targetPath, mode, rollbackPath, recorded); err != nil {
		return false, err
	}
	if err := src.Close(); err != nil {
		return true, fmt.Errorf("closing the spent rollback copy %s: %w", rollbackPath, err)
	}

	// The copy is spent: it is the running binary now. Remove it and drop the
	// record, so a later file at the rollback path cannot inherit the digest and
	// its mere presence cannot re-arm probation.
	if err := os.Remove(rollbackPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return true, fmt.Errorf("removing the spent rollback copy %s: %w", rollbackPath, err)
	}
	if err := ClearRollbackDigest(installDir, serviceName); err != nil {
		return true, err
	}
	return true, nil
}

// executableMode returns the permission bits the restored binary should carry:
// those of the rollback copy, which the staged update renamed out of the
// executable path and which therefore already has the mode the service ran
// under. A copy that somehow lost its execute bit still gets one, since the
// whole point of the restore is to produce something the init system can spawn.
func executableMode(f *os.File, path string) (os.FileMode, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("inspecting %s: %w", path, err)
	}
	return info.Mode().Perm() | 0o100, nil
}

// copyIntoPlace writes src to a temporary file next to targetPath and renames it
// over the target, so the target is never observed half-written and an
// interrupted restore leaves the old binary intact.
//
// The bytes are hashed on their way into the staging file and the result is
// compared against recorded before the rename, so what lands at targetPath is
// what was verified — not merely what a descriptor held at the time of an
// earlier pass. A mismatch removes the staging file and wraps
// ErrRollbackUnverified, so the caller quarantines the source copy.
func copyIntoPlace(src io.Reader, targetPath string, mode os.FileMode, srcPath, recorded string) error {
	tmpPath := targetPath + ".restore"
	if err := os.Remove(tmpPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clearing the stale restore staging file %s: %w", tmpPath, err)
	}
	//nolint:gosec // the mode is the one the rollback copy already carried
	dst, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("creating %s: %w", tmpPath, err)
	}
	// Explicitly, because the umask masks the mode O_CREATE was given and a
	// restore must not hand back a binary the service account can no longer exec.
	if err := dst.Chmod(mode); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("setting the mode of %s: %w", tmpPath, err)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(dst, h), src); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if digest := hex.EncodeToString(h.Sum(nil)); digest != recorded {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("%w: %s hashes to %s, recorded %s",
			ErrRollbackUnverified, srcPath, digest, recorded)
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("syncing %s: %w", tmpPath, err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}
	if err := replaceRestoredBinary(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("restoring %s: %w", targetPath, err)
	}
	return nil
}

// QuarantineRollbackFile moves an unverifiable rollback copy aside and returns
// the path it was moved to. Leaving it in place would be worse than removing
// it: its mere presence re-arms staged probation at every start, so each start
// would burn a full RollbackTimeout window and re-escalate through
// OnCriticalFailure over a file that can never be restored. It is renamed
// rather than deleted so the operator can still inspect what was planted, and
// the recorded digest is dropped so a later file cannot inherit it.
//
// A non-empty path with a non-nil error means the file was moved but the digest
// could not be cleared — the quarantine succeeded, and a stale digest only ever
// refuses a file. Callers must report on the path, not on the error alone.
func QuarantineRollbackFile(installDir, serviceName, rollbackPath string) (string, error) {
	quarantinePath := rollbackPath + ".invalid"
	if err := os.Rename(rollbackPath, quarantinePath); err != nil {
		return "", fmt.Errorf("quarantining %s: %w", rollbackPath, err)
	}
	if err := ClearRollbackDigest(installDir, serviceName); err != nil {
		return quarantinePath, err
	}
	return quarantinePath, nil
}

// DeleteState removes the state file for a given service. Returns nil if the
// file does not exist.
func DeleteState(installDir, serviceName string) error {
	path, err := checkedStateFilePath(installDir, serviceName)
	if err != nil {
		return err
	}
	if err := removeFileFn(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("deleting state file: %w", err)
	}
	return nil
}
