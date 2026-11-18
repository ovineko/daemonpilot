//go:build e2e_native && darwin

package native

import (
	"errors"
	"syscall"
)

func workerProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
