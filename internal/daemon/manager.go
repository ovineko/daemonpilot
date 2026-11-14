package daemon

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// maxCommandOutputBytes caps how much init-system command output is embedded in
// an error, so a chatty init script cannot flood the operator's terminal.
const maxCommandOutputBytes = 512

// stripControlChars removes C0 and C1 control characters, keeping only newline
// and tab. Init-system output is printed straight to the operator's terminal,
// so an escape sequence echoed by an init script could otherwise reposition the
// cursor or spoof surrounding output.
func stripControlChars(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			return -1
		default:
			return r
		}
	}, s)
}

// runErr wraps a failed init-system command, appending its combined output when
// there is any. Without it an operator only sees "exit status 1" instead of the
// actual cause reported by systemctl, launchctl or an init script.
func runErr(action string, out []byte, err error) error {
	msg := strings.TrimSpace(stripControlChars(strings.ToValidUTF8(string(out), "")))
	if msg == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	if len(msg) > maxCommandOutputBytes {
		msg = strings.ToValidUTF8(msg[:maxCommandOutputBytes], "") + " (output truncated)"
	}
	return fmt.Errorf("%s: %w: %s", action, err, msg)
}

// ServiceStatus is the state an init system reports for an installed service.
type ServiceStatus int

// Service states, defaulting to Unknown when an init system cannot answer.
const (
	ServiceStatusUnknown ServiceStatus = iota
	ServiceStatusRunning
	ServiceStatusStopped
	ServiceStatusFailed
)

func (s ServiceStatus) String() string {
	switch s {
	case ServiceStatusRunning:
		return "running"
	case ServiceStatusStopped:
		return "stopped"
	case ServiceStatusFailed:
		return "failed"
	case ServiceStatusUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// ServiceConfig describes the service to register with an init system.
type ServiceConfig struct {
	Name        string
	Description string
	ExecPath    string
	InstallDir  string
	WorkingDir  string

	// Scope selects machine-wide or per-user registration. The zero value,
	// ScopeSystem, generates exactly the manifests generated before scopes
	// existed.
	Scope Scope

	// LogDir is where managers that log to files should write. The zero
	// value keeps each manager's historical location, so system-scope
	// manifests stay byte-identical.
	LogDir string

	Environment       map[string]string
	Arguments         []string
	User              string
	Group             string
	NotifyReady       bool
	SystemdDirectives map[string]string
	LaunchdKeys       map[string]any
	OpenRCDirectives  map[string]string
	CrontabSchedule   string
}

// ServiceManager defines the interface for init system service managers.
// Each implementation handles a specific init system (systemd, launchd, etc.)
// and provides methods for service lifecycle management.
//
// CanRestartWatchdog reports whether the init system will automatically restart
// the watchdog process after it exits. Init systems with built-in supervision
// (systemd, launchd, dinit, finit, runit, s6, procd) return true. Init systems that only run
// processes once (BusyBox init, crontab) return false, requiring exec-through
// for safe watchdog updates.
//
// Name returns a short identifier for the init system (e.g. "systemd", "crontab").
//
// scatter the same lifecycle across types every implementation must satisfy anyway
//
//nolint:interfacebloat // one interface per init system: splitting it would only
type ServiceManager interface {
	Install(config ServiceConfig) error
	Uninstall() error
	Start() error
	Stop() error
	Enable() error
	Disable() error
	Status() (ServiceStatus, error)
	GetManifestPath() string
	GenerateConfig(config ServiceConfig) (string, error)
	WriteConfig(content string) error
	ReadInstalledConfig() (string, error)
	CanRestartWatchdog() bool
	// SupportsEnable reports whether this init system supports enabling/disabling
	// auto-start on boot via Enable() and Disable().
	SupportsEnable() bool
	// SupportsStart reports whether this init system supports manually starting
	// and stopping the service via Start() and Stop().
	SupportsStart() bool
	// SupportsStatus reports whether this init system can query service status
	// via Status().
	SupportsStatus() bool
	// SupportsScope reports whether this init system can register a service in
	// the given scope. Every init system supports ScopeSystem; only those with
	// a per-user instance support ScopeUser.
	SupportsScope(scope Scope) bool
	Name() string
}

// EnvironmentFileWriter is an optional interface for service managers that
// store environment variables in separate files rather than inline in the
// service manifest (e.g. dinit uses .env files).
type EnvironmentFileWriter interface {
	WriteEnvironmentFile(env map[string]string) error
}

// EnvironmentFileReadWriter extends EnvironmentFileWriter with the ability
// to read and restore the current environment file content for rollback.
type EnvironmentFileReadWriter interface {
	EnvironmentFileWriter
	ReadEnvironmentFile() ([]byte, error)
	RestoreEnvironmentFile(content []byte) error
}

// LingerChecker is an optional interface for service managers whose user scope
// depends on the invoking user having lingering enabled. It reports the state
// alongside the user name a remedy command must name, so callers can build an
// actionable message without duplicating the account lookup.
type LingerChecker interface {
	LingeringEnabled() (bool, string, error)
}

// DomainReporter is an optional interface for service managers that resolve a
// domain at install time which every later operation must address. The resolved
// value is persisted in the state file rather than re-derived, because the
// session state it was resolved from can change between commands.
type DomainReporter interface {
	ResolvedDomain() string
}

// ConfigRollbackWriter is an optional interface for service managers whose
// WriteConfig restores what it overwrote when the init system then refuses the
// new manifest. That restore is wrong when the caller is itself rolling back:
// what is on disk at that point is the manifest being rolled back _away from_,
// so putting it back — and, for launchd, reloading it — reinstates the very
// configuration the caller just rejected, while the caller reports that the
// rollback failed. Callers performing a rollback use this method instead; the
// content they write is what the state file already describes, so leaving it in
// place on failure is the correct outcome.
type ConfigRollbackWriter interface {
	WriteConfigForRollback(content string) error
}

// RegistrationVerifier is an optional interface for service managers that can
// ask the init system where it believes the manifest lives, so an install can
// fail immediately when the two disagree instead of at the next boot.
type RegistrationVerifier interface {
	VerifyRegistered() error
}

// DetectFunc reports whether its init system is the one running on this host.
type DetectFunc func() bool

type detectorEntry struct {
	name   string
	detect DetectFunc
	create func() ServiceManager
}

var detectors []detectorEntry

func init() {
	detectors = defaultDetectors()
}

func defaultDetectors() []detectorEntry {
	// Platform entries come FIRST: on Windows the SCM is the only manager that can
	// match, and off Windows this is empty, so the unix ordering below is exactly
	// what it always was.
	return append(platformDetectors(), []detectorEntry{
		{
			name:   "systemd",
			detect: detectSystemd,
			create: func() ServiceManager { return newSystemdManager() },
		},
		{
			name:   "launchd",
			detect: detectLaunchd,
			create: func() ServiceManager { return newLaunchdManager() },
		},
		{
			name:   "openrc",
			detect: detectOpenRC,
			create: func() ServiceManager { return newOpenrcManager() },
		},
		{
			name:   "dinit",
			detect: detectDinit,
			create: func() ServiceManager { return newDinitManager() },
		},
		{
			name:   "finit",
			detect: detectFinit,
			create: func() ServiceManager { return newFinitManager() },
		},
		{
			name:   "runit",
			detect: detectRunit,
			create: func() ServiceManager { return newRunitManager() },
		},
		{
			name:   "s6",
			detect: detectS6,
			create: func() ServiceManager { return newS6Manager() },
		},
		{
			name:   "procd",
			detect: detectProcd,
			create: func() ServiceManager { return newProcdManager() },
		},
		{
			name:   "busybox",
			detect: detectBusybox,
			create: func() ServiceManager { return newBusyboxManager() },
		},
		{
			name:   "sysvinit",
			detect: detectSysvinit,
			create: func() ServiceManager { return newSysvinitManager() },
		},
		{
			name:   "crontab",
			detect: detectCrontab,
			create: func() ServiceManager { return newCrontabManager() },
		},
	}...)
}

// DetectServiceManager returns a manager for the init system running on this
// host, trying detectors in priority order and falling back to crontab.
func DetectServiceManager() (ServiceManager, error) {
	getDetectionLog().Debug("init detection: starting service manager detection")
	for _, d := range detectors {
		getDetectionLog().Debug("init detection: checking "+d.name, "detector", d.name)
		result := d.detect()
		getDetectionLog().Debug("init detection: "+d.name+": detected="+strconv.FormatBool(result), "detector", d.name, "detected", result)
		if result {
			mgr := d.create()
			getDetectionLog().Debug("init detection: selected manager="+d.name, "manager", d.name)
			return mgr, nil
		}
	}
	getDetectionLog().Debug("init detection: no supported service manager detected")
	return nil, errors.New("no supported service manager detected")
}

// SkippedDetector records an init system that was detected on this host but
// cannot register a service in the requested scope.
type SkippedDetector struct {
	Name   string
	Reason string
}

// Detection is the result of a scope-aware detection pass.
type Detection struct {
	// Manager is the selected manager for the requested scope.
	Manager ServiceManager
	// HostInitSystem is the init system detected on this host *before* scope
	// filtering. Decisions about the host itself — such as reducing logging on
	// embedded init systems — must key off this rather than Manager.Name(),
	// which may name a fallback selected only because of the scope.
	HostInitSystem string
	// Skipped lists the init systems that were detected but cannot serve the
	// requested scope, in detector order.
	Skipped []SkippedDetector
}

// DetectForScope returns the manager for the init system running on this host
// that can register a service in the requested scope, along with the unfiltered
// host init system and the detectors skipped on the way.
//
// On error the returned Detection still carries whatever was learned before the
// failure — HostInitSystem in particular — because host-level decisions such as
// the embedded log filter must survive a scope-filtering failure. Manager is nil
// whenever err is non-nil.
func DetectForScope(scope Scope) (Detection, error) {
	getDetectionLog().Debug("init detection: starting scope-aware detection", "scope", scope.String())

	var det Detection
	for _, d := range detectors {
		if !d.detect() {
			getDetectionLog().Debug("init detection: "+d.name+": detected=false", "detector", d.name, "detected", false)
			continue
		}
		getDetectionLog().Debug("init detection: "+d.name+": detected=true", "detector", d.name, "detected", true)

		mgr := d.create()
		if det.HostInitSystem == "" {
			det.HostInitSystem = d.name
		}

		if err := checkUserManagerReachable(scope, d.name, mgr); err != nil {
			return det, err
		}

		if !mgr.SupportsScope(scope) {
			reason := "does not support " + scope.String() + " scope"
			getDetectionLog().Debug("init detection: "+d.name+": skipped: "+reason, "detector", d.name, "scope", scope.String())
			det.Skipped = append(det.Skipped, SkippedDetector{Name: d.name, Reason: reason})
			continue
		}

		det.Manager = mgr
		getDetectionLog().Debug("init detection: selected manager="+d.name, "manager", d.name, "scope", scope.String())
		// Selecting a manager only after skipping the host's own init system is
		// a downgrade, not a detection success: on a host running OpenRC or
		// runit, a user-scope install lands on the crontab fallback, which
		// cannot restart the watchdog. Say so — the alternative is an operator
		// who believes their init system is supervising the service.
		if len(det.Skipped) > 0 {
			logDetectionWarn("init detection: "+d.name+" was selected only because "+
				strings.Join(skippedNames(det.Skipped), ", ")+" cannot register a service in "+scope.String()+
				" scope; this host's init system is not supervising the service",
				"manager", d.name, "scope", scope.String(), "host_init_system", det.HostInitSystem)
		}
		return det, nil
	}

	if len(det.Skipped) > 0 {
		return det, fmt.Errorf("%w: no init system on this host can register a service in %s scope (skipped: %s)",
			ErrScopeNotSupported, scope, strings.Join(skippedNames(det.Skipped), ", "))
	}
	return det, errors.New("no supported service manager detected")
}

func skippedNames(skipped []SkippedDetector) []string {
	names := make([]string, 0, len(skipped))
	for _, s := range skipped {
		names = append(names, s.Name)
	}
	return names
}

// checkUserManagerReachable aborts detection when the host runs systemd but its
// per-user manager cannot be reached. Falling through to the next detector
// would silently swap the supervision model — crontab cannot restart the
// watchdog and only runs the service at boot — so the operator must be told.
func checkUserManagerReachable(scope Scope, name string, mgr ServiceManager) error {
	if scope != ScopeUser || name != "systemd" {
		return nil
	}
	sm, ok := mgr.(*systemdManager)
	if !ok || sm.userManagerReachable() {
		return nil
	}
	u, err := sm.userName()
	if err != nil {
		// The account name is only needed to make the remedy copy-pasteable;
		// the unreachable manager is still the error worth reporting.
		return fmt.Errorf("%w: systemd is running on this host but its per-user manager is not reachable "+
			"from this session; enable it with `loginctl enable-linger <your user>`, or run the command "+
			"from a real user session (`machinectl shell <your user>@`, `systemd-run --user`)",
			ErrUserManagerUnreachable)
	}
	return fmt.Errorf("%w: systemd is running on this host but its per-user manager is not reachable "+
		"from this session; enable it with `loginctl enable-linger %s`, or run the command from a real "+
		"user session (`machinectl shell %s@`, `systemd-run --user`)", ErrUserManagerUnreachable, u, u)
}

// NewServiceManagerByName constructs a manager for an already-known init
// system, without detecting. Lifecycle commands use it so they act on the init
// system the install actually used rather than on whatever the current session
// detects.
func NewServiceManagerByName(name string) (ServiceManager, error) {
	for _, d := range defaultDetectors() {
		if d.name == name {
			return d.create(), nil
		}
	}
	return nil, fmt.Errorf("unknown init system %q", name)
}

// ManagerOptions carries everything a detected manager needs to know before
// lifecycle calls act on it. Fields left zero are not applied, so callers only
// specify what they know.
type ManagerOptions struct {
	// ServiceName is the unit/label/entry name the manager acts on. Required.
	ServiceName string
	// InstallDir is where the service binary lives. Only managers keeping
	// their own state file next to the binary (crontab) use it.
	InstallDir string
	// Scope is the install scope the manager should act in. The zero value,
	// ScopeSystem, leaves every manager behaving exactly as before.
	Scope Scope
	// LogDir is where managers that log to files should write. The zero
	// value keeps each manager's historical location.
	LogDir string
	// LaunchdDomain pins the launchd domain target recorded at install time
	// ("gui/501", "user/501"). Empty leaves the manager to resolve it.
	LaunchdDomain string
	// PreviousLaunchdDomain carries the domain a previous install recorded to
	// the install path, which resolves the domain afresh rather than pinning
	// it. Install boots the old target out so a re-install from a session with
	// different login state cannot leave the earlier agent loaded and
	// unreachable. Only install sets it; lifecycle verbs pin LaunchdDomain.
	PreviousLaunchdDomain string
}

// InitServiceManagerWithOptions binds an already-detected manager to a service
// name and any additional context in opts, so lifecycle calls act on the right
// unit without reinstalling it.
func InitServiceManagerWithOptions(mgr ServiceManager, opts ManagerOptions) error {
	if mgr == nil {
		return errors.New("nil service manager")
	}
	if opts.ServiceName == "" {
		return errors.New("service name is required")
	}
	bindServiceName(mgr, opts.ServiceName)
	if m, ok := mgr.(*crontabManager); ok {
		if opts.InstallDir != "" {
			m.installDir = opts.InstallDir
		}
		m.scope = opts.Scope
		m.logDir = opts.LogDir
	}
	if m, ok := mgr.(*systemdManager); ok {
		m.scope = opts.Scope
	}
	if m, ok := mgr.(*launchdManager); ok {
		m.scope = opts.Scope
		m.logDir = opts.LogDir
		if opts.LaunchdDomain != "" {
			m.domain = opts.LaunchdDomain
		}
		m.previousDomain = opts.PreviousLaunchdDomain
	}
	// Platform managers keeping their manifest beside the binary need the install
	// dir the same way crontab does; they just cannot be named in a type assertion
	// from this file.
	if b, ok := mgr.(installDirBinder); ok && opts.InstallDir != "" {
		b.bindInstallDir(opts.InstallDir)
	}
	return nil
}

// InitServiceManager binds an already-detected manager to a service name, so
// lifecycle calls act on the right unit without reinstalling it.
func InitServiceManager(mgr ServiceManager, name string) {
	bindServiceName(mgr, name)
}

func bindServiceName(mgr ServiceManager, name string) {
	switch m := mgr.(type) {
	case *systemdManager:
		m.serviceName = name
	case *launchdManager:
		m.label = launchdLabel(name)
	case *openrcManager:
		m.serviceName = name
	case *dinitManager:
		m.serviceName = name
	case *finitManager:
		m.serviceName = name
	case *sysvinitManager:
		m.serviceName = name
		if m.statFunc != nil {
			m.supportsKSPrefix = m.detectKSPrefixSupport()
		}
	case *runitManager:
		m.serviceName = name
	case *s6Manager:
		m.serviceName = name
	case *procdManager:
		m.serviceName = name
	case *busyboxManager:
		m.serviceName = name
		if m.statFunc != nil {
			m.supportsKSPrefix = m.detectKSPrefixSupport()
		}
	case *crontabManager:
		m.serviceName = name
	default:
		// A manager whose type lives in a build-tagged file cannot appear in the
		// switch above, so it binds through the interface instead. Without this
		// fallthrough a Windows manager would silently keep an empty service name
		// and every lifecycle call would answer "service name not set".
		if b, ok := mgr.(serviceNameBinder); ok {
			b.bindName(name)
		}
	}
}

// serviceNameBinder is implemented by managers defined in platform-specific files,
// which the type switch in bindServiceName cannot name.
type serviceNameBinder interface {
	bindName(name string)
}

// installDirBinder is the same for managers that keep their manifest beside the
// binary rather than in a system directory.
type installDirBinder interface {
	bindInstallDir(dir string)
}

func detectSystemd() bool {
	if runtime.GOOS != "linux" {
		getDetectionLog().Debug("init detection: systemd: detected=false reason=not linux", "detector", "systemd", "detected", false)
		return false
	}
	mgr := newSystemdManager()
	return mgr.detect()
}

func (m *systemdManager) detect() bool {
	getDetectionLog().Debug("init detection: systemd: checking systemd detection", "detector", "systemd")
	_, err := m.statFunc("/run/systemd/system")
	if err != nil {
		getDetectionLog().Debug("init detection: systemd: stat /run/systemd/system -> "+err.Error(), "detector", "systemd", "path", "/run/systemd/system", "error", err)
		getDetectionLog().Debug("init detection: systemd: detected=false reason=systemd directory not found", "detector", "systemd", "detected", false)
		return false
	}
	getDetectionLog().Debug("init detection: systemd: stat /run/systemd/system -> found", "detector", "systemd", "path", "/run/systemd/system")
	getDetectionLog().Debug("init detection: systemd: detected=true reason=systemd directory found", "detector", "systemd", "detected", true)
	return true
}

func detectLaunchd() bool {
	getDetectionLog().Debug("init detection: launchd: checking launchd detection", "detector", "launchd")
	if runtime.GOOS != "darwin" {
		getDetectionLog().Debug("init detection: launchd: detected=false reason=not darwin", "detector", "launchd", "detected", false)
		return false
	}
	path, err := exec.LookPath("launchctl")
	if err == nil {
		getDetectionLog().Debug("init detection: launchd: lookpath launchctl -> "+path, "detector", "launchd", "path", path)
		getDetectionLog().Debug("init detection: launchd: detected=true reason=launchctl found in PATH", "detector", "launchd", "detected", true)
		return true
	}
	getDetectionLog().Debug("init detection: launchd: lookpath launchctl -> "+err.Error(), "detector", "launchd", "error", err)
	getDetectionLog().Debug("init detection: launchd: detected=false reason=launchctl not found", "detector", "launchd", "detected", false)
	return false
}

func detectOpenRC() bool {
	if runtime.GOOS != "linux" {
		getDetectionLog().Debug("init detection: openrc: detected=false reason=not linux", "detector", "openrc", "detected", false)
		return false
	}
	mgr := newOpenrcManager()
	return mgr.detect()
}

func (m *openrcManager) detect() bool {
	getDetectionLog().Debug("init detection: openrc: checking OpenRC detection", "detector", "openrc")
	path, err := m.lookPathFunc("rc-service")
	if err == nil {
		getDetectionLog().Debug("init detection: openrc: lookpath rc-service -> "+path, "detector", "openrc", "path", path)
		getDetectionLog().Debug("init detection: openrc: detected=true reason=rc-service found in PATH", "detector", "openrc", "detected", true)
		return true
	}
	getDetectionLog().Debug("init detection: openrc: lookpath rc-service -> "+err.Error(), "detector", "openrc", "error", err)

	_, statErr := m.statFunc("/sbin/openrc")
	if statErr == nil {
		getDetectionLog().Debug("init detection: openrc: stat /sbin/openrc -> found", "detector", "openrc", "path", "/sbin/openrc")
		getDetectionLog().Debug("init detection: openrc: detected=true reason=/sbin/openrc found", "detector", "openrc", "detected", true)
		return true
	}
	getDetectionLog().Debug("init detection: openrc: stat /sbin/openrc -> "+statErr.Error(), "detector", "openrc", "path", "/sbin/openrc", "error", statErr)
	getDetectionLog().Debug("init detection: openrc: detected=false reason=rc-service and /sbin/openrc not found", "detector", "openrc", "detected", false)
	return false
}

func detectCrontab() bool {
	mgr := newCrontabManager()
	return mgr.detect()
}

// InitSystemType returns the short identifier of the init system behind mgr.
func InitSystemType(mgr ServiceManager) string {
	return mgr.Name()
}
