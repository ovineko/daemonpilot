//go:build windows

package daemonpilot

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/windows"
)

// pendingDeleteSuffix marks a binary that could not be deleted because it was
// still executing, and has been moved aside and scheduled for removal at reboot.
const pendingDeleteSuffix = ".pending-delete-"

// removeInstalledBinary deletes the installed binary, working around the rule
// that exists only here: a RUNNING image may not be deleted, but it MAY be
// renamed.
//
// The case is not exotic, it is the normal one: `<install dir>\<name>.exe
// uninstall` is the documented way to uninstall, so the process doing the
// removing IS the image being removed. A plain os.Remove answers "Access is
// denied", and the uninstall then reported success while leaving the binary —
// and with it a directory that cannot be deleted either.
//
// Moving it aside frees the install path immediately, so a reinstall works at
// once and only the aside copy lingers. That copy is deleted on the spot if
// nothing holds it, and otherwise handed to MoveFileEx with
// MOVEFILE_DELAY_UNTIL_REBOOT — the OS removes it during the next boot, before
// anything can open it.
func removeInstalledBinary(path string, logger Logger) error {
	if err := os.Remove(path); err == nil {
		return nil
	} else if !os.IsPermission(err) && !isAccessDenied(err) {
		//nolint:wrapcheck // the caller decides how to report this
		return err
	}

	aside := path + pendingDeleteSuffix + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := os.Rename(path, aside); err != nil {
		return fmt.Errorf("the binary is in use and could not be moved aside: %w", err)
	}

	if err := os.Remove(aside); err == nil {
		return nil
	}

	// Still held — by this very process, when the uninstall was run from the
	// installed binary. Let the OS finish the job at the next boot.
	from, err := windows.UTF16PtrFromString(aside)
	if err != nil {
		return fmt.Errorf("scheduling %s for deletion: %w", aside, err)
	}
	if err := windows.MoveFileEx(from, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT); err != nil {
		return fmt.Errorf("scheduling %s for deletion at reboot: %w", aside, err)
	}
	if logger != nil {
		logger.Info("the binary was still executing, so it was moved aside and will be deleted at the next reboot",
			"path", path, "pending", aside)
	}
	return nil
}

// isAccessDenied reports the Windows error a running image produces, which is
// not always mapped to os.ErrPermission.
func isAccessDenied(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
