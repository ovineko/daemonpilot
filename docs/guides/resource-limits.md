# Resource Limits

The watchdog can constrain the worker it spawns on supported platforms. These
per-process limits reduce risk; they do not isolate every resource or guarantee
that host-wide exhaustion cannot affect other processes.

```go
oomScore := 500

s, err := daemonpilot.New(daemonpilot.Config{
	Name:           "myapp",
	WorkerFunc:     workerMain,
	MaxMemoryBytes: 512 * 1024 * 1024, // RLIMIT_AS for the worker
	OOMScoreAdj:    &oomScore,         // Linux only
})
```

The worker applies the environment-provided limits through
`procguard.ApplyLimitsToSelf` before entering `WorkerFunc`. Applying them in the
worker preserves FreeBSD support and leaves the watchdog unrestricted.

## Memory Limit

`MaxMemoryBytes` sets `RLIMIT_AS` — the maximum address space — on the worker
process. The limit is clamped to the inherited hard limit, so an over-ambitious
value cannot raise a limit the operator set. Exceeding it makes allocations fail
inside the worker; Go turns that into a fatal out-of-memory error, the worker
exits, and the watchdog restarts it with backoff.

`RLIMIT_AS` bounds _virtual_ address space, not resident memory. Runtimes that
reserve large address ranges without touching them can trip a limit that looks
generous, so pick the value against observed behaviour rather than an estimate.

### Platform Support

`MaxMemoryBytes` is only effective on Linux and FreeBSD:

| Platform | Behaviour                                                                                                                                                                                     |
| -------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Linux    | Applied.                                                                                                                                                                                      |
| FreeBSD  | Applied.                                                                                                                                                                                      |
| macOS    | The Go runtime's existing address-space reservation usually prevents lowering `RLIMIT_AS` to a practical limit. Failure is logged by the worker, which continues without the requested limit. |
| OpenBSD  | Unsupported. The worker logs `memory limit not supported on this platform, continuing without limit`.                                                                                         |
| Windows  | Unsupported. The worker logs `memory limit not supported on this platform, continuing without limit`. Job Objects provide tree cleanup, not this memory limit.                                |

On the platforms where the limit is not applied, use the init system's own
accounting instead — `MemoryMax=` in a systemd drop-in, or a cgroup/jail limit.

## OOM Score

`OOMScoreAdj` (Linux only) biases the kernel's out-of-memory killer, from
`-1000` (kill last) to `1000` (kill first). Raising the worker's score makes the
kernel prefer it over unrelated processes under memory pressure — a sensible
choice when the worker is restartable and the rest of the system is not.

The setting applies to the worker. The watchdog keeps its inherited score rather
than receiving `OOMScoreAdj` itself.

Off Linux, the OOM adjustment is a silent no-op. Unsupported memory limits still
produce the existing warning and allow the worker to continue.

Process cleanup is independent of these resource limits. Windows uses kill-on-close
Job Objects for workers and exec-through children, so watchdog death and restarts
clean up the assigned tree. Descendants receive the grace period before escalation;
see [Architecture](architecture.md#worker-process-trees) for Unix behavior and the
BSD natural-exit exception.

## Worker cgroup v2 isolation (Linux)

`Config.WorkerCgroupMemoryMax` isolates the worker in a cgroup v2 leaf and
caps its total memory in bytes; the leaf's swap is pinned to zero, so the cap
is enforced by the kernel's memcg OOM kill rather than absorbed by reclaim.
procguard creates the leaf and places the worker at clone
(`CLONE_INTO_CGROUP`, kernel 5.7+), so the limit holds before any worker code
runs and covers every worker descendant; hard kills and liveness read the
leaf's membership, closing the `setsid` escape that process groups have. The
request is fail-closed: a host without a delegated cgroup v2 parent fails
worker launch instead of running the worker unlimited.

`Config.WorkerCgroupParent` names the delegated parent (a systemd user service
slice, for example). Empty derives the parent from the watchdog's own cgroup,
which usually cannot host controller limits while the watchdog remains its
member. The setting is Linux-only and independent of `MaxMemoryBytes` (which
applies RLIMIT_AS inside the worker); a cgroup is not a parent-death mechanism
and not a security boundary — a child able to write cgroup filesystems can
migrate out of it.

## When Not to Bother

Use a measured limit on supported hosts when an allocation failure and restart
is preferable to unchecked address-space growth. A limit chosen without testing
the Go runtime's reservations and the application's workload can create a crash
loop, including on small embedded devices. Use host-level controls when the goal
is to bound resident memory or account for an entire descendant tree.
