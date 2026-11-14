//go:build windows

package daemon

import (
	"fmt"
	"syscall"
)

// defaultKill is unsupported on Windows, which has no signal delivery. The
// BusyBox manager that uses it only ever runs on unix-like systems.
func defaultKill(pid int, sig syscall.Signal) error {
	return fmt.Errorf("sending signal %d to pid %d: not supported on windows", sig, pid)
}
