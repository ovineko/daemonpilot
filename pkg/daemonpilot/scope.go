package daemonpilot

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/ovineko/daemonpilot/internal/daemon"
)

// InstallScope selects who owns an installation: the whole machine or the
// invoking user.
type InstallScope string

const (
	// ScopeSystem installs machine-wide, under the system init system.
	// It is the zero-value default and requires root.
	ScopeSystem InstallScope = "system"

	// ScopeUser installs under the invoking user's account, using the
	// per-user instance of the init system where one exists.
	ScopeUser InstallScope = "user"
)

// String returns the scope as it appears in state files and error messages.
func (s InstallScope) String() string { return string(s) }

// normalized returns the scope with the zero value resolved to ScopeSystem.
func (s InstallScope) normalized() InstallScope {
	if s == "" {
		return ScopeSystem
	}
	return s
}

// daemonScope maps the public scope onto the daemon package's scope.
func (s InstallScope) daemonScope() daemon.Scope {
	if s == ScopeUser {
		return daemon.ScopeUser
	}
	return daemon.ScopeSystem
}

// ErrNoHomeDir is returned when a user-scope installation is requested but the
// home directory cannot be resolved. User-scope paths are derived from $HOME
// alone, so an empty home has no correct silent fallback.
var ErrNoHomeDir = errors.New("daemonpilot: user scope requires a home directory, but $HOME is not set")

// userHomeDir is the test seam for home directory resolution. User-scope paths
// derive from $HOME alone, so this is the only identity lookup the public
// package makes; the linger user name is resolved inside internal/daemon.
var userHomeDir = os.UserHomeDir

// defaultInstallDir returns the install directory for the given OS and scope.
// The home argument is only consulted for ScopeUser; callers must reject an
// empty home before calling with ScopeUser.
func defaultInstallDir(goos, name string, scope InstallScope, home string) string {
	if scope == ScopeUser {
		if goos == "darwin" {
			return filepath.Join(home, "Library", "Application Support", name)
		}
		return filepath.Join(home, ".local", "share", name)
	}
	if goos == "darwin" {
		return "/usr/local/bin"
	}
	return "/opt/" + name
}

// defaultLogDir returns the directory init systems that log to files should
// write to. An empty result means "keep the manager's historical constant", so
// system scope leaves every existing manifest unchanged. The home argument is
// only consulted for ScopeUser; callers must reject an empty home before
// calling with ScopeUser.
func defaultLogDir(goos, name string, scope InstallScope, home string) string {
	if scope != ScopeUser {
		return ""
	}
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Logs", name)
	}
	return filepath.Join(home, ".local", "state", name)
}
