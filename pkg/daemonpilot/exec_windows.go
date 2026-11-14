//go:build windows

package daemonpilot

import (
	"fmt"
)

func execSelf(_ string) error {
	return fmt.Errorf("exec restart not supported on Windows; the init system must restart the process")
}
