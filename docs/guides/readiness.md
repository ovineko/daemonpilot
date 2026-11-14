# Readiness Signaling

A worker that has started is not necessarily a worker that works. Calling
`daemonpilot.NotifyReady()` once initialization finishes tells the watchdog the
difference.

```go
func workerMain(ctx context.Context) error {
	db, err := connectDB()
	if err != nil {
		return err
	}

	if err := daemonpilot.NotifyReady(); err != nil {
		return err
	}

	return serve(ctx, db)
}
```

## What It Changes

- **The backoff resets.** A worker that reached readiness proved the binary is
  viable, so the next crash starts from the shortest delay instead of inheriting
  the previous failure streak.
- **The state is observable.** Readiness is part of the runtime state the
  watchdog tracks and reports.
- **systemd is told too.** When the init system is systemd, `NotifyReady` also
  sends `sd_notify("READY=1")`.

Setting `Config.NotifyReady: true` makes the generated systemd unit use
`Type=notify` instead of `Type=simple`, so systemd treats the service as
starting until your worker signals. Dependent units then wait for real
readiness rather than for the process to exist.

```go
s, err := daemonpilot.New(daemonpilot.Config{
	Name:        "myapp",
	WorkerFunc:  workerMain,
	NotifyReady: true,
})
```

Only set it if the worker actually calls `NotifyReady()` — with `Type=notify`
and no notification, systemd waits until it times out and then considers the
start failed.

The watchdog readiness message requires inherited-pipe IPC, which is unavailable
on Windows. `NotifyReady` there returns without notifying the watchdog; backoff
and staged probation rely on sustained uptime instead.

## Running Outside a Watchdog

`NotifyReady` is safe to call when IPC is not configured, such as in tests or
when the binary is run directly: missing IPC is not an error. An error writing
to an established IPC connection, or sending systemd notification, is returned.
