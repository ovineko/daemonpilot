package process

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestSetupSignalHandler(t *testing.T) {
	ctx, cancel := SetupSignalHandler(context.Background())
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatal("context should not be cancelled yet")
	default:
	}
}

func TestSetupSignalHandlerCancelsOnSignal(t *testing.T) {
	ctx, cancel := SetupSignalHandler(context.Background())
	defer cancel()

	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("failed to find own process: %v", err)
	}

	if err := proc.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("failed to send signal: %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(testsupport.Scale(2 * time.Second)):
		t.Fatal("context was not cancelled after signal")
	}
}

func TestSetupSignalHandlerParentCancel(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	ctx, cancel := SetupSignalHandler(parent)
	defer cancel()

	parentCancel()

	select {
	case <-ctx.Done():
	case <-time.After(testsupport.Scale(2 * time.Second)):
		t.Fatal("context was not cancelled after parent cancel")
	}
}
