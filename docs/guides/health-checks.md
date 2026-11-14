# Health Checks

A crashed worker is easy to notice — it exits. A worker that is running but no
longer serving is not. `HealthFunc` covers the second case.

```go
s, err := daemonpilot.New(daemonpilot.Config{
	Name:       "myapp",
	WorkerFunc: workerMain,
	HealthFunc: func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"http://localhost:8080/health", nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("health check returned %d", resp.StatusCode)
		}
		return nil
	},
	HealthInterval: 15 * time.Second, // default: 30s
	HealthFailures: 5,                // default: 3
})
```

## How It Runs

The watchdog — not the worker — runs `HealthFunc` on a ticker. It therefore
keeps working when the worker is wedged, which is the point.

Each failure increments a counter; a success resets it to zero. When the counter
reaches `HealthFailures`, the watchdog restarts the worker. Unix sends group
`SIGTERM`, waits up to `GracefulTimeout`, then escalates to `SIGKILL`; Windows
allows voluntary exit through the grace window before terminating the job.
After requesting restart the failure counter starts from zero again.

## Interaction with Backoff

A health restart does not reset the restart backoff. If a worker keeps failing
its health check, each restart is slower than the last, up to `MaxRestartDelay`.
This is deliberate: an unhealthy worker restarted at full cadence forever
hammers whatever it depends on. Backoff still resets on genuine evidence of
health — the worker signalling [readiness](readiness.md), or running longer than
`StableThreshold`.

The consecutive-failure count is visible in
`RuntimeInfo.LastHealthCheck.ConsecutiveFailures`. The Prometheus
`daemonpilot_healthcheck_failures_total` counter accumulates observed increases
between metric refreshes, not every individual check; polling can miss failures
that reset between samples. See [Observability](observability.md).

## Writing a Good Check

- **Check the path your users take.** A handler that only returns `200 OK`
  proves the HTTP server is up, not that the service works.
- **Respect the context.** Each check receives a deadline of `HealthInterval`;
  use it for requests and honor cancellation. Daemonpilot does not forcibly interrupt
  a callback that ignores its context, and that can block checks and shutdown.
- **Keep it cheap.** It runs every `HealthInterval` forever.
- **Do not check dependencies you cannot fix by restarting.** Restarting the
  worker because a remote database is down turns one outage into two.
