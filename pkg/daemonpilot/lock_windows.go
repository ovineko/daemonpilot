//go:build windows

package daemonpilot

// platformLockStatus reports that there is no lock to take here, which is not
// the same as failing to take one — see LockStatusUnsupported.
func platformLockStatus() LockStatus { return LockStatusUnsupported }

// acquireInstanceLock is a no-op on Windows. Single-instance enforcement via a
// named mutex or Job Object is a future addition; the flock-based lock only
// applies to the Unix init-supervised deployments.
//
// What that actually costs: the SCM already refuses to run two copies of the
// same service, so the exposure is a manual `<binary> run` racing the installed
// service over one InstallDir. Real, small, and reported as `unsupported` rather
// than hidden.
func acquireInstanceLock(_ string, _ lockOwner) (release func(), err error) {
	return func() {}, nil
}
