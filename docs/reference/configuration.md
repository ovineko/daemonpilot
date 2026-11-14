# Configuration

## Config

Passed to `daemonpilot.New`, which returns `(*daemonpilot.Daemonpilot, error)` after
defaulting and validation. Only `Name` and `WorkerFunc` are required when updates
are disabled. Import `github.com/ovineko/daemonpilot/pkg/daemonpilot/setup` for the
default watchdog and updater implementations.

| Field                       | Type                       | Default                                             | Description                                                                                                      |
| --------------------------- | -------------------------- | --------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------- |
| `Name`                      | `string`                   | (required)                                          | Service name; lowercase letters, digits, hyphens, underscores                                                    |
| `Description`               | `string`                   | `""`                                                | Description written into the init manifest                                                                       |
| `Version`                   | `string`                   | package `Version`                                   | Application version                                                                                              |
| `WatchdogVersion`           | `string`                   | package `WatchdogVersion`                           | Watchdog logic version; drives the update path                                                                   |
| `Scope`                     | `InstallScope`             | `ScopeSystem`                                       | Install machine-wide (`ScopeSystem`) or per-user (`ScopeUser`)                                                   |
| `AllowNoLinger`             | `bool`                     | `false`                                             | Allow a user-scope systemd install without lingering                                                             |
| `InstallDir`                | `string`                   | `/opt/{Name}` (non-macOS), `/usr/local/bin` (macOS) | Where the binary and state live; user-scope defaults below                                                       |
| `LogDir`                    | `string`                   | scope default (see below)                           | Where init systems that log to files write                                                                       |
| `WorkerFunc`                | `func(ctx) error`          | (required)                                          | Your application logic                                                                                           |
| `SelftestFunc`              | `func(ctx) SelftestResult` | `nil` (always passes)                               | Pre-flight checks run against a downloaded binary                                                                |
| `GracefulTimeout`           | `time.Duration`            | `30s`                                               | Grace period before forced process-tree cleanup; Unix sends SIGTERM first                                        |
| `StableThreshold`           | `time.Duration`            | `5m`                                                | Worker uptime after which backoff resets                                                                         |
| `MaxRestartDelay`           | `time.Duration`            | `60s`                                               | Ceiling on the backoff delay                                                                                     |
| `Logger`                    | `Logger`                   | `nil` (silent)                                      | Structured logger; `*slog.Logger` satisfies it                                                                   |
| `VerboseLogging`            | `bool`                     | `false`                                             | Keep full logging on embedded systems                                                                            |
| `CollectWatchdogLogs`       | `bool`                     | `false`                                             | Retain watchdog logs for worker pull collection                                                                  |
| `WatchdogLogBufferBytes`    | `int`                      | `262144` when collection is enabled                 | Byte cap for retained watchdog log lines                                                                         |
| `HealthFunc`                | `func(ctx) error`          | `nil` (disabled)                                    | Periodic health check                                                                                            |
| `HealthInterval`            | `time.Duration`            | `30s`                                               | How often the health check runs                                                                                  |
| `HealthFailures`            | `int`                      | `3`                                                 | Consecutive failures before the worker is restarted                                                              |
| `DumpWorkerOnHealthFailure` | `bool`                     | `false`                                             | Send SIGUSR1 on the first failed health probe; requires a worker handler, no-op on Windows                       |
| `MaxMemoryBytes`            | `int64`                    | `0` (no limit)                                      | Worker memory limit via `RLIMIT_AS` (Linux/FreeBSD only)                                                         |
| `OOMScoreAdj`               | `*int`                     | `nil` (unchanged)                                   | Linux OOM score adjustment, `-1000`…`1000`                                                                       |
| `OnCriticalFailure`         | `func(error)`              | `nil` (log only)                                    | Escalation for unrecoverable rollback/probation or repeated exec-through failures; restart attempts may continue |
| `NotifyReady`               | `bool`                     | `false`                                             | Generate a systemd `Type=notify` unit                                                                            |
| `RespawnStaleWatchdog`      | `bool`                     | `false`                                             | Before WorkerFunc, exit 101 if the resident exports a different non-empty watchdog version                       |
| `Update`                    | `*UpdateConfig`            | `nil` (disabled)                                    | Self-update settings                                                                                             |
| `ConfigPatchers`            | `[]ConfigPatcher`          | `nil`                                               | Manifest patchers run on install and update-config                                                               |
| `ServiceEnvironment`        | `map[string]string`        | `nil`                                               | Environment variables for the service                                                                            |
| `ServiceArguments`          | `[]string`                 | `nil`                                               | Extra CLI arguments for the service                                                                              |
| `CommandPrefix`             | `[]string`                 | `nil`                                               | Command words daemonpilot's own verbs sit behind                                                                 |
| `ServiceUser`               | `string`                   | `""`                                                | Run-as user (systemd, OpenRC, launchd system scope)                                                              |
| `ServiceGroup`              | `string`                   | `""`                                                | Run-as group (systemd, OpenRC, launchd system scope)                                                             |
| `SystemdDirectives`         | `map[string]string`        | `nil`                                               | Extra systemd `[Service]` directives                                                                             |
| `LaunchdKeys`               | `map[string]any`           | `nil`                                               | Extra launchd plist keys                                                                                         |
| `OpenRCDirectives`          | `map[string]string`        | `nil`                                               | Extra OpenRC init script variables                                                                               |
| `CrontabSchedule`           | `string`                   | `"@reboot"`                                         | Schedule for the crontab fallback                                                                                |

## InstallScope

| Value                   | Meaning                                                                     |
| ----------------------- | --------------------------------------------------------------------------- |
| `ScopeSystem` (default) | Machine-wide install; needs sufficient write access and manager permissions |
| `ScopeUser`             | Install under the invoking user's account, using the per-user init          |

`Scope` also chooses the default `InstallDir` (`~/.local/share/{Name}` on Linux,
`~/Library/Application Support/{Name}` on macOS). User scope is rejected together
with `ServiceUser` or `ServiceGroup`, which select a different account.
User-scope paths derive from `$HOME` alone; if it is unset, `New` returns
`daemonpilot.ErrNoHomeDir`.
Systemd, launchd and crontab support `ScopeUser` — see
[Install Scopes](../guides/init-systems.md#install-scopes) and
[Installation](../get-started/installation.md#install-scope).

Windows SCM supports only system scope and LocalSystem. `ServiceEnvironment`,
`ServiceUser`, `ServiceGroup`, and `ScopeUser` are rejected for Windows service
installation. Set an explicit native absolute `InstallDir` when selecting a
Windows installation location instead of relying on the non-macOS `/opt/{Name}`
default. See [Installation](../get-started/installation.md).

`AllowNoLinger` relaxes the lingering requirement for a user-scope systemd
install. Without lingering the service stops at logout and does not start at
boot, so the install fails by default with the `loginctl enable-linger <user>`
command in the message.

## LogDir

Init systems that redirect the service's output to a file — launchd and
crontab — write it under `LogDir`. Leaving the field empty selects the scope
default:

| Scope         | Platform | Default                 |
| ------------- | -------- | ----------------------- |
| `ScopeUser`   | Linux    | `~/.local/state/{Name}` |
| `ScopeUser`   | macOS    | `~/Library/Logs/{Name}` |
| `ScopeSystem` | any      | empty                   |

An empty value in system scope means "keep each manager's historical
location": `/var/log/{Name}/` for launchd's `StandardOutPath` and
`StandardErrorPath`, `/var/log/{Name}.log` for the crontab redirect. Like
`InstallDir`, the user-scope defaults come from `$HOME` and never from
`XDG_STATE_HOME`. The directory is created before the service is installed.

`LogDir` must be an absolute path; `daemonpilot.New` rejects a relative one, which
would otherwise be created next to whatever directory the install was invoked
from and leave the manifest pointing at a path the service cannot resolve.

On a **crontab** host, `LogDir` must additionally contain no newline, carriage
return or `%`. Cron splits its file on newlines before any shell quoting
applies, so the remainder of the value would become a second crontab entry that
`uninstall` could never find again, and cron rewrites an unescaped `%` into a
newline of its own, truncating the command. The same three characters are
refused in `InstallDir`, in every `ServiceArguments` and `CommandPrefix` entry and in every
`ServiceEnvironment` **value**, and `ServiceEnvironment` **keys** must match
`[A-Za-z_][A-Za-z0-9_]*`.

These crontab rules are checked by `install`, once the init system is known to
be crontab, and before the binary is copied — so a rejected value never orphans
a copy in `InstallDir`. They are deliberately not part of `Config.Validate`:
`%` is a cron hazard alone, and `Validate` runs from `daemonpilot.New` at every
process start, so rejecting it everywhere would stop an existing systemd or
launchd service whose environment carries a percent-encoded value from booting.

Init systems that log through the platform's own facility — systemd to the
journal, runit, s6 and procd to their own destinations — ignore `LogDir`.
OpenRC and dinit do not: they write to a hardcoded `/var/log/{Name}.log` and do
not honor `LogDir`. Neither supports user scope, so this only affects a
system-scope install that sets `LogDir` explicitly.

## UpdateConfig

| Field                      | Type                                                               | Default                | Description                                                                                                                   |
| -------------------------- | ------------------------------------------------------------------ | ---------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `Source`                   | `UpdateSource`                                                     | (required)             | Where updates come from                                                                                                       |
| `PublicKey`                | `ed25519.PublicKey`                                                | (required)             | Key that release signatures verify with                                                                                       |
| `SignatureContext`         | `string`                                                           | `""`                   | Prefix before the SHA-256 digest in built-in ed25519 verification; empty preserves legacy signatures                          |
| `VerifyFunc`               | `func(binaryPath string, signature []byte) error`                  | `nil`                  | Replaces built-in signature verification entirely; `SignatureContext` does not apply                                          |
| `CheckInterval`            | `time.Duration`                                                    | `5m`                   | How often to check for updates                                                                                                |
| `AcceptVersion`            | `func(current, offered string) error`                              | `nil` (accepts)        | Filters every offer before download without charging retries                                                                  |
| `PreRestartFunc`           | `func(context.Context, *UpdateInfo) error`                         | `nil` (proceeds)       | Coordinates downtime after selftest on both update paths; no library deadline                                                 |
| `SelftestTimeout`          | `time.Duration`                                                    | `60s`                  | Limit for the staged binary's selftest                                                                                        |
| `StagedUpdateFunc`         | `func(context.Context, *UpdateInfo, SelftestResult) (bool, error)` | `nil` (rejects staged) | Approves a staged update; refusal or error consumes a retry attempt unless cancelled                                          |
| `RollbackConfirmFunc`      | `func(context.Context) (bool, error)`                              | `nil` (auto-confirm)   | Startup confirmation; bypassed by stability probation                                                                         |
| `RollbackTimeout`          | `time.Duration`                                                    | `5m`                   | Confirmation timeout or, with stability probation, the worker-stability window                                                |
| `StagedRollbackOnUnstable` | `bool`                                                             | `false`                | Commit after readiness plus 30 seconds of continuous worker uptime, or `StableThreshold`; otherwise restore the verified copy |
| `RetryMaxAttempts`         | `int`                                                              | `6`                    | Maximum failed attempts for the most recent failing version                                                                   |
| `RetryBaseBackoff`         | `time.Duration`                                                    | `30s`                  | First retry delay, then doubles                                                                                               |
| `RetryMaxBackoff`          | `time.Duration`                                                    | `15m`                  | Upper bound on retry delays                                                                                                   |
| `DisableRetryBudget`       | `bool`                                                             | `false`                | Retry each offer on every poll without budgeting                                                                              |

Retry state lives only in watchdog memory and does not delay discovery of new
versions. Zero retry settings select defaults; negatives are invalid. See
[Retry Budget](../guides/self-update.md#retry-budget).

Validation runs in `daemonpilot.New`, so a missing source or a public key that is
not 32 bytes is reported at startup, even when `VerifyFunc` is set. All retry and
update durations must be nonnegative; zero selects defaults. `PublicKey` is still
required with custom verification. `RollbackTimeout` should exceed
`StableThreshold` when stability probation relies on uptime rather than readiness.
Readiness and other dynamic watchdog IPC calls are unavailable on Windows; use
the uptime criterion there. Fast-path updates do not have this rollback window.

Use a purpose and version ending in NUL for `SignatureContext`, for example
`"myapp-binary-v1\x00"`: distinct tags separate uses of a shared signing key and
the NUL delimits the tag before the digest. The signer must use the identical
context; an empty or different context fails verification. See
[Signing releases](../guides/self-update.md#domain-separated-signatures) and
[HTTP releases](../guides/http-releases.md#signing-and-keys).

## UpdateSource and Reports

An update source implements:

```go
type UpdateSource interface {
	CheckUpdate(ctx context.Context, currentVersion string) (*UpdateInfo, error)
	DownloadUpdate(ctx context.Context, info *UpdateInfo) (io.ReadCloser, []byte, error)
}
```

`CheckUpdate` returns `nil, nil` when there is no offer. The download returns a
stream and raw signature bytes; daemonpilot closes the stream. `UpdateInfo` carries
`Version`, `WatchdogVersion`, `Arch` (`"os/arch"`), `Size` (`int64`), `SHA256`,
and `Digest` (hex-encoded digests, snake-case JSON keys). Positive `Size` and
the digest fields are checked against the download. With `UpdateConfig.NewHash`
set — even explicitly to `sha256.New` — a nonempty `SHA256` field rejects the
offer and `Digest` carries the configured digest; otherwise both fields mean
SHA-256, each must be valid hex of the digest size, and both present must
agree. A manifest with no digest field skips the hash comparison but never the
signature check. The executable's actual `watchdog-version` output, not
manifest metadata, selects the replacement path. Version ordering is
application policy: implement `AcceptVersion` to reject downgrades or
incompatible releases.

`SelftestFunc` returns `SelftestResult{OK bool, Checks map[string]CheckResult}`.
Each `CheckResult` has `OK bool`, optional `Message string`, and optional
`LatencyMS int64` in milliseconds. The CLI serializes these as `success`,
`checks`, `ok`, `message`, and `latency_ms`. No version or timestamp fields are
added. A nil selftest callback succeeds without application-specific validation.

See [Self-Update](../guides/self-update.md) for the lifecycle and
[HTTP Releases](../guides/http-releases.md) for the supplied source and server.

## Backoff Behaviour

Restart delays double from one second up to `MaxRestartDelay`. The counter
resets when the worker signals [readiness](../guides/readiness.md) or runs
longer than `StableThreshold`. A restart after a successful update skips the
delay; a restart caused by a failing health check does not reset the counter.

## Version Fields

`Version` and `WatchdogVersion` default to package-level variables set with
`-ldflags` at build time — see
[Installation](../get-started/installation.md#version-injection). Setting the
`Config` fields explicitly overrides them.

## Runtime-Populated Fields

The remaining exported `Config` fields are launch state, not application options:

| Field                    | Type         | Meaning                                                                         |
| ------------------------ | ------------ | ------------------------------------------------------------------------------- |
| `LockStatus`             | `LockStatus` | Single-instance result set by `run`: held, measured unavailable, or unsupported |
| `CanInitRestartWatchdog` | `bool`       | Whether the bound manager supports watchdog restart; selects handover behavior  |
| `InitSystemName`         | `string`     | Detected host init-system name used for runtime metadata and logging            |
| `ExecThrough`            | `bool`       | Internal proxy-child mode set by `--exec-through` or its launch environment     |

Do not force these values to simulate a different manager or bypass the instance
lock. Use [runtime information](../guides/observability.md) to inspect a launch.

## Logging Writers

```go
func NewNonBlockingWriter(w io.Writer, buffer int) *NonBlockingWriter
func (n *NonBlockingWriter) Write(p []byte) (int, error)
func (n *NonBlockingWriter) Dropped() int64
func SupervisionLogWriter(path string, maxBytes int64) io.Writer
```

`NewNonBlockingWriter` defaults a nonpositive buffer to 1024 queued writes and
uses `io.Discard` for a nil destination. Writes copy their input and return
`len(p), nil`; overflow increments `Dropped` once per dropped write. Destination
errors are ignored. The queue is bounded by write count, not byte count. Its one
drain goroutine lives for the process lifetime, with no flush or close API.

`SupervisionLogWriter` uses the SCM detector on Windows and returns `os.Stderr`
on other platforms, in a console process, on detection failure, or if opening or
rotating the log fails. In a Windows service it attempts both the file and stderr
even when either write fails. A nonpositive `maxBytes` defaults to 8 MiB. The
parent directory must exist; new files use mode 0600. One previous generation is
kept at `path+".1"`. Rotation occurs at open for an already-full file and while
writing. Large writes can be split between generations to enforce the cap.
An existing oversized file is preserved as the first previous generation.
Create one writer in watchdog `run` mode, not in selftest or worker processes.
The writer is process-lived; multiple processes must not rotate the same path.

See [Observability](../guides/observability.md#log-destinations) for usage and
[HTTP Releases](../guides/http-releases.md) for `httpsource.Config` and
`release.HandlerConfig`.

## Embedding in a CLI that already has a `run` command

Daemonpilot's verbs are `install`, `uninstall`, `run`, `worker`, `status`,
`start`, `stop`, `restart`, `enable`, `disable`, `validate`, `update-config`,
`selftest`, `version` and `watchdog-version`. An application whose own CLI
owns one of those names — `run` is the common one — puts them behind its own
words:

```go
s, err := daemonpilot.New(daemonpilot.Config{
	Name:          "myapp",
	CommandPrefix: []string{"daemon", "service"},
	WorkerFunc:    serve,
})
```

The service manifest then runs `myapp daemon service run`, the watchdog spawns
`myapp daemon service worker`, and the update probes run
`myapp daemon service selftest`. The application routes those words to
`s.RunMode(verb, args...)` from its own parser; `Run()` is for a program whose
arguments are daemonpilot's alone.

Prefix entries must be nonempty command words, without surrounding whitespace,
NUL bytes, or a leading `-`. They are arguments, not a shell command string.

## Programmatic Entry Points

```go
func New(config Config) (*Daemonpilot, error)
func (s *Daemonpilot) Run() error
func (s *Daemonpilot) RunMode(mode string, args ...string) error
func (s *Daemonpilot) Install() error
func (s *Daemonpilot) Uninstall() error
func (s *Daemonpilot) Validate() error
```

`Install`, `Uninstall`, and `Validate` use the same handlers as their CLI verbs.
Other lifecycle commands can be dispatched through `RunMode`. The embedding
application owns error reporting and process exit codes; returning an error does
not itself terminate `main`.

`Config.Validate() error` checks requirements without filling defaults;
`Config.SetDefaults() error` fills zero values and may fail when user scope has no
home. Prefer `New`, which applies both in the correct order.

`Logger` requires `Debug`, `Info`, `Warn`, and `Error`, each with
`(msg string, args ...any)`. A `*slog.Logger` satisfies it.
`NewDefaultLogger(level)` supplies a JSON stderr logger; `NoopLogger()` discards
logs. `VerboseLogging` does not enable a nil logger.

For worker-facing runtime helpers and optional metrics, see
[Observability](../guides/observability.md). For manifest patcher signatures and
`PatchInfo`, see [Config Patchers](../guides/config-patchers.md). Worker restart
requests are documented in the
[Watchdog-Worker Protocol](../guides/watchdog-worker-protocol.md).

`PurgeInstallDir(dir)` is an explicit recursive cleanup helper, not part of
`Uninstall` or a CLI command. Call it only after uninstallation and only for a
dedicated application directory; it is unsafe for shared binary directories.
On Windows, running images already scheduled for reboot deletion can remain.
