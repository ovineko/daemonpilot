// Package process integrates application signals and platform service control.
package process

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// SetupSignalHandler returns a context that is cancelled on SIGTERM or SIGINT —
// or, for a process running as a Windows service, on the SCM's Stop request.
// The returned cancel func stops signal delivery and must always be called.
func SetupSignalHandler(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-serviceStopChan():
			// A Windows service is sent no signals at all: its stop arrives as an
			// SCM control request, so without this case a Stop would reach a
			// handler with no way to reach the running work and the process would
			// be terminated hard instead of unwinding. The channel is nil off
			// Windows and a receive on nil blocks forever, so this case can never
			// be selected there and the behaviour is unchanged.
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(sigCh)
	}()

	return ctx, cancel
}
