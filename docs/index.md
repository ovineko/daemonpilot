# daemonpilot

A Go library for building self-managing, long-running services. Embed it in your
binary and the same executable can register itself with the host's init system,
supervise its own worker process, and verify and selftest signed updates — without
shipping a separate application-specific supervisor executable.

## Quick Start

Follow [Installation](get-started/installation.md) to configure module dependencies,
then import the public API and register its default implementations:

```go
import (
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	_ "github.com/ovineko/daemonpilot/pkg/daemonpilot/setup"
)
```

```go
s, err := daemonpilot.New(daemonpilot.Config{
	Name:        "myapp",
	Description: "My long-running service",
	WorkerFunc:  workerMain,
})
if err != nil {
	log.Fatal(err)
}
if err := s.Run(); err != nil {
	log.Fatal(err)
}
```

Build the binary, then let it install itself:

```bash
sudo ./myapp install
./myapp status
```

See the [Quick Start](get-started/quick-start.md) for the full walkthrough.

## What You Get

- **Daemonization** — the binary registers itself with whichever init system the
  host runs: systemd, launchd, OpenRC, dinit, Finit, sysvinit, runit, s6, procd,
  BusyBox init, crontab, or Windows SCM. See [Init Systems](guides/init-systems.md).
- **Watchdog supervision** — a parent process spawns your worker, restarts it on
  crashes with exponential backoff, and stops cleanly on shutdown. See
  [Architecture](guides/architecture.md).
- **Signed self-update** — streaming download, Ed25519 verification, pre-flight
  selftest, and approval plus optional rollback for watchdog-changing updates.
  See [Self-Update](guides/self-update.md).
- **Exec-through** — safe watchdog replacement even on init systems that never
  restart a process they started.
- **Health checks** — a periodic probe restarts an unhealthy worker. See
  [Health Checks](guides/health-checks.md).
- **Observability** — runtime information, optional Prometheus collectors, and
  readiness signaling. Dynamic watchdog queries use IPC on Unix; Windows exposes
  static launch metadata. See [Observability](guides/observability.md).
- **Resource limits** — worker memory caps on Linux/FreeBSD and Linux OOM score
  adjustment. See [Resource Limits](guides/resource-limits.md).

## Design Notes

The watchdog delegates process mechanics to `github.com/ovineko/procguard` while
daemonpilot owns lifecycle and update policy. Prometheus integration is a separate,
optional package. Platform capabilities differ: user-scope service installation
is available with systemd, launchd, and crontab, while Windows uses machine-wide
SCM services. Embedded hosts can use managers such as procd and BusyBox init
without depending on systemd. Updates include worker shutdown/startup latency;
they are not zero-downtime traffic handoffs.

## Documentation

- **[Installation](get-started/installation.md)** — module requirements, version
  injection, cross-compilation
- **[Quick Start](get-started/quick-start.md)** — from `go get` to an installed
  service
- **[Guides](guides/architecture.md)** — how the watchdog, updates, and init
  integration actually work
- **[CLI Commands](reference/cli.md)** — every subcommand your binary gains
- **[Configuration](reference/configuration.md)** — every `Config` field

## License

MIT © Alexander Svinarev (shibanet0)
