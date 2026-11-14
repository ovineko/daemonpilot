package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

const openrcInitScriptTemplate = `#!/sbin/openrc-run

name="{{.Name}}"
description="{{.Description}}"
command="{{.ExecPath}}"
command_args="{{.CommandArgs}}"
{{- if .Supervisor}}
supervisor=supervise-daemon
{{- else}}
command_background=true
pidfile="/run/${name}.pid"
{{- end}}
directory="{{.WorkingDir}}"
output_log="/var/log/${name}.log"
error_log="/var/log/${name}.err"
{{- if .CommandUser}}
command_user="{{.CommandUser}}"
{{- end}}
{{- range .EnvironmentLines}}
{{.}}
{{- end}}
{{- range .ExtraDirectives}}
{{.}}
{{- end}}

start() {
 eval "set -- $command_args"
{{- if .Supervisor}}
 set -- "$command" -- "$@"
 eval "set -- ${supervise_daemon_args-${start_stop_daemon_args-}} \"\$@\""
 set -- ${respawn_delay:+--respawn-delay "$respawn_delay"} ${respawn_max:+--respawn-max "$respawn_max"} ${respawn_period:+--respawn-period "$respawn_period"} ${retry:+--retry "$retry"} ${healthcheck_delay:+--healthcheck-delay "$healthcheck_delay"} ${healthcheck_timer:+--healthcheck-timer "$healthcheck_timer"} "$@"
{{- else}}
 set -- -- "$@"
 eval "set -- ${start_stop_daemon_args-} \"\$@\""
 set -- --exec "$command" --pidfile "$pidfile" ${procname:+--name "$procname"} "$@"
 case "$command_background" in true|yes|1) set -- --background --make-pidfile "$@";; esac
{{- end}}
 set -- ${directory:+--chdir "$directory"} ${chroot:+--chroot "$chroot"} ${command_user:+--user "$command_user"} ${umask:+--umask "$umask"} ${notify:+--notify "$notify"} ${capabilities:+--capabilities "$capabilities"} ${secbits:+--secbits "$secbits"} ${no_new_privs:+--no-new-privs} "$@"
 set -- ${input_file:+--stdin "$input_file"} ${output_log:+--stdout "$output_log"} ${error_log:+--stderr "$error_log"} ${output_logger:+--stdout-logger "$output_logger"} ${error_logger:+--stderr-logger "$error_logger"} "$@"
{{- if .Supervisor}}
 supervise-daemon "${RC_SVCNAME:-$name}" --start "$@"
{{- else}}
 start-stop-daemon --start "$@"
{{- end}}
}

depend() {
	need net
	after firewall
}
`

type openrcManager struct {
	serviceName  string
	executor     commandExecutor
	fs           fileSystem
	lookPathFunc func(string) (string, error)
	statFunc     func(string) (os.FileInfo, error)
}

func newOpenrcManager() *openrcManager {
	return &openrcManager{
		executor:     &execCommandExecutor{},
		fs:           &osFileSystem{},
		lookPathFunc: exec.LookPath,
		statFunc:     os.Stat,
	}
}

func (m *openrcManager) Install(config ServiceConfig) error {
	m.serviceName = config.Name

	content, err := generateOpenRCScript(config, m.hasSupervisor())
	if err != nil {
		return fmt.Errorf("generating init script: %w", err)
	}

	if err := m.fs.WriteFile(m.initScriptPath(), []byte(content), 0o755); err != nil {
		return fmt.Errorf("writing init script: %w", err)
	}

	return nil
}

func sanitizeShellValue(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`$`, `\$`,
		"`", "\\`",
	)
	result := r.Replace(s)

	return result
}

type openrcTemplateData struct {
	Name             string
	Description      string
	ExecPath         string
	CommandArgs      string
	WorkingDir       string
	CommandUser      string
	Supervisor       bool
	EnvironmentLines []string
	ExtraDirectives  []string
}

func buildOpenRCTemplateData(config ServiceConfig, supervisor bool) openrcTemplateData {
	var commandArgs string
	var commandArgsSb92 strings.Builder
	for i, arg := range config.Arguments {
		if i > 0 {
			commandArgsSb92.WriteString(" ")
		}
		commandArgsSb92.WriteString(sanitizeShellValue(shellQuoteIfNeeded(arg)))
	}
	commandArgs += commandArgsSb92.String()

	var commandUser string
	if config.User != "" {
		commandUser = sanitizeShellValue(config.User)
		if config.Group != "" {
			commandUser += ":" + sanitizeShellValue(config.Group)
		}
	}

	var envLines []string
	if len(config.Environment) > 0 {
		keys := make([]string, 0, len(config.Environment))
		for k := range config.Environment {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := config.Environment[k]
			envLines = append(envLines, fmt.Sprintf("export %s=%s", k, shellQuote(v)))
		}
	}

	var directives []string
	if len(config.OpenRCDirectives) > 0 {
		keys := make([]string, 0, len(config.OpenRCDirectives))
		for k := range config.OpenRCDirectives {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			directives = append(directives, k+"=\""+sanitizeShellValue(config.OpenRCDirectives[k])+"\"")
		}
	}

	return openrcTemplateData{
		Name:             sanitizeShellValue(config.Name),
		Description:      sanitizeShellValue(config.Description),
		ExecPath:         sanitizeShellValue(config.ExecPath),
		CommandArgs:      commandArgs,
		WorkingDir:       sanitizeShellValue(config.WorkingDir),
		CommandUser:      commandUser,
		Supervisor:       supervisor,
		EnvironmentLines: envLines,
		ExtraDirectives:  directives,
	}
}

// GenerateOpenRCInitScript renders the /etc/init.d OpenRC script for the
// configured service.
func GenerateOpenRCInitScript(config ServiceConfig) (string, error) {
	return generateOpenRCScript(config, false)
}

func generateOpenRCScript(config ServiceConfig, supervisor bool) (string, error) {
	if err := validateServiceText(config, config.ExecPath, "openrc"); err != nil {
		return "", err
	}
	if err := validateEnvKeys(config.Environment); err != nil {
		return "", fmt.Errorf("openrc: %w", err)
	}
	data := buildOpenRCTemplateData(config, supervisor)

	tmpl, err := template.New("openrc").Parse(openrcInitScriptTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing openrc template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing openrc template: %w", err)
	}
	return buf.String(), nil
}

func (m *openrcManager) Uninstall() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}

	// Best-effort removal from runlevel; service may not have been added
	_, _ = m.executor.Run("rc-update", "del", m.serviceName, "default")

	if err := m.fs.Remove(m.initScriptPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing init script: %w", err)
	}

	return nil
}

func (m *openrcManager) Start() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if out, err := m.executor.Run("rc-service", m.serviceName, "start"); err != nil {
		return runErr("starting service", out, err)
	}
	return nil
}

func (m *openrcManager) Stop() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if out, err := m.executor.Run("rc-service", m.serviceName, "stop"); err != nil {
		return runErr("stopping service", out, err)
	}
	return nil
}

func (m *openrcManager) Enable() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if out, err := m.executor.Run("rc-update", "add", m.serviceName, "default"); err != nil {
		return runErr("enabling service", out, err)
	}
	return nil
}

func (m *openrcManager) Disable() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if out, err := m.executor.Run("rc-update", "del", m.serviceName, "default"); err != nil {
		return runErr("disabling service", out, err)
	}
	return nil
}

func (m *openrcManager) GetManifestPath() string {
	return m.initScriptPath()
}

func (m *openrcManager) GenerateConfig(config ServiceConfig) (string, error) {
	m.serviceName = config.Name
	return generateOpenRCScript(config, m.hasSupervisor())
}

func (m *openrcManager) ReadInstalledConfig() (string, error) {
	data, err := os.ReadFile(m.initScriptPath())
	if err != nil {
		return "", fmt.Errorf("reading init script: %w", err)
	}
	return string(data), nil
}

func (m *openrcManager) WriteConfig(content string) error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if err := m.fs.WriteFile(m.initScriptPath(), []byte(content), 0o755); err != nil {
		return fmt.Errorf("writing init script: %w", err)
	}
	return nil
}

func (m *openrcManager) CanRestartWatchdog() bool {
	return m.hasSupervisor()
}

func (m *openrcManager) SupportsEnable() bool { return true }

func (m *openrcManager) SupportsStart() bool { return true }

func (m *openrcManager) SupportsStatus() bool { return true }

func (m *openrcManager) SupportsScope(scope Scope) bool { return scope == ScopeSystem }
func (m *openrcManager) Name() string {
	return "openrc"
}

func (m *openrcManager) Status() (ServiceStatus, error) {
	if m.serviceName == "" {
		return ServiceStatusUnknown, errors.New("service name not set")
	}

	output, err := m.executor.Run("rc-service", m.serviceName, "status")
	status := strings.TrimSpace(string(output))

	switch {
	case strings.Contains(status, "started"):
		return ServiceStatusRunning, nil
	case strings.Contains(status, "stopped"):
		return ServiceStatusStopped, nil
	case strings.Contains(status, "crashed"):
		return ServiceStatusFailed, nil
	default:
		if err != nil {
			return ServiceStatusUnknown, runErr("checking service status", output, err)
		}
		return ServiceStatusUnknown, nil
	}
}

func (m *openrcManager) initScriptPath() string {
	return filepath.Join("/etc/init.d", m.serviceName)
}

func (m *openrcManager) hasSupervisor() bool {
	_, err := m.lookPathFunc("supervise-daemon")
	return err == nil
}
