package daemonpilot

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// PurgeInstallDir removes an application's install directory and all remaining data
// after Uninstall. Use only a dedicated application directory, never a shared bin directory.
// On Windows, renamed running images scheduled for deletion at reboot may remain.
func PurgeInstallDir(dir string) error {
	clean := filepath.Clean(dir)
	absolute, err := filepath.Abs(clean)
	if err != nil {
		return fmt.Errorf("resolving install directory: %w", err)
	}
	if dir == "" || clean == "." || clean == ".." || filepath.Dir(clean) == clean || filepath.Dir(absolute) == absolute {
		return fmt.Errorf("refusing to purge unsafe install directory %q", dir)
	}
	if err := os.RemoveAll(absolute); err != nil {
		if runtime.GOOS == "windows" {
			entries, readErr := os.ReadDir(absolute)
			if readErr == nil && onlyPendingDeleteLeftovers(entries) {
				return nil
			}
		}
		return fmt.Errorf("purging install directory %q: %w", dir, err)
	}
	return nil
}

func onlyPendingDeleteLeftovers(entries []fs.DirEntry) bool {
	if len(entries) == 0 {
		return false
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return false
		}
		name, stamp, ok := strings.Cut(entry.Name(), ".pending-delete-")
		if !ok || len(name) <= len(".exe") || !strings.HasSuffix(name, ".exe") {
			return false
		}
		nanos, err := strconv.ParseInt(stamp, 36, 64)
		if err != nil || nanos <= 0 || strconv.FormatInt(nanos, 36) != stamp {
			return false
		}
	}
	return true
}
