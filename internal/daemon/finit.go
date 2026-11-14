package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/template"
)

type finitManager struct {
	serviceName   string
	reloadPending bool
	executor      commandExecutor
	fs            fileSystem
	statFunc      func(string) (os.FileInfo, error)
	lookPathFunc  func(string) (string, error)
	readFileFunc  func(string) ([]byte, error)
	readlinkFunc  func(string) (string, error)
	symlinkFunc   func(string, string) error
}

func newFinitManager() *finitManager {
	return &finitManager{
		executor:     &execCommandExecutor{},
		fs:           &osFileSystem{},
		statFunc:     os.Stat,
		lookPathFunc: exec.LookPath,
		readFileFunc: os.ReadFile,
		readlinkFunc: os.Readlink,
		symlinkFunc:  os.Symlink,
	}
}

func detectFinit() bool {
	if runtime.GOOS != "linux" {
		getDetectionLog().Debug("init detection: finit: detected=false reason=not linux", "detector", "finit", "detected", false)
		return false
	}
	mgr := newFinitManager()
	return mgr.detect()
}

func (m *finitManager) Name() string {
	return "finit"
}

func (m *finitManager) CanRestartWatchdog() bool {
	return true
}

func (m *finitManager) SupportsEnable() bool { return true }

func (m *finitManager) SupportsStart() bool  { return true }
func (m *finitManager) SupportsStatus() bool { return true }

func (m *finitManager) SupportsScope(scope Scope) bool { return scope == ScopeSystem }

func (m *finitManager) GetManifestPath() string {
	return "/etc/finit.d/available/" + m.serviceName + ".conf"
}

func (m *finitManager) Install(config ServiceConfig) error {
	m.serviceName = config.Name

	content, err := m.GenerateConfig(config)
	if err != nil {
		return fmt.Errorf("generating finit service file: %w", err)
	}

	if err := m.fs.MkdirAll("/etc/finit.d/available", 0o755); err != nil {
		return fmt.Errorf("creating /etc/finit.d/available directory: %w", err)
	}

	if err := m.fs.WriteFile(m.GetManifestPath(), []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing finit service file: %w", err)
	}

	return m.WriteEnvironmentFile(config.Environment)
}

func (m *finitManager) Uninstall() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}

	if m.hasInitctlFinit() {
		_, _ = m.executor.Run("initctl", "stop", m.serviceName)
	}

	enabledPath := "/etc/finit.d/enabled/" + m.serviceName + ".conf"
	if err := m.fs.Remove(enabledPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing finit enabled symlink: %w", err)
	}

	if m.hasInitctlFinit() {
		_, _ = m.executor.Run("initctl", "reload")
	}

	if err := m.fs.Remove(m.GetManifestPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing finit service file: %w", err)
	}

	if err := m.fs.Remove(m.envPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing finit environment file: %w", err)
	}
	return nil
}

const finitServiceTemplate = `# Service configuration for {{.Name}}
service [{{.Runlevels}}]{{if .Wrapped}} name:{{.Name}}{{end}} {{.Command}} -- {{.Description}}
`

type finitTemplateData struct {
	Name        string
	Runlevels   string
	Command     string
	Description string
	Wrapped     bool
}

func formatFinitCommand(execPath string, args []string) string {
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, quoteFinitArg(execPath))
	for _, a := range args {
		parts = append(parts, quoteFinitArg(a))
	}
	return strings.Join(parts, " ")
}

func buildFinitTemplateData(config ServiceConfig, execPath string) finitTemplateData {
	desc := config.Description
	if desc == "" {
		desc = config.Name + " service"
	}
	command := formatFinitCommand(execPath, config.Arguments)
	// Finit's own env: files expand $VAR and run `cmd` and $(cmd) inside values,
	// and trim or re-split the rest, so a value is never handed to that parser.
	// The environment is a POSIX shell fragment the wrapper sources instead.
	script := ""
	if env := finitEnvFile(config); env != "" {
		script = ". " + env + "&&"
	}
	if config.WorkingDir != "" {
		script += `cd "$1"&&shift&&`
	}
	if script != "" {
		args := []string{"-c", script + `exec "$@"`, "daemonpilot"}
		if config.WorkingDir != "" {
			args = append(args, config.WorkingDir)
		}
		args = append(append(args, execPath), config.Arguments...)
		command = formatFinitCommand("/bin/sh", args)
	}
	return finitTemplateData{
		Name:        config.Name,
		Runlevels:   "234",
		Command:     command,
		Wrapped:     script != "",
		Description: desc,
	}
}

// GenerateFinitServiceFile renders the /etc/finit.d configuration stanza for the
// configured service.
func GenerateFinitServiceFile(config ServiceConfig, execPath string) (string, error) {
	if err := validateServiceText(config, execPath, "finit"); err != nil {
		return "", err
	}
	if err := validateLineEnvironment(config.Environment); err != nil {
		return "", fmt.Errorf("finit: %w", err)
	}
	if err := validateFinitCommand(config, execPath); err != nil {
		return "", err
	}
	data := buildFinitTemplateData(config, execPath)

	tmpl, err := template.New("finit").Parse(finitServiceTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing finit template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing finit template: %w", err)
	}
	return buf.String(), nil
}

func (m *finitManager) Start() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	if !m.hasInitctlFinit() {
		return errors.New("initctl command not found")
	}
	if m.reloadPending {
		if out, err := m.executor.Run("initctl", "reload"); err != nil {
			return runErr("reloading finit configuration", out, err)
		}
		m.reloadPending = false
	}
	if out, err := m.executor.Run("initctl", "start", m.serviceName); err != nil {
		return runErr("starting service", out, err)
	}
	return nil
}

func (m *finitManager) Stop() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	if !m.hasInitctlFinit() {
		return errors.New("initctl command not found")
	}
	if out, err := m.executor.Run("initctl", "stop", m.serviceName); err != nil {
		return runErr("stopping service", out, err)
	}
	return nil
}

func (m *finitManager) Enable() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	if _, err := m.statFunc(m.GetManifestPath()); err != nil {
		return fmt.Errorf("service file not found for %s", m.serviceName)
	}
	if err := m.fs.MkdirAll("/etc/finit.d/enabled", 0o755); err != nil {
		return fmt.Errorf("creating /etc/finit.d/enabled directory: %w", err)
	}
	_ = m.fs.Remove(m.enabledSymlink())
	if err := m.symlinkFunc(m.GetManifestPath(), m.enabledSymlink()); err != nil {
		return fmt.Errorf("enabling service: %w", err)
	}
	if m.hasInitctlFinit() {
		if out, err := m.executor.Run("initctl", "reload"); err != nil {
			return runErr("reloading finit configuration", out, err)
		}
	}
	return nil
}

func (m *finitManager) Disable() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	if err := m.fs.Remove(m.enabledSymlink()); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("disabling service: %w", err)
	}
	if m.hasInitctlFinit() {
		if out, err := m.executor.Run("initctl", "reload"); err != nil {
			return runErr("reloading finit configuration", out, err)
		}
	}
	return nil
}

func (m *finitManager) Status() (ServiceStatus, error) {
	if m.serviceName == "" {
		return ServiceStatusUnknown, errors.New("service name not set; call Install first or set service name")
	}
	if !m.hasInitctlFinit() {
		return ServiceStatusUnknown, errors.New("initctl command not found")
	}
	output, err := m.executor.Run("initctl", "status", m.serviceName)
	if err != nil {
		return ServiceStatusUnknown, nil //nolint:nilerr // a failed status query means the init system does not know this service
	}
	line := strings.ToLower(strings.TrimSpace(string(output)))
	switch {
	case strings.Contains(line, "running"):
		return ServiceStatusRunning, nil
	case strings.Contains(line, "stopped"):
		return ServiceStatusStopped, nil
	default:
		return ServiceStatusUnknown, nil
	}
}

func (m *finitManager) GenerateConfig(config ServiceConfig) (string, error) {
	m.serviceName = config.Name
	return GenerateFinitServiceFile(config, m.execPath(config))
}

func (m *finitManager) WriteConfig(content string) error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	if err := m.fs.MkdirAll("/etc/finit.d/available", 0o755); err != nil {
		return fmt.Errorf("creating /etc/finit.d/available directory: %w", err)
	}
	if err := m.fs.WriteFile(m.GetManifestPath(), []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing finit config: %w", err)
	}
	m.reloadPending = true
	return nil
}

func (m *finitManager) ReadInstalledConfig() (string, error) {
	if m.serviceName == "" {
		return "", errors.New("service name not set; call Install first or set service name")
	}
	data, err := m.readFileFunc(m.GetManifestPath())
	if err != nil {
		return "", fmt.Errorf("reading finit service file: %w", err)
	}
	return string(data), nil
}

func (m *finitManager) ReadEnvironmentFile() ([]byte, error) {
	if m.serviceName == "" {
		return nil, errors.New("service name not set")
	}
	data, err := m.readFileFunc(m.envPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading finit environment file: %w", err)
	}
	return data, nil
}

func (m *finitManager) RestoreEnvironmentFile(content []byte) error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if content == nil {
		if err := m.fs.Remove(m.envPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing finit environment file: %w", err)
		}
	} else if err := m.fs.WriteFile(m.envPath(), content, 0o600); err != nil {
		return fmt.Errorf("writing finit environment file: %w", err)
	}
	m.reloadPending = true
	return nil
}

func (m *finitManager) WriteEnvironmentFile(env map[string]string) error {
	if err := validateLineEnvironment(env); err != nil {
		return err
	}
	if len(env) == 0 {
		return m.RestoreEnvironmentFile(nil)
	}
	return m.RestoreEnvironmentFile([]byte(formatFinitEnvironment(env)))
}

func (m *finitManager) detect() bool {
	log := getDetectionLog()
	log.Debug("init detection: finit: checking finit detection", "detector", "finit")

	if _, err := m.statFunc("/etc/finit.d"); err == nil {
		log.Debug("init detection: finit: stat /etc/finit.d -> found", "detector", "finit", "path", "/etc/finit.d")
		procTarget, procErr := m.readlinkFunc("/proc/1/exe")
		if procErr == nil && strings.Contains(strings.ToLower(procTarget), "finit") {
			log.Debug("init detection: finit: detected=true reason=/etc/finit.d found", "detector", "finit", "detected", true)
			return true
		}
		if procErr != nil {
			log.Debug("init detection: finit: /etc/finit.d found but cannot read /proc/1/exe", "detector", "finit", "error", procErr)
		} else {
			log.Debug("init detection: finit: /etc/finit.d found but PID 1 is not finit", "detector", "finit", "pid1", procTarget)
		}
	} else {
		log.Debug("init detection: finit: stat /etc/finit.d -> "+err.Error(), "detector", "finit", "path", "/etc/finit.d", "error", err)
	}

	if _, err := m.statFunc("/etc/finit.conf"); err == nil {
		log.Debug("init detection: finit: stat /etc/finit.conf -> found", "detector", "finit", "path", "/etc/finit.conf")
		procTarget, procErr := m.readlinkFunc("/proc/1/exe")
		if procErr == nil && strings.Contains(strings.ToLower(procTarget), "finit") {
			log.Debug("init detection: finit: detected=true reason=/etc/finit.conf found", "detector", "finit", "detected", true)
			return true
		}
		if procErr != nil {
			log.Debug("init detection: finit: /etc/finit.conf found but cannot read /proc/1/exe", "detector", "finit", "error", procErr)
		} else {
			log.Debug("init detection: finit: /etc/finit.conf found but PID 1 is not finit", "detector", "finit", "pid1", procTarget)
		}
	} else {
		log.Debug("init detection: finit: stat /etc/finit.conf -> "+err.Error(), "detector", "finit", "path", "/etc/finit.conf", "error", err)
	}

	_, statErr := m.statFunc("/sbin/finit")
	if statErr == nil {
		log.Debug("init detection: finit: stat /sbin/finit -> found", "detector", "finit", "path", "/sbin/finit")
		procTarget, err := m.readlinkFunc("/proc/1/exe")
		if err == nil && strings.Contains(strings.ToLower(procTarget), "finit") {
			log.Debug("init detection: finit: /proc/1/exe points to finit", "detector", "finit", "target", procTarget, "detected", true)
			return true
		}
		log.Debug("init detection: finit: found /sbin/finit but not running as init", "detector", "finit", "detected", false)
		return false
	}
	log.Debug("init detection: finit: stat /sbin/finit -> "+statErr.Error(), "detector", "finit", "path", "/sbin/finit", "error", statErr)

	log.Debug("init detection: finit: detected=false reason=finit not found", "detector", "finit", "detected", false)
	return false
}

func (m *finitManager) execPath(config ServiceConfig) string {
	if config.ExecPath != "" {
		return config.ExecPath
	}
	installDir := config.InstallDir
	if installDir == "" {
		installDir = filepath.Join("/opt", config.Name)
	}
	return filepath.Join(installDir, config.Name)
}

func (m *finitManager) hasInitctlFinit() bool {
	_, err := m.lookPathFunc("initctl")
	return err == nil
}

func (m *finitManager) enabledSymlink() string {
	return "/etc/finit.d/enabled/" + m.serviceName + ".conf"
}

func finitEnvFile(config ServiceConfig) string {
	if len(config.Environment) == 0 {
		return ""
	}
	return "/etc/finit.d/available/" + config.Name + ".env"
}

func (m *finitManager) envPath() string { return "/etc/finit.d/available/" + m.serviceName + ".env" }

func quoteFinitArg(value string) string {
	if !strings.HasSuffix(value, "--") && shellSafeRe.MatchString(value) {
		return value
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(value) + `"`
}

// formatFinitEnvironment renders the environment as a POSIX shell fragment
// sourced by the service wrapper; single quotes keep every byte literal.
func formatFinitEnvironment(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString("export " + key + "=" + shellQuote(env[key]) + "\n")
	}
	return b.String()
}
