//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	// windowsRecoveryResetSeconds is how long a service must stay up before its
	// failure count resets. A day: shorter turns a slow crash-loop into an endless
	// sequence of "first failures", each taking the shortest restart delay.
	windowsRecoveryResetSeconds = 86400
	// windowsStopTimeout bounds the wait for a service to actually stop.
	windowsStopTimeout = 30 * time.Second
	windowsStopPoll    = 250 * time.Millisecond
	// windowsNoError is the exit code the SCM records for a clean stop.
	windowsNoError = 0
)

// windowsManager registers the service with the Service Control Manager. It is the
// thin, untestable-without-Windows half; everything that can be got wrong without
// an SCM — rendering, parsing, command-line quoting — lives in windowsconfig.go so
// its tests run on any host.
//
// Unlike every other manager here it does NOT shell out (no `sc.exe`): the SCM has
// a real API, and parsing `sc query` output is both locale-dependent and a way to
// mistake a failure for a state.
type windowsManager struct {
	serviceName string
	installDir  string
	fs          fileSystem
	readFile    func(string) ([]byte, error)
}

func newWindowsManager() *windowsManager {
	return &windowsManager{
		fs:       &osFileSystem{},
		readFile: os.ReadFile,
	}
}

// bindName satisfies serviceNameBinder. The type switch in bindServiceName cannot
// name a windows-only type, so the binding arrives through this interface instead.
func (m *windowsManager) bindName(name string) { m.serviceName = name }

// bindInstallDir lets ManagerOptions reach the manifest location, which lives
// beside the binary rather than in a system directory.
func (m *windowsManager) bindInstallDir(dir string) { m.installDir = dir }

func (m *windowsManager) Name() string { return windowsInitSystemName }

func (m *windowsManager) Install(config ServiceConfig) error {
	m.serviceName = config.Name
	if config.InstallDir != "" {
		m.installDir = config.InstallDir
	}

	execPath := windowsExecPath(config)
	content, err := GenerateWindowsServiceConfig(config, execPath)
	if err != nil {
		return err
	}
	def, err := parseWindowsServiceConfig(content)
	if err != nil {
		return err
	}
	if err := m.fs.WriteFile(m.GetManifestPath(), []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing service definition: %w", err)
	}
	return m.applyDefinition(def)
}

// applyDefinition creates the service, or converges an existing one onto the
// definition. Converging rather than delete-and-recreate is deliberate: a delete is
// only completed once every handle closes, so a recreate races its own teardown and
// fails with ERROR_SERVICE_MARKED_FOR_DELETE — on a reinstall, which is exactly
// when it must not.
func (m *windowsManager) applyDefinition(def windowsServiceDef) error {
	// Refuse to register a service against an image that is not there. The SCM
	// accepts the registration happily and only complains at START, with "the
	// system cannot find the file specified" — and the install path treats a
	// failed start as a WARNING and reports success. So the whole failure is
	// otherwise invisible: an agent row that never heartbeats, a service that
	// cannot run, and a green installer.
	if def.ImagePath != "" {
		if _, err := os.Stat(def.ImagePath); err != nil {
			return fmt.Errorf(
				"refusing to register service %q against an image that is not there (%s): %w",
				def.Name, def.ImagePath, err)
		}
	}

	scm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connecting to the service control manager (this needs Administrator): %w", err)
	}
	defer scm.Disconnect() //nolint:errcheck // nothing actionable on a close failure

	cfg := mgr.Config{
		ServiceType:    windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:      mgr.StartAutomatic,
		ErrorControl:   mgr.ErrorNormal,
		BinaryPathName: def.BinaryPath,
		DisplayName:    def.DisplayName,
		Description:    def.Description,
	}

	service, err := scm.OpenService(def.Name)
	if err == nil {
		defer service.Close() //nolint:errcheck
		if err := service.UpdateConfig(cfg); err != nil {
			return fmt.Errorf("updating service %q: %w", def.Name, err)
		}
		return m.setRecovery(service)
	}

	// The IMAGE and the ARGUMENTS, separately — never the assembled command line.
	//
	// CreateService IGNORES cfg.BinaryPathName and builds the stored path itself
	// as EscapeArg(exepath) + " " + EscapeArg(arg)..., so handing it the quoted
	// line escapes the WHOLE string into a single token and the SCM ends up
	// looking for a file whose name contains quotes and a space. Registration
	// still succeeds; the only symptom is "The system cannot find the file
	// specified" at START, which reads as a missing binary and sends you to look
	// at a file sitting exactly where it should be.
	//
	// UpdateConfig above is the opposite: it takes cfg.BinaryPathName verbatim as
	// the full command line. The two calls genuinely differ — do not "simplify"
	// them into one shape.
	service, err = scm.CreateService(def.Name, def.ImagePath, cfg, def.Arguments...)
	if err != nil {
		return fmt.Errorf("creating service %q (this needs Administrator): %w", def.Name, err)
	}
	defer service.Close() //nolint:errcheck
	return m.setRecovery(service)
}

// setRecovery is what makes CanRestartWatchdog true, and the second call is the
// one that is easy to miss. By default the SCM applies recovery actions ONLY when
// a service dies without reporting SERVICE_STOPPED — but daemonpilot's watchdog
// exits deliberately and with a meaningful code (a staged update asks to be
// respawned that way). Without SetRecoveryActionsOnNonCrashFailures the SCM treats
// that orderly exit as "it stopped because it meant to" and never restarts it, so
// an auto-update would take the node down instead of rolling it forward.
func (m *windowsManager) setRecovery(service *mgr.Service) error {
	actions := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 1 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}
	if err := service.SetRecoveryActions(actions, windowsRecoveryResetSeconds); err != nil {
		return fmt.Errorf("setting recovery actions: %w", err)
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("enabling recovery on a non-zero orderly exit: %w", err)
	}
	return nil
}

func (m *windowsManager) Uninstall() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	// Stop first: a delete only completes once the service is stopped and every
	// handle is closed, so deleting a running service leaves it marked for deletion
	// and still running until reboot.
	_ = m.Stop()

	if err := m.withService(func(service *mgr.Service) error {
		return service.Delete()
	}); err != nil && !errors.Is(err, errWindowsServiceAbsent) {
		return err
	}

	if err := m.fs.Remove(m.GetManifestPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing service definition: %w", err)
	}
	return nil
}

func (m *windowsManager) Start() error {
	return m.withService(func(service *mgr.Service) error {
		status, err := service.Query()
		if err != nil {
			return fmt.Errorf("querying service: %w", err)
		}
		if status.State == svc.Running || status.State == svc.StartPending {
			return nil // idempotent, like every other manager's Start
		}
		if err := service.Start(); err != nil {
			return fmt.Errorf("starting service: %w", err)
		}
		return nil
	})
}

func (m *windowsManager) Stop() error {
	return m.withService(func(service *mgr.Service) error {
		status, err := service.Query()
		if err != nil {
			return fmt.Errorf("querying service: %w", err)
		}
		if status.State == svc.Stopped {
			return nil
		}
		if _, err := service.Control(svc.Stop); err != nil {
			return fmt.Errorf("stopping service: %w", err)
		}
		// Control returns as soon as the request is delivered. Waiting is what
		// makes Uninstall's delete and a restart work; a caller that raced this
		// would see ERROR_SERVICE_MARKED_FOR_DELETE or a start refused because the
		// old instance still holds the name.
		deadline := time.Now().Add(windowsStopTimeout)
		for time.Now().Before(deadline) {
			status, err := service.Query()
			if err != nil {
				return fmt.Errorf("querying service while stopping: %w", err)
			}
			if status.State == svc.Stopped {
				return nil
			}
			time.Sleep(windowsStopPoll)
		}
		return fmt.Errorf("service %q did not stop within %s", m.serviceName, windowsStopTimeout)
	})
}

func (m *windowsManager) Enable() error  { return m.setStartType(mgr.StartAutomatic) }
func (m *windowsManager) Disable() error { return m.setStartType(mgr.StartDisabled) }

func (m *windowsManager) setStartType(startType uint32) error {
	return m.withService(func(service *mgr.Service) error {
		cfg, err := service.Config()
		if err != nil {
			return fmt.Errorf("reading service config: %w", err)
		}
		cfg.StartType = startType
		if err := service.UpdateConfig(cfg); err != nil {
			return fmt.Errorf("updating service start type: %w", err)
		}
		return nil
	})
}

func (m *windowsManager) Status() (ServiceStatus, error) {
	if m.serviceName == "" {
		return ServiceStatusUnknown, errors.New("service name not set")
	}
	var out ServiceStatus
	err := m.withService(func(service *mgr.Service) error {
		status, qErr := service.Query()
		if qErr != nil {
			return fmt.Errorf("querying service: %w", qErr)
		}
		switch status.State {
		case svc.Running, svc.StartPending, svc.ContinuePending:
			out = ServiceStatusRunning
		case svc.Stopped:
			// A service that stopped with a non-zero code FAILED; reporting it as
			// merely stopped hides a crash-looping node behind a tidy word.
			if status.Win32ExitCode != 0 && status.Win32ExitCode != windowsNoError {
				out = ServiceStatusFailed
				return nil
			}
			out = ServiceStatusStopped
		case svc.StopPending, svc.Paused, svc.PausePending:
			out = ServiceStatusStopped
		default:
			out = ServiceStatusUnknown
		}
		return nil
	})
	if errors.Is(err, errWindowsServiceAbsent) {
		// Not installed is a state, not a failure to find out — the same answer
		// every other manager gives for a missing unit.
		return ServiceStatusStopped, nil
	}
	if err != nil {
		return ServiceStatusUnknown, err
	}
	return out, nil
}

func (m *windowsManager) GetManifestPath() string {
	return windowsManifestPath(m.installDir, m.serviceName)
}

func (m *windowsManager) GenerateConfig(config ServiceConfig) (string, error) {
	m.serviceName = config.Name
	if config.InstallDir != "" {
		m.installDir = config.InstallDir
	}
	return GenerateWindowsServiceConfig(config, windowsExecPath(config))
}

// WriteConfig records the definition AND applies it, because unlike a systemd unit
// this file is not what the system reads — the SCM database is. A write that
// touched only the file would leave the operator looking at a manifest describing a
// service that is running something else.
func (m *windowsManager) WriteConfig(content string) error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	def, err := parseWindowsServiceConfig(content)
	if err != nil {
		return err
	}
	if err := m.fs.WriteFile(m.GetManifestPath(), []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing service definition: %w", err)
	}
	return m.applyDefinition(def)
}

func (m *windowsManager) ReadInstalledConfig() (string, error) {
	if m.serviceName == "" {
		return "", errors.New("service name not set; call Install first or set service name")
	}
	data, err := m.readFile(m.GetManifestPath())
	if err != nil {
		return "", fmt.Errorf("reading service definition: %w", err)
	}
	return string(data), nil
}

// UpdateBinaryPath changes only the executable path, retaining the service
// arguments and any forward-compatible manifest fields.
func (m *windowsManager) UpdateBinaryPath(imagePath string) error {
	content, err := m.ReadInstalledConfig()
	if err != nil {
		return err
	}
	oldDefinition, err := parseWindowsServiceConfig(content)
	if err != nil {
		return err
	}
	updated, err := updateWindowsServiceImagePath(content, imagePath)
	if err != nil {
		return fmt.Errorf("updating service image path: %w", err)
	}
	newDefinition, err := parseWindowsServiceConfig(updated)
	if err != nil {
		return err
	}
	manifestPath := m.GetManifestPath()
	if err := writeWindowsManifestAtomically(manifestPath, updated); err != nil {
		return fmt.Errorf("writing updated service definition: %w", err)
	}
	if err := m.applyDefinition(newDefinition); err != nil {
		fileErr := writeWindowsManifestAtomically(manifestPath, content)
		scmErr := m.applyDefinition(oldDefinition)
		if fileErr != nil || scmErr != nil {
			return errors.Join(
				fmt.Errorf("updating service image path: %w", err),
				wrapOptionalError("restoring the previous service manifest", fileErr),
				wrapOptionalError("restoring the previous SCM image path", scmErr),
			)
		}
		return fmt.Errorf("updating service image path: %w", err)
	}
	return nil
}

func writeWindowsManifestAtomically(path, content string) error {
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("creating temporary service manifest: %w", err)
	}
	tmpPath := file.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if err := file.Chmod(0o644); err != nil {
		_ = file.Close()
		cleanup()
		return fmt.Errorf("setting service manifest permissions: %w", err)
	}
	if _, err := io.WriteString(file, content); err != nil {
		_ = file.Close()
		cleanup()
		return fmt.Errorf("writing temporary service manifest: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		cleanup()
		return fmt.Errorf("syncing temporary service manifest: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return fmt.Errorf("closing temporary service manifest: %w", err)
	}
	from, err := windows.UTF16PtrFromString(tmpPath)
	if err != nil {
		cleanup()
		return fmt.Errorf("encoding temporary service manifest path: %w", err)
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		cleanup()
		return fmt.Errorf("encoding service manifest path: %w", err)
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		cleanup()
		return fmt.Errorf("publishing service manifest: %w", err)
	}
	return nil
}

func wrapOptionalError(message string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", message, err)
}

// CanRestartWatchdog is true because setRecovery configures the SCM to restart the
// service — including after the orderly non-zero exit daemonpilot uses to ask for a
// respawn.
func (m *windowsManager) CanRestartWatchdog() bool { return true }

func (m *windowsManager) SupportsEnable() bool { return true }
func (m *windowsManager) SupportsStart() bool  { return true }
func (m *windowsManager) SupportsStatus() bool { return true }

// SupportsScope: a Windows service is machine-wide. There is no per-user analogue —
// a scheduled task is a different mechanism with different semantics, not a user
// scope for this one — so ScopeUser is refused rather than silently registered
// machine-wide.
func (m *windowsManager) SupportsScope(scope Scope) bool { return scope == ScopeSystem }

// errWindowsServiceAbsent distinguishes "there is no such service" from "the SCM
// could not be asked". Folding them makes an access-denied read as not-installed,
// which turns a permissions problem into a phantom reinstall.
var errWindowsServiceAbsent = errors.New("windows: service is not installed")

// withService opens the service, runs fn, and closes it.
func (m *windowsManager) withService(fn func(*mgr.Service) error) error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	scm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connecting to the service control manager (this needs Administrator): %w", err)
	}
	defer scm.Disconnect() //nolint:errcheck

	service, err := scm.OpenService(m.serviceName)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return fmt.Errorf("%w: %q", errWindowsServiceAbsent, m.serviceName)
		}
		return fmt.Errorf("opening service %q: %w", m.serviceName, err)
	}
	defer service.Close() //nolint:errcheck
	return fn(service)
}

// detectWindows: the SCM is always present on Windows, so there is nothing to probe
// for. The build tag is the detection.
func (m *windowsManager) detect() bool { return true }
