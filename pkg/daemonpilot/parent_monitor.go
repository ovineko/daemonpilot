package daemonpilot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/ovineko/procguard"
)

func workerParentMonitor(ctx context.Context, logger Logger) (context.Context, context.CancelFunc) {
	if logger == nil {
		return context.WithCancel(ctx)
	}
	disabled := parseBool(os.Getenv(EnvDisableParentMonitor))
	pid := 0
	if raw := os.Getenv(EnvParentPID); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &pid); err != nil {
			logger.Warn("invalid DAEMONPILOT_PARENT_PID, parent monitoring disabled", "value", raw, "error", err)
		}
	}
	if disabled {
		logger.Info("parent death monitoring disabled")
		return context.WithCancel(ctx)
	}
	if pid <= 0 {
		logger.Warn("parent monitor config validation failed, continuing without monitoring", "error", fmt.Errorf("parent PID must be positive, got %d", pid))
		return context.WithCancel(ctx)
	}
	monCtx, cancel, err := startParentMonitorFn(ctx, pid, 0)
	if err != nil {
		switch {
		case runtime.GOOS == "windows" && errors.Is(err, procguard.ErrUnsupported):
			logger.Info("parent death monitor not available on Windows; watchdog Job Object provides process-tree cleanup")
		case runtime.GOOS == "linux":
			logger.Warn("prctl PR_SET_PDEATHSIG failed, continuing without kernel-level parent death monitoring", "error", err)
		default:
			logger.Warn("parent monitor config validation failed, continuing without monitoring", "error", err)
		}
		return context.WithCancel(ctx)
	}
	if runtime.GOOS == "linux" {
		logger.Info("parent death signal configured via prctl")
	} else {
		logger.Info("parent death monitor started (unix polling)", "parentPID", pid, "pollInterval", 5*time.Second)
	}
	logged := make(chan struct{})
	go func() {
		defer close(logged)
		<-monCtx.Done()
		var exited *procguard.ParentExitError
		if errors.As(context.Cause(monCtx), &exited) {
			message := "parent process died, cancelling context"
			if runtime.GOOS == "linux" {
				message = "parent process already exited (race condition detected)"
			}
			logger.Warn(message, "expectedPID", exited.ExpectedPID, "currentPID", exited.ObservedPID)
		}
	}()
	return monCtx, func() { cancel(); <-logged }
}
