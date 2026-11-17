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

type sysvinitManager struct {
	serviceName      string
	supportsKSPrefix bool
	executor         commandExecutor
	fs               fileSystem
	statFunc         func(string) (os.FileInfo, error)
	lookPathFunc     func(string) (string, error)
	readlinkFunc     func(string) (string, error)
	readFileFunc     func(string) ([]byte, error)
	renameFunc       func(string, string) error
	symlinkFunc      func(string, string) error
}

func newSysvinitManager() *sysvinitManager {
	return &sysvinitManager{
		executor:     &execCommandExecutor{},
		fs:           &osFileSystem{},
		statFunc:     os.Stat,
		lookPathFunc: exec.LookPath,
		readlinkFunc: os.Readlink,
		readFileFunc: os.ReadFile,
		renameFunc:   os.Rename,
		symlinkFunc:  os.Symlink,
	}
}

func detectSysvinit() bool {
	if runtime.GOOS != "linux" {
		getDetectionLog().Debug("init detection: sysvinit: detected=false reason=not linux", "detector", "sysvinit", "detected", false)
		return false
	}
	mgr := newSysvinitManager()
	return mgr.detect()
}

func (m *sysvinitManager) Name() string {
	return "sysvinit"
}

func (m *sysvinitManager) CanRestartWatchdog() bool {
	return false
}

func (m *sysvinitManager) SupportsEnable() bool {
	return true
}

func (m *sysvinitManager) SupportsStart() bool  { return true }
func (m *sysvinitManager) SupportsStatus() bool { return true }

func (m *sysvinitManager) SupportsScope(scope Scope) bool { return scope == ScopeSystem }

func (m *sysvinitManager) Install(config ServiceConfig) error {
	m.serviceName = config.Name

	content, err := GenerateSysvinitInitScript(config)
	if err != nil {
		return fmt.Errorf("generating init script: %w", err)
	}

	if m.hasRunlevelDirs() {
		if err := m.fs.WriteFile(m.initScriptPath(), []byte(content), 0o755); err != nil {
			return fmt.Errorf("writing init script: %w", err)
		}
		return nil
	}

	m.supportsKSPrefix = m.detectKSPrefixSupport()

	for _, path := range []string{m.initScriptPath(), m.enabledScriptPath(), m.disabledScriptPath(), m.flagFilePath()} {
		if err := m.fs.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("cleaning up %s: %w", path, err)
		}
	}

	if m.supportsKSPrefix {
		if err := m.fs.WriteFile(m.disabledScriptPath(), []byte(content), 0o755); err != nil {
			return fmt.Errorf("writing init script: %w", err)
		}
	} else {
		if err := m.fs.WriteFile(m.initScriptPath(), []byte(content), 0o755); err != nil {
			return fmt.Errorf("writing init script: %w", err)
		}
		if err := m.fs.WriteFile(m.flagFilePath(), []byte("disabled\n"), 0o644); err != nil {
			return fmt.Errorf("creating disabled flag: %w", err)
		}
	}

	return nil
}

func (m *sysvinitManager) Uninstall() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}

	_ = m.Disable()

	for _, path := range []string{m.initScriptPath(), m.enabledScriptPath(), m.disabledScriptPath()} {
		if err := m.fs.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing init script %s: %w", path, err)
		}
	}

	_ = m.fs.Remove(m.flagFilePath())

	return nil
}

func (m *sysvinitManager) Start() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if m.hasServiceCommand() && m.hasRunlevelDirs() {
		if out, err := m.executor.Run("service", m.serviceName, "start"); err != nil {
			return runErr("starting service", out, err)
		}
		return nil
	}
	scriptPath := m.resolveScriptPath()
	if out, err := m.executor.Run(scriptPath, "start"); err != nil {
		return runErr("starting service", out, err)
	}
	return nil
}

func (m *sysvinitManager) Stop() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if m.hasServiceCommand() && m.hasRunlevelDirs() {
		if out, err := m.executor.Run("service", m.serviceName, "stop"); err != nil {
			return runErr("stopping service", out, err)
		}
		return nil
	}
	scriptPath := m.resolveScriptPath()
	if out, err := m.executor.Run(scriptPath, "stop"); err != nil {
		return runErr("stopping service", out, err)
	}
	return nil
}

// Enable uses a cascading strategy to enable the service at boot:
//  1. update-rc.d (Debian/Ubuntu)
//  2. chkconfig (RHEL/CentOS)
//  3. Direct symlink creation in /etc/rc*.d/ (classical SysV without tools)
//  4. BusyBox-style rename: K95name -> S95name in /etc/init.d/
func (m *sysvinitManager) Enable() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if m.hasUpdateRcD() {
		return m.enableDebian()
	}
	if m.hasChkconfig() {
		return m.enableRHEL()
	}
	if m.hasRunlevelDirs() {
		return m.enableSymlinks()
	}
	return m.enableBusyBox()
}

// Disable uses the same cascading strategy as Enable (see Enable comment).
func (m *sysvinitManager) Disable() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if m.hasUpdateRcD() {
		return m.disableDebian()
	}
	if m.hasChkconfig() {
		return m.disableRHEL()
	}
	if m.hasRunlevelDirs() {
		return m.disableSymlinks()
	}
	return m.disableBusyBox()
}

func (m *sysvinitManager) Status() (ServiceStatus, error) {
	if m.serviceName == "" {
		return ServiceStatusUnknown, errors.New("service name not set")
	}

	var output []byte
	var err error
	if m.hasServiceCommand() && m.hasRunlevelDirs() {
		output, err = m.executor.Run("service", m.serviceName, "status")
	} else {
		output, err = m.executor.Run(m.resolveScriptPath(), "status")
	}

	line := strings.ToLower(strings.TrimSpace(string(output)))

	if err != nil {
		if strings.Contains(line, "not running") || strings.Contains(line, "stopped") || strings.Contains(line, "dead") {
			return ServiceStatusStopped, nil
		}
		var exitErr *exec.ExitError
		if ok := errors.As(err, &exitErr); ok {
			code := exitErr.ExitCode()
			if code >= 1 && code <= 3 {
				return ServiceStatusStopped, nil
			}
		}
		return ServiceStatusUnknown, runErr("status command failed", output, err)
	}

	if strings.Contains(line, "not running") || strings.Contains(line, "stopped") || strings.Contains(line, "dead") {
		return ServiceStatusStopped, nil
	}
	if strings.Contains(line, "running") {
		return ServiceStatusRunning, nil
	}

	return ServiceStatusUnknown, nil
}

func (m *sysvinitManager) GetManifestPath() string {
	return m.resolveScriptPath()
}

// stop() closes three nested shell `if` blocks in a row, so the template holds
// consecutive `fi` tokens. dupword reads those as a repeated word and its
// autofix silently deletes one, which leaves the generated script unparseable.
//
// The async brace group creates a redirected subshell; OpenBSD sh rejects the parenthesized form.
//
//nolint:dupword // consecutive `fi` tokens close nested shell `if` blocks
const sysvinitInitScriptTemplate = "#!/bin/sh\n### BEGIN INIT INFO\n# Provides:          {{.Name}}\n# Required-Start:    $remote_fs $syslog $network\n# Required-Stop:     $remote_fs $syslog $network\n# Default-Start:     2 3 4 5\n# Default-Stop:      0 1 6\n# Short-Description: {{.Description}}\n### END INIT INFO\n\nNAME=\"{{.Name}}\"\nDAEMON={{.ExecPath}}\nPIDFILE=\"/var/run/$NAME.pid\"\nSTOPFILE=\"/var/run/$NAME.stop\"\n{{- if .WorkingDir}}\nWORKDIR={{.WorkingDir}}\n{{- end}}\n{{- if .User}}\nUSER={{.User}}\n{{- end}}\n\n[ -x \"$DAEMON\" ] || exit 5\n[ -f \"/etc/init.d/$NAME.disabled\" ] && exit 0\n{{- if .EnvLines}}\n\n# Environment variables\n{{- range .EnvLines}}\nexport {{.}}\n{{- end}}\n{{- end}}\n\n# supervise runs the daemon under a respawn loop so an OOM-killed or crashed\n# daemon comes back WITHOUT a reboot — sysvinit itself never restarts a service\n# that dies, which is exactly why a low-RAM node could get stuck on the old\n# version after a failed update. It stops respawning on a clean exit (0 = an\n# intentional permanent stop, e.g. a delete-agent teardown) or when stop\n# requests it via STOPFILE, so it never fights an operator stop. PIDFILE holds\n# the daemon PID (not the loop), so stop()/status() keep working unchanged.\nsupervise() {\n    while [ ! -f \"$STOPFILE\" ]; do\n{{- if .WorkingDir}}\n        cd \"$WORKDIR\" || exit 1\n{{- end}}\n{{- if .User}}\n        DPID=$(su -s /bin/sh \"$USER\" -c {{.UserCommand}})\n        [ -z \"$DPID\" ] && { sleep 2; continue; }\n        echo \"$DPID\" > \"$PIDFILE\"\n        # Cannot wait across the su boundary, so poll; any non-stop exit respawns.\n        while kill -0 \"$DPID\" 2>/dev/null; do\n            [ -f \"$STOPFILE\" ] && break\n            sleep 1\n        done\n        [ -f \"$STOPFILE\" ] && break\n        sleep 2\n{{- else}}\n        \"$DAEMON\" {{.Args}} >/dev/null 2>&1 &\n        DPID=$!\n        echo \"$DPID\" > \"$PIDFILE\"\n        wait \"$DPID\"\n        RC=$?\n        { [ -f \"$STOPFILE\" ] || [ \"$RC\" -eq 0 ]; } && break\n        sleep 2\n{{- end}}\n    done\n    rm -f \"$PIDFILE\"\n}\n\nstart() {\n    echo \"Starting $NAME...\"\n    rm -f \"$STOPFILE\"\n    { supervise; } </dev/null >/dev/null 2>&1 &\n    sleep 1\n    PID=\n    if [ -f \"$PIDFILE\" ]; then\n        PID=$(cat \"$PIDFILE\" 2>/dev/null)\n    fi\n    if [ -n \"$PID\" ] && kill -0 \"$PID\" 2>/dev/null; then\n        echo \"$NAME started.\"\n    else\n        echo \"$NAME failed to start.\"\n        return 1\n    fi\n}\n\nstop() {\n    echo \"Stopping $NAME...\"\n    # Signal the supervisor to stop respawning BEFORE killing the daemon, so the\n    # kill below is not immediately undone by a respawn.\n    touch \"$STOPFILE\"\n    if [ -f \"$PIDFILE\" ]; then\n        PID=$(cat \"$PIDFILE\")\n        if kill -0 \"$PID\" 2>/dev/null; then\n            PROC_EXE=$(readlink \"/proc/$PID/exe\" 2>/dev/null)\n            if [ -n \"$PROC_EXE\" ]; then\n                PROC_EXE_CLEAN=$(echo \"$PROC_EXE\" | sed 's/ (deleted)$//')\n                DAEMON_REAL=$(readlink -f \"$DAEMON\" 2>/dev/null || echo \"$DAEMON\")\n                if [ \"$PROC_EXE_CLEAN\" != \"$DAEMON\" ] && [ \"$PROC_EXE_CLEAN\" != \"$DAEMON_REAL\" ]; then\n                    echo \"PID $PID does not belong to $NAME (exe: $PROC_EXE), removing stale PID file\"\n                    rm -f \"$PIDFILE\" \"$STOPFILE\"\n                    return 1\n                fi\n            elif [ -f \"/proc/$PID/comm\" ]; then\n                PROC_CMD=$(cat \"/proc/$PID/comm\" 2>/dev/null)\n                DAEMON_BASE=$(basename \"$DAEMON\")\n                DAEMON_BASE_CMP=$(printf '%.15s' \"$DAEMON_BASE\")\n                if [ -n \"$PROC_CMD\" ] && [ \"$PROC_CMD\" != \"$DAEMON_BASE_CMP\" ]; then\n                    echo \"PID $PID does not belong to $NAME (found: $PROC_CMD), removing stale PID file\"\n                    rm -f \"$PIDFILE\" \"$STOPFILE\"\n                    return 1\n                fi\n            fi\n            kill \"$PID\"\n            TIMEOUT=30\n            while kill -0 \"$PID\" 2>/dev/null && [ \"$TIMEOUT\" -gt 0 ]; do\n                sleep 1\n                TIMEOUT=$((TIMEOUT - 1))\n            done\n            if kill -0 \"$PID\" 2>/dev/null; then\n                kill -9 \"$PID\"\n            fi\n        fi\n    fi\n    # Wait for the supervisor to observe STOPFILE and exit (it removes PIDFILE).\n    STOP_TIMEOUT=30\n    while [ -f \"$PIDFILE\" ] && [ \"$STOP_TIMEOUT\" -gt 0 ]; do\n        sleep 1\n        STOP_TIMEOUT=$((STOP_TIMEOUT - 1))\n    done\n    rm -f \"$PIDFILE\" \"$STOPFILE\"\n    echo \"$NAME stopped.\"\n}\n\nstatus() {\n    if [ -f \"$PIDFILE\" ]; then\n        PID=$(cat \"$PIDFILE\")\n        if kill -0 \"$PID\" 2>/dev/null; then\n            echo \"$NAME is running (PID $PID)\"\n            return 0\n        else\n            echo \"$NAME is not running (stale PID file)\"\n            return 1\n        fi\n    else\n        echo \"$NAME is not running\"\n        return 3\n    fi\n}\n\ncase \"$1\" in\n    start)\n        start\n        ;;\n    stop)\n        stop\n        ;;\n    restart)\n        stop\n        sleep 1\n        start\n        ;;\n    status)\n        status\n        ;;\n    *)\n        echo \"Usage: $0 {start|stop|restart|status}\"\n        exit 1\n        ;;\nesac\n"

type sysvinitTemplateData struct {
	Name        string
	Description string
	ExecPath    string
	WorkingDir  string
	User        string
	Args        string
	UserCommand string
	EnvLines    []string
}

func sanitizeDescription(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

func buildSysvinitTemplateData(config ServiceConfig) sysvinitTemplateData {
	data := sysvinitTemplateData{
		Name:        config.Name,
		Description: sanitizeDescription(config.Description),
		ExecPath:    shellQuote(config.ExecPath),
	}
	if config.WorkingDir != "" {
		data.WorkingDir = shellQuote(config.WorkingDir)
	}
	if config.User != "" {
		data.User = shellQuote(config.User)
	}
	if len(config.Arguments) > 0 {
		quoted := make([]string, len(config.Arguments))
		for i, a := range config.Arguments {
			quoted[i] = shellQuote(a)
		}
		data.Args = strings.Join(quoted, " ")
	}
	if len(config.Environment) > 0 {
		for _, k := range sortedKeys(config.Environment) {
			data.EnvLines = append(data.EnvLines, fmt.Sprintf("%s=%s", k, shellQuote(config.Environment[k])))
		}
	}
	data.UserCommand = shellQuote(shellQuote(config.ExecPath) + " " + data.Args + " >/dev/null 2>&1 & echo $!")
	return data
}

// GenerateSysvinitInitScript renders the LSB-style /etc/init.d script for
// System V init.
func GenerateSysvinitInitScript(config ServiceConfig) (string, error) {
	if err := validateServiceText(config, config.ExecPath, "sysvinit"); err != nil {
		return "", err
	}
	if err := validateEnvKeys(config.Environment); err != nil {
		return "", fmt.Errorf("sysvinit: %w", err)
	}
	data := buildSysvinitTemplateData(config)

	tmpl, err := template.New("sysvinit").Parse(sysvinitInitScriptTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing sysvinit template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing sysvinit template: %w", err)
	}
	return buf.String(), nil
}

func (m *sysvinitManager) GenerateConfig(config ServiceConfig) (string, error) {
	m.serviceName = config.Name
	return GenerateSysvinitInitScript(config)
}

func (m *sysvinitManager) WriteConfig(content string) error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if err := m.fs.WriteFile(m.resolveScriptPath(), []byte(content), 0o755); err != nil {
		return fmt.Errorf("writing init script: %w", err)
	}
	return nil
}

func (m *sysvinitManager) ReadInstalledConfig() (string, error) {
	if m.serviceName == "" {
		return "", errors.New("service name not set; call Install first or set service name")
	}
	data, err := m.readFileFunc(m.resolveScriptPath())
	if err != nil {
		return "", fmt.Errorf("reading init script: %w", err)
	}
	return string(data), nil
}

// otherInitNames are init systems that also provide /sbin/init or PID 1. A
// match means this host runs one of them, not classic System V init.
var otherInitNames = []string{"systemd", "busybox", "runit", "s6", "procd", "dinit", "finit"}

func pointsToOtherInit(target string) bool {
	lower := strings.ToLower(target)
	for _, name := range otherInitNames {
		if strings.Contains(lower, name) {
			return true
		}
	}
	return false
}

func (m *sysvinitManager) detect() bool {
	log := getDetectionLog()
	log.Debug("init detection: sysvinit: checking sysvinit detection", "detector", "sysvinit")

	hasInittab := false
	if _, err := m.statFunc("/etc/inittab"); err == nil {
		log.Debug("init detection: sysvinit: found /etc/inittab", "detector", "sysvinit")
		hasInittab = true
	}

	if !hasInittab {
		if _, err := m.statFunc("/sbin/init"); err != nil {
			log.Debug("init detection: sysvinit: /sbin/init not found", "detector", "sysvinit", "detected", false)
			return false
		}

		target, err := m.readlinkFunc("/sbin/init")
		if err == nil {
			if pointsToOtherInit(target) {
				log.Debug("init detection: sysvinit: /sbin/init is symlink to other init",
					"detector", "sysvinit", "target", target, "detected", false)
				return false
			}
		}
	}

	procTarget, err := m.readlinkFunc("/proc/1/exe")
	if err != nil {
		if hasInittab {
			log.Debug("init detection: sysvinit: detected via /etc/inittab (cannot read /proc/1/exe)",
				"detector", "sysvinit", "detected", true)
			return true
		}
		log.Debug("init detection: sysvinit: cannot read /proc/1/exe",
			"detector", "sysvinit", "error", err, "detected", false)
		return false
	}

	if pointsToOtherInit(procTarget) {
		log.Debug("init detection: sysvinit: /proc/1/exe points to other init",
			"detector", "sysvinit", "target", procTarget, "detected", false)
		return false
	}

	indicator := "/sbin/init"
	if hasInittab {
		indicator = "/etc/inittab"
	}
	log.Debug("init detection: sysvinit: detected via "+indicator+" and /proc/1/exe",
		"detector", "sysvinit", "detected", true)
	return true
}

func (m *sysvinitManager) initScriptPath() string {
	return filepath.Join("/etc/init.d", m.serviceName)
}

// hasRunlevelDirs detects whether the system uses classical SysV init with
// /etc/rc*.d/ directories (Debian, RHEL) or BusyBox-style init where scripts
// live directly in /etc/init.d/ with S##/K## prefixes. The presence of
// /etc/rc0.d/ distinguishes the two styles.
func (m *sysvinitManager) hasRunlevelDirs() bool {
	_, err := m.statFunc("/etc/rc0.d")
	return err == nil
}

func (m *sysvinitManager) enabledScriptPath() string {
	return filepath.Join("/etc/init.d", "S95"+m.serviceName)
}

func (m *sysvinitManager) disabledScriptPath() string {
	return filepath.Join("/etc/init.d", "K95"+m.serviceName)
}

func (m *sysvinitManager) flagFilePath() string {
	return filepath.Join("/etc/init.d", m.serviceName+".disabled")
}

func (m *sysvinitManager) detectKSPrefixSupport() bool {
	if _, err := m.statFunc("/etc/init.d/rcS"); err == nil {
		return true
	}
	if _, err := m.statFunc("/etc/init.d/rcK"); err == nil {
		return true
	}
	if _, err := m.statFunc("/etc/rc.d"); err == nil {
		return true
	}
	return false
}

func (m *sysvinitManager) resolveScriptPath() string {
	if _, err := m.statFunc(m.initScriptPath()); err == nil {
		return m.initScriptPath()
	}
	if _, err := m.statFunc(m.enabledScriptPath()); err == nil {
		return m.enabledScriptPath()
	}
	if _, err := m.statFunc(m.disabledScriptPath()); err == nil {
		return m.disabledScriptPath()
	}
	return m.initScriptPath()
}

func (m *sysvinitManager) hasServiceCommand() bool {
	_, err := m.lookPathFunc("service")
	return err == nil
}

func (m *sysvinitManager) hasUpdateRcD() bool {
	_, err := m.lookPathFunc("update-rc.d")
	return err == nil
}

func (m *sysvinitManager) enableDebian() error {
	if out, err := m.executor.Run("update-rc.d", m.serviceName, "defaults"); err != nil {
		return runErr("enabling service with update-rc.d", out, err)
	}
	return nil
}

func (m *sysvinitManager) disableDebian() error {
	if out, err := m.executor.Run("update-rc.d", "-f", m.serviceName, "remove"); err != nil {
		return runErr("disabling service with update-rc.d", out, err)
	}
	return nil
}

func (m *sysvinitManager) hasChkconfig() bool {
	_, err := m.lookPathFunc("chkconfig")
	return err == nil
}

func (m *sysvinitManager) enableRHEL() error {
	if out, err := m.executor.Run("chkconfig", "--add", m.serviceName); err != nil {
		return runErr("registering service with chkconfig", out, err)
	}
	if out, err := m.executor.Run("chkconfig", m.serviceName, "on"); err != nil {
		return runErr("enabling service with chkconfig", out, err)
	}
	return nil
}

func (m *sysvinitManager) disableRHEL() error {
	if out, err := m.executor.Run("chkconfig", m.serviceName, "off"); err != nil {
		return runErr("disabling service with chkconfig", out, err)
	}
	if out, err := m.executor.Run("chkconfig", "--del", m.serviceName); err != nil {
		return runErr("unregistering service with chkconfig", out, err)
	}
	return nil
}

// enableBusyBox activates the service using K/S rename or flag file removal.
func (m *sysvinitManager) enableBusyBox() error {
	if !m.supportsKSPrefix {
		if err := m.fs.Remove(m.flagFilePath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing disabled flag: %w", err)
		}
		return nil
	}
	if _, err := m.statFunc(m.enabledScriptPath()); err == nil {
		return nil
	}
	if _, err := m.statFunc(m.disabledScriptPath()); err == nil {
		return m.renameFunc(m.disabledScriptPath(), m.enabledScriptPath())
	}
	if _, err := m.statFunc(m.initScriptPath()); err == nil {
		return m.renameFunc(m.initScriptPath(), m.enabledScriptPath())
	}
	return fmt.Errorf("no init script found for %s", m.serviceName)
}

// disableBusyBox deactivates the service using K/S rename or flag file creation.
func (m *sysvinitManager) disableBusyBox() error {
	if !m.supportsKSPrefix {
		if err := m.fs.WriteFile(m.flagFilePath(), []byte("disabled\n"), 0o644); err != nil {
			return fmt.Errorf("creating disabled flag: %w", err)
		}
		return nil
	}
	if _, err := m.statFunc(m.disabledScriptPath()); err == nil {
		return nil
	}
	if _, err := m.statFunc(m.enabledScriptPath()); err == nil {
		return m.renameFunc(m.enabledScriptPath(), m.disabledScriptPath())
	}
	if _, err := m.statFunc(m.initScriptPath()); err == nil {
		return m.renameFunc(m.initScriptPath(), m.disabledScriptPath())
	}
	return fmt.Errorf("no init script found for %s", m.serviceName)
}

func (m *sysvinitManager) enableSymlinks() error {
	startLevels := []string{"2", "3", "4", "5"}
	stopLevels := []string{"0", "1", "6"}
	target := m.initScriptPath()

	for _, level := range startLevels {
		link := fmt.Sprintf("/etc/rc%s.d/S20%s", level, m.serviceName)
		if _, err := m.statFunc(link); err == nil {
			continue
		}
		if err := m.symlinkFunc(target, link); err != nil {
			return fmt.Errorf("creating symlink %s: %w", link, err)
		}
	}
	for _, level := range stopLevels {
		link := fmt.Sprintf("/etc/rc%s.d/K20%s", level, m.serviceName)
		if _, err := m.statFunc(link); err == nil {
			continue
		}
		if err := m.symlinkFunc(target, link); err != nil {
			return fmt.Errorf("creating symlink %s: %w", link, err)
		}
	}
	return nil
}

func (m *sysvinitManager) disableSymlinks() error {
	startLevels := []string{"2", "3", "4", "5"}
	stopLevels := []string{"0", "1", "6"}

	for _, level := range startLevels {
		link := fmt.Sprintf("/etc/rc%s.d/S20%s", level, m.serviceName)
		if err := m.fs.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing symlink %s: %w", link, err)
		}
	}
	for _, level := range stopLevels {
		link := fmt.Sprintf("/etc/rc%s.d/K20%s", level, m.serviceName)
		if err := m.fs.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing symlink %s: %w", link, err)
		}
	}
	return nil
}
