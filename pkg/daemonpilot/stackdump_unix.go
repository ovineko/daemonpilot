//go:build !windows

package daemonpilot

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func startStackDumpHandler(ctx context.Context, dir string, onDump func(string, error)) bool {
	if ctx.Err() != nil {
		return true
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	go func() {
		defer signal.Stop(signals)
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
				if ctx.Err() != nil {
					return
				}
				path, err := writeStackDump(dir)
				if onDump != nil {
					onDump(path, err)
				}
			}
		}
	}()
	return true
}
