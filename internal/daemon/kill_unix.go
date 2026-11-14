//go:build !windows

package daemon

import "syscall"

// defaultKill signals a process by PID. Signal 0 performs an existence check
// without delivering anything.
func defaultKill(pid int, sig syscall.Signal) error {
	//nolint:wrapcheck // callers add their own context; the raw errno stays inspectable
	return syscall.Kill(pid, sig)
}
