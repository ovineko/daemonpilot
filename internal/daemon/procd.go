package daemon

import (
	"bytes"
	"encoding/json"
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

type procdManager struct {
	serviceName  string
	executor     commandExecutor
	fs           fileSystem
	statFunc     func(string) (os.FileInfo, error)
	lookPathFunc func(string) (string, error)
	readFileFunc func(string) ([]byte, error)
}

func newProcdManager() *procdManager {
	return &procdManager{
		executor:     &execCommandExecutor{},
		fs:           &osFileSystem{},
		statFunc:     os.Stat,
		lookPathFunc: exec.LookPath,
		readFileFunc: os.ReadFile,
	}
}

func (m *procdManager) Install(config ServiceConfig) error {
	m.serviceName = config.Name

	content, err := GenerateProcdInitScript(config, m.execPath(config))
	if err != nil {
		return fmt.Errorf("generating init script: %w", err)
	}

	if err := m.fs.WriteFile(m.initScript(), []byte(content), 0o755); err != nil {
		return fmt.Errorf("writing init script: %w", err)
	}

	if out, err := m.executor.Run(m.initScript(), "enable"); err != nil {
		return runErr("enabling service", out, err)
	}

	return nil
}

func (m *procdManager) Uninstall() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}

	_, _ = m.executor.Run(m.initScript(), "stop")
	_, _ = m.executor.Run(m.initScript(), "disable")

	if err := m.fs.Remove(m.initScript()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing init script: %w", err)
	}

	return nil
}

const procdInitScriptTemplate = `#!/bin/sh /etc/rc.common

START=95
STOP=05
USE_PROCD=1

start_service() {
	procd_open_instance
	procd_set_param command {{.ExecPath}}{{if .Args}} {{.Args}}{{end}}
	procd_set_param respawn
{{- if .EnvLines}}
 procd_set_param env{{range .EnvLines}} {{.}}{{end}}
{{- end}}
	procd_close_instance
}

stop_service() {
	:
}
`

type procdTemplateData struct {
	ExecPath string
	Name     string
	Args     string
	EnvLines []string
}

func buildProcdTemplateData(config ServiceConfig, execPath string) procdTemplateData {
	data := procdTemplateData{
		ExecPath: shellQuote(execPath),
		Name:     config.Name,
	}
	if config.WorkingDir != "" {
		data.ExecPath = shellQuote("/bin/sh")
		config.Arguments = workingDirectoryArgs(config, execPath)
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

// GenerateProcdInitScript renders the procd init script used on OpenWrt.
func GenerateProcdInitScript(config ServiceConfig, execPath string) (string, error) {
	if err := validateServiceText(config, execPath, "procd"); err != nil {
		return "", err
	}
	if err := validateEnvKeys(config.Environment); err != nil {
		return "", fmt.Errorf("procd: %w", err)
	}
	data := buildProcdTemplateData(config, execPath)

	tmpl, err := template.New("procd").Parse(procdInitScriptTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing procd template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing procd template: %w", err)
	}
	return buf.String(), nil
}

func detectProcd() bool {
	if runtime.GOOS != "linux" {
		getDetectionLog().Debug("init detection: procd: detected=false reason=not linux", "detector", "procd", "detected", false)
		return false
	}
	mgr := newProcdManager()
	return mgr.detect()
}

func (m *procdManager) Start() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if out, err := m.executor.Run(m.initScript(), "start"); err != nil {
		return runErr("starting service", out, err)
	}
	return nil
}

func (m *procdManager) Stop() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if out, err := m.executor.Run(m.initScript(), "stop"); err != nil {
		return runErr("stopping service", out, err)
	}
	return nil
}

func (m *procdManager) Enable() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if out, err := m.executor.Run(m.initScript(), "enable"); err != nil {
		return runErr("enabling service", out, err)
	}
	return nil
}

func (m *procdManager) Disable() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if out, err := m.executor.Run(m.initScript(), "disable"); err != nil {
		return runErr("disabling service", out, err)
	}
	return nil
}

func (m *procdManager) Status() (ServiceStatus, error) {
	if m.serviceName == "" {
		return ServiceStatusUnknown, errors.New("service name not set")
	}
	output, err := m.executor.Run("service", m.serviceName, "info")
	if err != nil {
		output2, err2 := m.executor.Run("pgrep", "-f", m.serviceName)
		if err2 != nil {
			return ServiceStatusStopped, nil //nolint:nilerr // pgrep exits non-zero when nothing matches, so the service is stopped
		}
		if len(strings.TrimSpace(string(output2))) > 0 {
			return ServiceStatusRunning, nil
		}
		return ServiceStatusStopped, nil
	}
	var services map[string]struct {
		Instances map[string]struct {
			Running bool `json:"running"`
		} `json:"instances"`
	}
	if json.Unmarshal(output, &services) == nil && services != nil {
		for _, instance := range services[m.serviceName].Instances {
			if instance.Running {
				return ServiceStatusRunning, nil
			}
		}
		return ServiceStatusStopped, nil
	}
	line := strings.ToLower(strings.TrimSpace(string(output)))
	if strings.Contains(line, "running") {
		return ServiceStatusRunning, nil
	}
	if strings.Contains(line, "inactive") || strings.Contains(line, "stopped") {
		return ServiceStatusStopped, nil
	}
	return ServiceStatusUnknown, nil
}

func (m *procdManager) GetManifestPath() string {
	return m.initScript()
}

func (m *procdManager) GenerateConfig(config ServiceConfig) (string, error) {
	m.serviceName = config.Name
	return GenerateProcdInitScript(config, m.execPath(config))
}

func (m *procdManager) WriteConfig(content string) error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	if err := m.fs.WriteFile(m.initScript(), []byte(content), 0o755); err != nil {
		return fmt.Errorf("writing init script: %w", err)
	}
	return nil
}

func (m *procdManager) ReadInstalledConfig() (string, error) {
	if m.serviceName == "" {
		return "", errors.New("service name not set; call Install first or set service name")
	}
	data, err := m.readFileFunc(m.initScript())
	if err != nil {
		return "", fmt.Errorf("reading init script: %w", err)
	}
	return string(data), nil
}

func (m *procdManager) CanRestartWatchdog() bool {
	return true
}

func (m *procdManager) SupportsEnable() bool { return true }

func (m *procdManager) SupportsStart() bool { return true }

func (m *procdManager) SupportsStatus() bool { return true }

func (m *procdManager) SupportsScope(scope Scope) bool { return scope == ScopeSystem }

func (m *procdManager) Name() string {
	return "procd"
}

func (m *procdManager) initScript() string {
	return filepath.Join("/etc/init.d", m.serviceName)
}

func (m *procdManager) execPath(config ServiceConfig) string {
	if config.ExecPath != "" {
		return config.ExecPath
	}
	installDir := config.InstallDir
	if installDir == "" {
		installDir = filepath.Join("/opt", config.Name)
	}
	return filepath.Join(installDir, config.Name)
}

func (m *procdManager) detect() bool {
	getDetectionLog().Debug("init detection: procd: checking procd detection", "detector", "procd")
	if _, err := m.statFunc("/sbin/procd"); err == nil {
		getDetectionLog().Debug("init detection: procd: stat /sbin/procd -> found", "detector", "procd", "path", "/sbin/procd")
		_, shErr := m.statFunc("/lib/functions/procd.sh")
		if shErr == nil {
			getDetectionLog().Debug("init detection: procd: stat /lib/functions/procd.sh -> found", "detector", "procd", "path", "/lib/functions/procd.sh")
			getDetectionLog().Debug("init detection: procd: detected=true reason=/sbin/procd and /lib/functions/procd.sh found", "detector", "procd", "detected", true)
			return true
		}
		getDetectionLog().Debug("init detection: procd: stat /lib/functions/procd.sh -> "+shErr.Error(), "detector", "procd", "path", "/lib/functions/procd.sh", "error", shErr)
	} else {
		getDetectionLog().Debug("init detection: procd: stat /sbin/procd -> "+err.Error(), "detector", "procd", "path", "/sbin/procd", "error", err)
	}
	getDetectionLog().Debug("init detection: procd: detected=false reason=procd detection criteria not met", "detector", "procd", "detected", false)
	return false
}
