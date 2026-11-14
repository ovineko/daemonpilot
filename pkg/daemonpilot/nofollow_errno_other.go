//go:build !windows && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package daemonpilot

import (
	"errors"
	"syscall"
)

// symlinkRefused reports whether err is O_NOFOLLOW refusing a symlink (ELOOP on
// Linux; the BSD counterpart lists the errnos other kernels use).
func symlinkRefused(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}
