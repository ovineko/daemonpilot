//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package daemonpilot

import (
	"errors"
	"syscall"
)

// symlinkRefused reports whether err is O_NOFOLLOW refusing a symlink. The
// errno differs per kernel: ELOOP on Darwin and OpenBSD, EMLINK on FreeBSD and
// DragonFly, EFTYPE on NetBSD.
func symlinkRefused(err error) bool {
	return errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) || errors.Is(err, syscall.EFTYPE)
}
