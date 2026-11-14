# Watchdog–Worker Protocol

This page is the contract between daemonpilot's watchdog and the worker process
that runs an embedding application's `WorkerFunc`. It covers process identity,
the worker environment, exit codes, readiness and the inherited-pipe protocol.

## Process Model

The init system starts the binary in `run` mode. That process is the watchdog.
The watchdog starts the same binary in `worker` mode, and the worker calls
`Config.WorkerFunc`. The watchdog owns restart policy, health checks, update
decisions; procguard supplies backoff and process lifecycle mechanics. The worker
owns application work.

A normal worker restart creates a new worker process while the watchdog and its
in-memory state survive. A fast-path update replaces the binary on disk and the
resident watchdog starts a worker from it; the old watchdog process therefore
survives while the worker changes.

A staged update changes watchdog logic as well. When init can restart the
watchdog, the current implementation requests `ErrExecRestart`; on Unix,
`handleRun` uses `syscall.Exec` to load the installed binary directly. When init
cannot restart the watchdog, the old watchdog starts the new binary with
`--exec-through` and remains as a signal-forwarding proxy. A later staged update
makes the child exit 0, so the proxy reloads the disk image without growing the
chain. Direct exec preserves the process ID; an exec-through spawn creates a new
process ID. Workers stop before their supervising watchdog changes.

With `StagedRollbackOnUnstable`, the child owns worker probation while running,
using the same readiness and stability rules as an ordinary watchdog. Commit
removes the rollback copy and digest. Rollback restores the verified copy and
makes the child exit 0 for immediate proxy restart with backoff reset. The proxy
waits for child exit and only restores on repeated child crashes or start
failures; watchdog uptime alone never commits worker probation. See
[rollback ownership and integrity](self-update.md#rollback).

## Parent Death and Graceful Stop

The worker passes `DAEMONPILOT_PARENT_PID` to `procguard.WatchParent`; a detected
mismatch cancels application work and logs the expected and observed parent PIDs.
Linux adds kernel SIGTERM delivery, handled by daemonpilot's own signal handler.
macOS/BSD use PID polling. `DAEMONPILOT_DISABLE_PARENT_MONITOR` disables this
cooperative monitor for tests.

On Windows, the parent assigns workers and exec-through children to kill-on-close
Job Objects. Watchdog death kills the assigned tree; worker restarts terminate
assigned descendants as well. Assignment follows process creation, leaving a small initial
window without Job Object protection. The cooperative monitor is unavailable on
Windows, but disabling it does not disable the Job Object.

Stopping a worker gives descendants remaining in its group or job the grace
period before escalation, even if the leader exits first. Linux, macOS, and
Windows also clean up descendants after natural leader exit. On FreeBSD/OpenBSD
(and NetBSD/DragonFly), requested stop always waits the full grace and kills the
group, while natural leader exit leaves descendants alone. These are procguard's
platform guarantees, not an IPC capability.

## Worker Environment

The watchdog inherits its own environment and adds these values before starting
the worker:

| Constant                  | Environment variable                 | Contract                                                                                                        |
| ------------------------- | ------------------------------------ | --------------------------------------------------------------------------------------------------------------- |
| `EnvMode`                 | `DAEMONPILOT_MODE`                   | `worker` for a supervised worker.                                                                               |
| `EnvVersion`              | `DAEMONPILOT_VERSION`                | Application version selected by the watchdog.                                                                   |
| `EnvWatchdogVersion`      | `DAEMONPILOT_WATCHDOG_VERSION`       | Watchdog-logic version resident in the parent.                                                                  |
| `EnvBinaryPath`           | `DAEMONPILOT_BINARY_PATH`            | Path of the binary that started the watchdog.                                                                   |
| `EnvInitSystem`           | `DAEMONPILOT_INIT_SYSTEM`            | Detected init-system name.                                                                                      |
| `EnvCanAutoRestart`       | `DAEMONPILOT_CAN_AUTO_RESTART`       | `true` when init can restart an exited watchdog.                                                                |
| `EnvIsExecThrough`        | `DAEMONPILOT_IS_EXEC_THROUGH`        | `true` when this watchdog is the exec-through child.                                                            |
| `EnvLockStatus`           | `DAEMONPILOT_LOCK_STATUS`            | `ok` (`LockStatusHeld`), `unlocked` (`LockStatusUnlocked`), or Windows `unsupported` (`LockStatusUnsupported`). |
| `EnvParentPID`            | `DAEMONPILOT_PARENT_PID`             | Decimal watchdog PID used by orphan prevention.                                                                 |
| `EnvIPCFD`                | `DAEMONPILOT_IPC_FD`                 | `requestFD,responseFD`, two inherited anonymous-pipe descriptors.                                               |
| `EnvMaxMemoryBytes`       | `DAEMONPILOT_MAX_MEMORY_BYTES`       | Decimal worker limit; present only when `Config.MaxMemoryBytes > 0`.                                            |
| `EnvOOMScoreAdj`          | `DAEMONPILOT_OOM_SCORE_ADJ`          | Decimal adjustment; present only when `Config.OOMScoreAdj` is set.                                              |
| `EnvDisableParentMonitor` | `DAEMONPILOT_DISABLE_PARENT_MONITOR` | Forwarded only when already set; intended for tests.                                                            |

An exec-through watchdog is itself started with `DAEMONPILOT_EXEC_THROUGH=1`.
The worker factory strips inherited `DAEMONPILOT_` variables before setting its own
protocol values, so this internal routing value is not inherited. Worker code uses `EnvIsExecThrough`
(`DAEMONPILOT_IS_EXEC_THROUGH`) for the supported runtime-state contract.

`ReadStaticEnv` reads the static values. `GetRuntimeInfo` combines them with a
type-1 IPC snapshot when the watchdog is reachable.

Windows has no inherited-pipe IPC. `GetRuntimeInfo` returns static information
there; explicit readiness and watchdog log collection are unavailable. Probation
uses sustained worker uptime rather than a readiness message.

`ResidentWatchdogStale(ownWatchdogVersion)` reports a mismatch only when the
resident's version is non-empty. Empty or absent means a watchdog too old to
understand exit 101. Set `Config.RespawnStaleWatchdog` to check before `WorkerFunc`
and request a restart automatically, or perform compatibility work yourself and
then call `RespawnWatchdog()`. Flush application logs before either exit helper;
`os.Exit` does not run deferred calls. `StopPermanently()` exits 100 to end
supervision. See [rolling out watchdog code](self-update.md#rolling-out-watchdog-code).

## Exit Codes Are Requests

The worker uses two reserved exit codes as a language with its watchdog:

- `ExitPermanent` (`100`) asks the watchdog to stop supervision without
  restarting the worker.
- `ExitRespawnWatchdog` (`101`) asks the watchdog to stop and relaunch its own
  process image from the current on-disk binary.

An older watchdog that does not recognize a reserved code treats it like an
ordinary worker failure: it applies backoff and starts another worker. It cannot
perform the requested transition. This is why the watchdog version is exported
to the worker and why compatibility decisions belong in the worker binary.

The worker exit is what makes new watchdog logic reach a running deployment.
Replacing the file on disk changes future process images, not the already
running watchdog. Exit code `101` crosses that boundary by asking the resident
watchdog to return `ErrExecRestart`. On Unix, `handleRun` calls `syscall.Exec`
directly, preserving the PID and loading the current disk image regardless of
init capability. Exec-through is selected for a staged update when init cannot
restart the watchdog, not by exit 101.

Windows cannot directly re-exec: this request returns an error from `run`, and
an installed SCM service depends on recovery to restart. Console runs require
an external restart. See [Self-Update](self-update.md#fast-path-and-staged-path).

## IPC Transport and Framing

IPC uses two anonymous `os.Pipe` pairs, not a Unix-domain or filesystem socket.
The watchdog passes the worker the request write end and response read end via
`exec.Cmd.ExtraFiles`; `DAEMONPILOT_IPC_FD` names their descriptor numbers. The
watchdog keeps the opposite ends. A new pair and `ipc.Server` are created for
each worker spawn.
This descriptor transport is Unix-only; Windows launches omit these files and
run the worker without IPC.

The worker always initiates. Requests start with a one-byte message type.
Responses, where present, are one JSON object followed by `\n`. Calls on
`ipc.Client` are serialized, so response framing cannot be interleaved.

| Constant         | Type byte | Request payload                                      | Response                             |
| ---------------- | --------: | ---------------------------------------------------- | ------------------------------------ |
| `MsgTypeRequest` |       `1` | none                                                 | newline-delimited JSON `DynamicInfo` |
| `MsgTypeReady`   |       `2` | none                                                 | none                                 |
| `MsgTypeLogs`    |       `3` | 8-byte unsigned big-endian finished-through sequence | newline-delimited JSON `LogBatch`    |

Unknown message types are ignored. Closing a pipe or cancelling the worker's
context ends that worker's server.

`NotifyReady` sends `MsgTypeReady`. Receipt commits that this worker reached
readiness: the watchdog marks it ready and stable for restart-backoff purposes.
On systemd, the public function also sends `READY=1` through `sd_notify`. The
message is a one-way signal; it has no IPC acknowledgement.

## Pulling Watchdog Logs

Set `Config.CollectWatchdogLogs` to opt in. `Config.WatchdogLogBufferBytes`
sets the byte cap and defaults to `DefaultWatchdogLogBufferBytes` (256 KiB)
when collection is enabled. With collection disabled, no ring is allocated and
the configured `Logger` behaves as before.

Each captured log line receives a monotonically increasing `LogEntry.Sequence`.
`LogBatch.Entries` contains at most 128 lines and `LogBatch.Dropped` is the
cumulative count evicted because of the byte cap. A line larger than the entire
cap is itself counted as dropped. Sequence gaps therefore have an explicit
cause rather than being silent.

Call `CollectWatchdogLogs(finishedThrough)` from the worker. The cursor means
only that the client asserts it no longer needs any line through that sequence;
shipping, persistence and delivery semantics belong to the embedding
application. The watchdog deletes exactly those acknowledged lines and returns
what remains. Calling again with the same cursor can return the same retained
lines because collection does not acknowledge them; new writes or capacity
eviction can change the batch. A new worker starts with
zero and receives everything still retained, while lines acknowledged by a
previous worker stay deleted.

Three constraints are part of the design:

- **Zero disk.** Retention is memory-only. A default disk stream would wear
  flash-backed storage and fail on read-only filesystems.
- **Pull, never push.** The worker asks and the watchdog answers. Logging cannot
  make the supervisor depend on a worker continuously draining a pushed stream.
- **Collection is not acknowledgement.** Returning lines does not remove them.
  A worker that dies after a response must not destroy evidence it never
  finished with.

When nobody collects, the ring evicts its oldest lines and increments
`LogBatch.Dropped`. Memory remains bounded, and logging does not wait for an IPC
client. A client that stops reading can block only its per-worker IPC server
while it writes that response; it cannot block the logger or ring.

## Limits and Non-Guarantees

This protocol does not make logs durable. Watchdog process death loses the
memory ring and its cursor state. A watchdog restart begins a new sequence and
does not remember acknowledgements from the previous process. If the watchdog
is wedged, or no worker ever runs and collects again, nothing delivers the
retained lines. Capacity eviction is intentional and reported, not prevented.

IPC also depends on a responsive watchdog and intact inherited descriptors.
`GetRuntimeInfo` deliberately falls back to static environment data when IPC is
unavailable; log collection instead returns an error because there is no batch
to return honestly.

## Worker Liveness and Diagnostics

`NewLiveness(path)` returns a `*Liveness`. `Path() string` returns that path.
The worker calls `Mark(grace time.Duration) error` after making progress; the
watchdog uses `Check() error` or assigns `HealthFunc() func(context.Context) error`
to `Config.HealthFunc`:

```go
liveness := daemonpilot.NewLiveness(filepath.Join(installDir, "worker.liveness"))
cfg.HealthFunc = liveness.HealthFunc()
// In the worker, after successful progress:
if err := liveness.Mark(2 * time.Minute); err != nil {
    logger.Warn("could not record worker progress", "error", err)
}
```

Create the parent directory first. Mark replaces the file atomically using a
same-directory temporary file with mode 0600. The bytes are exactly
`strconv.FormatInt(time.Now().Add(grace).UnixNano(), 10)`, with no newline or
metadata. **This format must stay compatible with already deployed watchdogs.**
Check accepts surrounding whitespace and returns an error wrapping
`ErrLivenessLapsed` only for a parsed deadline that has passed; the error includes
how long ago, rounded to a second. Missing, unreadable, and malformed files are
healthy to avoid restarting a fresh or healthy worker over a filesystem hiccup.
The application chooses its grace period and whether to remove stale liveness
state when starting a fresh worker.

`StartStackDumpHandler(ctx context.Context, dir string, onDump func(path string,
err error)) bool` is the worker counterpart of `Config.DumpWorkerOnHealthFailure`.
Call it in `WorkerFunc` to arm SIGUSR1. Each received signal captures all goroutine
stacks, writes `<dir>/goroutine-dump-<unix seconds>.txt` with mode 0600 and writes
the same dump to stderr, then calls the optional callback with any write error.
Create the directory first. Two dumps in the same second share a filename; the
later dump replaces the file's contents. The handler stops on context cancellation.
Windows returns false and starts nothing.

## Embedding in an Application CLI

`(*Daemonpilot).RunMode(mode string, args ...string) error` dispatches a supported
command without reading or changing `os.Args`. For an application's own command,
call `s.RunMode("run")`. Set `Config.CommandPrefix` when daemonpilot commands live
under an application's subcommand, for example `[]string{"daemon"}`. Every
re-execution then uses that prefix: workers start with `daemon worker`,
exec-through children with `daemon run --exec-through`, and probes with
`daemon selftest` or `daemon watchdog-version`. The application's dispatcher
must route these commands back to `RunMode`. Unknown modes return an error.
`Run()` dispatches `os.Args[1]` as the mode; it does not strip `CommandPrefix`.
Applications with a prefix must parse their own CLI and call `RunMode`.

`ErrNotInstalled` can be tested with `errors.Is` on errors from `Uninstall`,
`Validate`, or `RunMode("update-config")`; existing human-readable messages remain
unchanged. After uninstall, `PurgeInstallDir(dir string) error` removes remaining
application data. Only pass a dedicated application directory, never a shared
location such as `/usr/local/bin`. Empty, dot, parent, and root paths are refused;
a missing directory succeeds. On Windows the only tolerated leftovers are renamed
running images matching `<binary>.exe.pending-delete-<base36 UnixNano>`, which
uninstall schedules for deletion at reboot. Other leftovers remain errors.

Import `github.com/ovineko/daemonpilot/pkg/daemonpilot` and blank-import
`github.com/ovineko/daemonpilot/pkg/daemonpilot/setup` to wire implementations.
Put application logic in `WorkerFunc` and use `daemonpilot.New`, then `Run` or
`RunMode` to dispatch daemonpilot commands.
