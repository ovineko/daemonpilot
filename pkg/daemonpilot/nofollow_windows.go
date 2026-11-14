//go:build windows

package daemonpilot

import (
	"fmt"
	"os"
)

// openRegularNoFollow has no O_NOFOLLOW equivalent on Windows, where the
// install directory is not group-writable by construction and reparse points
// require privilege to create. It checks the type through Lstat and opens.
func openRegularNoFollow(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspecting %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is %w (mode %s)", path, errNotRegularFile, info.Mode())
	}
	f, err := os.Open(path) //nolint:gosec // a caller-named install artifact is the whole point
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return f, nil
}

// writeFileNoFollow falls back to os.WriteFile on Windows for the same reason.
func writeFileNoFollow(path string, data []byte, perm os.FileMode) error {
	//nolint:wrapcheck // the platform stub's error is the whole message
	return os.WriteFile(path, data, perm)
}
