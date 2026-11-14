//go:build !windows

package daemonpilot

import (
	"fmt"
	"os"
	"syscall"
)

func execSelf(binaryPath string) error {
	args := append([]string{binaryPath}, os.Args[1:]...)
	//nolint:gosec // re-executing our own installed binary is the point of exec-through
	if err := syscall.Exec(binaryPath, args, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", binaryPath, err)
	}
	return nil
}
