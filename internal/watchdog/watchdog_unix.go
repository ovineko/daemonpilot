//go:build !windows

package watchdog

import (
	"fmt"
	"syscall"

	"github.com/ovineko/procguard"
)

func dumpProcessDiagnostics(supervisor *procguard.Supervisor) error {
	if err := supervisor.Signal(syscall.SIGUSR1); err != nil {
		return fmt.Errorf("sending SIGUSR1 to worker: %w", err)
	}
	return nil
}
