package daemon

import (
	"errors"
	"fmt"
	"os/user"
)

// ErrUnknownUser is returned when the invoking user's account name cannot be
// resolved, leaving both the linger check and its remedy command without the
// name they need.
var ErrUnknownUser = errors.New("cannot resolve the invoking user's account name")

// currentUserFunc is a seam so tests can substitute the invoking user.
var currentUserFunc = user.Current

// currentUserName returns the invoking user's name for use in operator-facing
// remedies. It reads user.Current rather than $USER, which under `sudo -u` is
// often still root.
//
// The lookup can genuinely fail — a CGO-less build resolving an NSS/LDAP
// account has no /etc/passwd entry to read — so the failure is returned rather
// than papered over with a placeholder. The name is also a path component in
// systemdManager.LingeringEnabled, where a placeholder would stat a path that
// never exists and report lingering as disabled for every such account.
func currentUserName() (string, error) {
	u, err := currentUserFunc()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnknownUser, err)
	}
	if u == nil || u.Username == "" {
		return "", ErrUnknownUser
	}
	return u.Username, nil
}

// Scope selects whether a service is registered machine-wide or under the
// invoking user's account. The zero value is ScopeSystem, so existing callers
// keep producing exactly the manifests they produced before scopes existed.
type Scope int

const (
	// ScopeSystem registers the service with the machine-wide init system.
	ScopeSystem Scope = iota
	// ScopeUser registers the service with the per-user instance of the init
	// system, where one exists.
	ScopeUser
)

// String returns the scope as it appears in state files and error messages.
func (s Scope) String() string {
	switch s {
	case ScopeSystem:
		return "system"
	case ScopeUser:
		return "user"
	default:
		return "system"
	}
}

// ErrScopeNotSupported is returned when an init system cannot register a
// service in the requested scope.
var ErrScopeNotSupported = errors.New("init system does not support the requested install scope")

// ErrUserGroupInUserScope is returned when a user-scope service configuration
// carries a User or Group directive. A user-scope service already runs as the
// invoking user, and setting these requires privileges the scope does not have.
var ErrUserGroupInUserScope = errors.New("user scope does not support User or Group directives")

// ErrNoHomeDir is returned when a user-scope artifact path cannot be resolved
// because $HOME is empty. Joining an empty home would yield a relative path,
// which systemd rejects in ExecStart= and which would scatter state files into
// whatever the working directory happens to be.
var ErrNoHomeDir = errors.New("cannot resolve user home directory: $HOME is not set")

// ErrUserManagerUnreachable is returned when the host runs an init system that
// supports user scope, but its per-user instance cannot be reached from the
// current session.
var ErrUserManagerUnreachable = errors.New("per-user init system manager is unreachable")
