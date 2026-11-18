package daemonpilot

import (
	"path/filepath"
	"runtime"
	"strings"
)

// SCM passes BinaryPathName to CreateProcess as a command line, so Windows
// appends .exe to an extensionless executable name.
func installedBinaryName(serviceName string) string {
	return installedBinaryNameFor(serviceName, runtime.GOOS)
}

// InstalledBinaryName returns the file name used for an installed service
// binary on the current platform. The normalization is idempotent, so callers
// may safely apply it at every boundary even when a name is already normalized.
func InstalledBinaryName(serviceName string) string {
	return installedBinaryName(serviceName)
}

// InstalledBinaryNameFor returns the installed binary name for goos. Taking
// the platform explicitly keeps the Windows path contract testable on any host.
func InstalledBinaryNameFor(serviceName, goos string) string {
	return installedBinaryNameFor(serviceName, goos)
}

func installedBinaryNameFor(serviceName, goos string) string {
	if goos != "windows" || strings.EqualFold(filepath.Ext(serviceName), ".exe") {
		return serviceName
	}
	return serviceName + ".exe"
}

// Install, service registration, updates, and cleanup must use the same path.
func installedBinaryPath(cfg *Config) string {
	return filepath.Join(cfg.InstallDir, installedBinaryName(cfg.Name))
}

// StagedNewPath retains the executable extension so update probes can run on
// Windows, where os/exec resolves extensions through PATHEXT.
func StagedNewPath(binaryPath string) string {
	return stagedNewPathFor(binaryPath, runtime.GOOS)
}

func stagedNewPathFor(binaryPath, goos string) string {
	if goos != "windows" {
		return binaryPath + stagedNewSuffix
	}
	ext := filepath.Ext(binaryPath)
	return strings.TrimSuffix(binaryPath, ext) + stagedNewSuffix + ext
}

// LegacyStagedNewPath identifies the old staging filename for cleanup.
func LegacyStagedNewPath(binaryPath string) string { return binaryPath + stagedNewSuffix }

// SupersededBinarySuffix marks an image moved aside because Windows cannot
// overwrite a running executable. The updater sweeps these files on restart.
const SupersededBinarySuffix = ".superseded-"

const stagedNewSuffix = ".new"
