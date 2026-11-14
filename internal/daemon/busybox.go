// Package daemon registers services with the host's init system, detecting
// which one is in use and generating the manifests it expects.
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
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"
)

const defaultGracefulTimeout = 10 * time.Second

type busyboxManager struct {
	serviceName      string
	supportsKSPrefix bool
	executor         commandExecutor
	fs               fileSystem
	statFunc         func(string) (os.FileInfo, error)
	readlinkFunc     func(string) (string, error)
	lookPathFunc     func(string) (string, error)
	readFileFunc     func(string) ([]byte, error)
	killFunc         func(int, syscall.Signal) error
	readDirFunc      func(string) ([]os.DirEntry, error)
	sleepFunc        func(time.Duration)
	renameFunc       func(string, string) error
	gracefulTimeout  time.Duration
}

func newBusyboxManager() *busyboxManager {
	return &busyboxManager{
		executor:        &execCommandExecutor{},
		fs:              &osFileSystem{},
		statFunc:        os.Stat,
		readlinkFunc:    os.Readlink,
		lookPathFunc:    exec.LookPath,
		readFileFunc:    os.ReadFile,
		killFunc:        defaultKill,
		readDirFunc:     os.ReadDir,
		sleepFunc:       time.Sleep,
		renameFunc:      os.Rename,
		gracefulTimeout: defaultGracefulTimeout,
	}
}

func (m *busyboxManager) Install(config ServiceConfig) error {
	m.serviceName = config.Name
	m.supportsKSPrefix = m.detectKSPrefixSupport()

	content, err := GenerateBusyboxInitScript(config, m.execPath(config))
	if err != nil {
		return fmt.Errorf("generating init script: %w", err)
	}

	for _, path := range []string{m.plainScriptPath(), m.initScriptPath(), m.disabledScriptPath(), m.flagFilePath()} {
		if err := m.fs.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("cleaning up %s: %w", path, err)
		}
	}

	if m.supportsKSPrefix {
		if err := m.fs.WriteFile(m.disabledScriptPath(), []byte(content), 0o755); err != nil {
			return fmt.Errorf("writing init script: %w", err)
		}
	} else {
		if err := m.fs.WriteFile(m.plainScriptPath(), []byte(content), 0o755); err != nil {
			return fmt.Errorf("writing init script: %w", err)
		}
		if err := m.fs.WriteFile(m.flagFilePath(), []byte("disabled\n"), 0o644); err != nil {
			return fmt.Errorf("creating disabled flag: %w", err)
		}
	}

	return nil
}

func (m *busyboxManager) Uninstall() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}

	_, _ = m.executor.Run(m.resolveScriptPath(), "stop")

	if err := m.fs.Remove(m.initScriptPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing init script: %w", err)
	}
	if err := m.fs.Remove(m.disabledScriptPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing disabled init script: %w", err)
	}
	if err := m.fs.Remove(m.plainScriptPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing plain init script: %w", err)
	}

	_ = m.fs.Remove(m.pidFilePath())
	_ = m.fs.Remove(m.flagFilePath())

	return nil
}

func (m *busyboxManager) Start() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if out, err := m.executor.Run(m.resolveScriptPath(), "start"); err != nil {
		return runErr("starting service", out, err)
	}
	return nil
}

func (m *busyboxManager) Stop() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}

	pid, err := m.readPID()
	if err != nil {
		pid, err = m.findPIDByBinary()
		if err != nil {
			return fmt.Errorf("finding process: %w", err)
		}
	}

	if !m.validatePID(pid) {
		_ = m.fs.Remove(m.pidFilePath())
		return fmt.Errorf("stale PID file: process %d is not %s", pid, m.serviceName)
	}

	if err := m.killFunc(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("sending SIGTERM: %w", err)
	}

	deadline := time.Now().Add(m.gracefulTimeout)
	for time.Now().Before(deadline) {
		if err := m.killFunc(pid, 0); err != nil {
			_ = m.fs.Remove(m.pidFilePath())
			return nil //nolint:nilerr // kill(pid, 0) failing means the process is already gone
		}
		m.sleepFunc(200 * time.Millisecond)
	}

	if err := m.killFunc(pid, syscall.SIGKILL); err != nil {
		if err := m.killFunc(pid, 0); err != nil {
			_ = m.fs.Remove(m.pidFilePath())
			return nil //nolint:nilerr // kill(pid, 0) failing means the process is already gone
		}
		return fmt.Errorf("sending SIGKILL: %w", err)
	}

	_ = m.fs.Remove(m.pidFilePath())
	return nil
}

func (m *busyboxManager) Enable() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if !m.supportsKSPrefix {
		if err := m.fs.Remove(m.flagFilePath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing disabled flag: %w", err)
		}
		return nil
	}
	if _, err := m.statFunc(m.initScriptPath()); err == nil {
		return nil
	}
	if _, err := m.statFunc(m.disabledScriptPath()); err != nil {
		return fmt.Errorf("no init script found for %s", m.serviceName)
	}
	if err := m.renameFunc(m.disabledScriptPath(), m.initScriptPath()); err != nil {
		return fmt.Errorf("enabling service: %w", err)
	}
	return nil
}

func (m *busyboxManager) Disable() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if !m.supportsKSPrefix {
		if err := m.fs.WriteFile(m.flagFilePath(), []byte("disabled\n"), 0o644); err != nil {
			return fmt.Errorf("creating disabled flag: %w", err)
		}
		return nil
	}
	if _, err := m.statFunc(m.disabledScriptPath()); err == nil {
		return nil
	}
	if _, err := m.statFunc(m.initScriptPath()); err != nil {
		return fmt.Errorf("no init script found for %s", m.serviceName)
	}
	if err := m.renameFunc(m.initScriptPath(), m.disabledScriptPath()); err != nil {
		return fmt.Errorf("disabling service: %w", err)
	}
	return nil
}

// The daemon is run under a small shell supervisor (respawn loop) instead of
// being backgrounded once: BusyBox init never restarts a service that dies, so
// an OOM-killed or crashed watchdog would otherwise stay dead until a reboot
// (the RISC-V failure mode). The loop respawns the daemon on an abnormal exit
// and stops on a clean exit (0 = an intentional permanent stop) or when `stop`
// requests it via the stop-flag file — so it never fights an operator stop or a
// delete-agent teardown. PIDFILE holds the daemon PID (not the loop), so `stop`
// and `status` keep working unchanged.
const busyboxInitScriptTemplate = `#!/bin/sh
DAEMON={{.ExecPath}}
NAME="{{.Name}}"
PIDFILE="/var/run/${NAME}.pid"
STOPFILE="/var/run/${NAME}.stop"
{{- range .EnvLines}}
export {{.}}
{{- end}}

if [ -f "/etc/init.d/{{.Name}}.disabled" ]; then exit 0; fi

supervise() {
{{- if .WorkingDir}}
  cd {{.WorkingDir}} || exit 1
{{- end}}
  while [ ! -f "${STOPFILE}" ]; do
    "${DAEMON}"{{if .Args}} {{.Args}}{{end}} &
    DPID=$!
    echo "${DPID}" > "${PIDFILE}"
    wait "${DPID}"
    RC=$?
    if [ -f "${STOPFILE}" ] || [ "${RC}" -eq 0 ]; then
      break
    fi
    sleep 2
  done
  rm -f "${PIDFILE}"
}

case "$1" in
  start)
    printf "Starting ${NAME}: "
    rm -f "${STOPFILE}"
    (supervise) </dev/null >/dev/null 2>&1 &
    echo "OK"
    ;;
  stop)
    printf "Stopping ${NAME}: "
    touch "${STOPFILE}"
    if [ -f "${PIDFILE}" ]; then
      kill $(cat "${PIDFILE}") 2>/dev/null
    fi
    T=30
    while [ -f "${PIDFILE}" ] && [ "${T}" -gt 0 ]; do sleep 1; T=$((T - 1)); done
    rm -f "${STOPFILE}" "${PIDFILE}"
    echo "OK"
    ;;
  restart)
    $0 stop
    sleep 1
    $0 start
    ;;
  *)
    echo "Usage: $0 {start|stop|restart}"
    exit 1
    ;;
esac
`

type busyboxTemplateData struct {
	ExecPath   string
	WorkingDir string
	Name       string
	Args       string
	EnvLines   []string
}

func buildBusyboxTemplateData(config ServiceConfig, execPath string) busyboxTemplateData {
	data := busyboxTemplateData{
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

// GenerateBusyboxInitScript renders the /etc/init.d script for BusyBox init,
// including the shell respawn loop that substitutes for BusyBox's lack of
// service restarts.
func GenerateBusyboxInitScript(config ServiceConfig, execPath string) (string, error) {
	if err := validateServiceText(config, execPath, "busybox"); err != nil {
		return "", err
	}
	if err := validateEnvKeys(config.Environment); err != nil {
		return "", fmt.Errorf("busybox: %w", err)
	}
	data := buildBusyboxTemplateData(config, execPath)

	tmpl, err := template.New("busybox").Parse(busyboxInitScriptTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing busybox template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing busybox template: %w", err)
	}
	return buf.String(), nil
}

func detectBusybox() bool {
	mgr := newBusyboxManager()
	return mgr.detect()
}

func (m *busyboxManager) Status() (ServiceStatus, error) {
	if m.serviceName == "" {
		return ServiceStatusUnknown, errors.New("service name not set")
	}

	pid, err := m.readPID()
	if err != nil {
		if _, statErr := m.statFunc(m.pidFilePath()); errors.Is(statErr, fs.ErrNotExist) {
			return ServiceStatusStopped, nil
		}
		return ServiceStatusUnknown, nil
	}

	if _, err := m.statFunc(fmt.Sprintf("/proc/%d", pid)); err != nil {
		return ServiceStatusStopped, nil //nolint:nilerr // a missing /proc entry means the service is not running
	}

	if !m.validatePID(pid) {
		_ = m.fs.Remove(m.pidFilePath())
		return ServiceStatusStopped, nil
	}

	return ServiceStatusRunning, nil
}

func isBusyboxPath(path string) bool {
	lower := strings.ToLower(path)
	found := strings.Contains(lower, "busybox")
	getDetectionLog().Debug("init detection: busybox: isBusyboxPath check", "path", path, "found", found)
	return found
}

func (m *busyboxManager) GetManifestPath() string {
	return m.resolveScriptPath()
}

func (m *busyboxManager) GenerateConfig(config ServiceConfig) (string, error) {
	m.serviceName = config.Name
	return GenerateBusyboxInitScript(config, m.execPath(config))
}

func (m *busyboxManager) WriteConfig(content string) error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}
	if err := m.fs.WriteFile(m.resolveScriptPath(), []byte(content), 0o755); err != nil {
		return fmt.Errorf("writing init script: %w", err)
	}
	return nil
}

func (m *busyboxManager) ReadInstalledConfig() (string, error) {
	if m.serviceName == "" {
		return "", errors.New("service name not set; call Install first or set service name")
	}
	data, err := m.readFileFunc(m.resolveScriptPath())
	if err != nil {
		return "", fmt.Errorf("reading init script: %w", err)
	}
	return string(data), nil
}

func (m *busyboxManager) CanRestartWatchdog() bool {
	return false
}

func (m *busyboxManager) SupportsEnable() bool { return true }

func (m *busyboxManager) SupportsStart() bool { return true }

func (m *busyboxManager) SupportsStatus() bool { return true }

func (m *busyboxManager) SupportsScope(scope Scope) bool { return scope == ScopeSystem }

func (m *busyboxManager) Name() string {
	return "busybox"
}

func cleanExeBase(target string) string {
	base := filepath.Base(target)
	return strings.TrimSuffix(base, " (deleted)")
}

func (m *busyboxManager) initScriptPath() string {
	return filepath.Join("/etc/init.d", "S95"+m.serviceName)
}

func (m *busyboxManager) disabledScriptPath() string {
	return filepath.Join("/etc/init.d", "K95"+m.serviceName)
}

func (m *busyboxManager) plainScriptPath() string {
	return filepath.Join("/etc/init.d", m.serviceName)
}

func (m *busyboxManager) flagFilePath() string {
	return filepath.Join("/etc/init.d", m.serviceName+".disabled")
}

func (m *busyboxManager) pidFilePath() string {
	return filepath.Join("/var/run", m.serviceName+".pid")
}

func (m *busyboxManager) execPath(config ServiceConfig) string {
	if config.ExecPath != "" {
		return config.ExecPath
	}
	installDir := config.InstallDir
	if installDir == "" {
		installDir = filepath.Join("/opt", config.Name)
	}
	return filepath.Join(installDir, config.Name)
}

func (m *busyboxManager) detect() bool {
	getDetectionLog().Debug("init detection: busybox: checking BusyBox detection", "detector", "busybox")
	if runtime.GOOS != "linux" {
		getDetectionLog().Debug("init detection: busybox: detected=false reason=not linux", "detector", "busybox", "detected", false)
		return false
	}
	if m.excludedByOtherInit() {
		return false
	}
	return m.hasBusyboxIndicators()
}

// excludedByOtherInit reports whether a more specific init system is present.
// BusyBox ships the init binary on systems that run something else on top of
// it, so these exclusions must be checked before any positive indicator.
func (m *busyboxManager) excludedByOtherInit() bool {
	if _, err := m.statFunc("/run/systemd/system"); err == nil {
		getDetectionLog().Debug("init detection: busybox: systemd detected, excluding busybox", "detector", "busybox", "path", "/run/systemd/system")
		getDetectionLog().Debug("init detection: busybox: detected=false reason=systemd present", "detector", "busybox", "detected", false)
		return true
	}

	if _, err := m.lookPathFunc("rc-service"); err == nil {
		getDetectionLog().Debug("init detection: busybox: openrc rc-service found, excluding busybox", "detector", "busybox")
		getDetectionLog().Debug("init detection: busybox: detected=false reason=openrc present", "detector", "busybox", "detected", false)
		return true
	}
	if _, err := m.statFunc("/sbin/openrc"); err == nil {
		getDetectionLog().Debug("init detection: busybox: /sbin/openrc found, excluding busybox", "detector", "busybox")
		getDetectionLog().Debug("init detection: busybox: detected=false reason=openrc present", "detector", "busybox", "detected", false)
		return true
	}
	if _, err := m.statFunc("/sbin/openrc-run"); err == nil {
		getDetectionLog().Debug("init detection: busybox: /sbin/openrc-run found, excluding busybox", "detector", "busybox")
		getDetectionLog().Debug("init detection: busybox: detected=false reason=openrc present", "detector", "busybox", "detected", false)
		return true
	}
	if _, err := m.statFunc("/bin/openrc"); err == nil {
		getDetectionLog().Debug("init detection: busybox: /bin/openrc found, excluding busybox", "detector", "busybox")
		getDetectionLog().Debug("init detection: busybox: detected=false reason=openrc present", "detector", "busybox", "detected", false)
		return true
	}

	if _, err := m.lookPathFunc("runsvdir"); err == nil {
		if _, err := m.statFunc("/var/service"); err == nil {
			getDetectionLog().Debug("init detection: busybox: runit runsvdir in path and /var/service exists, excluding busybox", "detector", "busybox")
			getDetectionLog().Debug("init detection: busybox: detected=false reason=runit present", "detector", "busybox", "detected", false)
			return true
		}
	} else {
		for _, p := range []string{"/sbin/runsvdir", "/usr/bin/runsvdir"} {
			if _, err := m.statFunc(p); err == nil {
				if _, err := m.statFunc("/var/service"); err == nil {
					getDetectionLog().Debug("init detection: busybox: runit "+p+" and /var/service found, excluding busybox", "detector", "busybox")
					getDetectionLog().Debug("init detection: busybox: detected=false reason=runit present", "detector", "busybox", "detected", false)
					return true
				}
				break
			}
		}
	}

	if _, err := m.lookPathFunc("s6-svscan"); err == nil {
		getDetectionLog().Debug("init detection: busybox: s6-svscan found, excluding busybox", "detector", "busybox")
		getDetectionLog().Debug("init detection: busybox: detected=false reason=s6 present", "detector", "busybox", "detected", false)
		return true
	}
	if _, err := m.statFunc("/usr/bin/s6-svscan"); err == nil {
		getDetectionLog().Debug("init detection: busybox: /usr/bin/s6-svscan found, excluding busybox", "detector", "busybox")
		getDetectionLog().Debug("init detection: busybox: detected=false reason=s6 present", "detector", "busybox", "detected", false)
		return true
	}

	if _, err := m.statFunc("/sbin/procd"); err == nil {
		if _, err := m.statFunc("/lib/functions/procd.sh"); err == nil {
			getDetectionLog().Debug("init detection: busybox: procd detected, excluding busybox", "detector", "busybox")
			getDetectionLog().Debug("init detection: busybox: detected=false reason=procd present", "detector", "busybox", "detected", false)
			return true
		}
	}

	return false
}

// hasBusyboxIndicators reports whether PID 1 is BusyBox: it checks the symlink
// target, its resolved path, /sbin/init, and finally the binary's contents (the
// nanokvm case, where no path carries a "busybox" marker).
func (m *busyboxManager) hasBusyboxIndicators() bool {
	proc1Target, proc1Err := m.readlinkFunc("/proc/1/exe")
	if proc1Err == nil {
		getDetectionLog().Debug("init detection: busybox: readlink /proc/1/exe -> "+proc1Target, "detector", "busybox", "target", proc1Target)
		if isBusyboxPath(proc1Target) {
			getDetectionLog().Debug("init detection: busybox: found 'busybox' in path", "detector", "busybox")
			getDetectionLog().Debug("init detection: busybox: detected=true reason=PID 1 is busybox", "detector", "busybox", "detected", true)
			return true
		}

		resolved, resolveErr := m.resolveSymlinks(proc1Target, 10)
		if resolveErr == nil && resolved != proc1Target {
			getDetectionLog().Debug("init detection: busybox: resolved symlinks for target", "from", proc1Target, "to", resolved)
			if isBusyboxPath(resolved) {
				getDetectionLog().Debug("init detection: busybox: found 'busybox' in resolved path", "detector", "busybox")
				getDetectionLog().Debug("init detection: busybox: detected=true reason=PID 1 resolves to busybox", "detector", "busybox", "detected", true)
				return true
			}
		}
	} else {
		getDetectionLog().Debug("init detection: busybox: readlink /proc/1/exe -> "+proc1Err.Error(), "detector", "busybox", "error", proc1Err)
	}

	initTarget, err := m.readlinkFunc("/sbin/init")
	if err == nil {
		getDetectionLog().Debug("init detection: busybox: readlink /sbin/init -> "+initTarget, "detector", "busybox", "target", initTarget)
		if isBusyboxPath(initTarget) {
			getDetectionLog().Debug("init detection: busybox: found 'busybox' in /sbin/init target", "detector", "busybox")
			getDetectionLog().Debug("init detection: busybox: detected=true reason=/sbin/init points to busybox", "detector", "busybox", "detected", true)
			return true
		}
	} else {
		getDetectionLog().Debug("init detection: busybox: readlink /sbin/init -> "+err.Error(), "detector", "busybox", "error", err)
	}

	if _, err := m.statFunc("/sbin/init"); err == nil {
		getDetectionLog().Debug("init detection: busybox: stat /sbin/init -> found, running --help", "detector", "busybox")
		output, execErr := m.executor.Run("/sbin/init", "--help")
		if execErr == nil {
			helpOutput := strings.ToLower(string(output))
			if strings.Contains(helpOutput, "busybox") {
				getDetectionLog().Debug("init detection: busybox: /sbin/init --help contains 'busybox'", "detector", "busybox")
				getDetectionLog().Debug("init detection: busybox: detected=true reason=/sbin/init --help contains busybox", "detector", "busybox", "detected", true)
				return true
			}
			getDetectionLog().Debug("init detection: busybox: /sbin/init --help does not contain 'busybox'", "detector", "busybox")
		} else {
			getDetectionLog().Debug("init detection: busybox: /sbin/init --help failed: "+execErr.Error(), "detector", "busybox", "error", execErr)
		}
	} else {
		getDetectionLog().Debug("init detection: busybox: stat /sbin/init -> "+err.Error(), "detector", "busybox", "error", err)
	}

	if proc1Err == nil {
		if m.checkBinaryForBusyBox(proc1Target) {
			getDetectionLog().Debug("init detection: busybox: detected=true reason=binary content contains BusyBox", "detector", "busybox", "detected", true)
			return true
		}
	}

	getDetectionLog().Debug("init detection: busybox: detected=false reason=no busybox indicators found", "detector", "busybox", "detected", false)
	return false
}

func (m *busyboxManager) checkBinaryForBusyBox(path string) bool {
	getDetectionLog().Debug("init detection: busybox: checking binary content", "path", path)
	data, err := m.readFileFunc(path)
	if err != nil {
		getDetectionLog().Debug("init detection: busybox: failed to read binary", "path", path, "error", err)
		return false
	}
	header := data
	if len(header) > 512 {
		header = header[:512]
	}
	content := string(header)
	if strings.Contains(content, "BusyBox") || strings.Contains(content, "busybox") {
		getDetectionLog().Debug("init detection: busybox: found BusyBox in binary content", "path", path)
		return true
	}
	getDetectionLog().Debug("init detection: busybox: no BusyBox found in binary content", "path", path)
	return false
}

func (m *busyboxManager) resolveSymlinks(path string, maxDepth int) (string, error) {
	for i := range maxDepth {
		target, err := m.readlinkFunc(path)
		if err != nil {
			getDetectionLog().Debug("init detection: busybox: resolveSymlinks: not a symlink or end of chain", "path", path, "depth", i)
			return path, nil //nolint:nilerr // readlink failing means this is not a symlink, so the chain ends here
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		getDetectionLog().Debug("init detection: busybox: resolveSymlinks: followed link", "from", path, "to", target, "depth", i)
		path = target
	}
	return "", fmt.Errorf("too many symlink levels (max %d)", maxDepth)
}

func (m *busyboxManager) detectKSPrefixSupport() bool {
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

func (m *busyboxManager) readPID() (int, error) {
	data, err := m.readFileFunc(m.pidFilePath())
	if err != nil {
		return 0, fmt.Errorf("reading PID file: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parsing PID: %w", err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("invalid PID: %d", pid)
	}
	return pid, nil
}

func (m *busyboxManager) validatePID(pid int) bool {
	exePath := fmt.Sprintf("/proc/%d/exe", pid)
	target, err := m.readlinkFunc(exePath)
	if err != nil {
		return false
	}
	return cleanExeBase(target) == m.serviceName
}

func (m *busyboxManager) findPIDByBinary() (int, error) {
	entries, err := m.readDirFunc("/proc")
	if err != nil {
		return 0, fmt.Errorf("reading /proc: %w", err)
	}

	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		exePath := filepath.Join("/proc", entry.Name(), "exe")
		target, err := m.readlinkFunc(exePath)
		if err != nil {
			continue
		}
		if cleanExeBase(target) == m.serviceName {
			return pid, nil
		}
	}

	return 0, fmt.Errorf("no process found for %s", m.serviceName)
}

func (m *busyboxManager) resolveScriptPath() string {
	if _, err := m.statFunc(m.initScriptPath()); err == nil {
		return m.initScriptPath()
	}
	if _, err := m.statFunc(m.disabledScriptPath()); err == nil {
		return m.disabledScriptPath()
	}
	if _, err := m.statFunc(m.plainScriptPath()); err == nil {
		return m.plainScriptPath()
	}
	return m.initScriptPath()
}
