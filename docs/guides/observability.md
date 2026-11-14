# Observability

## Lifecycle log vocabulary

Lifecycle records use the same structured keys across installation, supervision,
updates, and removal. Handlers may add operation-specific fields, but must not
rename these keys:

| Key                | Meaning                                                                                                       |
| ------------------ | ------------------------------------------------------------------------------------------------------------- |
| `phase`            | `install`, `run`, `update`, or `uninstall`                                                                    |
| `step`             | Stable name of the operation within the phase                                                                 |
| `outcome`          | `started`, `succeeded`, `failed`, `degraded`, `skipped`, or a documented decision such as `restart_scheduled` |
| `service_name`     | Configured service name                                                                                       |
| `install_dir`      | Directory containing the supervised binary and state                                                          |
| `scope`            | `system` or `user` when scope affects the operation                                                           |
| `init_system`      | Selected service manager                                                                                      |
| `version`          | Worker/application version involved in the event                                                              |
| `watchdog_version` | Supervision protocol version involved in the event                                                            |
| `error`            | Error text for an unsuccessful operation                                                                      |

INFO is the lifecycle narrative: decisions, starts, and successful outcomes.
DEBUG is repeatable diagnostic detail, such as an update poll that found
nothing. WARN means supervision continues with a degraded capability or after a
recovered failure. ERROR means the operation cannot safely continue or a human
must act. A failure that is handled successfully is therefore WARN, not ERROR.

Long-running steps emit `outcome=started` before beginning and a terminal
outcome when they finish. A final `started` record with no matching outcome
identifies the operation that was still in progress when logging stopped.

## RuntimeInfo

`daemonpilot.GetRuntimeInfo()` answers questions the worker cannot answer about
itself: how often it has been restarted, how long the watchdog has been up, what
the last health check said, whether an update is pending.

```go
info, err := daemonpilot.GetRuntimeInfo()
if err != nil {
	return err
}

log.Printf("mode=%s version=%s init=%s restarts=%d uptime=%s",
	info.Mode, info.Version, info.InitSystem,
	info.RestartCount, info.WatchdogUptime)

if info.IsExecThrough {
	log.Println("running via exec-through (init cannot auto-restart)")
}
```

Static fields — mode, versions, init system, binary path, embedded flag — come
from environment variables the watchdog sets. Dynamic fields — restart count,
backoff, uptimes, health and update status — come from the watchdog over IPC.
When IPC is unavailable (a binary run directly, for instance) the static half is
still returned rather than an error, so a status endpoint keeps working outside
a watchdog.

Windows runs workers without inherited-pipe IPC, so these dynamic fields are
unavailable there even when the watchdog is healthy. Do not interpret zero
dynamic values as a measurement of watchdog state on that platform.

It is safe for concurrent use, which makes it convenient to serve directly:

```go
http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
	info, _ := daemonpilot.GetRuntimeInfo()
	_ = json.NewEncoder(w).Encode(info)
})
```

## Prometheus Metrics

Metrics live in their own package, `pkg/daemonpilot/metrics`. Importing it is what
pulls in the Prometheus client — around nine megabytes of binary with its
protobuf dependency — so an application that exports nothing never carries it:

```go
import daemonpilotmetrics "github.com/ovineko/daemonpilot/pkg/daemonpilot/metrics"

registry := prometheus.NewRegistry()
metrics, err := daemonpilotmetrics.Register(registry)
if err != nil {
	return err
}

// Refresh from RuntimeInfo every 10 seconds.
metrics.StartUpdater(ctx, 10*time.Second, daemonpilot.GetRuntimeInfo)

http.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
```

| Metric                                    | Type    | Description                                                   |
| ----------------------------------------- | ------- | ------------------------------------------------------------- |
| `daemonpilot_worker_restarts_total`       | Counter | Total worker process restarts                                 |
| `daemonpilot_worker_uptime_seconds`       | Gauge   | Current worker uptime                                         |
| `daemonpilot_watchdog_uptime_seconds`     | Gauge   | Current watchdog uptime                                       |
| `daemonpilot_current_backoff_seconds`     | Gauge   | Current restart backoff delay                                 |
| `daemonpilot_healthcheck_failures_total`  | Counter | Observed increases in consecutive health-check failures       |
| `daemonpilot_update_last_check_timestamp` | Gauge   | Unix timestamp of the last update check                       |
| `daemonpilot_version_info`                | Gauge   | Version info (labels: version, watchdog_version, init_system) |

Counters accumulate positive differences between runtime snapshots, so resets
do not move them backwards. This is sampling: failures followed by a counter
reset between refreshes are not counted. Watchdog restart history is likewise
not a durable event ledger.

## Logging

`Config.Logger` takes any implementation of the `Logger` interface, which
`*slog.Logger` satisfies:

```go
// Standard library slog
daemonpilot.Config{Logger: slog.Default()}

// Built-in default logger
daemonpilot.Config{Logger: daemonpilot.NewDefaultLogger(daemonpilot.LogLevelInfo)}

// Silent (the default)
daemonpilot.Config{Logger: nil}
```

On embedded init systems — BusyBox, procd, s6, runit — logging is automatically
reduced to critical errors. These devices usually log to flash, and a chatty
supervisor wears it out. Set `VerboseLogging: true` to override this when you
are debugging a device.

## Embedded System Detection

`GetSystemInfo` reports what kind of host the service is on, so an application
can adapt the same way the library does:

```go
info := daemonpilot.GetSystemInfo("busybox")
fmt.Println(info.IsEmbedded)       // true
fmt.Println(info.RecommendedLevel) // "silent"
fmt.Println(info.StorageWarning)   // flash storage caveat

// Or from inside a worker:
rtInfo, _ := daemonpilot.GetRuntimeInfo()
if rtInfo.IsEmbedded {
	// Reduce disk writes, keep logs in tmpfs, and so on.
}
```

## Log Destinations

A slow journal or console can hold a logger's shared lock and stall application
work. Use `daemonpilot.NewNonBlockingWriter(destination, 1024)` for a best-effort
mirror. It copies each write, drops writes when the queue fills, and exposes the
number dropped through `Dropped()`. Keep any required durable logging separate;
this writer provides neither delivery guarantees nor flushing.

For the watchdog's own logs, use the rotating Windows service destination:

```go
destination := daemonpilot.SupervisionLogWriter(logPath, 8<<20)
mirror := daemonpilot.NewNonBlockingWriter(destination, 1024)
logger := slog.New(slog.NewJSONHandler(mirror, nil))
```

Run this initialization only in watchdog `run` mode. Other platforms use stderr;
a Windows SCM service also writes to `logPath`, retaining one previous generation
at `logPath+".1"`. Rotation occurs while running as well as at startup, and a
stderr failure does not prevent the file from receiving the log. The parent
folder must already exist. See [Logging Writers](../reference/configuration.md#logging-writers)
for defaults and lifetime details.
