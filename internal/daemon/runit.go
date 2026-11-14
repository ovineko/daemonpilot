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

type runitManager struct {
	serviceName  string
	executor     commandExecutor
	fs           fileSystem
	statFunc     func(string) (os.FileInfo, error)
	lookPathFunc func(string) (string, error)
	readFileFunc func(string) ([]byte, error)
	symlinkFunc  func(string, string) error
	removeFunc   func(string) error
	readlinkFunc func(string) (string, error)
}

func newRunitManager() *runitManager {
	return &runitManager{
		executor:     &execCommandExecutor{},
		fs:           &osFileSystem{},
		statFunc:     os.Stat,
		lookPathFunc: exec.LookPath,
		readFileFunc: os.ReadFile,
		symlinkFunc:  os.Symlink,
		removeFunc:   os.Remove,
		readlinkFunc: os.Readlink,
	}
}

func (m *runitManager) Install(config ServiceConfig) error {
	m.serviceName = config.Name

	if err := m.fs.MkdirAll(m.serviceDir(), 0o755); err != nil {
		return fmt.Errorf("creating service directory: %w", err)
	}

	content, err := GenerateRunitRunScript(config, m.execPath(config))
	if err != nil {
		return fmt.Errorf("generating run script: %w", err)
	}

	if err := m.fs.WriteFile(m.runScript(), []byte(content), 0o755); err != nil {
		return fmt.Errorf("writing run script: %w", err)
	}

	if err := m.symlinkFunc(m.serviceDir(), m.activeSymlink()); err != nil && !os.IsExist(err) {
		return fmt.Errorf("activating service: %w", err)
	}

	return nil
}

func (m *runitManager) Uninstall() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}

	_ = m.Stop()

	_ = m.removeFunc(m.activeSymlink())

	if err := m.fs.Remove(m.runScript()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing run script: %w", err)
	}

	if err := m.fs.RemoveAll(m.serviceDir()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing service directory: %w", err)
	}

	return nil
}

func (m *runitManager) Start() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	return startSupervisedService(m.executor, supervisionStartTimeout, supervisionStartInterval, "sv", "up", m.serviceName)
}

func (m *runitManager) Stop() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if out, err := m.executor.Run("sv", "-w", "30", "stop", m.serviceName); err != nil {
		return runErr("stopping service", out, err)
	}
	return nil
}

const runitRunScriptTemplate = `#!/bin/sh
{{- range .EnvLines}}
export {{.}}
{{- end}}
{{- if .WorkingDir}}
cd {{.WorkingDir}} || exit 1
{{- end}}
exec {{.ExecPath}}{{if .Args}} {{.Args}}{{end}}
`

type runitTemplateData struct {
	ExecPath   string
	WorkingDir string
	Name       string
	Args       string
	EnvLines   []string
}

func buildRunitTemplateData(config ServiceConfig, execPath string) runitTemplateData {
	data := runitTemplateData{
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

// GenerateRunitRunScript renders the runit run script that execs the service in
// the foreground.
func GenerateRunitRunScript(config ServiceConfig, execPath string) (string, error) {
	if err := validateServiceText(config, execPath, "runit"); err != nil {
		return "", err
	}
	if err := validateEnvKeys(config.Environment); err != nil {
		return "", fmt.Errorf("runit: %w", err)
	}
	data := buildRunitTemplateData(config, execPath)

	tmpl, err := template.New("runit").Parse(runitRunScriptTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing runit template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing runit template: %w", err)
	}
	return buf.String(), nil
}

func detectRunit() bool {
	if runtime.GOOS != "linux" {
		getDetectionLog().Debug("init detection: runit: detected=false reason=not linux", "detector", "runit", "detected", false)
		return false
	}
	mgr := newRunitManager()
	return mgr.detect()
}

func (m *runitManager) Enable() error {
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

func (m *runitManager) Disable() error {
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

func (m *runitManager) Status() (ServiceStatus, error) {
	if m.serviceName == "" {
		return ServiceStatusUnknown, errors.New("service name not set")
	}
	output, err := m.executor.Run("sv", "status", m.serviceName)
	if err != nil {
		return ServiceStatusUnknown, nil //nolint:nilerr // a failed status query means the init system does not know this service
	}
	line := strings.ToLower(strings.TrimSpace(string(output)))
	if strings.HasPrefix(line, "run:") {
		return ServiceStatusRunning, nil
	}
	if strings.HasPrefix(line, "down:") {
		return ServiceStatusStopped, nil
	}
	return ServiceStatusUnknown, nil
}

func (m *runitManager) GetManifestPath() string {
	return m.runScript()
}

func (m *runitManager) GenerateConfig(config ServiceConfig) (string, error) {
	m.serviceName = config.Name
	return GenerateRunitRunScript(config, m.execPath(config))
}

func (m *runitManager) WriteConfig(content string) error {
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

func (m *runitManager) ReadInstalledConfig() (string, error) {
	if m.serviceName == "" {
		return "", errors.New("service name not set; call Install first or set service name")
	}
	data, err := m.readFileFunc(m.runScript())
	if err != nil {
		return "", fmt.Errorf("reading run script: %w", err)
	}
	return string(data), nil
}

func (m *runitManager) CanRestartWatchdog() bool {
	return true
}

func (m *runitManager) SupportsEnable() bool { return true }

func (m *runitManager) SupportsStart() bool { return true }

func (m *runitManager) SupportsStatus() bool { return true }

func (m *runitManager) SupportsScope(scope Scope) bool { return scope == ScopeSystem }

func (m *runitManager) Name() string {
	return "runit"
}

func (m *runitManager) serviceDir() string {
	return filepath.Join("/etc/sv", m.serviceName)
}

func (m *runitManager) runScript() string {
	return filepath.Join("/etc/sv", m.serviceName, "run")
}

func (m *runitManager) activeSymlink() string {
	return filepath.Join("/var/service", m.serviceName)
}

func (m *runitManager) execPath(config ServiceConfig) string {
	if config.ExecPath != "" {
		return config.ExecPath
	}
	installDir := config.InstallDir
	if installDir == "" {
		installDir = filepath.Join("/opt", config.Name)
	}
	return filepath.Join(installDir, config.Name)
}

func (m *runitManager) detect() bool {
	getDetectionLog().Debug("init detection: runit: checking runit detection", "detector", "runit")
	hasRunsvdir := false
	if path, err := m.lookPathFunc("runsvdir"); err == nil {
		getDetectionLog().Debug("init detection: runit: lookpath runsvdir -> "+path, "detector", "runit", "path", path)
		hasRunsvdir = true
	} else {
		getDetectionLog().Debug("init detection: runit: lookpath runsvdir -> "+err.Error(), "detector", "runit", "error", err)
		for _, p := range []string{"/sbin/runsvdir", "/usr/bin/runsvdir"} {
			_, statErr := m.statFunc(p)
			if statErr == nil {
				getDetectionLog().Debug("init detection: runit: stat "+p+" -> found", "detector", "runit", "path", p)
				hasRunsvdir = true
				break
			}
			getDetectionLog().Debug("init detection: runit: stat "+p+" -> "+statErr.Error(), "detector", "runit", "path", p, "error", statErr)
		}
	}

	if !hasRunsvdir {
		getDetectionLog().Debug("init detection: runit: detected=false reason=runsvdir not found", "detector", "runit", "detected", false)
		return false
	}

	fi, err := m.statFunc("/var/service")
	if err != nil {
		getDetectionLog().Debug("init detection: runit: stat /var/service -> "+err.Error(), "detector", "runit", "path", "/var/service", "error", err)
		getDetectionLog().Debug("init detection: runit: detected=false reason=/var/service directory not found", "detector", "runit", "detected", false)
		return false
	}
	if fi == nil || !fi.IsDir() {
		getDetectionLog().Debug("init detection: runit: stat /var/service -> not a directory", "detector", "runit", "path", "/var/service")
		getDetectionLog().Debug("init detection: runit: detected=false reason=/var/service is not a directory", "detector", "runit", "detected", false)
		return false
	}
	getDetectionLog().Debug("init detection: runit: stat /var/service -> found", "detector", "runit", "path", "/var/service")
	getDetectionLog().Debug("init detection: runit: detected=true reason=runsvdir and /var/service directory found", "detector", "runit", "detected", true)
	return true
}
