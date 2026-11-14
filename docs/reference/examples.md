# Examples

The [`examples/`][examples] directory contains embedding examples:

| Example              | What it shows                                                                                                                    |
| -------------------- | -------------------------------------------------------------------------------------------------------------------------------- |
| `minimal`            | Runnable service with worker supervision and no self-update                                                                      |
| `with-updates`       | Service wiring for signing, staged approval, and rollback callbacks; requires application-specific key and update source setup   |
| `http-server`        | Runnable HTTP server with graceful shutdown                                                                                      |
| `http-update-source` | Importable helper package configuring `pkg/httpsource` with bearer authentication and release-compatible URLs, not an executable |

[examples]: https://github.com/ovineko/daemonpilot/tree/main/examples

## HTTP Service with Health Checks

This complete program binds its listener before signaling readiness, exposes the
same health endpoint the watchdog probes, and shuts down on worker cancellation:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	_ "github.com/ovineko/daemonpilot/pkg/daemonpilot/setup"
)

func main() {
	s, err := daemonpilot.New(daemonpilot.Config{
		Name:           "myapp",
		Description:    "HTTP service with health checks",
		WorkerFunc:     workerMain,
		Logger:         slog.Default(),
		HealthFunc:     healthCheck,
		HealthInterval: 15 * time.Second,
		HealthFailures: 3,
		NotifyReady:    true,
	})
	if err == nil {
		err = s.Run()
	}
	if err != nil {
		slog.Error("service failed", "error", err)
		panic(err)
	}
}

func workerMain(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	if err := daemonpilot.NotifyReady(); err != nil {
		return err
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			_ = srv.Close()
		}
	}()

	err = srv.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-stopped
		return nil
	}
	return err
}

func healthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://127.0.0.1:8080/health", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health status %d", resp.StatusCode)
	}
	return nil
}
```

Health probes run in the watchdog. The worker binds only to loopback; choose a
port and interface appropriate for your application. Readiness resets restart
backoff and can notify systemd; dynamic watchdog IPC is unavailable on Windows.
See [Readiness](../guides/readiness.md) and
[Health Checks](../guides/health-checks.md).

Resource limits are deliberately not set in this example. `MaxMemoryBytes` is a
virtual-address-space limit rather than an RSS budget, so a small value can prevent
the Go runtime from starting. Choose values using
[Resource Limits](../guides/resource-limits.md), not the expected HTTP heap size.

## Running an Example

Complete the development setup in
[CONTRIBUTING.md](https://github.com/ovineko/daemonpilot/blob/main/CONTRIBUTING.md).
Dependencies are downloaded through Go modules. From the repository root:

```bash
go build -o minimal ./examples/minimal
sudo ./minimal install
./minimal status
sudo ./minimal uninstall
```

These commands modify the host's service registration. To run without installing,
use `./minimal run` for supervision or `./minimal worker` for foreground work.
On Windows, use `minimal.exe` and an elevated terminal for SCM installation.
A user-scope service requires `Scope: daemonpilot.ScopeUser` in the application
configuration before building; omit `sudo` for its lifecycle commands.

The `with-updates` example requires replacing its demonstration public key and
providing an update endpoint; its default configuration is not a working release
service. Its approval callbacks demonstrate the API but approve immediately, so
replace them with application policy. [HTTP Releases](../guides/http-releases.md)
provides the concrete signing and distribution workflow, and
[Self-Update](../guides/self-update.md) explains each update path.
