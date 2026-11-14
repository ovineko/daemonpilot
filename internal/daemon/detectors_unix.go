//go:build !windows

package daemon

// platformDetectors is empty off Windows, so defaultDetectors() returns exactly the
// list it returned before platform detectors existed — the ordering every existing
// test and every fielded install depends on is unchanged.
func platformDetectors() []detectorEntry { return nil }
