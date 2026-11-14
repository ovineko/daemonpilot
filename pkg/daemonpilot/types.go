package daemonpilot

import (
	"context"
	"crypto/ed25519"
	"errors"
	"hash"
	"time"
)

// ErrExecRestart is returned by the watchdog when a staged update requires
// restarting the process via exec. The caller should exec into the new binary.
var ErrExecRestart = errors.New("exec restart required")

// ErrNotInstalled identifies an operation that requires an installed service.
var ErrNotInstalled = errors.New("is not installed")

// ErrLingerRequired reports a user-scope install refused because the account
// has no lingering and Config.AllowNoLinger is not set. An application that
// offers a service ending with the session checks for it and says so in its
// own words, since AllowNoLinger is not something its users can act on.
var ErrLingerRequired = errors.New("lingering is required")

// Worker exit codes are a stable contract with deployed watchdogs.
const (
	// ExitPermanent is returned by a worker to tell the watchdog not to
	// restart it and to stop supervising (e.g. after an "unregister").
	ExitPermanent = 100

	// ExitRespawnWatchdog is returned by a worker to ask the watchdog to
	// relaunch itself from the (possibly updated) on-disk binary.
	ExitRespawnWatchdog = 101
)

// Version is the application version, injected at build time via ldflags.
var Version = "dev"

// WatchdogVersion is the watchdog logic version, injected at build time via ldflags.
// Changes to this value trigger staged (confirmed) updates instead of automatic ones.
var WatchdogVersion = "1"

// DefaultWatchdogLogBufferBytes is the default memory cap for retained
// watchdog logs when collection is enabled.
const DefaultWatchdogLogBufferBytes = 256 * 1024

// Config is the main configuration for the daemonpilot library.
type Config struct {
	// Name is the service name used for init-system manifests, binary naming, and logging.
	Name string

	// Description is the service description used in init-system manifests.
	Description string

	// Version is the application version. Typically set via ldflags at build time.
	Version string

	// WatchdogVersion is the watchdog logic version. Changes trigger staged updates.
	WatchdogVersion string

	// RespawnStaleWatchdog requests exit 101 before WorkerFunc when the resident
	// watchdog exports a different version. Default: false.
	RespawnStaleWatchdog bool

	// InstallDir is the directory where the binary is installed.
	// Defaults to /opt/{Name} on Linux, /usr/local/bin on macOS. Under
	// ScopeUser it defaults to ~/.local/share/{Name} on Linux and
	// ~/Library/Application Support/{Name} on macOS.
	InstallDir string

	// Scope selects whether the service is installed machine-wide or under
	// the invoking user's account. The zero value is ScopeSystem.
	Scope InstallScope

	// LogDir is the directory init systems that log to files write to.
	// Empty selects the scope default: the init system's historical location
	// under ScopeSystem, and ~/.local/state/{Name} on Linux or
	// ~/Library/Logs/{Name} on macOS under ScopeUser.
	LogDir string

	// AllowNoLinger permits a user-scope install on systemd even when
	// lingering is disabled for the user. Without lingering the service is
	// torn down at logout and does not start at boot, so an install is
	// refused unless this is set.
	AllowNoLinger bool

	// Update holds self-update configuration. If nil, self-update is disabled.
	Update *UpdateConfig

	// WorkerFunc is the main application logic, called in worker mode.
	// The provided context is cancelled on SIGTERM.
	WorkerFunc func(ctx context.Context) error

	// LockStatus reports whether run established the single-instance guarantee.
	// It is set before the watchdog starts and exported to the worker process.
	LockStatus LockStatus

	// SelftestFunc runs self-test checks on the binary.
	// Called in selftest mode. Should complete quickly (seconds, not minutes).
	// If nil, selftest always returns success.
	SelftestFunc func(ctx context.Context) SelftestResult

	// GracefulTimeout is how long to wait for worker to exit after SIGTERM before sending SIGKILL.
	// Default: 30 seconds.
	GracefulTimeout time.Duration

	// StableThreshold is the minimum worker uptime before the backoff counter resets.
	// Default: 5 minutes.
	StableThreshold time.Duration

	// MaxRestartDelay is the maximum backoff delay between worker restarts.
	// Default: 60 seconds.
	MaxRestartDelay time.Duration

	// Logger is the structured logger. If nil, logging is completely silent.
	// Compatible with stdlib log/slog: a *slog.Logger can be passed directly.
	Logger Logger

	// VerboseLogging forces verbose log output even on embedded systems
	// (busybox, procd, s6, runit) where logging is normally reduced to
	// critical errors only to protect flash storage from excessive writes.
	VerboseLogging bool

	// CollectWatchdogLogs retains watchdog log lines in a bounded in-memory
	// ring so workers can pull them with CollectWatchdogLogs. Default: false.
	CollectWatchdogLogs bool

	// WatchdogLogBufferBytes caps the retained log ring by line bytes.
	// Default: 256 KiB when CollectWatchdogLogs is true.
	WatchdogLogBufferBytes int

	// ConfigPatchers is an optional list of functions that modify generated init-system
	// configuration after generation but before writing to disk. Patchers are applied
	// in order. If nil or empty, configs are written as generated.
	ConfigPatchers []ConfigPatcher

	// ServiceEnvironment specifies environment variables for the service process.
	ServiceEnvironment map[string]string

	// ServiceArguments specifies additional CLI arguments passed after the "run" subcommand.
	ServiceArguments []string

	// CommandPrefix is the words that come before daemonpilot's own verb whenever
	// this binary is executed again: the service manifest runs
	// "<binary> <prefix...> run", the watchdog spawns
	// "<binary> <prefix...> worker", and the update probes run
	// "<binary> <prefix...> selftest" and "... watchdog-version".
	//
	// It exists for an application whose CLI already owns those names. A tool
	// with its own "run" command can put daemonpilot's verbs under one of its
	// own, for example CommandPrefix{"daemon", "service"}, and dispatch them
	// with RunMode. Empty means the verbs are top-level, which is what a
	// program written around daemonpilot wants.
	CommandPrefix []string

	// ServiceUser specifies the user to run the service as (systemd/openrc).
	ServiceUser string

	// ServiceGroup specifies the group to run the service as (systemd/openrc).
	ServiceGroup string

	// SystemdDirectives specifies custom key=value pairs for the systemd [Service] section.
	// Values are emitted verbatim apart from newline stripping, `%` specifier
	// escaping and the removal of trailing whitespace and backslashes, which
	// would otherwise fold the next directive into the value's line. Values of
	// command-line directives (ExecStartPre=, ExecStop= and the other Exec*
	// keys) additionally have `$` and `\` escaped, so they are passed to the
	// command literally rather than expanded by systemd. Backslashes in the
	// remaining directives are the caller's responsibility: systemd unquotes
	// list-valued settings such as ReadWritePaths= and ExecSearchPath=, so a
	// literal backslash in those has to be doubled by the caller.
	SystemdDirectives map[string]string

	// LaunchdKeys specifies custom keys for the launchd plist.
	LaunchdKeys map[string]any

	// OpenRCDirectives specifies custom variables for the OpenRC init script.
	OpenRCDirectives map[string]string

	// CrontabSchedule specifies a custom crontab schedule (default "@reboot").
	CrontabSchedule string

	// NotifyReady indicates that the application uses daemonpilot.NotifyReady()
	// to signal readiness. When true and the init system is systemd, the
	// generated unit file uses Type=notify instead of Type=simple.
	NotifyReady bool

	// CanInitRestartWatchdog indicates whether the detected init system will
	// automatically restart the watchdog after it exits. When true, staged
	// updates use the exit path (init restarts with new binary). When false,
	// staged updates use exec-through (spawn new binary as child).
	// Set automatically during run command initialization.
	CanInitRestartWatchdog bool

	// InitSystemName is the name of the detected init system (e.g., "systemd",
	// "launchd", "busybox"). Set automatically during run command initialization.
	InitSystemName string

	// ExecThrough indicates this watchdog was spawned as a child by a root
	// watchdog during a staged update on an init system that cannot auto-restart.
	// Set automatically from --exec-through flag or DAEMONPILOT_EXEC_THROUGH env var.
	ExecThrough bool

	// OnCriticalFailure is called when an exec-through rollback fails repeatedly.
	// Both the new and old binaries have failed after rollback. The user can
	// decide to exit, notify a manager, send an alert, etc.
	// If nil, the watchdog logs a critical error and continues attempting restarts.
	OnCriticalFailure func(err error)

	// HealthFunc is called periodically by the watchdog to check worker health.
	// If it returns an error, the failure counter increments. After HealthFailures
	// consecutive failures, the worker is restarted.
	// If nil, health checks are disabled.
	HealthFunc func(ctx context.Context) error

	// HealthInterval is how often the watchdog runs HealthFunc.
	// Default: 30 seconds.
	HealthInterval time.Duration

	// HealthFailures is the number of consecutive health check failures before
	// the watchdog restarts the worker.
	// Default: 3.
	HealthFailures int

	// DumpWorkerOnHealthFailure makes the watchdog send SIGUSR1 to the worker on the
	// FIRST health-check failure, so a genuinely wedged worker dumps its state before the
	// restart destroys the evidence. Without it, every wedge is diagnosed from the outside.
	//
	// OPT-IN, and it MUST stay that way: a process that does not install a SIGUSR1 handler
	// is TERMINATED by it. Only set this when the worker handles SIGUSR1 (e.g. by dumping
	// goroutine stacks with StartStackDumpHandler). No-op on Windows. Default: false.
	DumpWorkerOnHealthFailure bool

	// MaxMemoryBytes sets the maximum virtual memory (RLIMIT_AS) for the worker
	// process. Effective on Linux and FreeBSD only: macOS rejects any RLIMIT_AS
	// below the address space the Go runtime has already reserved (the watchdog
	// logs a warning and continues unlimited), and OpenBSD and Windows have no
	// RLIMIT_AS at all. Embedded systems (busybox, procd, s6, runit) typically
	// don't need limits. Default: 0 (no limit).
	MaxMemoryBytes int64

	// WorkerCgroupMemoryMax isolates the worker in a Linux cgroup v2 leaf and
	// caps its total memory in bytes; the leaf's swap is pinned to zero, so
	// the cap is enforced by the kernel's memcg OOM kill. The limit applies at
	// clone, before any worker code runs, and covers every worker descendant.
	// The request is fail-closed: a host that cannot provide the delegated
	// cgroup fails worker launch instead of running the worker unlimited.
	// Linux only; zero disables. Independent of MaxMemoryBytes, which applies
	// RLIMIT_AS inside the worker.
	WorkerCgroupMemoryMax int64

	// WorkerCgroupParent is the delegated cgroup v2 directory worker leaves
	// are created under. Empty lets procguard derive it from the watchdog's
	// own cgroup, which usually cannot host controller limits while the
	// watchdog remains its member; set an explicit delegated parent (for
	// example the systemd user service slice) whenever a limit is requested.
	WorkerCgroupParent string

	// OOMScoreAdj sets /proc/{pid}/oom_score_adj for the worker process on Linux.
	// Valid range: -1000 to 1000. Higher values make the process more likely to
	// be killed by the OOM killer. If nil, the OOM score is not changed.
	// Ignored on non-Linux platforms.
	OOMScoreAdj *int
}

// UpdateConfig holds self-update settings.
type UpdateConfig struct {
	// Source is the update source implementation. Required.
	Source UpdateSource

	// PublicKey is the ed25519 public key for signature verification. Required.
	PublicKey ed25519.PublicKey

	// SignatureContext prefixes the SHA-256 digest in built-in ed25519 verification.
	// Default: "", preserving existing bare-digest signatures byte for byte.
	// Use a purpose and version ending in a NUL byte, e.g. "myapp-binary-v1\x00",
	// to separate uses of the same key and delimit the context from the digest.
	// VerifyFunc replaces the built-in check entirely; this context does not apply to it.
	SignatureContext string

	// VerifyFunc, when set, replaces the built-in signature verification
	// (ed25519 over SignatureContext followed by the SHA-256 digest using PublicKey).
	// It receives the path of the staged binary on disk and the raw signature
	// bytes returned by DownloadUpdate, and must return nil only when the
	// signature is valid. Use this when the update source signs a different
	// message shape (e.g. a domain-separation context prepended to the full
	// binary bytes). PublicKey is still required and remains available for
	// the callback's own use.
	VerifyFunc func(binaryPath string, signature []byte) error

	// VerifyMessage, when set, replaces the built-in Ed25519 verification at
	// the message level: it receives the exact bytes the library would hand
	// to Ed25519 — SignatureContext followed by the release digest — plus the
	// raw signature bytes, and must return nil only when they verify. nil
	// keeps the built-in Ed25519 check; nil does not skip verification, and a
	// failing callback never falls back to Ed25519 or to an unsigned apply.
	// Keys and parameters travel in the closure, so PublicKey is not required
	// for custom verifiers. Mutually exclusive with VerifyFunc.
	VerifyMessage func(message, signature []byte) error

	// NewHash creates the hasher for the release digest: one independent
	// hasher per operation, fed the staged bytes streaming, never the whole
	// binary in memory. nil means sha256.New. Changing the hash does not
	// disable signature verification; the built-in Ed25519 check then signs
	// SignatureContext followed by the custom digest, and the release
	// manifest must carry the matching digest in UpdateInfo.Digest. Setting
	// NewHash — explicitly, including sha256.New — switches the manifest to
	// the Digest field: a nonempty sha256 field rejects the offer regardless
	// of its value. NewHash is never inferred from the manifest.
	NewHash func() hash.Hash

	// AcceptVersion is called for every offer before download. An error skips the
	// offer without charging the retry budget. Nil accepts every version.
	// Version ordering belongs to the application; daemonpilot never compares versions.
	AcceptVersion func(current, offered string) error

	// PreRestartFunc runs after verification and selftest, before either update
	// path swaps the binary, and before StagedUpdateFunc. An error removes the
	// staged download and charges the retry budget, except on cancellation.
	// Nil proceeds immediately. The caller owns any deadline.
	PreRestartFunc func(ctx context.Context, info *UpdateInfo) error

	// CheckInterval is how often to check for updates. Default: 5 minutes.
	CheckInterval time.Duration

	// RetryMaxAttempts limits attempts for the most recent failing offered version. Default: 6.
	RetryMaxAttempts int

	// RetryBaseBackoff is the first delay after an update failure. Default: 30 seconds.
	RetryBaseBackoff time.Duration

	// RetryMaxBackoff caps the exponentially increasing retry delay. Default: 15 minutes.
	RetryMaxBackoff time.Duration

	// DisableRetryBudget retries every offer on every check, without a retry limit.
	// Default: false. Retry state otherwise lives only in the watchdog's memory.
	DisableRetryBudget bool

	// SelftestTimeout is the maximum time for selftest execution. Default: 60 seconds.
	SelftestTimeout time.Duration

	// StagedUpdateFunc is called when WatchdogVersion changes (staged update path).
	// Returns true to approve the update, false to reject it.
	// If nil, staged updates are automatically rejected.
	StagedUpdateFunc func(ctx context.Context, info *UpdateInfo, report SelftestResult) (approved bool, err error)

	// RollbackConfirmFunc is called after a watchdog update when a .rollback file exists.
	// Returns true to confirm the update succeeded, false or timeout triggers rollback.
	// If nil, the .rollback file is deleted immediately (assumes success).
	RollbackConfirmFunc func(ctx context.Context) (confirmed bool, err error)

	// RollbackTimeout is how long to wait for rollback confirmation before
	// auto-rolling back. With StagedRollbackOnUnstable it is instead the
	// probation window: how long the new watchdog has to get the worker healthy
	// before restoring the previous binary. It must exceed StableThreshold so a
	// healthy worker can commit via the uptime fallback. The two uses are
	// mutually exclusive (StagedRollbackOnUnstable bypasses RollbackConfirmFunc).
	// Default: 5 minutes.
	RollbackTimeout time.Duration

	// StagedRollbackOnUnstable makes a staged (watchdog-contract-changing) update
	// provisional and worker-stability-gated instead of confirmed at startup.
	// After the new watchdog restarts, it keeps the previous binary as
	// <name>.rollback and commits (deletes it) only once the worker has proven
	// healthy — either it signalled readiness via NotifyReady, or it ran
	// continuously for StableThreshold. If neither happens within RollbackTimeout
	// (the new watchdog cannot spawn/supervise a working worker — e.g. a broken
	// contract change), the watchdog restores the previous binary and re-execs
	// it. This closes the gap where a startup-only RollbackConfirmFunc would
	// confirm a watchdog that reaches the manager but cannot supervise the
	// worker. When true, the startup RollbackConfirmFunc path is bypassed.
	StagedRollbackOnUnstable bool
}

// UpdateInfo describes an available update.
type UpdateInfo struct {
	// Version is the new binary's application version.
	Version string `json:"version"`

	// WatchdogVersion is the new binary's watchdog logic version.
	WatchdogVersion string `json:"watchdog_version"`

	// Arch is the target architecture (e.g., "linux/amd64", "darwin/arm64").
	Arch string `json:"arch"`

	// Size is the binary size in bytes.
	Size int64 `json:"size"`

	// SHA256 is the hex-encoded SHA-256 hash of the binary. When
	// UpdateConfig.NewHash is unset, either SHA256 or Digest is accepted;
	// if both are present, they must match. Setting NewHash — even explicitly
	// to sha256.New — rejects any nonempty SHA256 and uses Digest instead.
	SHA256 string `json:"sha256,omitempty"`

	// Digest is the hex-encoded digest of the release hash — the algorithm
	// chosen by UpdateConfig.NewHash, SHA-256 by default. With NewHash set it
	// is the only accepted digest field. Without it, Digest must be a valid
	// hex SHA-256, and when both Digest and SHA256 are present they must
	// match; a mismatch rejects the offer.
	Digest string `json:"digest,omitempty"`
}

// SelftestResult is the output of a selftest run.
type SelftestResult struct {
	OK     bool                   `json:"success"`
	Checks map[string]CheckResult `json:"checks"`
}

// CheckResult is the result of a single selftest check.
type CheckResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	// LatencyMS is the check latency in milliseconds.
	LatencyMS int64 `json:"latency_ms,omitempty"`
}

// ErrRollbackRestored is returned by HandleRollback when the old binary is
// restored and the watchdog should exit for the init system to restart it.
var ErrRollbackRestored = errors.New("rollback restored, watchdog restart required")

// ErrRollbackUnverified reports a .rollback binary that does not match the
// digest recorded when the staged update created it — or that has no recorded
// digest at all, or that is not a regular file. Restoring it would execute a
// binary that never passed the signature chain, which any process sharing the
// install directory's uid could have planted.
var ErrRollbackUnverified = errors.New("rollback binary failed integrity verification")

// ErrRollbackDigestUnreadable reports that a digest could not be obtained at
// all — an unreadable or half-written state file on the recorded side, or a
// rollback copy that could not be opened or read on the computed side — as
// opposed to a digest that was read and did not match. It deliberately does not
// wrap ErrRollbackUnverified: that daemonpilot is the callers' signal to
// quarantine the copy, and a failed read says nothing about the file's
// provenance. The state file is rewritten in place, so a reader can observe it
// truncated while an install or update-config rewrites it, and the copy itself
// can fail to open under fd exhaustion or I/O error; quarantining on either
// would destroy a valid rollback copy over a condition that may not repeat. The
// restore is still refused — it just stays refusable, and the next attempt can
// succeed.
var ErrRollbackDigestUnreadable = errors.New("rollback binary digest could not be read")

// ErrAlreadyRunning is returned by the run command when another instance of
// this service already holds the single-instance lock in InstallDir.
var ErrAlreadyRunning = errors.New("another instance is already running")

// ErrLockUnavailable reports that the host was measured to be incapable of
// creating and locking a probe file in the lock directory. A run may continue
// without the single-instance guarantee because no peer could acquire it.
var ErrLockUnavailable = errors.New("single-instance locking is unavailable")

type lockUnavailableError struct {
	cause error
	probe error
}

func (e *lockUnavailableError) Error() string {
	return "single-instance locking is unavailable: original lock failure: " + e.cause.Error() +
		"; lock probe failure: " + e.probe.Error()
}

func (e *lockUnavailableError) Unwrap() error { return ErrLockUnavailable }
