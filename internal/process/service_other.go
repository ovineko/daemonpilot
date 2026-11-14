//go:build !windows

package process

// RunAsService is the identity off Windows: every init system here starts an
// ordinary process and stops it with a signal, which SetupSignalHandler already
// handles. Keeping the call site platform-independent is what lets the run path
// stay one function instead of two that drift.
func RunAsService(_ string, body func() error) error { return body() }

// serviceStopChan is nil off Windows, and a receive on a nil channel blocks
// forever — so the select in SetupSignalHandler behaves exactly as it did before
// the service case existed.
func serviceStopChan() <-chan struct{} { return nil }
