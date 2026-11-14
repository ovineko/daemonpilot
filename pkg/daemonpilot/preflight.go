package daemonpilot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ovineko/daemonpilot/internal/daemon"
	"github.com/ovineko/daemonpilot/internal/process"
)

// ErrNeedsPrivileges is returned when an install cannot write one of the paths
// it is about to mutate. It is matched with errors.Is so a caller can turn a
// permission failure into its own exit code without parsing the message.
var ErrNeedsPrivileges = errors.New("daemonpilot: insufficient privileges for the installation paths")

// geteuid is the test seam for the effective uid. It is used only to word the
// error message: whether a path is writable is decided by probing it, never by
// comparing a uid — a non-zero euid is neither necessary (a group-writable
// manifest directory, CAP_DAC_OVERRIDE) nor sufficient (a read-only /etc).
var geteuid = os.Geteuid

// preflightInstall probes every directory the install is about to write before
// the first mutation, so a permission failure names the path and a remedy
// instead of surfacing a raw EACCES from the middle of the sequence.
//
// manifestPath is the manager's own manifest destination; an empty path — a
// manager that keeps no manifest file — is skipped. So is a relative one: it
// would resolve against the CLI's working directory, which the install never
// writes to, so probing it would refuse an install over a directory that has
// nothing to do with the manifest.
var preflightInstall = func(cfg *Config, manifestPath string) error {
	targets := []probeTarget{{label: "install directory", path: cfg.InstallDir}}
	if filepath.IsAbs(manifestPath) {
		targets = append(targets, probeTarget{label: "manifest directory", path: filepath.Dir(manifestPath)})
	}

	var failures []string
	for i := range targets {
		probed, err := probeDirWritable(targets[i].path)
		targets[i].probed = probed
		if err != nil {
			targets[i].err = err
			failures = append(failures, targets[i].describe())
		}
	}
	if len(failures) == 0 {
		return nil
	}

	return fmt.Errorf("%w: installing %q as euid %d: %s; probed paths: %s; %s",
		ErrNeedsPrivileges, cfg.Name, geteuid(), strings.Join(failures, "; "),
		describeTargets(targets), installPrivilegeRemedy(cfg))
}

// ErrNotExecutable is returned when the binary placed in the install directory
// cannot be executed from there. It is matched with errors.Is.
var ErrNotExecutable = errors.New("daemonpilot: the installed binary cannot be executed")

// execProbeTimeout bounds the version probe. The subcommand only prints a
// constant, so anything slower than this is a stuck filesystem, not work. It is
// a var so tests can shorten it rather than sleeping out the real window.
var execProbeTimeout = 10 * time.Second

// runVersionProbe executes the installed binary's watchdog-version subcommand,
// exactly as the updater does with a staged binary. It is a package-level var
// so tests can classify the failure modes without a real noexec mount.
//
// The probe runs in its own process group and the timeout kills the group, not
// just the direct child, for the same reason the updater's selftest does: the
// host program owns its argument parsing, so a binary that does not route
// watchdog-version may instead start doing its real work and spawn children of
// its own. Killing only the child would leave those orphaned by an install.
var runVersionProbe = func(path string, prefix []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), execProbeTimeout)
	defer cancel()

	return process.RunProbe(ctx, path, DaemonpilotArgs(prefix, "watchdog-version"), nil, nil)
}

// preflightExec runs the copied binary once from its destination. A write probe
// is not sufficient: /home is frequently mounted noexec on hardened fleets and
// ~/.local/share inherits that, which yields a green write preflight, a
// successful install, and then a 203/EXEC restart loop forever, because the
// generated unit carries Restart=always with StartLimitIntervalSec=0. The same
// probe catches a wrong-architecture binary and a missing dynamic loader.
//
// Only failures from the exec layer itself are fatal. A non-zero exit status is
// not: the host program owns its argument parsing, and refusing an install
// because a wrapper does not route watchdog-version would break callers whose
// binary is otherwise perfectly runnable.
var preflightExec = func(cfg *Config, path string) error {
	err := runVersionProbe(path, cfg.CommandPrefix)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES):
		return fmt.Errorf("%w: %s is not executable from its install directory: %v; "+
			"the destination filesystem appears to be mounted noexec, or the file lost its execute bit; "+
			"install into a directory on an exec-capable filesystem", ErrNotExecutable, path, err)
	case errors.Is(err, syscall.ENOEXEC):
		return fmt.Errorf("%w: %s is not a valid executable on this platform (%s/%s): %v; "+
			"the binary may be built for a different architecture, or its dynamic loader is missing",
			ErrNotExecutable, path, runtime.GOOS, runtime.GOARCH, err)
	default:
		cfg.Logger.Debug("install exec probe returned a non-fatal error",
			"path", path, "error", err.Error())
		return nil
	}
}

// ErrScopeCollision is returned when a service is already installed in the
// scope opposite the one being installed into. It is matched with errors.Is.
var ErrScopeCollision = errors.New("daemonpilot: the service is already installed in the other scope")

// crossScopeStat is the test seam for probing the other scope's manifest. It is
// Lstat, not Stat: a dangling symlink left at the manifest path is still a
// collision, and one init system would still load it if the target reappeared.
var crossScopeStat = os.Lstat

// crossScopeManifestPath returns the manifest path the given init system would
// use for this service in scope, or an empty string when the pair has no
// deterministic path.
//
// The path is derived from the manager itself rather than reconstructed here,
// so it cannot drift from the one the install actually writes.
//
// Two limitations are deliberate, and both make the check miss a collision
// rather than invent one. A user-scope path is built from this process's $HOME,
// so the system→user direction only sees the collision when the install is run
// from the account that owns the user-scope install; under sudo, $HOME is
// root's. And crontab, the third manager that supports user scope, is exempt:
// both of its scopes edit the invoking user's crontab and its manifest lives in
// InstallDir, so it has no scope-deterministic path to probe.
func crossScopeManifestPath(initSystem, name string, scope InstallScope) string {
	switch initSystem {
	case "systemd", "launchd":
	default:
		// Every other manager either supports system scope only, or — crontab —
		// writes the same place in both scopes, so there is no other-scope path
		// to collide with.
		return ""
	}

	mgr, err := newManagerByName(initSystem)
	if err != nil {
		return ""
	}
	if err := initManager(mgr, daemon.ManagerOptions{
		ServiceName: name, Scope: scope.daemonScope(),
	}); err != nil {
		return ""
	}
	return mgr.GetManifestPath()
}

// preflightCrossScope refuses an install when the same service is already
// installed in the opposite scope. Two supervisors racing over one binary name
// is the failure it prevents, and it is checked through the manifest path
// rather than the install directory because InstallDir may be set explicitly —
// a documented way to use the library — while the manifest path is
// deterministic from the service name in both scopes.
var preflightCrossScope = func(cfg *Config, initSystem string) error {
	other := ScopeSystem
	if cfg.Scope.normalized() == ScopeSystem {
		other = ScopeUser
	}

	path := crossScopeManifestPath(initSystem, cfg.Name, other)
	if path == "" {
		return nil
	}
	if _, err := crossScopeStat(path); err != nil {
		return nil
	}

	return fmt.Errorf("%w: %q is already installed in %s scope, with its %s manifest at %s; "+
		"installing it in %s scope too would leave two supervisors racing over the same binary; "+
		"uninstall the %s-scope installation first, or install under a different Config.Name",
		ErrScopeCollision, cfg.Name, other, initSystem, path,
		cfg.Scope.normalized(), other)
}

type probeTarget struct {
	label  string
	path   string
	probed string
	err    error
}

func (t probeTarget) describe() string {
	if t.probed != "" && t.probed != t.path {
		return fmt.Sprintf("%s %s is not writable (probed its nearest existing ancestor %s): %v",
			t.label, t.path, t.probed, t.err)
	}
	return fmt.Sprintf("%s %s is not writable: %v", t.label, t.path, t.err)
}

// describeTargets names every probed path, including the ones that succeeded,
// so the operator sees the full set the install depends on rather than only
// whichever one happened to fail first.
func describeTargets(targets []probeTarget) string {
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		parts = append(parts, fmt.Sprintf("%s=%s", t.label, t.path))
	}
	return strings.Join(parts, ", ")
}

// installPrivilegeRemedy words the way out of a failed probe. The euid is not
// consulted to decide anything here either: elevation is offered for a
// system-scope install because that is where it can help, and a user-scope
// install is told to fix the paths it owns.
func installPrivilegeRemedy(cfg *Config) string {
	if cfg.Scope.normalized() == ScopeUser {
		return "remedy: create the paths above or fix their ownership; " +
			"a user-scope install writes only under your home directory and never elevates"
	}

	remedy := "remedy: re-run the install with elevated privileges (for example under sudo), " +
		"or set Config.Scope = daemonpilot.ScopeUser to install under your own account"
	home, err := userHomeDir()
	if err != nil || home == "" {
		return remedy
	}
	return fmt.Sprintf("%s, which would install into %s with logs in %s",
		remedy,
		defaultInstallDir(runtime.GOOS, cfg.Name, ScopeUser, home),
		defaultLogDir(runtime.GOOS, cfg.Name, ScopeUser, home))
}

// probeDirWritable reports whether dir — or, when it does not exist yet, the
// nearest ancestor that does — accepts a new file, by creating and removing
// one. It returns the directory it actually probed.
func probeDirWritable(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("no directory configured")
	}

	probed, err := nearestExistingDir(dir)
	if err != nil {
		return "", err
	}

	f, err := os.CreateTemp(probed, ".daemonpilot-preflight-*")
	if err != nil {
		return probed, fmt.Errorf("creating a probe file: %w", err)
	}
	name := f.Name()
	closeErr := f.Close()
	if err := os.Remove(name); err != nil {
		return probed, fmt.Errorf("removing the probe file: %w", err)
	}
	if closeErr != nil {
		return probed, fmt.Errorf("closing the probe file: %w", closeErr)
	}
	return probed, nil
}

// nearestExistingDir walks up from dir until it finds a directory that exists.
// A missing install directory is normal — install creates it — so writability
// of the parent that will hold it is what matters.
func nearestExistingDir(dir string) (string, error) {
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("%s exists but is not a directory", dir)
			}
			return dir, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("inspecting %s: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no existing ancestor of %s", dir)
		}
		dir = parent
	}
}
