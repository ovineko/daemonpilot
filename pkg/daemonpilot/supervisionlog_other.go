//go:build !windows

package daemonpilot

func supervisionFileNeeded() bool { return false }
