package process

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ovineko/procguard"
)

// RunProbe executes a daemonpilot selftest or version command and reports its status.
func RunProbe(ctx context.Context, path string, command []string, stdout, stderr io.Writer) error {
	// Probes have no shutdown protocol; retain immediate timeout escalation.
	child, err := procguard.Start(ctx, procguard.Spec{
		Path: path, Args: command, Stdout: stdout, Stderr: stderr,
		GracePeriod: time.Nanosecond,
	})
	if err != nil {
		return fmt.Errorf("start %s process: %w", probeName(command), err)
	}
	exit := child.Wait()
	if exit.Err != nil {
		return fmt.Errorf("%s process exited with error: %w", probeName(command), exit.Err)
	}
	if exit.Signal != nil {
		return fmt.Errorf("%s process exited with error: signal: %s", probeName(command), exit.Signal)
	}
	if exit.Code != 0 {
		return fmt.Errorf("%s process exited with error: exit status %d", probeName(command), exit.Code)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s process interrupted: %w", probeName(command), err)
	}
	return nil
}

// probeName is the verb a probe ran, for a message that names what failed.
func probeName(command []string) string {
	if len(command) == 0 {
		return "probe"
	}

	return command[len(command)-1]
}
