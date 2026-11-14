//go:build e2e_docker && !windows

package main

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/ovineko/daemonpilot/e2e/docker/internal/wire"
)

func grandchild(c wire.Config) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	emit(c, "grandchild-start", "")
	<-ctx.Done()
	emit(c, "grandchild-stop", "")
	return nil
}
