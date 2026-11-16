package daemonpilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ovineko/daemonpilot/internal/daemon"
	"github.com/ovineko/daemonpilot/internal/process"
	"github.com/ovineko/procguard"
)

var initSystemTypeFn = daemon.InitSystemType

var timeNow = time.Now

// instanceLockAttempts / instanceLockRetryDelay bound the retry when the
// single-instance lock is momentarily held by a departing previous instance.
const (
	instanceLockAttempts   = 5
	instanceLockRetryDelay = 200 * time.Millisecond
)

var (
	detectManager         = daemon.DetectServiceManager
	detectForScope        = daemon.DetectForScope
	newManagerByName      = daemon.NewServiceManagerByName
	initManager           = daemon.InitServiceManagerWithOptions
	getExecutable         = os.Executable
	mkdirAll              = os.MkdirAll
	statFileFn            = os.Stat
	lstatFileFn           = os.Lstat
	copyFileFn            = copyFile
	saveStateFn           = SaveState
	loadStateFn           = LoadState
	deleteStateFn         = DeleteState
	readInstalledConfigFn = func(mgr daemon.ServiceManager) (string, error) {
		return mgr.ReadInstalledConfig()
	}
	setupSignalHandler = process.SetupSignalHandler
	// runAsServiceFn wraps the supervision run in the Windows Service Control
	// Manager protocol, and is the identity on every other platform (and on
	// Windows when started from a console). A seam like the rest of these so a
	// handler test can drive the run path without an SCM.
	runAsServiceFn = process.RunAsService
	execSelfFn     = execSelf
	// acquireLockFn is the single-instance lock seam. Handler tests drive mock
	// managers over install directories that do not exist on the test host,
	// where the real lock could never be taken and every one of them would now
	// fail; the lock itself is covered in lock_test.go against real files.
	acquireLockFn        = acquireInstanceLock
	startParentMonitorFn = procguard.WatchParent
	newUpdaterFn         func(cfg *UpdateConfig, logger Logger, installDir, binaryName string) UpdaterFacade
	newWatchdogFn        func(cfg *Config) Runner
)

// Runner is the interface for components that can run with a context.
type Runner interface {
	Run(ctx context.Context) error
}

// UpdaterFacade is the interface for update management operations used by
// the run command handler to perform stale file cleanup and rollback handling.
type UpdaterFacade interface {
	CleanupStaleFiles() error
	CheckRollbackFile(ctx context.Context, binaryPath string) (bool, error)
	HandleRollback(ctx context.Context, rollbackPath string, rollbackTimeout time.Duration) error
}

// SetWatchdogFactory registers the factory function for creating watchdog
// instances. This is called by the setup package to wire internal
// implementations without creating circular imports.
func SetWatchdogFactory(fn func(cfg *Config) Runner) {
	newWatchdogFn = fn
}

// SetUpdaterFactory registers the factory function for creating updater
// facade instances. This is called by the setup package to wire internal
// implementations without creating circular imports.
func SetUpdaterFactory(fn func(cfg *UpdateConfig, logger Logger, installDir, binaryName string) UpdaterFacade) {
	newUpdaterFn = fn
}

// prepareManager detects the init system that can serve opts.Scope and binds
// the resulting manager to the service described by opts, the sequence every
// lifecycle handler starts with.
//
// Detection is scope-aware so a user-scope install on a host whose init system
// has no per-user instance fails outright rather than silently registering a
// machine-wide service and recording it as user-scope. For ScopeSystem every
// manager qualifies, so this behaves exactly as unfiltered detection did.
func prepareManager(opts daemon.ManagerOptions) (daemon.ServiceManager, error) {
	det, err := detectForScope(opts.Scope)
	if err != nil {
		return nil, fmt.Errorf("detecting service manager: %w", err)
	}
	if err := initManager(det.Manager, opts); err != nil {
		return nil, fmt.Errorf("initializing service manager: %w", err)
	}
	return det.Manager, nil
}

// prepareInstalledManager loads the state file and binds the manager the
// install actually recorded, rather than whatever the current session detects.
// A session that cannot see the installing session's init system — `sudo -u`
// without a logind session, for one — would otherwise select a different
// manager and report success for work it never did.
//
// Without a state file it falls back to detection, so commands still work for a
// service registered outside daemonpilot. A state file that exists but cannot be
// read or decoded is an error, not an absence: these handlers mutate the
// service, and guessing the init system is how a start/enable ends up acting on
// a manager the install never used. Only status and uninstall — which read, or
// must stay able to clean up — downgrade an unreadable file to a warning.
func prepareInstalledManager(cfg *Config, opts daemon.ManagerOptions) (daemon.ServiceManager, error) {
	state, err := loadInstalledState(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w; repair or remove %s, or reinstall the service",
			err, StateFilePath(cfg.InstallDir, cfg.Name))
	}
	return managerForState(cfg, state, opts)
}

// loadInstalledState reads the state file, returning nil when the service has
// no state file. Use it only where the state file is genuinely required — the
// drift checks behind validate and update-config — so an unreadable file is a
// diagnosis rather than a silent skip.
//
// A state file that records no install is reported as absent: RecordRollbackDigest
// writes one for a service that was never installed through daemonpilot, and it
// carries none of the fields these callers bind to.
func loadInstalledState(cfg *Config) (*StateData, error) {
	state, err := loadStateFn(cfg.InstallDir, cfg.Name)
	if err != nil {
		return nil, fmt.Errorf("loading state: %w", err)
	}
	if !state.recordsAnInstall() {
		return nil, nil //nolint:nilnil // documented contract: no install record means no state
	}
	return state, nil
}

// readableState loads the state file for commands that only use it to sharpen
// their behavior. A state file that cannot be read is downgraded to a warning
// and treated as absent: it is written 0600, so `status` run by a user other
// than the installer hits EACCES, and a truncated file would otherwise make
// `uninstall` refuse — leaving the artifacts it is meant to remove in place.
func readableState(cfg *Config) *StateData {
	state, err := loadStateFn(cfg.InstallDir, cfg.Name)
	if err != nil {
		cfg.Logger.Warn("failed to read the state file, falling back to init system detection",
			"path", StateFilePath(cfg.InstallDir, cfg.Name), "error", err.Error())
		return nil
	}
	if !state.recordsAnInstall() {
		return nil
	}
	return state
}

// managerForState binds the manager recorded in state, falling back to
// detection when there is no usable state.
func managerForState(cfg *Config, state *StateData, opts daemon.ManagerOptions) (daemon.ServiceManager, error) {
	opts.Scope = cfg.Scope.daemonScope()
	// The log directory travels with the manager so the paths that rewrite a
	// manifest without a ServiceConfig — WriteConfig, and the rollback that
	// follows a failed update-config — can still create what they redirect into.
	opts.LogDir = cfg.LogDir

	if state == nil || state.InitSystem == "" {
		return prepareManager(opts)
	}

	// Pin the domain the install resolved, so launchctl acts on the domain the
	// agent was actually bootstrapped into rather than on whatever this session
	// would resolve to now.
	opts.LaunchdDomain = state.LaunchdDomain

	if installed := state.EffectiveScope(); installed != cfg.Scope.normalized() {
		return nil, fmt.Errorf(
			"service %q was installed in %s scope but this command is configured for %s scope; run it with the scope it was installed in",
			cfg.Name, installed, cfg.Scope.normalized())
	}

	mgr, err := newManagerByName(state.InitSystem)
	if err != nil {
		return nil, fmt.Errorf("service manager for %q: %w", state.InitSystem, err)
	}
	if err := initManager(mgr, opts); err != nil {
		return nil, fmt.Errorf("initializing service manager: %w", err)
	}
	return mgr, nil
}

// manifestPathForState returns the manifest path a lifecycle command should act
// on. It is the manager's own path, not the one recorded in the state file:
// install hands the state file to Config.ServiceUser so the staged updater can
// record a rollback digest, so a recorded path is a value an unprivileged
// account can rewrite, and update-config would then have root create and remove
// "<path>.backup" wherever it pointed. The manager derives the path from the
// service name, scope and install directory instead. A divergence is reported
// rather than silently preferred, since it also means the state file is stale.
//
// The manager's path can still be empty — a manager that keeps no manifest —
// in which case the recorded value is the only thing left to fall back on.
func manifestPathForState(cfg *Config, mgr daemon.ServiceManager, state *StateData) string {
	path := mgr.GetManifestPath()
	if path == "" {
		return state.ManifestPath
	}
	if state.ManifestPath != "" && state.ManifestPath != path {
		cfg.Logger.Warn("recorded manifest path differs from the one this init system resolves; using the resolved path",
			"recorded", state.ManifestPath, "resolved", path)
	}
	return path
}

func detectInitSystemType(mgr daemon.ServiceManager) string {
	return initSystemTypeFn(mgr)
}

// checkInitSystemDrift fails when the host no longer runs the init system the
// install recorded, so the operator is told to reinstall instead of being shown
// a confusing diff against a manifest of the wrong format.
//
// The comparison must use a fresh detection: the manager passed to lifecycle
// commands is constructed *from* state.InitSystem, so comparing its name with
// the recorded one can never differ. Detection keys off the host init system
// before scope filtering, which is what "the host changed" means.
//
// A detection failure is not drift. It is reported as a warning and the check is
// skipped, so a session that cannot reach the init system still validates the
// manifest it can read.
func checkInitSystemDrift(cfg *Config, state *StateData) error {
	det, err := detectForScope(cfg.Scope.daemonScope())
	if err != nil {
		cfg.Logger.Warn("failed to detect the host init system, skipping the drift check",
			"error", err.Error())
		return nil
	}

	host := det.HostInitSystem
	if host == "" {
		host = managerInitSystemType(det.Manager)
	}
	if host == state.InitSystem {
		return nil
	}
	// The recorded init system names the manager the install *selected*, which
	// is not the host's own init system whenever scope filtering fell back —
	// a user-scope install on OpenRC or runit is served by crontab. Detection
	// selecting that same fallback again is not drift: the manifest still has
	// the format the recorded manager writes.
	if selected := managerInitSystemType(det.Manager); selected == state.InitSystem {
		return nil
	}
	return fmt.Errorf("init system changed from %q to %q; uninstall and reinstall the service", state.InitSystem, host)
}

// managerInitSystemType is detectInitSystemType for a manager that may be nil:
// Detection carries a nil Manager whenever detection failed part-way.
func managerInitSystemType(mgr daemon.ServiceManager) string {
	if mgr == nil {
		return ""
	}
	return detectInitSystemType(mgr)
}

func installWithPatchers(mgr daemon.ServiceManager, svcConfig daemon.ServiceConfig, patchers []ConfigPatcher) error {
	if len(patchers) == 0 {
		if err := mgr.Install(svcConfig); err != nil {
			return fmt.Errorf("%s: %w", svcConfig.Name, err)
		}
		return nil
	}

	content, err := mgr.GenerateConfig(svcConfig)
	if err != nil {
		return fmt.Errorf("generating config: %w", err)
	}

	info := PatchInfo{
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		InitSystem:  detectInitSystemType(mgr),
		ServiceName: svcConfig.Name,
		FilePath:    mgr.GetManifestPath(),
	}

	for i, patcher := range patchers {
		content, err = patcher(mgr.GetManifestPath(), content, info)
		if err != nil {
			return fmt.Errorf("applying config patcher %d: %w", i, err)
		}
	}

	if err := mgr.WriteConfig(content); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	if ew, ok := mgr.(daemon.EnvironmentFileWriter); ok {
		if err := ew.WriteEnvironmentFile(svcConfig.Environment); err != nil {
			_ = ew.WriteEnvironmentFile(nil)
			_ = removeFileFn(mgr.GetManifestPath())
			return fmt.Errorf("writing environment file: %w", err)
		}
	}

	return nil
}

// requireLingering gates a user-scope install on the invoking user having
// lingering enabled, and reports the state for the state file.
//
// Without lingering the per-user manager is stopped at logout and is not
// started at boot, so the service quietly ceases to exist — precisely the
// guarantee a supervisor is installed to provide. A warning is not enough: run
// from CI or Ansible, stderr goes nowhere and the operator learns about it only
// when the service is already gone. AllowNoLinger is the deliberate opt-out,
// for a session-scoped service that is meant to end with the session.
//
// Lingering is per-user, not per-service, so enabling it is left to the
// operator: doing it here would silently mutate account state.
func requireLingering(cfg *Config, mgr daemon.ServiceManager) (bool, error) {
	if cfg.Scope.normalized() != ScopeUser {
		return false, nil
	}
	checker, ok := mgr.(daemon.LingerChecker)
	if !ok {
		return false, nil
	}

	enabled, userName, err := checker.LingeringEnabled()
	if err != nil {
		return false, fmt.Errorf("user-scope install of %q cannot verify lingering: %w", cfg.Name, err)
	}
	if enabled {
		return true, nil
	}
	if cfg.AllowNoLinger {
		cfg.Logger.Warn("lingering is disabled: the service will stop at logout and will not start at boot",
			"user", userName, "remedy", "loginctl enable-linger "+userName)
		return false, nil
	}
	// The alternative — installing a service that ends with the session — is
	// the application's to offer in its own words: AllowNoLinger is a field
	// of this library, not something its users can act on.
	return false, fmt.Errorf(
		"%w: user-scope install of %q needs lingering, which is disabled for user %s; "+
			"without it systemd stops the service at logout and does not start it at boot; "+
			"run: loginctl enable-linger %s",
		ErrLingerRequired, cfg.Name, userName, userName)
}

// verifyRegistration asks the init system where it believes the manifest lives
// and fails the install when that is not where it was written. Managers that
// cannot answer are skipped.
func verifyRegistration(cfg *Config, mgr daemon.ServiceManager) error {
	v, ok := mgr.(daemon.RegistrationVerifier)
	if !ok {
		return nil
	}
	if err := v.VerifyRegistered(); err != nil {
		return fmt.Errorf("%s: %w", cfg.Name, err)
	}
	return nil
}

// copyBinaryToInstallDir places the running executable at destPath, unless it
// is already the same file — reinstalling over a running service must not copy
// a binary onto itself. It reports whether the copy created the destination,
// which is what decides whether a later failure may remove it again.
func copyBinaryToInstallDir(cfg *Config, execPath, destPath string) (bool, error) {
	absExec, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		absExec, err = filepath.Abs(execPath)
		if err != nil {
			return false, fmt.Errorf("resolving executable path: %w", err)
		}
	}
	absDest, err := filepath.EvalSymlinks(destPath)
	if err != nil {
		absDest, err = filepath.Abs(destPath)
		if err != nil {
			return false, fmt.Errorf("resolving destination path: %w", err)
		}
	}

	if absExec == absDest || isSameFile(execPath, destPath) {
		return false, nil
	}

	if err := mkdirAll(cfg.InstallDir, 0o755); err != nil {
		return false, fmt.Errorf("creating install directory: %w", err)
	}
	// Stat before the copy, not after: overwriting an existing binary is a
	// reinstall, and a failure later must not delete the file the previous
	// install left behind and its state file still points at. Only a definite
	// ErrNotExist counts as absence — an inconclusive stat (EACCES on a
	// restricted parent, EIO) must read as "it was already there", since the
	// unwind that follows deletes the binary and removes the registration.
	_, statErr := statFileFn(destPath)
	existed := !errors.Is(statErr, fs.ErrNotExist)
	if err := copyFileFn(execPath, destPath); err != nil {
		return false, fmt.Errorf("copying binary: %w", err)
	}
	cfg.Logger.Info("copied binary to install directory", "dest", destPath)
	return !existed, nil
}

// instanceLockPath is the single-instance lock the run command takes.
func instanceLockPath(cfg *Config) string {
	return filepath.Join(cfg.InstallDir, cfg.Name+".lock")
}

// lockOwner is the account install handed the lock file to, as far as this
// process can tell. uid is -1 when there is no service account at all; when
// unresolved is set there is one but os/user could not resolve it, which the
// lock refuses to act on rather than guess.
type lockOwner struct {
	uid        int
	name       string
	unresolved bool
}

var (
	chownFn      = chownFileNoFollow
	lookupUserFn = user.Lookup
)

// ensureServiceUserLockFile hands the single-instance lock file to
// Config.ServiceUser. The install directory is created by root and stays
// root-owned, so an unprivileged service account cannot create a file in it —
// and run refuses to start at all without the lock, which under Restart=always
// with StartLimitIntervalSec=0 is an unbounded crash loop rather than a
// diagnosis. Creating the file here, owned by the account that will need it,
// is the same reasoning as chowning a system-scope launchd log directory to the
// UserName the job drops to.
//
// It reports whether this call created the file, so a failed install can unwind
// it. Chown does not follow symlinks: the file was just created with O_EXCL, but
// the directory may be group-writable.
func ensureServiceUserLockFile(cfg *Config) (bool, error) {
	if cfg.ServiceUser == "" {
		return false, nil
	}

	uid, gid, err := lookupServiceOwner(cfg)
	if err != nil {
		// os/user resolves through /etc/passwd alone in a CGO_ENABLED=0 build,
		// so an account that systemd resolves through NSS (sssd, LDAP,
		// systemd-userdbd) is unresolvable here — as is the common packaging
		// order that creates the account in a postinstall step. Before this
		// check existed the install succeeded and wrote ServiceUser verbatim
		// into the unit file, so refusing now would fail installs that used to
		// work. Warn instead: the lock file stays root-owned, which is exactly
		// the state every release before this one shipped.
		cfg.Logger.Warn("could not resolve the service account; leaving the single-instance lock file to root",
			"user", cfg.ServiceUser, "error", err.Error(),
			"consequence", "the service will not be able to take its lock until the account exists and install is re-run")
		return false, nil
	}

	lockPath := instanceLockPath(cfg)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600) //nolint:gosec // fixed path in our InstallDir
	created := err == nil
	switch {
	case created:
		_ = f.Close()
	case errors.Is(err, fs.ErrExist):
		// A reinstall over a lock file the previous install left behind, or one
		// the running service is holding right now. Re-chown it rather than
		// replace it: unlinking would detach the inode the live instance holds.
		//
		// O_EXCL also reports EEXIST for a symlink at the path, which run
		// refuses outright with O_NOFOLLOW. Since every lock error is fatal
		// there, accepting it here would report a green install that then
		// crash-loops on every start — the failure this whole path prevents.
		if info, statErr := lstatFileFn(lockPath); statErr == nil && info.Mode()&fs.ModeSymlink != 0 {
			return false, fmt.Errorf(
				"the single-instance lock path %s is a symbolic link; refusing to hand it to user %q: "+
					"the service will refuse to follow it at startup — remove it and reinstall",
				lockPath, cfg.ServiceUser)
		}
	default:
		return false, fmt.Errorf("creating the single-instance lock file %s for user %q: %w",
			lockPath, cfg.ServiceUser, err)
	}

	if err := chownFn(lockPath, uid, gid); err != nil {
		return created, fmt.Errorf("giving the single-instance lock file %s to user %q: %w",
			lockPath, cfg.ServiceUser, err)
	}
	cfg.Logger.Info("single-instance lock file assigned to the service account",
		"path", lockPath, "user", cfg.ServiceUser)
	return created, nil
}

// chownStateFileToServiceUser hands the state file to Config.ServiceUser, for
// the same reason ensureServiceUserLockFile hands over the lock file: root
// creates it 0600 in a root-owned install directory, and the unprivileged
// worker has to write it. A staged update records the rollback digest there
// before it displaces the binary, and refuses to proceed without one — so a
// state file the service account cannot write is a self-update that aborts on
// every poll, and a rollback copy that reads back as unverifiable and gets
// quarantined.
func chownStateFileToServiceUser(cfg *Config) error {
	if cfg.ServiceUser == "" {
		return nil
	}
	uid, gid, err := lookupServiceOwner(cfg)
	if err != nil {
		// Same reasoning as ensureServiceUserLockFile: an account this build
		// cannot resolve is not a reason to fail an install that used to work.
		cfg.Logger.Warn("could not resolve the service account; leaving the state file to root",
			"user", cfg.ServiceUser, "error", err.Error(),
			"consequence", "self-update cannot record a rollback digest until the account exists and install is re-run")
		return nil
	}
	path := StateFilePath(cfg.InstallDir, cfg.Name)
	if err := chownFn(path, uid, gid); err != nil {
		// The service is registered and running by now, so this is reported as a
		// completed install with a degraded capability rather than as a failure
		// that leaves the operator guessing what still exists on disk.
		return fmt.Errorf("service %q was installed, but its state file %s could not be given to user %q: %w; "+
			"self-update will not be able to record a rollback digest — fix its ownership and re-run install",
			cfg.Name, path, cfg.ServiceUser, err)
	}
	cfg.Logger.Info("state file assigned to the service account", "path", path, "user", cfg.ServiceUser)
	return nil
}

// takeInstanceLock refuses to start a second watchdog over the same InstallDir
// (e.g. a manual run racing the init-supervised service), which would corrupt
// the shared on-disk state. Contention and a path-specific failure stay fatal.
// A host measured to be incapable of locking in this directory continues in a
// visible degraded state because refusing it cannot exclude another instance.
func takeInstanceLock(cfg *Config, logger Logger) (func(), error) {
	cfg.LockStatus = platformLockStatus()
	// The lock file has to be created in InstallDir, and nothing on the run path
	// creates that directory — a run before the first install, or a binary
	// started from a directory the packaging never made, would now fail fatally
	// where it used to warn and continue.

	if err := mkdirAll(cfg.InstallDir, 0o755); err != nil {
		logger.Warn("could not create the install directory before locking",
			"install_dir", cfg.InstallDir, "error", err.Error())
	}

	lockPath := instanceLockPath(cfg)
	var releaseLock func()
	var lockErr error
	// Retry briefly: a departing previous instance may still hold the lock
	// during teardown after an init-system restart. A genuine second instance
	// keeps failing and is refused after the short window.
	for range instanceLockAttempts {
		releaseLock, lockErr = acquireLockFn(lockPath, serviceUserUID(cfg))
		if !errors.Is(lockErr, ErrAlreadyRunning) {
			break
		}
		time.Sleep(instanceLockRetryDelay)
	}
	if errors.Is(lockErr, ErrAlreadyRunning) {
		logger.Error("another instance is already running for this install dir; refusing to start",
			"install_dir", cfg.InstallDir)
		return nil, lockErr
	}
	if errors.Is(lockErr, ErrLockUnavailable) {
		cfg.LockStatus = LockStatusUnlocked
		var unavailable *lockUnavailableError
		if errors.As(lockErr, &unavailable) {
			logger.Error("single-instance locking is unavailable; continuing without the lock",
				"path", lockPath, "error", unavailable.cause.Error(), "probe_error", unavailable.probe.Error())
		} else {
			logger.Error("single-instance locking is unavailable; continuing without the lock",
				"path", lockPath, "error", lockErr.Error(), "probe_error", "unavailable classification did not include probe details")
		}
		return func() {}, nil
	}
	if lockErr != nil {
		// A permission error is the upgrade path, not an attack: an install made
		// before the lock file existed has a root-owned install directory and an
		// unprivileged ServiceUser, and a binary replaced out of band — a
		// package, a config-management run, a manual copy — never gets the file
		// install would have created and chowned. Under Restart=always that is
		// an unbounded crash loop, so the error has to name the way out.
		remedy := ""
		if errors.Is(lockErr, fs.ErrPermission) {
			remedy = fmt.Sprintf("; re-run install so it creates %s and hands it to the service account", lockPath)
		}
		logger.Error("could not acquire the single-instance lock; refusing to start without it",
			"lock_path", lockPath, "error", lockErr.Error())
		return nil, fmt.Errorf("single-instance lock: %w%s", lockErr, remedy)
	}
	return releaseLock, nil
}

// serviceUserUID resolves Config.ServiceUser into the answer to "who is the
// lock file allowed to belong to besides me": install chowns that file to the
// service account, so a root invocation must not read it as a foreign account's
// squatted file.
//
// An unresolvable name is reported as such rather than flattened to "no service
// account". The two are opposite instructions to the root replacement path: no
// service account means a foreign lock file really is a squatter, while an
// account this build cannot resolve — a CGO_ENABLED=0 binary against an
// NSS-provided account — is most likely the legitimate owner, and recreating
// its file root-owned 0600 is the unbounded crash loop the chown at install
// exists to prevent.
func serviceUserUID(cfg *Config) lockOwner {
	if cfg.ServiceUser == "" {
		return lockOwner{uid: -1}
	}
	u, err := lookupUserFn(cfg.ServiceUser)
	if err != nil {
		return lockOwner{uid: -1, unresolved: true, name: cfg.ServiceUser}
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return lockOwner{uid: -1, unresolved: true, name: cfg.ServiceUser}
	}
	return lockOwner{uid: uid, name: cfg.ServiceUser}
}

// lookupServiceOwner resolves Config.ServiceUser and Config.ServiceGroup to ids,
// returning -1 for an unset group — the value chown reads as "leave unchanged".
func lookupServiceOwner(cfg *Config) (uid int, gid int, err error) {
	u, err := lookupUserFn(cfg.ServiceUser)
	if err != nil {
		return 0, 0, fmt.Errorf("looking up Config.ServiceUser %q: %w", cfg.ServiceUser, err)
	}
	if uid, err = strconv.Atoi(u.Uid); err != nil {
		return 0, 0, fmt.Errorf("parsing the uid of user %q: %w", cfg.ServiceUser, err)
	}
	gid = -1
	if cfg.ServiceGroup != "" {
		g, lookupErr := user.LookupGroup(cfg.ServiceGroup)
		if lookupErr != nil {
			return 0, 0, fmt.Errorf("looking up Config.ServiceGroup %q: %w", cfg.ServiceGroup, lookupErr)
		}
		if gid, err = strconv.Atoi(g.Gid); err != nil {
			return 0, 0, fmt.Errorf("parsing the gid of group %q: %w", cfg.ServiceGroup, err)
		}
	}
	return uid, gid, nil
}

// unwindPartialInstall removes what this install put in the install directory
// when a later step failed before the state file was written, and reports the
// cleanup outcome alongside the original error rather than in place of it. Only
// artifacts this install created are touched; ones it found are left to the
// install being replaced.
func unwindPartialInstall(
	cfg *Config, mgr daemon.ServiceManager, destPath string, registered, createdBinary, createdLock bool, cause error,
) error {
	// Undo the registration first, while the binary it names is still there.
	// Removing the copy and leaving the manifest is the worse of the two
	// half-states: launchd auto-loads every plist under ~/Library/LaunchAgents
	// at the next login, so the failed install comes back on its own, spawns a
	// path that no longer exists, and no state file records it.
	//
	// That reasoning only holds when the copy is going away too. On a reinstall
	// the binary was already there and stays, so the manifest this install just
	// wrote still names a working executable: removing it would delete a live
	// service's registration over a transient enable or verification failure,
	// while the untouched state file goes on pointing at it. The manager's own
	// manifest rollback is the only unwind that belongs to that case.
	if registered && createdBinary {
		if err := mgr.Uninstall(); err != nil {
			cfg.Logger.Warn("failed to remove the service registration created by the failed install",
				"path", mgr.GetManifestPath(), "error", err.Error())
		} else {
			cfg.Logger.Info("removed the service registration created by the failed install",
				"path", mgr.GetManifestPath())
		}
	}
	if createdLock {
		lockPath := instanceLockPath(cfg)
		if err := removeFileFn(lockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			cfg.Logger.Warn("failed to remove the lock file created by the failed install",
				"path", lockPath, "error", err.Error())
		}
	}
	return cleanupCopiedBinary(cfg, destPath, createdBinary, cause)
}

// cleanupCopiedBinary removes a binary this install copied when a later step
// failed before the state file was written. Without it the copy is an orphan:
// no state file records it, so uninstall reports the service as not installed
// and refuses. A binary that already existed is left alone — it belongs to the
// install being replaced. The cleanup outcome is reported alongside the
// original error rather than in place of it.
func cleanupCopiedBinary(cfg *Config, destPath string, created bool, cause error) error {
	if !created {
		return cause
	}
	if err := removeFileFn(destPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w; the binary copied to %s could not be removed: %w", cause, destPath, err)
	}
	cfg.Logger.Info("removed the binary copied by the failed install", "path", destPath)
	return cause
}

// gateInstallBeforeCopy runs every check that must happen while the install has
// still mutated nothing, and returns the path of the running binary to copy.
func gateInstallBeforeCopy(cfg *Config, mgr daemon.ServiceManager) (string, error) {
	// The manager refuses the values cron acts on too, but only from Install,
	// which runs after the copy — and the failed install writes no manifest and
	// no state file, so uninstall then reports the service as not installed and
	// the copy is orphaned.
	if mgr.Name() == "crontab" {
		if err := cfg.validateCrontabFields(); err != nil {
			return "", err
		}
	}

	execPath, err := getExecutable()
	if err != nil {
		return "", fmt.Errorf("getting executable path: %w", err)
	}

	// Probe every directory the install writes before touching any of them, so
	// a permission failure names the path and the way out instead of surfacing
	// a raw EACCES from the middle of the sequence, with the binary already
	// copied and nothing recording it.
	if err := preflightInstall(cfg, mgr.GetManifestPath()); err != nil {
		return "", err
	}

	// The same service installed in both scopes leaves two supervisors racing
	// over one binary name, so refuse before anything is written.
	if err := preflightCrossScope(cfg, mgr.Name()); err != nil {
		return "", err
	}
	return execPath, nil
}

// handleInstall copies the binary to the install directory, registers the
// service with the OS init system, enables auto-start on boot, and starts
// the service immediately.
func handleInstall(cfg *Config) error {
	logger := cfg.Logger
	logger.Info("installation started", "phase", "install", "step", "begin", "outcome", "started",
		"service_name", cfg.Name, "install_dir", cfg.InstallDir, "scope", cfg.Scope.normalized().String(),
		"version", cfg.Version, "watchdog_version", cfg.WatchdogVersion)

	// Install is the one command that must detect rather than bind what state
	// records — the recorded init system may be the thing being replaced. The
	// launchd domain is the exception: a re-install resolves its own domain,
	// but it has to know which one the previous install bootstrapped into so it
	// can boot that agent out instead of orphaning it in a domain this session
	// no longer resolves to.
	previousState := readableState(cfg)
	previousDomain := ""
	if previousState != nil {
		previousDomain = previousState.LaunchdDomain
	}

	// InstallDir has to be bound here, not left to Install: crontabManager
	// derives its manifest path from it, and without it GetManifestPath returns
	// a bare relative name that gateInstallBeforeCopy would resolve — and probe
	// for writability — against the CLI's working directory.
	mgr, err := prepareManager(daemon.ManagerOptions{
		ServiceName: cfg.Name, Scope: cfg.Scope.daemonScope(), LogDir: cfg.LogDir,
		InstallDir:            cfg.InstallDir,
		PreviousLaunchdDomain: previousDomain,
	})
	if err != nil {
		logger.Error("installation stopped during init-system detection", "phase", "install", "step", "detect_init_system",
			"outcome", "failed", "service_name", cfg.Name, "scope", cfg.Scope.normalized().String(), "error", err.Error())
		return err
	}
	logger.Info("init system selected for installation", "phase", "install", "step", "detect_init_system",
		"outcome", "succeeded", "service_name", cfg.Name, "scope", cfg.Scope.normalized().String(), "init_system", mgr.Name())

	// Gate before copying anything: an install that cannot survive logout is
	// worse than no install, and refusing early leaves nothing to clean up.
	lingering, err := requireLingering(cfg, mgr)
	if err != nil {
		return err
	}

	execPath, err := gateInstallBeforeCopy(cfg, mgr)
	if err != nil {
		logger.Error("installation preflight failed; no binary was copied", "phase", "install", "step", "preflight",
			"outcome", "failed", "service_name", cfg.Name, "install_dir", cfg.InstallDir, "init_system", mgr.Name(), "error", err.Error())
		return err
	}
	logger.Info("installation preflight completed", "phase", "install", "step", "preflight", "outcome", "succeeded",
		"service_name", cfg.Name, "install_dir", cfg.InstallDir, "init_system", mgr.Name())

	destPath := installedBinaryPath(cfg)
	createdBinary, err := copyBinaryToInstallDir(cfg, execPath, destPath)
	if err != nil {
		return err
	}

	// From here to the state file write, the copy is on disk with nothing
	// recording it, so every failure unwinds it.
	createdLock := false
	registered := false
	failed := func(err error) error {
		return unwindPartialInstall(cfg, mgr, destPath, registered, createdBinary, createdLock, err)
	}

	// run refuses to start without the single-instance lock, so an unprivileged
	// ServiceUser that cannot create it in the root-owned install directory
	// would crash-loop instead of starting. Give it the file now, while the
	// install still has the privileges to do so.
	createdLock, err = ensureServiceUserLockFile(cfg)
	if err != nil {
		return failed(err)
	}

	// Writability says nothing about executability: a noexec install directory
	// passes every write probe and then fails to spawn on every restart. Run the
	// binary from its destination once, before the init system is told about it.
	if err := preflightExec(cfg, destPath); err != nil {
		return failed(err)
	}

	svcConfig := buildServiceConfig(cfg)

	// The detection logger is off by default, so the caveat is repeated here to
	// reach the operator running the install.
	if mgr.Name() == "crontab" {
		if caveat := daemon.CrontabRebootScheduleCaveat(cfg.CrontabSchedule); caveat != "" {
			logger.Warn(caveat, "name", cfg.Name)
		}
	}

	// A failed Install is not unwound here. launchd and crontab roll their own
	// manifest back (launchd restores and re-bootstraps the plist it overwrote,
	// crontab the entry manifest), and calling Uninstall on top of that would
	// remove the registration belonging to the install being replaced. The other
	// managers may leave the manifest they wrote before failing — a systemd unit
	// file whose daemon-reload failed, say — pointing at a binary the unwind
	// below deletes. That is recoverable rather than dangerous: the service is
	// neither enabled nor started, and handleUninstall falls back to
	// manifestExists when there is no state file, so uninstall still clears it.
	if err := installWithPatchers(mgr, svcConfig, cfg.ConfigPatchers); err != nil {
		return failed(err)
	}
	registered = true
	logger.Info("service manifest created", "phase", "install", "step", "register", "outcome", "succeeded",
		"service_name", cfg.Name, "init_system", mgr.Name(), "path", mgr.GetManifestPath())

	if err := verifyRegistration(cfg, mgr); err != nil {
		return failed(err)
	}

	if mgr.SupportsEnable() {
		if err := mgr.Enable(); err != nil {
			return failed(fmt.Errorf("%s: %w", cfg.Name, err))
		}
		logger.Info("service enabled for auto-start on boot", "phase", "install", "step", "enable", "outcome", "succeeded",
			"service_name", cfg.Name, "init_system", mgr.Name())
	} else {
		logger.Info("init system does not support a separate enable step", "phase", "install", "step", "enable",
			"outcome", "skipped", "service_name", cfg.Name, "init_system", mgr.Name())
	}

	// A start failure is REPORTED, not swallowed. It used to be a warning followed
	// by "installation complete … outcome=succeeded", on the reasoning that the
	// service can be started by hand — but nobody starts it by hand, because
	// nothing told them to: the installer printed success. That combination hid
	// three separate Windows faults in a row, each one a node that enrolled, held
	// an agent row and could never run.
	//
	// It is carried to the END rather than returned here, so the state file is
	// still written and the install stays bindable: uninstall, start and status all
	// work afterwards. Same shape as the state-file failure below — complete the
	// work, then say it did not come up.
	var startErr error
	if mgr.SupportsStart() {
		logger.Info("starting service after installation", "phase", "install", "step", "start", "outcome", "started",
			"service_name", cfg.Name, "init_system", mgr.Name())
		if err := mgr.Start(); err != nil {
			startErr = err
			logger.Error("failed to start service after install",
				"phase", "install", "step", "start", "outcome", "failed", "service_name", cfg.Name,
				"init_system", mgr.Name(), "error", err.Error())
		} else {
			logger.Info("service started successfully", "phase", "install", "step", "start", "outcome", "succeeded",
				"service_name", cfg.Name, "init_system", mgr.Name())
		}
	} else {
		logger.Info("init system does not support a separate start step", "phase", "install", "step", "start",
			"outcome", "skipped", "service_name", cfg.Name, "init_system", mgr.Name())
	}

	state := buildInstallState(cfg, mgr, recordedRollbackDigest(cfg), destPath, lingering)
	// The state file is not a record of the install, it is what every later
	// command binds to: the init system to act through, the scope it was
	// installed in, and — for launchd user scope — the domain the agent was
	// bootstrapped into. Without it, start/stop/status fall back to detection and
	// may address a different init system or an empty launchd domain, so an
	// install that cannot write it is reported as failed rather than succeeded.
	// The manifest is left in place: uninstall works without a state file.
	if err := saveStateFn(cfg.InstallDir, state); err != nil {
		return fmt.Errorf(
			"service %q was registered with %s but its state file could not be written: %w; "+
				"later commands cannot bind to the installed service — fix %s and reinstall, or run uninstall",
			cfg.Name, state.InitSystem, err, StateFilePath(cfg.InstallDir, cfg.Name))
	}
	if err := chownStateFileToServiceUser(cfg); err != nil {
		return err
	}

	if startErr != nil {
		return fmt.Errorf(
			"service %q was installed with %s but did not start: %w; "+
				"it is registered and enabled, so `%s start` will retry once the cause is fixed",
			cfg.Name, mgr.Name(), startErr, cfg.Name)
	}

	logger.Info("installation complete", "phase", "install", "step", "complete", "outcome", "succeeded",
		"service_name", cfg.Name, "install_dir", cfg.InstallDir, "scope", cfg.Scope.normalized().String(), "init_system", mgr.Name())
	return nil
}

// recordedRollbackDigest reads the rollback digest currently on disk, ignoring
// everything else in the state file. It deliberately goes to loadStateFn rather
// than readableState: RecordRollbackDigest writes a digest-only state file for a
// service that was never installed through daemonpilot, which readableState reports
// as absent — carrying the digest over is the one thing a caller must still do
// with such a file, since the .rollback copy it describes is live on disk.
// An unreadable file yields no digest, which is the same answer as no file.
func recordedRollbackDigest(cfg *Config) string {
	state, err := loadStateFn(cfg.InstallDir, cfg.Name)
	if err != nil || state == nil {
		return ""
	}
	return state.RollbackDigest
}

// buildInstallState assembles the state record the install writes.
// rollbackDigest is the digest recorded on disk for a staged update still on
// probation, or empty when there is none.
func buildInstallState(cfg *Config, mgr daemon.ServiceManager, rollbackDigest string,
	destPath string, lingering bool,
) *StateData {
	state := &StateData{
		ServiceName:  cfg.Name,
		InitSystem:   detectInitSystemType(mgr),
		ManifestPath: mgr.GetManifestPath(),
		BinaryPath:   destPath,
		InstalledAt:  timeNow(),
		Version:      cfg.Version,
	}
	// A reinstall over a staged update on probation must not drop the digest:
	// the .rollback copy stays on disk, and without its digest the next start
	// reads it as unverifiable, quarantines it and fires OnCriticalFailure —
	// destroying a valid safety net because of an unrelated install.
	state.RollbackDigest = rollbackDigest
	// System scope is encoded as the absence of these fields, keeping the state
	// file byte-identical to the one written before scopes existed.
	if cfg.Scope.normalized() == ScopeUser {
		state.Scope = ScopeUser
		state.InstallDir = cfg.InstallDir
		state.Lingering = lingering
		if reporter, ok := mgr.(daemon.DomainReporter); ok {
			state.LaunchdDomain = reporter.ResolvedDomain()
		}
	}
	return state
}

// manifestExists reports whether the manager's manifest is actually on disk.
// A manager that reports no path answers false rather than stat("").
func manifestExists(mgr daemon.ServiceManager) bool {
	path := mgr.GetManifestPath()
	if path == "" {
		return false
	}
	_, err := statFileFn(path)
	return err == nil
}

// handleUninstall stops the running service and then removes it from the OS
// init system. If the service cannot be stopped, uninstall proceeds with a
// warning to avoid leaving partial state.
func handleUninstall(cfg *Config) error {
	logger := cfg.Logger
	logger.Info("uninstallation started", "phase", "uninstall", "step", "begin", "outcome", "started",
		"service_name", cfg.Name, "install_dir", cfg.InstallDir, "scope", cfg.Scope.normalized().String())

	state := readableState(cfg)

	mgr, err := managerForState(cfg, state, daemon.ManagerOptions{ServiceName: cfg.Name, InstallDir: cfg.InstallDir})
	if err != nil {
		return err
	}

	// No state file does not mean nothing was installed: an install that failed
	// after writing the manifest — registration verification, enable, or the
	// state write itself — returns before or without a usable state file.
	// Refusing outright would make those artifacts unremovable, so the manifest
	// the detected manager points at decides.
	if state == nil {
		if !manifestExists(mgr) {
			return fmt.Errorf("service %q %w (no state file found in %s)", cfg.Name, ErrNotInstalled, cfg.InstallDir)
		}
		logger.Warn("no state file found, uninstalling the manifest reported by the detected init system",
			"name", cfg.Name, "path", mgr.GetManifestPath())
	}

	if mgr.SupportsStart() {
		logger.Info("stopping service", "phase", "uninstall", "step", "stop", "outcome", "started",
			"service_name", cfg.Name, "init_system", mgr.Name())
		if err := mgr.Stop(); err != nil {
			logger.Warn("failed to stop service, proceeding with uninstall", "name", cfg.Name, "error", err.Error())
		} else {
			logger.Info("service stopped", "phase", "uninstall", "step", "stop", "outcome", "succeeded",
				"service_name", cfg.Name, "init_system", mgr.Name())
		}
	} else {
		logger.Info("init system does not support a separate stop step", "phase", "uninstall", "step", "stop",
			"outcome", "skipped", "service_name", cfg.Name, "init_system", mgr.Name())
	}

	// A missing registration is reported, not fatal: the binary and the state
	// file still have to be cleaned up, and refusing here would strand them.
	if err := mgr.Uninstall(); err != nil {
		if !errors.Is(err, daemon.ErrCrontabEntryNotFound) {
			return fmt.Errorf("%s: %w", cfg.Name, err)
		}
		logger.Warn("no crontab entry found, continuing with uninstall", "name", cfg.Name)
	}
	logger.Info("service registration removed", "phase", "uninstall", "step", "deregister", "outcome", "succeeded",
		"service_name", cfg.Name, "init_system", mgr.Name())

	// update-config writes its backup next to the manifest, not next to the
	// binary, so it is swept from the manifest path the manager reports rather
	// than by the binary-relative suffix list.
	if manifestPath := mgr.GetManifestPath(); manifestPath != "" {
		removeInstallArtifact(manifestPath+".backup", logger)
	}

	// Not a plain os.Remove: the documented way to uninstall is to run the
	// INSTALLED binary, so on Windows the process doing the removing is the image
	// being removed — and a running image cannot be deleted there, only renamed.
	binaryPath := installedBinaryPath(cfg)
	binaryRemoved := true
	if err := removeInstalledBinary(binaryPath, logger); err != nil && !errors.Is(err, fs.ErrNotExist) {
		binaryRemoved = false
		logger.Warn("failed to remove binary", "path", binaryPath, "error", err.Error())
	} else if err == nil {
		logger.Info("removed binary", "path", binaryPath)
	}

	removeInstallArtifacts(binaryPath, logger)
	removeInstanceLockArtifact(cfg, logger)

	if err := deleteStateFn(cfg.InstallDir, cfg.Name); err != nil {
		logger.Warn("failed to delete state file", "error", err.Error())
	}

	// The service registration is gone either way, and that is the part that
	// matters — so this is not an error. But it is not "successfully" either: a
	// binary left behind also leaves a directory that cannot be deleted, and
	// saying "uninstalled successfully" over it is the same green-over-a-failure
	// that hid every earlier fault on this platform.
	if !binaryRemoved {
		logger.Warn("service uninstalled, but its binary could not be removed",
			"phase", "uninstall", "step", "complete", "outcome", "degraded",
			"service_name", cfg.Name, "install_dir", cfg.InstallDir, "binary", binaryPath,
			"hint", "delete the install directory after this process exits")
		return nil
	}

	logger.Info("service uninstalled successfully", "phase", "uninstall", "step", "complete", "outcome", "succeeded",
		"service_name", cfg.Name, "install_dir", cfg.InstallDir, "scope", cfg.Scope.normalized().String(), "init_system", mgr.Name())
	return nil
}

// installArtifactSuffixes are the files the watchdog and the updater leave in
// the install directory alongside the binary.
// The lock file is not in this list: it is removed by removeInstanceLockArtifact,
// which first proves nobody is holding it.
var installArtifactSuffixes = []string{".new", ".restore", ".rollback", ".rollback.invalid"}

// removeInstanceLockArtifact deletes the single-instance lock file, but only
// once it can take the lock itself. Stop() failure is only a warning here, so
// the supervised watchdog may still be running: unlinking the file detaches the
// inode it is flocking, and a later install plus run would create a fresh file
// that locks cleanly — two supervisors over one InstallDir, which is the
// corruption openOwnedLockFile refuses to create for exactly this reason. The
// lock is held across the removal and released after it, so a racer can only
// ever reach an inode that is still locked.
//
// Presence is decided with Lstat, not Stat: a symlink at the lock path with a
// missing target stats as absent, and leaving it would wedge the next install —
// ensureServiceUserLockFile refuses a symlinked lock path outright, and no
// daemonpilot command would be able to clear it. A symlink is removed without
// locking it, since a lock on it would be a lock on something else.
//
// The same applies to every other non-regular file — a FIFO above all, which
// openOwnedLockFile refuses for the same reason. No instance can be holding one
// as its lock, so there is nothing to prove by locking it first, and leaving it
// behind would wedge the next install exactly as a symlink would.
func removeInstanceLockArtifact(cfg *Config, logger Logger) {
	lockPath := instanceLockPath(cfg)
	info, err := lstatFileFn(lockPath)
	if err != nil {
		return
	}
	if !info.Mode().IsRegular() {
		logger.Warn("removing a non-regular file left at the single-instance lock path",
			"path", lockPath, "mode", info.Mode().String())
		removeInstallArtifact(lockPath, logger)
		return
	}
	release, err := acquireLockFn(lockPath, serviceUserUID(cfg))
	if err != nil {
		logger.Warn("leaving the single-instance lock file in place: it could not be locked, "+
			"so an instance may still be running",
			"path", lockPath, "error", err.Error())
		return
	}
	removeInstallArtifact(lockPath, logger)
	release()
}

// removeInstallArtifacts deletes the working files this service left next to its
// binary. A leftover .rollback is the reason this exists: after a reinstall, run
// calls CheckRollbackFile, finds the stale copy from the previous installation
// and acts on it. Only the artifacts named after this service are touched, and
// the install directory itself is left in place — it may be shared, and in
// system scope it is usually /usr/local/bin. Removal errors warn rather than
// fail: the service is already unregistered by this point.
func removeInstallArtifacts(binaryPath string, logger Logger) {
	for _, suffix := range installArtifactSuffixes {
		removeInstallArtifact(binaryPath+suffix, logger)
	}
	// The staged download is the one artifact whose name is not a plain suffix:
	// on Windows it has to stay executable, so ".new" goes before the extension.
	// Built from the same helper the updater writes it with, or uninstall leaves
	// a copy of the agent behind.
	if staged := StagedNewPath(binaryPath); staged != LegacyStagedNewPath(binaryPath) {
		removeInstallArtifact(staged, logger)
	}
	entries, err := os.ReadDir(filepath.Dir(binaryPath))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("failed to inspect superseded binaries", "path", filepath.Dir(binaryPath), "error", err.Error())
		}
		return
	}
	prefix := filepath.Base(binaryPath) + SupersededBinarySuffix
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
			removeInstallArtifact(filepath.Join(filepath.Dir(binaryPath), entry.Name()), logger)
		}
	}
}

// removeInstallArtifact deletes one leftover file, tolerating absence and
// warning rather than failing: the service is already unregistered by the time
// this runs.
func removeInstallArtifact(path string, logger Logger) {
	if err := removeFileFn(path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("failed to remove install artifact", "path", path, "error", err.Error())
		}
		return
	}
	logger.Info("removed install artifact", "path", path)
}

// logStateDivergence reports a mismatch between the recorded install and the
// configuration this process was started with. It never fails the run: the
// generated unit carries Restart=always with StartLimitIntervalSec=0, so an
// error here would be an unbounded crash loop instead of a diagnosis.
func logStateDivergence(cfg *Config, logger Logger) {
	state, err := loadStateFn(cfg.InstallDir, cfg.Name)
	if err != nil {
		logger.Warn("failed to load state file, continuing with the configured values",
			"error", err.Error())
		return
	}
	// A digest-only state file records no install, so it has neither a scope nor
	// an install dir to diverge from. Reading its absent scope as system scope
	// would report "install scope differs" on every start of a user-scope
	// service that has simply taken a staged update.
	if !state.recordsAnInstall() {
		return
	}
	if installed := state.EffectiveScope(); installed != cfg.Scope.normalized() {
		logger.Error("install scope differs from the configured scope, continuing with the configured scope",
			"installed_scope", installed.String(),
			"configured_scope", cfg.Scope.normalized().String(),
		)
	}
	if state.InstallDir != "" && state.InstallDir != cfg.InstallDir {
		logger.Error("install dir differs from the configured install dir, continuing with the configured install dir",
			"installed_install_dir", state.InstallDir,
			"configured_install_dir", cfg.InstallDir,
		)
	}
}

func recoverInterruptedWindowsRollback(cfg *Config, logger Logger) (bool, error) {
	if !isRollbackRecoveryExecutable(cfg) {
		return false, nil
	}
	binaryPath := installedBinaryPath(cfg)
	logger.Info("recovering an interrupted Windows rollback",
		"phase", "update", "step", "rollback_recovery", "outcome", "started",
		"service_name", cfg.Name, "install_dir", cfg.InstallDir)
	restored, restoreErr := RestoreRollbackBinary(cfg.InstallDir, cfg.Name, binaryPath+".rollback", binaryPath)
	if !restored {
		if restoreErr == nil {
			restoreErr = errors.New("rollback recovery image was not restored")
		}
		logger.Error("interrupted Windows rollback could not be recovered",
			"phase", "update", "step", "rollback_recovery", "outcome", "failed",
			"service_name", cfg.Name, "install_dir", cfg.InstallDir, "error", restoreErr.Error())
		return true, fmt.Errorf("recovering interrupted Windows rollback: %w", restoreErr)
	}
	if restoreErr != nil {
		logger.Warn("restored the installed binary from the SCM recovery image; rollback cleanup will retry after restart",
			"phase", "update", "step", "rollback_recovery", "outcome", "degraded",
			"service_name", cfg.Name, "install_dir", cfg.InstallDir, "error", restoreErr.Error())
	}
	logger.Info("recovered interrupted Windows rollback; restarting from the canonical binary",
		"phase", "update", "step", "rollback_recovery", "outcome", "succeeded",
		"service_name", cfg.Name, "install_dir", cfg.InstallDir)
	return true, ErrExecRestart
}

func handleRun(cfg *Config) error {
	logger := cfg.Logger
	logger.Info("supervision run started", "phase", "run", "step", "begin", "outcome", "started",
		"service_name", cfg.Name, "install_dir", cfg.InstallDir, "scope", cfg.Scope.normalized().String(),
		"version", cfg.Version, "watchdog_version", cfg.WatchdogVersion)

	logStateDivergence(cfg, logger)

	scope := cfg.Scope.daemonScope()
	det, err := detectForScope(scope)
	// The embedded log filter is a property of the host, not of the manager scope
	// filtering happened to select: a fallback manager chosen on an embedded board
	// must not switch the error-only filter off. It has to be set before the error
	// branches too — user scope on a busybox/procd board can never resolve a
	// manager, and dropping the filter there writes full Debug logging to flash.
	cfg.InitSystemName = det.HostInitSystem
	if err != nil {
		logger.Warn("failed to detect init system, assuming no auto-restart",
			"error", err.Error(), "host_init_system", cfg.InitSystemName)
	} else if initErr := initManager(det.Manager, daemon.ManagerOptions{ServiceName: cfg.Name, Scope: scope}); initErr != nil {
		logger.Warn("failed to initialize init system, assuming no auto-restart",
			"error", initErr.Error(), "host_init_system", cfg.InitSystemName)
	} else {
		mgr := det.Manager
		if cfg.InitSystemName == "" {
			cfg.InitSystemName = mgr.Name()
		}
		if !cfg.ExecThrough {
			cfg.CanInitRestartWatchdog = mgr.CanRestartWatchdog()
		}
		logger.Info("detected init system",
			"init_system", mgr.Name(),
			"host_init_system", cfg.InitSystemName,
			"can_restart_watchdog", cfg.CanInitRestartWatchdog,
		)
	}
	cfg.ApplyEmbeddedLogFilter()
	logger = cfg.Logger

	// Skipped for an exec-through child, which is a legitimate second process
	// spawned by the parent proxy that already holds the lock.
	if !cfg.ExecThrough {
		logger.Info("acquiring single-instance lock", "phase", "run", "step", "lock", "outcome", "started",
			"service_name", cfg.Name, "install_dir", cfg.InstallDir)
		releaseLock, err := takeInstanceLock(cfg, logger)
		if err != nil {
			return err
		}
		defer releaseLock()
		logger.Info("single-instance lock acquired", "phase", "run", "step", "lock", "outcome", "succeeded",
			"service_name", cfg.Name, "install_dir", cfg.InstallDir)
	} else {
		logger.Info("single-instance lock inherited from proxy parent", "phase", "run", "step", "lock", "outcome", "skipped",
			"service_name", cfg.Name, "install_dir", cfg.InstallDir)
	}

	ctx, cancel := setupSignalHandler(context.Background())
	defer cancel()
	if recovered, err := recoverInterruptedWindowsRollback(cfg, logger); recovered {
		return err
	}

	if cfg.Update != nil {
		if newUpdaterFn == nil {
			return errors.New("updater factory not registered; import daemonpilot/pkg/daemonpilot/setup for production use")
		}
		u := newUpdaterFn(cfg.Update, logger, cfg.InstallDir, cfg.Name)

		// The probes execute the downloaded binary, so they need the same
		// command prefix the installed one answers to.
		if setter, ok := u.(interface{ SetCommandPrefix(prefix []string) }); ok {
			setter.SetCommandPrefix(cfg.CommandPrefix)
		}

		if err := u.CleanupStaleFiles(); err != nil {
			logger.Error("failed to clean up stale files", "error", err.Error())
		}

		binaryPath := installedBinaryPath(cfg)
		hasRollback, err := u.CheckRollbackFile(ctx, binaryPath)
		if err != nil {
			logger.Error("failed to check rollback file", "error", err.Error())
		} else if hasRollback && !cfg.Update.StagedRollbackOnUnstable {
			// Legacy startup-confirm path. When StagedRollbackOnUnstable is set,
			// the .rollback file is left in place for the watchdog to resolve by
			// worker stability (see Watchdog.runProbation) instead.
			rollbackPath := binaryPath + ".rollback"
			if err := u.HandleRollback(ctx, rollbackPath, cfg.Update.RollbackTimeout); err != nil {
				if errors.Is(err, ErrRollbackRestored) {
					logger.Info("rollback restored, restarting with restored binary")
					return execSelfFn(binaryPath)
				}
				logger.Error("rollback handling failed", "error", err.Error())
			}
		}
	}

	if newWatchdogFn == nil {
		return errors.New("watchdog factory not registered; import daemonpilot/pkg/daemonpilot/setup for production use")
	}
	w := newWatchdogFn(cfg)
	err = w.Run(ctx)
	if errors.Is(err, ErrExecRestart) {
		binaryPath := installedBinaryPath(cfg)
		return execSelfFn(binaryPath)
	}
	//nolint:wrapcheck // the watchdog's error is this command's error; the CLI prints it verbatim
	return err
}

var stdoutWriter io.Writer = os.Stdout

func handleWorker(cfg *Config) error {
	if cfg.RespawnStaleWatchdog && ResidentWatchdogStale(cfg.WatchdogVersion) {
		cfg.Logger.Info("requesting resident watchdog restart", "phase", "worker",
			"outcome", "respawn_watchdog", "watchdog_version", cfg.WatchdogVersion,
			"resident_watchdog_version", ReadStaticEnv().WatchdogVersion)
		RespawnWatchdog()
	}

	ctx, cancel := setupSignalHandler(context.Background())
	defer cancel()

	if cfg.WorkerFunc == nil {
		return errors.New("WorkerFunc is not configured")
	}

	applyMemoryLimit(cfg.Logger)
	applyOOMScoreAdj(cfg.Logger)

	monCtx, monCancel := workerParentMonitor(ctx, cfg.Logger)
	defer monCancel()

	cfg.Logger.Info("starting worker")
	return cfg.WorkerFunc(monCtx)
}

func parseBool(s string) bool {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return false
	}
	return v
}

func applyMemoryLimit(logger Logger) {
	raw := os.Getenv(EnvMaxMemoryBytes)
	if raw == "" {
		return
	}

	var bytes int64
	if _, err := fmt.Sscanf(raw, "%d", &bytes); err != nil || bytes <= 0 {
		return
	}

	if err := procguard.ApplyLimitsToSelf(procguard.Limits{AddressSpace: bytes}); err != nil {
		if logger != nil {
			if errors.Is(err, procguard.ErrUnsupported) {
				logger.Warn("memory limit not supported on this platform, continuing without limit",
					"bytes", bytes)
			} else {
				logger.Warn("failed to set memory limit, continuing without limit",
					"bytes", bytes, "error", err.Error())
			}
		}
		return
	}

	if logger != nil {
		logger.Info("memory limit applied", "bytes", bytes)
	}
}

func applyOOMScoreAdj(logger Logger) {
	raw := os.Getenv(EnvOOMScoreAdj)
	if raw == "" {
		return
	}

	var value int
	if _, err := fmt.Sscanf(raw, "%d", &value); err != nil {
		return
	}

	if err := procguard.ApplyLimitsToSelf(procguard.Limits{OOMScoreAdj: &value}); err != nil {
		if errors.Is(err, procguard.ErrUnsupported) {
			return
		}
		if logger != nil {
			logger.Warn("failed to set OOM score adjustment, continuing without it",
				"value", value, "error", err.Error())
		}
		return
	}

	if logger != nil && runtime.GOOS == "linux" {
		logger.Info("OOM score adjustment applied", "value", value)
	}
}

func handleSelftest(cfg *Config) error {
	ctx, cancel := setupSignalHandler(context.Background())
	defer cancel()

	var result SelftestResult
	if cfg.SelftestFunc != nil {
		result = cfg.SelftestFunc(ctx)
	} else {
		result = SelftestResult{
			OK:     true,
			Checks: map[string]CheckResult{},
		}
	}

	if err := json.NewEncoder(stdoutWriter).Encode(result); err != nil {
		return fmt.Errorf("encoding selftest result: %w", err)
	}
	return nil
}

type versionOutput struct {
	Version         string `json:"version"`
	WatchdogVersion string `json:"watchdog_version"`
	GoVersion       string `json:"go_version"`
}

func handleVersion(cfg *Config) error {
	out := versionOutput{
		Version:         cfg.Version,
		WatchdogVersion: cfg.WatchdogVersion,
		GoVersion:       runtime.Version(),
	}
	if err := json.NewEncoder(stdoutWriter).Encode(out); err != nil {
		return fmt.Errorf("encoding version info: %w", err)
	}
	return nil
}

func handleWatchdogVersion(cfg *Config) error {
	if _, err := fmt.Fprintln(stdoutWriter, cfg.WatchdogVersion); err != nil {
		return fmt.Errorf("writing watchdog version: %w", err)
	}
	return nil
}

func statusMessage(status daemon.ServiceStatus) string {
	switch status {
	case daemon.ServiceStatusRunning:
		return "Service is active and running"
	case daemon.ServiceStatusStopped:
		return "Service is not running"
	case daemon.ServiceStatusFailed:
		return "Service has failed"
	case daemon.ServiceStatusUnknown:
		return "Unable to determine service status"
	default:
		return "Unable to determine service status"
	}
}

// handleStatus queries the OS init system for the current state of the service
// and prints the service name, status, and a human-readable description.
func handleStatus(cfg *Config) error {
	state := readableState(cfg)

	mgr, err := managerForState(cfg, state, daemon.ManagerOptions{ServiceName: cfg.Name, InstallDir: cfg.InstallDir})
	if err != nil {
		return err
	}

	if !mgr.SupportsStatus() {
		return fmt.Errorf("status not supported by %s", mgr.Name())
	}

	status, err := mgr.Status()
	if err != nil {
		return fmt.Errorf("%s: %w", cfg.Name, err)
	}

	if _, err := fmt.Fprintf(stdoutWriter, "Service: %s\nStatus: %s\n  \u21b3 %s\n", cfg.Name, status.String(), statusMessage(status)); err != nil {
		return fmt.Errorf("writing status: %w", err)
	}
	if note := lingeringNote(state, status); note != "" {
		if _, err := fmt.Fprintf(stdoutWriter, "  \u21b3 %s\n", note); err != nil {
			return fmt.Errorf("writing status: %w", err)
		}
	}
	return nil
}

// lingeringNote explains an inactive user-scope service whose per-user manager
// is torn down at logout and not started at boot, the most likely reason it is
// not running.
//
// The remedy is init-system specific, so the recorded init system selects it.
// Lingering is a systemd concept and state.Lingering is false for every manager
// that is not a daemon.LingerChecker, so keying on that field alone would tell a
// macOS operator to run loginctl.
func lingeringNote(state *StateData, status daemon.ServiceStatus) string {
	if state == nil || state.EffectiveScope() != ScopeUser {
		return ""
	}
	if status == daemon.ServiceStatusRunning {
		return ""
	}
	switch state.InitSystem {
	case "systemd":
		if state.Lingering {
			return ""
		}
		return "installed without lingering: systemd stops this service at logout and does not start it at boot " +
			"(enable it with `loginctl enable-linger`, then reinstall)"
	case "launchd":
		return "user-scope launchd agents have no equivalent of lingering: launchd stops this agent at logout " +
			"and does not start it before a login (install with Scope=system for a service that must survive logout)"
	default:
		return ""
	}
}

func getStartSuggestion(_ daemon.ServiceManager) string {
	return "check your init system documentation for start/stop support"
}

// handleStart starts the service via the OS init system.
func handleStart(cfg *Config) error {
	mgr, err := prepareInstalledManager(cfg, daemon.ManagerOptions{ServiceName: cfg.Name, InstallDir: cfg.InstallDir})
	if err != nil {
		return err
	}

	if !mgr.SupportsStart() {
		return fmt.Errorf("start not supported by %s: %s", mgr.Name(), getStartSuggestion(mgr))
	}

	if err := mgr.Start(); err != nil {
		return fmt.Errorf("%s: %w", cfg.Name, err)
	}

	cfg.Logger.Info("service started successfully", "name", cfg.Name)
	return nil
}

// handleStop stops the service via the OS init system.
func handleStop(cfg *Config) error {
	mgr, err := prepareInstalledManager(cfg, daemon.ManagerOptions{ServiceName: cfg.Name, InstallDir: cfg.InstallDir})
	if err != nil {
		return err
	}

	if !mgr.SupportsStart() {
		return fmt.Errorf("stop not supported by %s: %s", mgr.Name(), getStartSuggestion(mgr))
	}

	if err := mgr.Stop(); err != nil {
		return fmt.Errorf("%s: %w", cfg.Name, err)
	}

	cfg.Logger.Info("service stopped successfully", "name", cfg.Name)
	return nil
}

// handleRestart stops and then starts the service via the OS init system.
func handleRestart(cfg *Config) error {
	mgr, err := prepareInstalledManager(cfg, daemon.ManagerOptions{ServiceName: cfg.Name, InstallDir: cfg.InstallDir})
	if err != nil {
		return err
	}

	if !mgr.SupportsStart() {
		return fmt.Errorf("restart not supported by %s: %s", mgr.Name(), getStartSuggestion(mgr))
	}

	cfg.Logger.Info("stopping service", "name", cfg.Name)
	if err := mgr.Stop(); err != nil {
		if mgr.SupportsStatus() {
			status, statusErr := mgr.Status()
			if statusErr != nil {
				return fmt.Errorf("%s: %w (cannot verify service state: %w)", cfg.Name, err, statusErr)
			}
			if status == daemon.ServiceStatusRunning {
				return fmt.Errorf("%s: %w (service is still running, not starting a second instance)", cfg.Name, err)
			}
			if status == daemon.ServiceStatusUnknown {
				return fmt.Errorf("%s: %w (service state is unknown, not safe to start)", cfg.Name, err)
			}
		}
		cfg.Logger.Warn("failed to stop service, attempting start anyway", "name", cfg.Name, "error", err.Error())
	}

	cfg.Logger.Info("starting service", "name", cfg.Name)
	if err := mgr.Start(); err != nil {
		return fmt.Errorf("%s: %w", cfg.Name, err)
	}

	cfg.Logger.Info("service restarted successfully", "name", cfg.Name)
	return nil
}

func getEnableSuggestion(mgr daemon.ServiceManager) string {
	switch mgr.Name() {
	case "launchd":
		return "set RunAtLoad=true in LaunchdKeys configuration"
	default:
		return "check your init system documentation for enable/disable support"
	}
}

// handleEnable enables the service for automatic start on system boot.
func handleEnable(cfg *Config) error {
	mgr, err := prepareInstalledManager(cfg, daemon.ManagerOptions{ServiceName: cfg.Name, InstallDir: cfg.InstallDir})
	if err != nil {
		return err
	}

	if !mgr.SupportsEnable() {
		return fmt.Errorf("enable not supported by %s: %s", mgr.Name(), getEnableSuggestion(mgr))
	}

	if err := mgr.Enable(); err != nil {
		return fmt.Errorf("%s: %w", cfg.Name, err)
	}

	cfg.Logger.Info("service enabled successfully", "name", cfg.Name)
	return nil
}

// handleDisable disables automatic start of the service on system boot.
func handleDisable(cfg *Config) error {
	mgr, err := prepareInstalledManager(cfg, daemon.ManagerOptions{ServiceName: cfg.Name, InstallDir: cfg.InstallDir})
	if err != nil {
		return err
	}

	if !mgr.SupportsEnable() {
		return fmt.Errorf("disable not supported by %s: %s", mgr.Name(), getEnableSuggestion(mgr))
	}

	if err := mgr.Disable(); err != nil {
		return fmt.Errorf("%s: %w", cfg.Name, err)
	}

	cfg.Logger.Info("service disabled successfully", "name", cfg.Name)
	return nil
}

func buildServiceConfig(cfg *Config) daemon.ServiceConfig {
	args := append(DaemonpilotArgs(cfg.CommandPrefix, "run"), cfg.ServiceArguments...)
	return daemon.ServiceConfig{
		Name:              cfg.Name,
		Description:       cfg.Description,
		ExecPath:          installedBinaryPath(cfg),
		InstallDir:        cfg.InstallDir,
		WorkingDir:        cfg.InstallDir,
		Scope:             cfg.Scope.daemonScope(),
		LogDir:            cfg.LogDir,
		Environment:       cfg.ServiceEnvironment,
		Arguments:         args,
		User:              cfg.ServiceUser,
		Group:             cfg.ServiceGroup,
		NotifyReady:       cfg.NotifyReady,
		SystemdDirectives: cfg.SystemdDirectives,
		LaunchdKeys:       cfg.LaunchdKeys,
		OpenRCDirectives:  cfg.OpenRCDirectives,
		CrontabSchedule:   cfg.CrontabSchedule,
	}
}

func normalizeWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	var result []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return strings.Join(result, "\n")
}

func validateServiceConfig(cfg *Config) error {
	state, err := loadInstalledState(cfg)
	if err != nil {
		return err
	}
	if state == nil {
		return fmt.Errorf("service %q %w (no state file found)", cfg.Name, ErrNotInstalled)
	}

	mgr, err := managerForState(cfg, state, daemon.ManagerOptions{ServiceName: cfg.Name, InstallDir: cfg.InstallDir})
	if err != nil {
		return err
	}

	if err := checkInitSystemDrift(cfg, state); err != nil {
		return err
	}
	currentInitSystem := detectInitSystemType(mgr)

	svcConfig := buildServiceConfig(cfg)

	expectedContent, err := mgr.GenerateConfig(svcConfig)
	if err != nil {
		return fmt.Errorf("generating expected config: %w", err)
	}

	if len(cfg.ConfigPatchers) > 0 {
		info := PatchInfo{
			OS:          runtime.GOOS,
			Arch:        runtime.GOARCH,
			InitSystem:  currentInitSystem,
			ServiceName: cfg.Name,
			FilePath:    manifestPathForState(cfg, mgr, state),
		}
		for i, patcher := range cfg.ConfigPatchers {
			expectedContent, err = patcher(info.FilePath, expectedContent, info)
			if err != nil {
				return fmt.Errorf("applying config patcher %d: %w", i, err)
			}
		}
	}

	actualContent, err := readInstalledConfigFn(mgr)
	if err != nil {
		return fmt.Errorf("reading installed config: %w", err)
	}

	if normalizeWhitespace(expectedContent) != normalizeWhitespace(actualContent) {
		return fmt.Errorf("configuration drift detected for %q; run install or update-config to fix", cfg.Name)
	}

	return nil
}

// generatePatchedConfig renders the init-system config for cfg and runs the
// configured patchers over it.
func generatePatchedConfig(cfg *Config, mgr daemon.ServiceManager, svcConfig daemon.ServiceConfig, manifestPath, initSystem string) (string, error) {
	content, err := mgr.GenerateConfig(svcConfig)
	if err != nil {
		return "", fmt.Errorf("generating config: %w", err)
	}

	if len(cfg.ConfigPatchers) > 0 {
		info := PatchInfo{
			OS:          runtime.GOOS,
			Arch:        runtime.GOARCH,
			InitSystem:  initSystem,
			ServiceName: cfg.Name,
			FilePath:    manifestPath,
		}
		for i, patcher := range cfg.ConfigPatchers {
			content, err = patcher(manifestPath, content, info)
			if err != nil {
				return "", fmt.Errorf("applying config patcher %d: %w", i, err)
			}
		}
	}
	return content, nil
}

// writeConfigForRollback restores a previous manifest. Managers that undo their
// own manifest write when the init system refuses the result must not do so
// here: what is on disk is the manifest being rolled back away from, so putting
// it back — and, for launchd, reloading it into the domain — would leave the
// service running the configuration this rollback exists to remove, while the
// caller reports the rollback as failed.
//
//nolint:wrapcheck // a dispatcher: every caller reports the rollback failure with its own context
func writeConfigForRollback(mgr daemon.ServiceManager, content string) error {
	if w, ok := mgr.(daemon.ConfigRollbackWriter); ok {
		return w.WriteConfigForRollback(content)
	}
	return mgr.WriteConfig(content)
}

// syncEnvironmentFile writes the new environment file for managers that keep
// one (dinit), returning the previous contents so a later failure can restore
// them. Any failure here rolls the config back before returning.
func syncEnvironmentFile(mgr daemon.ServiceManager, svcConfig daemon.ServiceConfig, oldContent []byte, backupPath string, logger Logger) ([]byte, error) {
	var oldEnvContent []byte
	if erw, ok := mgr.(daemon.EnvironmentFileReadWriter); ok {
		content, readErr := erw.ReadEnvironmentFile()
		if readErr != nil {
			logger.Error("failed to read environment file for backup, rolling back config", "error", readErr.Error(), "backup", backupPath)
			if rbErr := writeConfigForRollback(mgr, string(oldContent)); rbErr != nil {
				logger.Error("rollback write failed after env read failure, preserving backup file", "error", rbErr.Error(), "backup", backupPath)
				return nil, fmt.Errorf("reading environment file for backup: %w (rollback also failed, backup preserved at %s)", readErr, backupPath)
			}
			if mgr.SupportsStart() {
				if startErr := mgr.Start(); startErr != nil {
					logger.Error("failed to restart service after env read failure rollback", "error", startErr.Error(), "backup", backupPath)
					return nil, fmt.Errorf("reading environment file for backup: %w (rolled back config but service restart failed, backup preserved at %s)", readErr, backupPath)
				}
			}
			_ = removeFileFn(backupPath)
			return nil, fmt.Errorf("reading environment file for backup: %w (rolled back to previous config)", readErr)
		}
		oldEnvContent = content
		if err := erw.WriteEnvironmentFile(svcConfig.Environment); err != nil {
			envRestored := true
			if rbEnvErr := erw.RestoreEnvironmentFile(oldEnvContent); rbEnvErr != nil {
				logger.Warn("failed to restore environment file after env write failure", "error", rbEnvErr.Error())
				envRestored = false
			}
			if rbErr := writeConfigForRollback(mgr, string(oldContent)); rbErr != nil {
				logger.Error("rollback write failed after env write failure, preserving backup file", "error", rbErr.Error(), "backup", backupPath)
				return nil, fmt.Errorf("writing environment file: %w (rollback also failed, backup preserved at %s)", err, backupPath)
			}
			if mgr.SupportsStart() {
				if startErr := mgr.Start(); startErr != nil {
					logger.Error("rollback start failed after env write failure, preserving backup file", "error", startErr.Error(), "backup", backupPath)
					return nil, fmt.Errorf("writing environment file: %w (rolled back config but service restart failed, backup preserved at %s)", err, backupPath)
				}
			}
			_ = removeFileFn(backupPath)
			if !envRestored {
				return nil, fmt.Errorf("writing environment file: %w (rolled back config but environment file restoration failed)", err)
			}
			return nil, fmt.Errorf("writing environment file: %w (rolled back to previous config)", err)
		}
	}

	return oldEnvContent, nil
}

// startWithConfigRollback starts the service with its new config and, if that
// fails, restores the previous config and environment file before reporting
// the original failure.
func startWithConfigRollback(mgr daemon.ServiceManager, oldContent, oldEnvContent []byte, backupPath string, logger Logger) error {
	if mgr.SupportsStart() {
		if err := mgr.Start(); err != nil {
			logger.Warn("failed to start with new config, rolling back", "error", err.Error())
			envRestored := true
			if erw, ok := mgr.(daemon.EnvironmentFileReadWriter); ok {
				if rbErr := erw.RestoreEnvironmentFile(oldEnvContent); rbErr != nil {
					logger.Warn("failed to restore environment file during rollback", "error", rbErr.Error())
					envRestored = false
				}
			}
			if rbErr := writeConfigForRollback(mgr, string(oldContent)); rbErr != nil {
				logger.Error("rollback write failed, preserving backup file", "error", rbErr.Error(), "backup", backupPath)
				return fmt.Errorf("new config: %w (rollback also failed, backup preserved at %s)", err, backupPath)
			}
			if startErr := mgr.Start(); startErr != nil {
				logger.Error("rollback start failed, preserving backup file", "error", startErr.Error(), "backup", backupPath)
				return fmt.Errorf("new config: %w (rolled back config but service restart failed, backup preserved at %s)", err, backupPath)
			}
			_ = removeFileFn(backupPath)
			if !envRestored {
				return fmt.Errorf("new config: %w (rolled back config but environment file restoration failed)", err)
			}
			return fmt.Errorf("new config: %w (rolled back to previous config)", err)
		}
	}

	return nil
}

func handleUpdateConfig(cfg *Config) error {
	logger := cfg.Logger

	state, err := loadInstalledState(cfg)
	if err != nil {
		return err
	}
	if state == nil {
		return fmt.Errorf("service %q %w (no state file found)", cfg.Name, ErrNotInstalled)
	}

	mgr, err := managerForState(cfg, state, daemon.ManagerOptions{ServiceName: cfg.Name, InstallDir: cfg.InstallDir})
	if err != nil {
		return err
	}

	if err := checkInitSystemDrift(cfg, state); err != nil {
		return err
	}
	currentInitSystem := detectInitSystemType(mgr)

	svcConfig := buildServiceConfig(cfg)
	// The manager's own path, never state.ManifestPath: install chowns the state
	// file to Config.ServiceUser so the staged updater can record a rollback
	// digest, which puts a recorded path inside the unprivileged account's
	// control. Root then creates and removes "<path>.backup" here — an
	// attacker-chosen path would be a 0644 root-owned write anywhere on the
	// filesystem. The manager recomputes the path from name, scope and install
	// dir, so it is the authoritative one either way.
	manifestPath := manifestPathForState(cfg, mgr, state)

	newContent, err := generatePatchedConfig(cfg, mgr, svcConfig, manifestPath, currentInitSystem)
	if err != nil {
		return err
	}

	backupPath := manifestPath + ".backup"
	oldContentStr, err := mgr.ReadInstalledConfig()
	if err != nil {
		return fmt.Errorf("reading current config: %w", err)
	}
	oldContent := []byte(oldContentStr)
	if err := writeFileFn(backupPath, oldContent, 0o644); err != nil {
		return fmt.Errorf("creating config backup: %w", err)
	}

	if mgr.SupportsStart() {
		if err := mgr.Stop(); err != nil {
			logger.Warn("failed to stop service before config update", "error", err.Error())
		}
	}

	if err := mgr.WriteConfig(newContent); err != nil {
		if rbErr := writeConfigForRollback(mgr, string(oldContent)); rbErr != nil {
			logger.Error("failed to restore config from backup, preserving backup file", "error", rbErr.Error(), "backup", backupPath)
			return fmt.Errorf("writing new config: %w (rollback also failed, backup preserved at %s)", err, backupPath)
		}
		if mgr.SupportsStart() {
			if startErr := mgr.Start(); startErr != nil {
				logger.Error("failed to restart service after config rollback, preserving backup file", "error", startErr.Error(), "backup", backupPath)
				return fmt.Errorf("writing new config: %w (rollback config restored but service restart failed, backup preserved at %s)", err, backupPath)
			}
		}
		_ = removeFileFn(backupPath)
		return fmt.Errorf("writing new config: %w", err)
	}

	oldEnvContent, err := syncEnvironmentFile(mgr, svcConfig, oldContent, backupPath, logger)
	if err != nil {
		return err
	}

	if err := startWithConfigRollback(mgr, oldContent, oldEnvContent, backupPath, logger); err != nil {
		return err
	}

	_ = removeFileFn(backupPath)

	state.InstalledAt = timeNow()
	state.Version = cfg.Version
	// Pin the name back to the one this command was invoked for, exactly as
	// RecordRollbackDigest does: SaveState derives the destination from it, and it
	// came out of a file the service account can write.
	state.ServiceName = cfg.Name
	// The manifest just written is the one at the resolved path, so record it.
	// Saving the stale value back would leave manifestPathForState warning about
	// a divergence on every later command with no way for it to ever clear.
	state.ManifestPath = manifestPath
	// state was read before the service was stopped and restarted. A staged
	// update that recorded a rollback digest in that window — the whole command
	// for a crontab-managed service, which is never stopped — would be written
	// back out of the stale snapshot, leaving its live .rollback copy
	// unverifiable and quarantined on the next start.
	state.RollbackDigest = recordedRollbackDigest(cfg)
	if err := saveStateFn(cfg.InstallDir, state); err != nil {
		logger.Warn("failed to update state file", "error", err.Error())
	}

	logger.Info("service configuration updated successfully", "name", cfg.Name)
	return nil
}

func handleValidate(cfg *Config) error {
	if err := validateServiceConfig(cfg); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	cfg.Logger.Info("service configuration is valid", "name", cfg.Name)
	return nil
}

func isSameFile(a, b string) bool {
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	if errA != nil || errB != nil {
		return false
	}
	return os.SameFile(infoA, infoB)
}

func copyFile(src, dst string) error {
	//nolint:gosec // copying the service binary the caller named is the point
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp.*")
	if err != nil {
		return fmt.Errorf("creating temp file next to %s: %w", dst, err)
	}
	tmpPath := out.Name()
	//nolint:gosec // an executable needs its exec bits
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("setting permissions on %s: %w", tmpPath, err)
	}

	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("copying %s to %s: %w", src, tmpPath, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("renaming %s to %s: %w", tmpPath, dst, err)
	}
	return nil
}
