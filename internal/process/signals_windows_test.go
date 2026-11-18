//go:build windows

package process

import (
	"context"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestSetupSignalHandlerCancelsOnServiceStop(t *testing.T) {
	ctx, cancel := SetupSignalHandler(context.Background())
	defer cancel()

	signalServiceStop()

	select {
	case <-ctx.Done():
	case <-time.After(testsupport.Scale(2 * time.Second)):
		t.Fatal("context was not cancelled after service stop")
	}
}
