//go:build windows

package daemonpilot

import "github.com/ovineko/daemonpilot/internal/process"

func supervisionFileNeeded() bool { return process.IsWindowsService() }
