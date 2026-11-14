//go:build windows

package watchdog

import "github.com/ovineko/procguard"

func dumpProcessDiagnostics(_ *procguard.Supervisor) error { return nil }
