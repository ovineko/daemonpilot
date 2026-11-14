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
	"strings"
	"text/template"
)

type dinitManager struct {
	serviceName   string
	reloadPending bool
	executor      commandExecutor
	fs            fileSystem
	statFunc      func(string) (os.FileInfo, error)
	lookPathFunc  func(string) (string, error)
	readFileFunc  func(string) ([]byte, error)
	readlinkFunc  func(string) (string, error)
}

func newDinitManager() *dinitManager {
	return &dinitManager{
		executor:     &execCommandExecutor{},
		fs:           &osFileSystem{},
		statFunc:     os.Stat,
		lookPathFunc: exec.LookPath,
		readFileFunc: os.ReadFile,
		readlinkFunc: os.Readlink,
	}
}

func detectDinit() bool {
	if runtime.GOOS != "linux" {
		getDetectionLog().Debug("init detection: dinit: detected=false reason=not linux", "detector", "dinit", "detected", false)
		return false
	}
	mgr := newDinitManager()
	return mgr.detect()
}

func (m *dinitManager) Name() string {
	return "dinit"
}

func (m *dinitManager) CanRestartWatchdog() bool {
	return true
}

func (m *dinitManager) SupportsEnable() bool { return true }

func (m *dinitManager) SupportsStart() bool  { return true }
func (m *dinitManager) SupportsStatus() bool { return true }

func (m *dinitManager) SupportsScope(scope Scope) bool { return scope == ScopeSystem }

func (m *dinitManager) GetManifestPath() string {
	return "/etc/dinit.d/" + m.serviceName
}

func (m *dinitManager) Install(config ServiceConfig) error {
	m.serviceName = config.Name

	content, err := m.GenerateConfig(config)
	if err != nil {
		return fmt.Errorf("generating dinit service file: %w", err)
	}

	if err := m.fs.MkdirAll("/etc/dinit.d", 0o755); err != nil {
		return fmt.Errorf("creating /etc/dinit.d directory: %w", err)
	}

	if err := m.fs.WriteFile(m.GetManifestPath(), []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing dinit service file: %w", err)
	}

	if len(config.Environment) > 0 {
		envContent := formatLineEnvironment(config.Environment)
		if err := m.fs.WriteFile(dinitEnvFilePath(config.Name), []byte(envContent), 0o600); err != nil {
			_ = m.fs.Remove(m.GetManifestPath())
			return fmt.Errorf("writing dinit env file: %w", err)
		}
	}

	return nil
}

func (m *dinitManager) Uninstall() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}

	_, _ = m.executor.Run("dinitctl", "stop", m.serviceName)
	if out, err := m.executor.Run("dinitctl", "disable", m.serviceName); err != nil {
		return runErr("disabling service", out, err)
	}
	if out, err := m.executor.Run("dinitctl", "unload", m.serviceName); err != nil {
		return runErr("unloading service", out, err)
	}

	if err := m.fs.Remove(m.GetManifestPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing dinit service file: %w", err)
	}

	envPath := dinitEnvFilePath(m.serviceName)
	if err := m.fs.Remove(envPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing dinit env file: %w", err)
	}

	return nil
}

const dinitServiceTemplate = `type = process
command = {{.Command}}
{{- if .WorkingDir}}
working-dir = {{.WorkingDir}}
{{- end}}
{{- if .User}}
run-as = {{.User}}
{{- end}}
logfile = {{.LogFile}}
restart = true
smooth-recovery = true
{{- range .EnvLines}}
env-file = {{.}}
{{- end}}
{{- range .Dependencies}}
depends-on = {{.}}
{{- end}}
`

type dinitTemplateData struct {
	Command      string
	WorkingDir   string
	User         string
	LogFile      string
	EnvLines     []string
	Dependencies []string
}

func quoteIfNeeded(s string) string {
	if s == "" || strings.ContainsAny(s, " \t\v\f#\"'\\") {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
	}
	return s
}

func formatDinitCommand(execPath string, args []string) string {
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, quoteIfNeeded(strings.ReplaceAll(execPath, "$", "$$")))
	for _, a := range args {
		parts = append(parts, quoteIfNeeded(strings.ReplaceAll(a, "$", "$$")))
	}
	return strings.Join(parts, " ")
}

func dinitEnvFilePath(name string) string {
	return "/etc/dinit.d/" + name + ".env"
}

func buildDinitTemplateData(config ServiceConfig, execPath string) dinitTemplateData {
	data := dinitTemplateData{
		Command:    formatDinitCommand(execPath, config.Arguments),
		WorkingDir: config.WorkingDir,
		User:       config.User,
		LogFile:    fmt.Sprintf("/var/log/%s.log", config.Name),
	}
	if config.WorkingDir != "" {
		data.WorkingDir = quoteIfNeeded(strings.ReplaceAll(config.WorkingDir, "$", "$$"))
	}
	if config.User != "" {
		data.User = quoteIfNeeded(config.User)
	}
	if len(config.Environment) > 0 {
		data.EnvLines = []string{dinitEnvFilePath(config.Name)}
	}
	return data
}

// GenerateDinitServiceFile renders the /etc/dinit.d service description for the
// configured service.
func GenerateDinitServiceFile(config ServiceConfig, execPath string) (string, error) {
	if err := validateServiceText(config, execPath, "dinit"); err != nil {
		return "", err
	}
	if err := validateLineEnvironment(config.Environment); err != nil {
		return "", fmt.Errorf("dinit: %w", err)
	}
	data := buildDinitTemplateData(config, execPath)

	tmpl, err := template.New("dinit").Parse(dinitServiceTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing dinit template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing dinit template: %w", err)
	}
	return buf.String(), nil
}

func (m *dinitManager) Start() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	if !m.hasDinitctl() {
		return errors.New("dinitctl command not found")
	}
	if m.reloadPending {
		if out, err := m.executor.Run("dinitctl", "reload", m.serviceName); err != nil {
			return runErr("reloading dinit configuration", out, err)
		}
		m.reloadPending = false
	}
	if out, err := m.executor.Run("dinitctl", "start", m.serviceName); err != nil {
		return runErr("starting service", out, err)
	}
	return nil
}

func (m *dinitManager) Stop() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	if !m.hasDinitctl() {
		return errors.New("dinitctl command not found")
	}
	if out, err := m.executor.Run("dinitctl", "stop", m.serviceName); err != nil {
		return runErr("stopping service", out, err)
	}
	return nil
}

func (m *dinitManager) Enable() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	if !m.hasDinitctl() {
		return errors.New("dinitctl command not found")
	}
	if out, err := m.executor.Run("dinitctl", "enable", m.serviceName); err != nil {
		return runErr("enabling service", out, err)
	}
	return nil
}

func (m *dinitManager) Disable() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	if !m.hasDinitctl() {
		return errors.New("dinitctl command not found")
	}
	if out, err := m.executor.Run("dinitctl", "disable", m.serviceName); err != nil {
		return runErr("disabling service", out, err)
	}
	return nil
}

func (m *dinitManager) Status() (ServiceStatus, error) {
	if m.serviceName == "" {
		return ServiceStatusUnknown, errors.New("service name not set; call Install first or set service name")
	}
	if !m.hasDinitctl() {
		return ServiceStatusUnknown, errors.New("dinitctl command not found")
	}
	output, err := m.executor.Run("dinitctl", "status", m.serviceName)
	if err != nil {
		return ServiceStatusUnknown, nil //nolint:nilerr // a failed status query means the init system does not know this service
	}
	line := strings.ToLower(strings.TrimSpace(string(output)))
	switch {
	case strings.Contains(line, "started"):
		return ServiceStatusRunning, nil
	case strings.Contains(line, "stopped"):
		return ServiceStatusStopped, nil
	case strings.Contains(line, "starting"):
		return ServiceStatusRunning, nil
	case strings.Contains(line, "stopping"):
		return ServiceStatusStopped, nil
	default:
		return ServiceStatusUnknown, nil
	}
}

func (m *dinitManager) GenerateConfig(config ServiceConfig) (string, error) {
	m.serviceName = config.Name
	return GenerateDinitServiceFile(config, m.execPath(config))
}

func (m *dinitManager) ReadEnvironmentFile() ([]byte, error) {
	if m.serviceName == "" {
		return nil, errors.New("service name not set; call Install first or set service name")
	}
	envPath := dinitEnvFilePath(m.serviceName)
	data, err := m.readFileFunc(envPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading dinit env file: %w", err)
	}
	return data, nil
}

func (m *dinitManager) RestoreEnvironmentFile(content []byte) error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	m.reloadPending = true
	envPath := dinitEnvFilePath(m.serviceName)
	if content == nil {
		if err := m.fs.Remove(envPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing dinit env file: %w", err)
		}
		return nil
	}
	if err := m.fs.WriteFile(envPath, content, 0o600); err != nil {
		return fmt.Errorf("restoring dinit env file: %w", err)
	}
	return nil
}

func (m *dinitManager) WriteEnvironmentFile(env map[string]string) error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	if err := validateLineEnvironment(env); err != nil {
		return err
	}
	m.reloadPending = true
	envPath := dinitEnvFilePath(m.serviceName)
	if len(env) == 0 {
		if err := m.fs.Remove(envPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing dinit env file: %w", err)
		}
		return nil
	}
	envContent := formatLineEnvironment(env)
	if err := m.fs.WriteFile(envPath, []byte(envContent), 0o600); err != nil {
		return fmt.Errorf("writing dinit env file: %w", err)
	}
	return nil
}

func (m *dinitManager) WriteConfig(content string) error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	if err := m.fs.MkdirAll("/etc/dinit.d", 0o755); err != nil {
		return fmt.Errorf("creating /etc/dinit.d directory: %w", err)
	}
	if err := m.fs.WriteFile(m.GetManifestPath(), []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing dinit service file: %w", err)
	}
	m.reloadPending = true
	return nil
}

func (m *dinitManager) ReadInstalledConfig() (string, error) {
	if m.serviceName == "" {
		return "", errors.New("service name not set; call Install first or set service name")
	}
	data, err := m.readFileFunc(m.GetManifestPath())
	if err != nil {
		return "", fmt.Errorf("reading dinit service file: %w", err)
	}
	return string(data), nil
}

func (m *dinitManager) detect() bool {
	log := getDetectionLog()
	log.Debug("init detection: dinit: checking dinit detection", "detector", "dinit")

	if _, err := m.statFunc("/etc/dinit.d"); err == nil {
		log.Debug("init detection: dinit: stat /etc/dinit.d -> found", "detector", "dinit", "path", "/etc/dinit.d")
		procTarget, procErr := m.readlinkFunc("/proc/1/exe")
		if procErr == nil && strings.Contains(strings.ToLower(procTarget), "dinit") {
			log.Debug("init detection: dinit: detected=true reason=/etc/dinit.d found", "detector", "dinit", "detected", true)
			return true
		}
		if procErr != nil {
			log.Debug("init detection: dinit: /etc/dinit.d found but cannot read /proc/1/exe", "detector", "dinit", "error", procErr)
		} else {
			log.Debug("init detection: dinit: /etc/dinit.d found but PID 1 is not dinit", "detector", "dinit", "pid1", procTarget)
		}
	} else {
		log.Debug("init detection: dinit: stat /etc/dinit.d -> "+err.Error(), "detector", "dinit", "path", "/etc/dinit.d", "error", err)
	}

	if path, err := m.lookPathFunc("dinitctl"); err == nil {
		log.Debug("init detection: dinit: lookpath dinitctl -> "+path, "detector", "dinit", "path", path)
		procTarget, procErr := m.readlinkFunc("/proc/1/exe")
		if procErr == nil && strings.Contains(strings.ToLower(procTarget), "dinit") {
			log.Debug("init detection: dinit: detected=true reason=dinitctl found in PATH", "detector", "dinit", "detected", true)
			return true
		}
		log.Debug("init detection: dinit: dinitctl found but PID 1 is not dinit", "detector", "dinit", "pid1", procTarget)
	} else {
		log.Debug("init detection: dinit: lookpath dinitctl -> "+err.Error(), "detector", "dinit", "error", err)
	}

	_, statErr := m.statFunc("/sbin/dinit")
	if statErr == nil {
		log.Debug("init detection: dinit: stat /sbin/dinit -> found", "detector", "dinit", "path", "/sbin/dinit")
		procTarget, err := m.readlinkFunc("/proc/1/exe")
		if err == nil && strings.Contains(strings.ToLower(procTarget), "dinit") {
			log.Debug("init detection: dinit: /proc/1/exe points to dinit", "detector", "dinit", "target", procTarget, "detected", true)
			return true
		}
		log.Debug("init detection: dinit: found /sbin/dinit but not running as init", "detector", "dinit", "detected", false)
		return false
	}
	log.Debug("init detection: dinit: stat /sbin/dinit -> "+statErr.Error(), "detector", "dinit", "path", "/sbin/dinit", "error", statErr)

	log.Debug("init detection: dinit: detected=false reason=dinit not found", "detector", "dinit", "detected", false)
	return false
}

func (m *dinitManager) execPath(config ServiceConfig) string {
	if config.ExecPath != "" {
		return config.ExecPath
	}
	installDir := config.InstallDir
	if installDir == "" {
		installDir = filepath.Join("/opt", config.Name)
	}
	return filepath.Join(installDir, config.Name)
}

func (m *dinitManager) hasDinitctl() bool {
	_, err := m.lookPathFunc("dinitctl")
	return err == nil
}
