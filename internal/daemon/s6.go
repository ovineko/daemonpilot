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

type s6Manager struct {
	serviceName  string
	scanDir      string
	serviceBase  string
	executor     commandExecutor
	fs           fileSystem
	statFunc     func(string) (os.FileInfo, error)
	lookPathFunc func(string) (string, error)
	readFileFunc func(string) ([]byte, error)
	readlinkFunc func(string) (string, error)
	symlinkFunc  func(string, string) error
	removeFunc   func(string) error
}

func newS6Manager() *s6Manager {
	return &s6Manager{
		scanDir:      "/var/service",
		serviceBase:  "/etc/s6/sv",
		executor:     &execCommandExecutor{},
		fs:           &osFileSystem{},
		statFunc:     os.Stat,
		lookPathFunc: exec.LookPath,
		readFileFunc: os.ReadFile,
		readlinkFunc: os.Readlink,
		symlinkFunc:  os.Symlink,
		removeFunc:   os.Remove,
	}
}

func (m *s6Manager) Install(config ServiceConfig) error {
	m.serviceName = config.Name

	if err := m.fs.MkdirAll(m.serviceDir(), 0o755); err != nil {
		return fmt.Errorf("creating service directory: %w", err)
	}

	content, err := GenerateS6RunScript(config, m.execPath(config))
	if err != nil {
		return fmt.Errorf("generating run script: %w", err)
	}

	if err := m.fs.WriteFile(m.runScript(), []byte(content), 0o755); err != nil {
		return fmt.Errorf("writing run script: %w", err)
	}

	if m.hasS6RC() {
		if err := m.fs.WriteFile(m.typeFile(), []byte("longrun\n"), 0o644); err != nil {
			return fmt.Errorf("writing type file: %w", err)
		}
		// s6-rc-update requires a compiled database path that varies by system;
		// best-effort: ignore errors since the service directory is already in place.
		_, _ = m.executor.Run("s6-rc-update", m.scanDir)
	}

	if err := m.symlinkFunc(m.serviceDir(), m.activeSymlink()); err != nil && !os.IsExist(err) {
		return fmt.Errorf("activating service: %w", err)
	}

	return nil
}

func (m *s6Manager) Uninstall() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}

	// Stop waits for the supervised process to go down; unlinking the service while
	// s6-supervise is still stopping it races the directory removal below.
	_ = m.Stop()

	_ = m.removeFunc(m.activeSymlink())

	if m.hasS6RC() {
		if err := m.fs.Remove(m.typeFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing type file: %w", err)
		}
		_, _ = m.executor.Run("s6-rc-update", m.scanDir)
	}

	if err := m.fs.Remove(m.runScript()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing run script: %w", err)
	}

	if err := m.fs.RemoveAll(m.serviceDir()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing service directory: %w", err)
	}

	return nil
}

func (m *s6Manager) Start() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	return startSupervisedService(m.executor, supervisionStartTimeout, supervisionStartInterval, "s6-svc", "-u", m.activeSymlink())
}

func (m *s6Manager) Stop() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if out, err := m.executor.Run("s6-svc", "-wD", "-T", "30000", "-d", m.activeSymlink()); err != nil {
		return runErr("stopping service", out, err)
	}
	return nil
}

func (m *s6Manager) Enable() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if _, err := m.statFunc(m.activeSymlink()); err == nil {
		return nil
	}
	if _, err := m.statFunc(m.serviceDir()); err != nil {
		return fmt.Errorf("service directory not found for %s", m.serviceName)
	}
	if err := m.symlinkFunc(m.serviceDir(), m.activeSymlink()); err != nil {
		return fmt.Errorf("enabling service: %w", err)
	}
	return nil
}

const s6RunScriptTemplate = `#!/bin/sh
{{- range .EnvLines}}
export {{.}}
{{- end}}
{{- if .WorkingDir}}
cd {{.WorkingDir}} || exit 1
{{- end}}
exec {{.ExecPath}}{{if .Args}} {{.Args}}{{end}}
`

type s6TemplateData struct {
	ExecPath   string
	WorkingDir string
	Name       string
	Args       string
	EnvLines   []string
}

func buildS6TemplateData(config ServiceConfig, execPath string) s6TemplateData {
	data := s6TemplateData{
		ExecPath: shellQuote(execPath),
		Name:     config.Name,
	}
	if config.WorkingDir != "" {
		data.WorkingDir = shellQuote(config.WorkingDir)
	}
	if len(config.Arguments) > 0 {
		quoted := make([]string, len(config.Arguments))
		for i, a := range config.Arguments {
			quoted[i] = shellQuote(a)
		}
		data.Args = strings.Join(quoted, " ")
	}
	if len(config.Environment) > 0 {
		keys := make([]string, 0, len(config.Environment))
		for k := range config.Environment {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			data.EnvLines = append(data.EnvLines, fmt.Sprintf("%s=%s", k, shellQuote(config.Environment[k])))
		}
	}
	return data
}

// GenerateS6RunScript renders the s6 run script that execs the service in the
// foreground.
func GenerateS6RunScript(config ServiceConfig, execPath string) (string, error) {
	if err := validateServiceText(config, execPath, "s6"); err != nil {
		return "", err
	}
	if err := validateEnvKeys(config.Environment); err != nil {
		return "", fmt.Errorf("s6: %w", err)
	}
	data := buildS6TemplateData(config, execPath)

	tmpl, err := template.New("s6").Parse(s6RunScriptTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing s6 template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing s6 template: %w", err)
	}
	return buf.String(), nil
}

func detectS6() bool {
	if runtime.GOOS != "linux" {
		getDetectionLog().Debug("init detection: s6: detected=false reason=not linux", "detector", "s6", "detected", false)
		return false
	}
	mgr := newS6Manager()
	return mgr.detect()
}

func (m *s6Manager) Disable() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if _, err := m.statFunc(m.activeSymlink()); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := m.removeFunc(m.activeSymlink()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("disabling service: %w", err)
	}
	return nil
}

func (m *s6Manager) Status() (ServiceStatus, error) {
	if m.serviceName == "" {
		return ServiceStatusUnknown, errors.New("service name not set")
	}
	output, err := m.executor.Run("s6-svstat", m.activeSymlink())
	if err != nil {
		return ServiceStatusUnknown, nil //nolint:nilerr // a failed status query means the init system does not know this service
	}
	line := strings.ToLower(strings.TrimSpace(string(output)))
	if strings.Contains(line, "up ") {
		return ServiceStatusRunning, nil
	}
	if strings.Contains(line, "down ") {
		return ServiceStatusStopped, nil
	}
	return ServiceStatusUnknown, nil
}

func (m *s6Manager) GetManifestPath() string {
	return m.runScript()
}

func (m *s6Manager) GenerateConfig(config ServiceConfig) (string, error) {
	m.serviceName = config.Name
	return GenerateS6RunScript(config, m.execPath(config))
}

func (m *s6Manager) WriteConfig(content string) error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	if err := m.fs.MkdirAll(m.serviceDir(), 0o755); err != nil {
		return fmt.Errorf("creating service directory: %w", err)
	}
	if err := m.fs.WriteFile(m.runScript(), []byte(content), 0o755); err != nil {
		return fmt.Errorf("writing run script: %w", err)
	}
	return nil
}

func (m *s6Manager) ReadInstalledConfig() (string, error) {
	if m.serviceName == "" {
		return "", errors.New("service name not set; call Install first or set service name")
	}
	data, err := m.readFileFunc(m.runScript())
	if err != nil {
		return "", fmt.Errorf("reading run script: %w", err)
	}
	return string(data), nil
}

func (m *s6Manager) CanRestartWatchdog() bool {
	return true
}

func (m *s6Manager) SupportsEnable() bool { return true }

func (m *s6Manager) SupportsStart() bool { return true }

func (m *s6Manager) SupportsStatus() bool { return true }

func (m *s6Manager) SupportsScope(scope Scope) bool { return scope == ScopeSystem }

func (m *s6Manager) Name() string {
	return "s6"
}

func (m *s6Manager) serviceDir() string {
	return filepath.Join(m.serviceBase, m.serviceName)
}

func (m *s6Manager) runScript() string {
	return filepath.Join(m.serviceBase, m.serviceName, "run")
}

func (m *s6Manager) typeFile() string {
	return filepath.Join(m.serviceBase, m.serviceName, "type")
}

func (m *s6Manager) activeSymlink() string {
	return filepath.Join(m.scanDir, m.serviceName)
}

func (m *s6Manager) execPath(config ServiceConfig) string {
	if config.ExecPath != "" {
		return config.ExecPath
	}
	installDir := config.InstallDir
	if installDir == "" {
		installDir = filepath.Join("/opt", config.Name)
	}
	return filepath.Join(installDir, config.Name)
}

func (m *s6Manager) detect() bool {
	log := getDetectionLog()
	log.Debug("init detection: s6: checking s6 detection", "detector", "s6")
	if path, err := m.lookPathFunc("s6-svscan"); err == nil {
		log.Debug("init detection: s6: lookpath s6-svscan -> "+path, "detector", "s6", "path", path)
		procTarget, procErr := m.readlinkFunc("/proc/1/exe")
		if procErr != nil || strings.Contains(strings.ToLower(procTarget), "s6") {
			log.Debug("init detection: s6: detected=true reason=s6-svscan found in PATH", "detector", "s6", "detected", true)
			return true
		}
		log.Debug("init detection: s6: s6-svscan found but PID 1 is not s6", "detector", "s6", "pid1", procTarget)
	} else {
		log.Debug("init detection: s6: lookpath s6-svscan -> "+err.Error(), "detector", "s6", "error", err)
	}
	for _, p := range []string{"/usr/bin/s6-svscan", "/bin/s6-svscan"} {
		if _, err := m.statFunc(p); err == nil {
			log.Debug("init detection: s6: stat "+p+" -> found", "detector", "s6", "path", p)
			procTarget, procErr := m.readlinkFunc("/proc/1/exe")
			if procErr != nil || strings.Contains(strings.ToLower(procTarget), "s6") {
				log.Debug("init detection: s6: detected=true reason=s6-svscan found at "+p, "detector", "s6", "detected", true)
				return true
			}
			log.Debug("init detection: s6: s6-svscan found at "+p+" but PID 1 is not s6", "detector", "s6", "pid1", procTarget)
		} else {
			log.Debug("init detection: s6: stat "+p+" -> "+err.Error(), "detector", "s6", "path", p, "error", err)
		}
	}
	log.Debug("init detection: s6: detected=false reason=s6-svscan not found", "detector", "s6", "detected", false)
	return false
}

func (m *s6Manager) hasS6RC() bool {
	_, err := m.lookPathFunc("s6-rc-update")
	return err == nil
}
