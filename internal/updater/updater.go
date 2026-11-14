// Package updater downloads, verifies, and installs signed binary updates,
// with rollback for updates that replace the watchdog itself.
package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ovineko/daemonpilot/internal/crypto"
	"github.com/ovineko/daemonpilot/internal/process"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

// Filesystem durability seam, swapped in tests to observe ordering.
var (
	syncFile = func(f *os.File) error {
		return f.Sync()
	}
	syncDir = func(dir string) error {
		//nolint:gosec // the install directory is the caller's own configured path
		d, err := os.Open(dir)
		if err != nil {
			return fmt.Errorf("open directory: %w", err)
		}
		defer func() { _ = d.Close() }()
		if err := d.Sync(); err != nil {
			return fmt.Errorf("sync directory: %w", err)
		}
		return nil
	}
)

// Updater downloads, verifies, and applies signed binary updates for one
// installed service.
type Updater struct {
	config      *daemonpilot.UpdateConfig
	logger      daemonpilot.Logger
	eventLogger daemonpilot.Logger
	installDir  string
	serviceName string
	binaryName  string
	// commandPrefix is Config.CommandPrefix: the words an application's CLI
	// puts daemonpilot's verbs behind. The probes run the downloaded binary, so
	// they have to reach the same verbs the installed one answers.
	commandPrefix []string
}

// SetCommandPrefix tells the updater which words come before daemonpilot's own
// verbs when this binary is executed again. It is an optional capability
// reached by type assertion, so the registered factory signature stays as it
// is for applications that never set a prefix.
func (u *Updater) SetCommandPrefix(prefix []string) {
	u.commandPrefix = append([]string(nil), prefix...)
}

// New returns an Updater for serviceName inside installDir.
func New(config *daemonpilot.UpdateConfig, logger daemonpilot.Logger, installDir, serviceName string) *Updater {
	return newForPlatform(config, logger, installDir, serviceName, runtime.GOOS)
}

func newForPlatform(config *daemonpilot.UpdateConfig, logger daemonpilot.Logger, installDir, serviceName, goos string) *Updater {
	binaryName := daemonpilot.InstalledBinaryNameFor(serviceName, goos)
	eventLogger := updateLogger{logger: logger, attrs: []any{
		"phase", "update", "service_name", serviceName, "install_dir", installDir,
	}}
	return &Updater{
		config:      config,
		logger:      logger,
		eventLogger: eventLogger,
		installDir:  installDir,
		serviceName: serviceName,
		binaryName:  binaryName,
	}
}

type updateLogger struct {
	logger daemonpilot.Logger
	attrs  []any
}

func (l updateLogger) Debug(msg string, args ...any) { l.logger.Debug(msg, l.args(args)...) }
func (l updateLogger) Info(msg string, args ...any)  { l.logger.Info(msg, l.args(args)...) }
func (l updateLogger) Warn(msg string, args ...any)  { l.logger.Warn(msg, l.args(args)...) }
func (l updateLogger) Error(msg string, args ...any) { l.logger.Error(msg, l.args(args)...) }

func (l updateLogger) args(args []any) []any {
	combined := make([]any, 0, len(l.attrs)+len(args))
	combined = append(combined, l.attrs...)
	return append(combined, args...)
}

// digestHash creates the hasher for one release digest operation: the
// configured NewHash, or SHA-256 when unset.
func (u *Updater) digestHash() hash.Hash {
	if u.config.NewHash != nil {
		return u.config.NewHash()
	}
	return sha256.New()
}

// CheckForUpdate asks the configured source whether a newer version than
// currentVersion exists. A nil UpdateInfo with a nil error means "no update
// available", which is also what an unconfigured source reports.
func (u *Updater) CheckForUpdate(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error) {
	// Routine and frequent (every poll) — Debug so it doesn't flood the journal. An
	// actual "update available" and any error stay at Info/Error.
	u.eventLogger.Debug("checking for update", "current_version", currentVersion)

	if u.config.Source == nil {
		return nil, nil //nolint:nilnil // no source configured means no update, not an error
	}

	info, err := u.config.Source.CheckUpdate(ctx, currentVersion)
	if err != nil {
		u.eventLogger.Error("update check failed", "error", err)
		return nil, fmt.Errorf("update source check: %w", err)
	}

	if info == nil {
		u.eventLogger.Debug("no update available")
		return nil, nil //nolint:nilnil // documented contract: no update available
	}

	u.eventLogger.Info("update available",
		"new_version", info.Version,
		"new_watchdog_version", info.WatchdogVersion,
		"size", info.Size,
	)
	return info, nil
}

// DownloadAndVerify downloads the update described by info to a staging file
// next to the installed binary, checks its SHA-256 and ed25519 signature, and
// returns the staging path. The staging file is removed if verification fails.
func (u *Updater) DownloadAndVerify(ctx context.Context, info *daemonpilot.UpdateInfo) (string, error) {
	u.eventLogger.Info("downloading update", "step", "download", "outcome", "started", "version", info.Version)

	binary, signature, err := u.config.Source.DownloadUpdate(ctx, info)
	if err != nil {
		return "", fmt.Errorf("download update: %w", err)
	}
	if binary == nil {
		return "", errors.New("download returned nil binary reader")
	}
	defer func() { _ = binary.Close() }()

	// The install directory is the only directory this updater ever writes to,
	// and every rename stays inside it. Create it when it is missing: failing
	// the open instead would make the update loop retry forever.
	//nolint:gosec // an install directory holding a service binary is traversable by design
	if err := os.MkdirAll(u.installDir, 0o755); err != nil {
		return "", fmt.Errorf("create install directory: %w", err)
	}

	newPath := daemonpilot.StagedNewPath(filepath.Join(u.installDir, u.binaryName))

	// Unlink whatever is at the staging path — the link itself, never its
	// target — and then create exclusively, so a symlink planted at this name
	// cannot redirect the write. A leftover .new from an interrupted update is
	// removed by the same call, so O_EXCL never wedges the update loop.
	if err := os.Remove(newPath); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("clear staging path: %w", err)
	}

	//nolint:gosec // an executable staged for install needs its exec bits
	f, err := os.OpenFile(newPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return "", fmt.Errorf("create file: %w", err)
	}

	h := u.digestHash()
	w := io.MultiWriter(f, h)

	const defaultMaxDownloadSize int64 = 512 * 1024 * 1024 // 512 MB

	limit := defaultMaxDownloadSize
	if info.Size > 0 && info.Size < defaultMaxDownloadSize {
		limit = info.Size
	}
	src := io.LimitReader(binary, limit)

	written, err := io.Copy(w, src)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(newPath)
		return "", fmt.Errorf("write binary: %w", err)
	}
	// Flush before the file is a rename candidate. These boards are flash-backed:
	// a power loss after the rename but before the data lands leaves a
	// zero-length binary with the rollback copy already renamed away.
	if err := syncFile(f); err != nil {
		_ = f.Close()
		_ = os.Remove(newPath)
		return "", fmt.Errorf("sync staged binary: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(newPath)
		return "", fmt.Errorf("close file: %w", err)
	}

	if info.Size > 0 && written != info.Size {
		_ = os.Remove(newPath)
		return "", fmt.Errorf("size mismatch: got %d bytes, want %d", written, info.Size)
	}

	digest := h.Sum(nil)
	expected, err := u.expectedDigest(info, h.Size())
	if err != nil {
		_ = os.Remove(newPath)
		return "", err
	}
	switch {
	case expected == "":
		u.eventLogger.Warn("update info has no digest, skipping hash check")
	case !strings.EqualFold(hex.EncodeToString(digest), expected):
		_ = os.Remove(newPath)
		return "", fmt.Errorf("digest mismatch: got %s, want %s", hex.EncodeToString(digest), expected)
	default:
		u.eventLogger.Info("digest verified", "hash", hex.EncodeToString(digest))
	}

	message := crypto.DigestMessage(u.config.SignatureContext, digest)
	switch {
	case u.config.VerifyFunc != nil:
		if err := u.config.VerifyFunc(newPath, signature); err != nil {
			_ = os.Remove(newPath)
			return "", fmt.Errorf("signature verification failed: %w", err)
		}
	case u.config.VerifyMessage != nil:
		if err := u.config.VerifyMessage(message, signature); err != nil {
			_ = os.Remove(newPath)
			return "", fmt.Errorf("signature verification failed: %w", err)
		}
	default:
		if !crypto.VerifySignature(u.config.PublicKey, message, signature) {
			_ = os.Remove(newPath)
			return "", errors.New("signature verification failed")
		}
	}
	u.eventLogger.Info("signature verified")

	u.eventLogger.Info("update downloaded and verified", "step", "download", "outcome", "succeeded", "version", info.Version, "path", newPath)
	return newPath, nil
}

const defaultVersionTimeout = 5 * time.Second

// expectedDigest resolves the manifest digest fields against the release hash
// configured in NewHash and returns the hex digest the download must match,
// or "" when the manifest declares none. It runs only after DownloadUpdate
// returns, because a source may mutate the offer on its way through. The
// fields are policy, not interchangeable aliases: under NewHash (including an
// explicit sha256.New) a nonempty sha256 field rejects the offer regardless
// of its value, and under the default hash both fields mean SHA-256 — every
// populated field must be well-formed hex of the configured digest size, and
// both present must agree, so a manifest cannot mean one thing for the hash
// check and another for signers.
func (u *Updater) expectedDigest(info *daemonpilot.UpdateInfo, size int) (string, error) {
	if u.config.NewHash != nil {
		if info.SHA256 != "" {
			return "", fmt.Errorf("digest fields disagree with the configured hash: sha256=%s names SHA-256 while UpdateConfig.NewHash selects the release hash — provide the digest field for the configured hash", info.SHA256)
		}
		if info.Digest == "" {
			return "", nil
		}
		if err := checkHexDigest("digest", info.Digest, size); err != nil {
			return "", err
		}
		return info.Digest, nil
	}
	switch {
	case info.Digest == "":
		if info.SHA256 == "" {
			return "", nil
		}
		if err := checkHexDigest("sha256", info.SHA256, size); err != nil {
			return "", err
		}
		return info.SHA256, nil
	case info.SHA256 == "":
		if err := checkHexDigest("digest", info.Digest, size); err != nil {
			return "", err
		}
		return info.Digest, nil
	}
	if err := checkHexDigest("digest", info.Digest, size); err != nil {
		return "", err
	}
	if err := checkHexDigest("sha256", info.SHA256, size); err != nil {
		return "", err
	}
	if !strings.EqualFold(info.Digest, info.SHA256) {
		return "", fmt.Errorf("digest fields disagree: digest=%s sha256=%s", info.Digest, info.SHA256)
	}
	return info.Digest, nil
}

// checkHexDigest refuses a digest field that is not hex of the expected hash
// size, so a malformed manifest is named as such instead of surfacing as a
// plain digest mismatch.
func checkHexDigest(name, value string, size int) error {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return fmt.Errorf("digest field %s is not valid hex: %s", name, value)
	}
	if len(decoded) != size {
		return fmt.Errorf("digest field %s is %d hex chars, want %d: %s", name, len(value), 2*size, value)
	}
	return nil
}

// GetBinaryWatchdogVersion runs the downloaded binary's watchdog-version
// subcommand, which decides whether an update takes the fast or staged path.
func (u *Updater) GetBinaryWatchdogVersion(ctx context.Context, binaryPath string) (string, error) {
	u.eventLogger.Info("getting watchdog version", "step", "probe_watchdog_version", "outcome", "started", "binary", binaryPath)

	ctx, cancel := context.WithTimeout(ctx, defaultVersionTimeout)
	defer cancel()

	var stdout bytes.Buffer
	err := process.RunProbe(ctx, binaryPath, daemonpilot.DaemonpilotArgs(u.commandPrefix, "watchdog-version"), &stdout, os.Stderr)
	if ctx.Err() != nil {
		u.eventLogger.Error("watchdog-version timed out")
		return "", fmt.Errorf("watchdog-version timed out after %s", defaultVersionTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("run watchdog-version probe: %w", err)
	}

	version := strings.TrimSpace(stdout.String())
	if version == "" {
		return "", errors.New("watchdog-version returned empty output")
	}

	u.eventLogger.Info("got watchdog version", "step", "probe_watchdog_version", "outcome", "succeeded", "watchdog_version", version)
	return version, nil
}

// ApplyFastUpdate swaps in the new binary when the watchdog version is
// unchanged: the next worker respawn picks it up with no watchdog restart.
func (u *Updater) ApplyFastUpdate(ctx context.Context, newBinaryPath string) error {
	u.eventLogger.Info("applying fast-path update", "step", "apply", "outcome", "started", "update_path", "fast", "new_binary", newBinaryPath)

	currentPath := filepath.Join(u.installDir, u.binaryName)

	u.eventLogger.Info("renaming new binary to current", "from", newBinaryPath, "to", currentPath)
	// Not a plain os.Rename: on Windows currentPath is the image this very
	// process is executing, and it may not be overwritten — only moved aside.
	if err := replaceBinary(newBinaryPath, currentPath); err != nil {
		return err
	}
	u.syncInstallDir()

	u.eventLogger.Info("fast-path update applied, ready to spawn new worker", "step", "apply", "outcome", "succeeded", "update_path", "fast")
	return nil
}

// ErrWatchdogRestart is returned by ApplyStagedUpdate to signal the caller
// that the watchdog process should exit with code 0 so the init system
// restarts it with the new binary.
var ErrWatchdogRestart = errors.New("watchdog restart required")

// crashBarrier is test-only. The crash-injection suite sets it to a function
// that must not return — it hard-kills the helper process at the named stage.
// Production builds leave it nil.
var crashBarrier func(stage string)

// ApplyStagedUpdate replaces the binary when the watchdog itself changes,
// keeping the previous one as a .rollback file. It returns ErrWatchdogRestart
// on success to tell the caller to hand over to the new binary.
func (u *Updater) ApplyStagedUpdate(ctx context.Context, newBinaryPath string, info *daemonpilot.UpdateInfo, selftestResult *daemonpilot.SelftestResult) error {
	u.eventLogger.Info("applying staged-path update", "step", "apply", "outcome", "started", "update_path", "staged", "new_binary", newBinaryPath, "version", info.Version)

	if u.config.StagedUpdateFunc != nil {
		approved, err := u.config.StagedUpdateFunc(ctx, info, *selftestResult)
		if err != nil {
			_ = os.Remove(newBinaryPath)
			return fmt.Errorf("staged update callback: %w", err)
		}
		if !approved {
			u.eventLogger.Info("staged update rejected by callback")
			_ = os.Remove(newBinaryPath)
			return errors.New("staged update rejected")
		}
		u.eventLogger.Info("staged update approved by callback")
	} else {
		u.eventLogger.Info("no StagedUpdateFunc configured, rejecting staged update")
		_ = os.Remove(newBinaryPath)
		return errors.New("staged update rejected: no StagedUpdateFunc configured")
	}

	currentPath := filepath.Join(u.installDir, u.binaryName)
	rollbackPath := filepath.Join(u.installDir, u.binaryName+".rollback")

	u.eventLogger.Info("saving current binary as rollback", "from", currentPath, "to", rollbackPath)
	if err := createRollbackBackup(currentPath, rollbackPath); err != nil {
		_ = os.Remove(newBinaryPath)
		return fmt.Errorf("create rollback: %w", err)
	}
	if crashBarrier != nil {
		crashBarrier("backup")
	}

	// Bind the rollback copy to this update before the new binary goes in.
	// A digest that cannot be recorded means a rollback that would be refused
	// later, so undo the rename rather than proceed without a safety net.
	if err := daemonpilot.RecordRollbackDigest(u.installDir, u.serviceName, rollbackPath); err != nil {
		// A permission error here is the upgrade path, not a fault: an install
		// made before the state file was handed to the service account leaves a
		// root-owned 0600 file the unprivileged updater cannot write, and every
		// poll would abort the same way. Name the way out rather than logging
		// only the raw errno, as the lock path does for the same reason.
		if errors.Is(err, fs.ErrPermission) {
			u.eventLogger.Error("failed to record rollback digest, aborting update; "+
				"re-run install so it hands the state file to the service account",
				"install_dir", u.installDir, "error", err)
		} else {
			u.eventLogger.Error("failed to record rollback digest, aborting update", "error", err)
		}
		if discardErr := discardRollbackBackup(rollbackPath, currentPath); discardErr != nil {
			u.eventLogger.Error("failed to discard the rollback copy after recording the digest failed", "error", discardErr)
			err = errors.Join(err, discardErr)
		}
		_ = os.Remove(newBinaryPath)
		return fmt.Errorf("record rollback digest: %w", err)
	}

	if crashBarrier != nil {
		crashBarrier("digest")
	}

	u.eventLogger.Info("renaming new binary to current", "from", newBinaryPath, "to", currentPath)
	if err := os.Rename(newBinaryPath, currentPath); err != nil {
		u.eventLogger.Error("rename failed, discarding the rollback copy", "error", err)
		if discardErr := discardRollbackBackup(rollbackPath, currentPath); discardErr != nil {
			u.eventLogger.Error("failed to discard the spent rollback copy", "error", discardErr)
			err = errors.Join(err, discardErr)
		} else {
			u.clearRollbackDigest()
		}
		return fmt.Errorf("rename new binary: %w", err)
	}
	u.syncInstallDir()
	if crashBarrier != nil {
		crashBarrier("publish")
	}

	u.eventLogger.Info("staged-path update applied, watchdog should exit for restart", "step", "apply", "outcome", "succeeded", "update_path", "staged")
	return ErrWatchdogRestart
}

// RunSelftest executes the downloaded binary's selftest subcommand as a
// pre-flight check, so a binary that cannot start is never installed.
func (u *Updater) RunSelftest(ctx context.Context, binaryPath string, timeout time.Duration) (*daemonpilot.SelftestResult, error) {
	u.eventLogger.Info("running selftest", "step", "selftest", "outcome", "started", "binary", binaryPath, "timeout", timeout)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout bytes.Buffer
	err := process.RunProbe(ctx, binaryPath, daemonpilot.DaemonpilotArgs(u.commandPrefix, "selftest"), &stdout, os.Stderr)
	if ctx.Err() != nil {
		u.eventLogger.Error("selftest timed out", "timeout", timeout)
		return nil, fmt.Errorf("selftest timed out after %s", timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("run selftest probe: %w", err)
	}

	var result daemonpilot.SelftestResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("parse selftest output: %w", err)
	}

	if !result.OK {
		u.eventLogger.Warn("selftest failed", "checks", result.Checks)
		return &result, nil
	}

	u.eventLogger.Info("selftest passed", "step", "selftest", "outcome", "succeeded", "checks", result.Checks)
	return &result, nil
}

// CheckRollbackFile reports whether a staged update left a usable .rollback
// file behind, meaning the current binary is still unconfirmed. A file that
// does not match the digest recorded when the update created it — or that was
// never recorded at all — is reported as absent and as an error, never as a
// pending rollback: acting on it would execute a binary the signature chain
// never saw.
func (u *Updater) CheckRollbackFile(ctx context.Context, binaryPath string) (bool, error) {
	rollbackPath := binaryPath + ".rollback"
	if _, err := os.Lstat(rollbackPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("check rollback file: %w", err)
	}
	if err := daemonpilot.VerifyRollbackBinary(u.installDir, u.serviceName, rollbackPath); err != nil {
		u.eventLogger.Error("ignoring unverifiable rollback file", "path", rollbackPath, "error", err)
		// A digest that could not be read is not a verdict about the copy: the
		// state file is rewritten in place, so a concurrent install or
		// update-config can be observed mid-write. Refuse this check and leave the
		// copy alone; the next one can succeed.
		if errors.Is(err, daemonpilot.ErrRollbackDigestUnreadable) {
			//nolint:wrapcheck // the daemonpilot error and its context are the message the caller logs
			return false, err
		}
		// Move it aside rather than leave it: it can never be restored, and its
		// presence alone re-arms staged probation at every start, costing a full
		// RollbackTimeout window and an OnCriticalFailure escalation each time.
		u.quarantineRollbackFile(rollbackPath)
		//nolint:wrapcheck // the daemonpilot error and its context are the message the caller logs
		return false, err
	}
	u.eventLogger.Info("rollback file detected", "path", rollbackPath)
	return true, nil
}

// HandleRollback resolves a pending staged update: it keeps the new binary
// once the application confirms it, and restores the previous binary when
// confirmation is denied or does not arrive within rollbackTimeout.
func (u *Updater) HandleRollback(ctx context.Context, rollbackPath string, rollbackTimeout time.Duration) error {
	u.eventLogger.Info("handling rollback", "rollback_path", rollbackPath, "timeout", rollbackTimeout)

	if u.config.RollbackConfirmFunc == nil {
		u.eventLogger.Info("no RollbackConfirmFunc configured, deleting rollback file")
		if err := os.Remove(rollbackPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("delete rollback file: %w", err)
		}
		u.clearRollbackDigest()
		return nil
	}

	type confirmResult struct {
		confirmed bool
		err       error
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, rollbackTimeout)
	defer cancel()

	ch := make(chan confirmResult, 1)
	go func() {
		confirmed, err := u.config.RollbackConfirmFunc(timeoutCtx)
		ch <- confirmResult{confirmed, err}
	}()

	select {
	case result := <-ch:
		if result.err != nil {
			u.eventLogger.Error("rollback confirmation callback failed", "error", result.err)
			return u.performRollback(rollbackPath)
		}
		if result.confirmed {
			u.eventLogger.Info("rollback confirmation received, deleting rollback file")
			if err := os.Remove(rollbackPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("delete rollback file: %w", err)
			}
			u.clearRollbackDigest()
			return nil
		}
		u.eventLogger.Info("rollback confirmation denied, restoring old binary")
		return u.performRollback(rollbackPath)
	case <-timeoutCtx.Done():
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for rollback confirmation: %w", err)
		}
		u.eventLogger.Warn("rollback confirmation timed out, restoring old binary", "timeout", rollbackTimeout)
		return u.performRollback(rollbackPath)
	}
}

// CleanupStaleFiles removes a leftover .new staging file from an update that
// was interrupted before it could be applied.
func (u *Updater) CleanupStaleFiles() error {
	// A binary moved aside by an in-place replacement is still held by the process
	// that was executing it, so it can only be removed once that process is gone —
	// which is now, at the start of its successor.
	sweepSuperseded(u.installDir, u.binaryName)

	live := filepath.Join(u.installDir, u.binaryName)
	legacyRollback := filepath.Join(u.installDir, u.serviceName+".rollback")
	rollback := live + ".rollback"
	if legacyRollback != rollback {
		for _, path := range []string{legacyRollback, legacyRollback + ".invalid"} {
			if _, err := os.Lstat(path); err == nil {
				u.eventLogger.Info("deleting a rollback artifact stored under the previous name", "path", path)
				_ = os.Remove(path)
			}
		}
	}
	// A node that ran the broken Windows cycle may hold a download under the OLD
	// spelling, which nothing looks at any more. Sweep it so it does not sit in
	// the install directory forever.
	if legacy := daemonpilot.LegacyStagedNewPath(live); legacy != daemonpilot.StagedNewPath(live) {
		if _, err := os.Lstat(legacy); err == nil {
			u.eventLogger.Info("deleting a download staged under the previous name", "path", legacy)
			_ = os.Remove(legacy)
		}
	}

	newPath := daemonpilot.StagedNewPath(live)
	// Lstat, not Stat: a symlink planted at the staging path whose target does
	// not exist would otherwise read as absent and survive the sweep.
	if _, err := os.Lstat(newPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("check stale file: %w", err)
	}
	u.eventLogger.Info("found stale .new file, deleting", "path", newPath)
	if err := os.Remove(newPath); err != nil {
		return fmt.Errorf("delete stale file: %w", err)
	}
	return nil
}

// syncInstallDir makes a completed rename durable. It is best effort: the
// rename has already happened, and platforms that refuse to fsync a directory
// (Windows) must not turn that into a failed update.
func (u *Updater) syncInstallDir() {
	u.syncInstallDirFor(runtime.GOOS)
}

func (u *Updater) syncInstallDirFor(goos string) {
	// Windows cannot flush a directory handle, and an NTFS rename is already
	// ordered, so attempting the sync only produces a misleading access error.
	if goos == "windows" {
		return
	}
	if err := syncDir(u.installDir); err != nil {
		u.eventLogger.Warn("failed to sync the install directory after rename", "error", err)
	}
}

func (u *Updater) performRollback(rollbackPath string) error {
	currentPath := filepath.Join(u.installDir, u.binaryName)

	u.eventLogger.Info("restoring rollback binary", "from", rollbackPath, "to", currentPath)
	restored, err := daemonpilot.RestoreRollbackBinary(u.installDir, u.serviceName, rollbackPath, currentPath)
	if !restored {
		if errors.Is(err, daemonpilot.ErrRollbackUnverified) {
			u.eventLogger.Error("refusing to restore an unverified rollback binary",
				"path", rollbackPath, "error", err)
			// Move it aside as the other rollback paths do. Left in place it re-arms
			// staged probation at every start — stagedProbationPath keys off presence
			// alone — burning a full RollbackTimeout window over a copy that can never
			// be restored.
			u.quarantineRollbackFile(rollbackPath)
			//nolint:wrapcheck // the daemonpilot error and its context are the message the caller logs
			return err
		}
		return fmt.Errorf("restore rollback: %w", err)
	}
	if err != nil {
		// The binary is in place; only the spent copy or its digest is left over.
		u.eventLogger.Warn("restored the rollback binary but failed to clean up the copy",
			"path", rollbackPath, "error", err)
	}
	u.syncInstallDir()
	return daemonpilot.ErrRollbackRestored
}

// quarantineRollbackFile moves a rollback copy that failed verification aside.
// Best effort: a file that cannot be moved is still never restored, it only
// keeps costing a probation window.
func (u *Updater) quarantineRollbackFile(rollbackPath string) {
	quarantinePath, moveErr := daemonpilot.QuarantineRollbackFile(u.installDir, u.serviceName, rollbackPath)
	switch {
	case quarantinePath == "":
		u.eventLogger.Warn("failed to quarantine the unverifiable rollback file",
			"path", rollbackPath, "error", moveErr)
	case moveErr != nil:
		// Moved, but the digest it described is still recorded. Harmless: a
		// stale digest only ever refuses a file, never accepts one.
		u.eventLogger.Warn("moved the unverifiable rollback file aside, but could not clear its recorded digest",
			"from", rollbackPath, "to", quarantinePath, "error", moveErr)
	default:
		u.eventLogger.Warn("moved the unverifiable rollback file aside for inspection",
			"from", rollbackPath, "to", quarantinePath)
	}
}

// clearRollbackDigest drops the recorded digest once the rollback copy it
// describes is gone. Best effort: a stale digest only ever refuses a file, it
// never accepts one.
func (u *Updater) clearRollbackDigest() {
	if err := daemonpilot.ClearRollbackDigest(u.installDir, u.serviceName); err != nil {
		u.eventLogger.Warn("failed to clear the recorded rollback digest", "error", err)
	}
}
