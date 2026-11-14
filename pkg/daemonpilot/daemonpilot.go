package daemonpilot

import (
	"errors"
	"fmt"
	"os"

	"github.com/ovineko/daemonpilot/internal/daemon"
)

var osArgs = func() []string { return os.Args }

// Daemonpilot is the main entry point for a daemonpilot-managed service.
// Create one with [New] and call [Daemonpilot.Run] from your main function.
type Daemonpilot struct {
	config Config
}

// New creates a Daemonpilot instance with the given configuration.
// It applies default values and validates the configuration, returning
// an error if required fields are missing or invalid.
func New(config Config) (*Daemonpilot, error) {
	if err := config.SetDefaults(); err != nil {
		return nil, err
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &Daemonpilot{config: config}, nil
}

// Validate checks that the installed service configuration matches the
// expected state. It loads the saved state file, detects the current init
// system, and compares the installed config against the expected generated
// config (with patchers applied). Returns nil if everything matches.
func (s *Daemonpilot) Validate() error {
	return validateServiceConfig(&s.config)
}

// Install registers the service with the host's init system: it detects the
// init system, copies the running binary into InstallDir, writes the manifest
// (with ServiceEnvironment/ServiceArguments applied), and enables + starts the
// service. It is the programmatic equivalent of the "install" subcommand, for
// callers that wrap it in their own CLI (e.g. after an enrollment step).
func (s *Daemonpilot) Install() error {
	daemon.InitDetectionLogging()
	return handleInstall(&s.config)
}

// Uninstall stops the service and removes it from the host's init system
// (manifest + binary + saved state). Programmatic equivalent of the
// "uninstall" subcommand.
func (s *Daemonpilot) Uninstall() error {
	daemon.InitDetectionLogging()
	return handleUninstall(&s.config)
}

// Run parses the CLI subcommand from os.Args and delegates to the
// appropriate handler (install, uninstall, run, worker, selftest, version,
// or watchdog-version).
func (s *Daemonpilot) Run() error {
	args := osArgs()
	if len(args) < 2 {
		name := "daemonpilot"
		if len(args) > 0 {
			name = args[0]
		}
		printUsage(os.Stderr, name)
		return errors.New("command error: no command specified")
	}
	return s.RunMode(args[1], args[2:]...)
}

// RunMode dispatches mode and args as Run would, without reading or changing os.Args.
// Worker re-execution still uses the worker command; args configure this dispatch only.
func (s *Daemonpilot) RunMode(mode string, args ...string) error {
	daemon.InitDetectionLogging()
	commandArgs := append([]string{s.config.Name, mode}, args...)
	cmd, err := parseCommand(commandArgs)
	if err != nil {
		printUsage(os.Stderr, s.config.Name)
		return fmt.Errorf("command error: %w", err)
	}

	s.config.Logger.Info("executing command", "command", cmd)

	switch cmd {
	case "install":
		return handleInstall(&s.config)
	case "uninstall":
		return handleUninstall(&s.config)
	case "run":
		s.config.ExecThrough = isExecThrough(commandArgs)
		// The supervision run is the only command an init system starts, so it is
		// the only one that has to speak the Windows Service Control Manager
		// protocol — a service that does not report Running within ~30s is killed
		// as unresponsive. process.RunAsService is the identity everywhere else,
		// and on Windows it is the identity too when the process was started from a
		// console, so `<binary> run` in a terminal is unchanged on every platform.
		return runAsServiceFn(s.config.Name, func() error { return handleRun(&s.config) })
	case "worker":
		return handleWorker(&s.config)
	case "selftest":
		return handleSelftest(&s.config)
	case "version":
		return handleVersion(&s.config)
	case "watchdog-version":
		return handleWatchdogVersion(&s.config)
	case "status":
		return handleStatus(&s.config)
	case "start":
		return handleStart(&s.config)
	case "stop":
		return handleStop(&s.config)
	case "restart":
		return handleRestart(&s.config)
	case "enable":
		return handleEnable(&s.config)
	case "disable":
		return handleDisable(&s.config)
	case "validate":
		return handleValidate(&s.config)
	case "update-config":
		return handleUpdateConfig(&s.config)
	default:
		return fmt.Errorf("unknown command: %s", cmd)
	}
}
