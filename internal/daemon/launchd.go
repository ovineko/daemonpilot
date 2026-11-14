package daemon

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

// ErrEnableNotSupported reports that launchd has no separate enable/disable
// step; boot behaviour is controlled by the RunAtLoad plist key instead.
var ErrEnableNotSupported = errors.New("enable/disable not supported by launchd: use RunAtLoad in LaunchdKeys configuration instead")

const launchdPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.ExecPath}}</string>
{{- range .ExtraArgs}}
		<string>{{.}}</string>
{{- end}}
	</array>
	<key>WorkingDirectory</key>
	<string>{{.WorkingDir}}</string>
	<key>KeepAlive</key>
	<true/>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardOutPath</key>
	<string>{{.LogPath}}.out.log</string>
	<key>StandardErrorPath</key>
	<string>{{.LogPath}}.err.log</string>
{{- if .UserName}}
	<key>UserName</key>
	<string>{{.UserName}}</string>
{{- end}}
{{- if .GroupName}}
	<key>GroupName</key>
	<string>{{.GroupName}}</string>
{{- end}}
{{- if .EnvironmentXML}}
	<key>EnvironmentVariables</key>
	<dict>
{{.EnvironmentXML}}	</dict>
{{- end}}
{{- range .CustomKeysXML}}
{{.}}
{{- end}}
</dict>
</plist>
`

type plistData struct {
	Label          string
	ExecPath       string
	WorkingDir     string
	LogPath        string
	UserName       string
	GroupName      string
	ExtraArgs      []string
	EnvironmentXML string
	CustomKeysXML  []string
}

type launchdManager struct {
	label string
	scope Scope
	// domain is the resolved launchd domain target ("system", "gui/501",
	// "user/501"). Once an agent has been bootstrapped into a domain every later
	// operation must name that same domain, so it is pinned in the state file and
	// restored here rather than re-resolved against a session whose graphical
	// state may since have changed.
	domain string
	// previousDomain is the domain the last install recorded, used by Install
	// alone to boot out an agent this session would otherwise orphan in a
	// domain it no longer resolves to. Lifecycle verbs use domain instead.
	previousDomain string
	// logDir is the resolved StandardOutPath parent, remembered from Install or
	// GenerateConfig so WriteConfig can create it too.
	logDir string
	// logUser and logGroup are the UserName/GroupName the plist drops
	// privileges to, remembered alongside logDir so the log directory can be
	// handed to that account. Empty for user scope, which never drops.
	logUser     string
	logGroup    string
	executor    commandExecutor
	fs          fileSystem
	homeFunc    func() (string, error)
	geteuidFunc func() int
}

func newLaunchdManager() *launchdManager {
	return &launchdManager{
		executor:    &execCommandExecutor{},
		fs:          &osFileSystem{},
		homeFunc:    os.UserHomeDir,
		geteuidFunc: os.Geteuid,
	}
}

// launchDaemonsDir is where system-wide LaunchDaemons live. Daemons here run at
// boot as root regardless of whether a user is logged in (unlike per-user
// LaunchAgents), which is what a supervised background service needs.
const launchDaemonsDir = "/Library/LaunchDaemons"

// launchdSystemDomain is the domain target machine-wide daemons are
// bootstrapped into.
const launchdSystemDomain = "system"

// launchdSystemLogRoot is the parent of the system-scope log directory. The
// per-service directory below it replaces the former /tmp/<name>.{out,err}.log
// destination: /tmp is world-writable, so any local user could pre-create those
// paths and have a root LaunchDaemon append to them.
const launchdSystemLogRoot = "/var/log"

// resolveLaunchdLogDir returns the directory the plist points its standard
// output and error at. An explicit ServiceConfig.LogDir always wins; otherwise
// system scope gets /var/log/<name> and user scope ~/Library/Logs/<name>,
// derived from $HOME alone.
func resolveLaunchdLogDir(config ServiceConfig, homeFunc func() (string, error)) (string, error) {
	if config.LogDir != "" {
		return config.LogDir, nil
	}
	if config.Scope != ScopeUser {
		return filepath.Join(launchdSystemLogRoot, config.Name), nil
	}

	if homeFunc == nil {
		homeFunc = os.UserHomeDir
	}
	home, err := homeFunc()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoHomeDir, err)
	}
	if home == "" {
		return "", ErrNoHomeDir
	}
	return filepath.Join(home, "Library", "Logs", config.Name), nil
}

func launchdLabel(name string) string {
	return "com." + name
}

func (m *launchdManager) Install(config ServiceConfig) error {
	m.label = launchdLabel(config.Name)
	m.scope = config.Scope

	logDir, err := resolveLaunchdLogDir(config, m.homeFunc)
	if err != nil {
		return err
	}
	config.LogDir = logDir
	m.logDir = logDir
	m.rememberLogOwner(config)

	content, err := GeneratePlistFile(config)
	if err != nil {
		return fmt.Errorf("generating plist file: %w", err)
	}

	path, err := m.plistPathErr()
	if err != nil {
		return err
	}

	domain, err := m.resolveDomain()
	if err != nil {
		return err
	}

	// The log directory is created before the plist, not after: launchd
	// auto-loads every plist under ~/Library/LaunchAgents at the next login, so
	// a plist written for an install that then failed on the log directory would
	// come back on its own and fail to spawn, with no state file recording it.
	// Failing first leaves any previous install exactly as it was.
	if err := m.ensureLogDir(); err != nil {
		return err
	}

	if err := m.fs.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating launchd directory: %w", err)
	}

	// Read before overwriting, so a failed bootstrap can put back whatever a
	// re-install just replaced. The read happens before anything is mutated, so
	// failing here is free.
	previousPlist, hadPlist, err := m.readPlistFile(path)
	if err != nil {
		return err
	}

	if err := m.fs.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing plist file: %w", err)
	}

	// Bootstrap into the resolved domain (the system domain requires root).
	// Bootout any prior instance first so a re-install is idempotent; the
	// bootout is harmless when nothing is loaded.
	//
	// Any domain other than the resolved one that could still hold this job is
	// booted out first: an agent installed from a graphical session lives in
	// gui/<uid>, and a re-install over SSH resolves user/<uid>, so booting out
	// only the new target would leave the old job loaded against the same binary
	// with nothing in the state file — which now records the new domain — able to
	// remove it.
	m.bootoutStaleDomains(domain)
	_, _ = m.executor.Run("launchctl", "bootout", domain+"/"+m.label)
	if out, err := m.executor.Run("launchctl", "bootstrap", domain, path); err != nil {
		restored := m.rollbackPlist(path, previousPlist, hadPlist)
		m.reloadRolledBackPlist(m.rollbackDomain(domain), path, restored)
		return runErr("bootstrapping service", out, err)
	}

	return nil
}

func escapeXML(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

func renderPlistValue(v any) string {
	switch val := v.(type) {
	case string:
		return "\t<string>" + escapeXML(val) + "</string>"
	case bool:
		if val {
			return "\t<true/>"
		}
		return "\t<false/>"
	case int:
		return fmt.Sprintf("\t<integer>%d</integer>", val)
	case int64:
		return fmt.Sprintf("\t<integer>%d</integer>", val)
	case float64:
		return fmt.Sprintf("\t<real>%g</real>", val)
	default:
		return fmt.Sprintf("\t<string>%s</string>", escapeXML(fmt.Sprintf("%v", val)))
	}
}

func buildPlistData(config ServiceConfig, logDir string) plistData {
	data := plistData{
		Label:      escapeXML(launchdLabel(config.Name)),
		ExecPath:   escapeXML(config.ExecPath),
		WorkingDir: escapeXML(config.WorkingDir),
		LogPath:    escapeXML(filepath.Join(logDir, config.Name)),
	}

	// UserName/GroupName are the launchd equivalents of systemd's User=/Group=.
	// They only mean anything to a machine-wide daemon: a LaunchAgent already
	// runs as the owning user, and launchd refuses to drop privileges from a
	// user domain. The user-scope combination is rejected earlier by Validate.
	if config.Scope != ScopeUser {
		data.UserName = escapeXML(config.User)
		data.GroupName = escapeXML(config.Group)
	}

	for _, arg := range config.Arguments {
		data.ExtraArgs = append(data.ExtraArgs, escapeXML(arg))
	}

	if len(config.Environment) > 0 {
		keys := make([]string, 0, len(config.Environment))
		for k := range config.Environment {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var envBuf bytes.Buffer
		for _, k := range keys {
			fmt.Fprintf(&envBuf, "\t\t<key>%s</key>\n", escapeXML(k))
			fmt.Fprintf(&envBuf, "\t\t<string>%s</string>\n", escapeXML(config.Environment[k]))
		}
		data.EnvironmentXML = envBuf.String()
	}

	if len(config.LaunchdKeys) > 0 {
		keys := make([]string, 0, len(config.LaunchdKeys))
		for k := range config.LaunchdKeys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			entry := "\t<key>" + escapeXML(k) + "</key>\n" + renderPlistValue(config.LaunchdKeys[k])
			data.CustomKeysXML = append(data.CustomKeysXML, entry)
		}
	}

	return data
}

// GeneratePlistFile renders the launchd property list for the configured
// service, merging any custom keys from LaunchdKeys.
func GeneratePlistFile(config ServiceConfig) (string, error) {
	if err := validateServiceText(config, config.ExecPath, "launchd"); err != nil {
		return "", err
	}
	logDir, err := resolveLaunchdLogDir(config, os.UserHomeDir)
	if err != nil {
		return "", err
	}
	data := buildPlistData(config, logDir)

	tmpl, err := template.New("plist").Parse(launchdPlistTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing plist template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing plist template: %w", err)
	}
	return buf.String(), nil
}

// ValidatePlistXML reports whether content parses as the XML plist launchd
// expects, so a malformed file is never written to disk.
func ValidatePlistXML(content string) error {
	decoder := xml.NewDecoder(strings.NewReader(content))
	for {
		_, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("invalid plist XML: %w", err)
		}
	}
}

func (m *launchdManager) Uninstall() error {
	if m.label == "" {
		return errors.New("service label not set; call Install first or set service label")
	}

	path, err := m.plistPathErr()
	if err != nil {
		return err
	}

	// An unreachable domain must not strand the plist: uninstall is the only way
	// to clean up, and refusing here would leave the agent registered on disk
	// with no supported way to remove it. The bootout is best-effort anyway.
	domain, err := m.resolveDomain()
	if err != nil {
		logDetectionWarn("cannot resolve the launchd domain, unloading from every per-user domain instead: "+err.Error(),
			"manager", "launchd", "label", m.label)
	} else {
		_, _ = m.executor.Run("launchctl", "bootout", domain+"/"+m.label)
	}

	// The resolved domain is not necessarily the one the agent was bootstrapped
	// into: uninstall reaches this manager through readableState, which reports
	// an unreadable or corrupt state file as "no previous install", so the pinned
	// domain is lost and resolveDomain probes fresh — a session with different
	// login state then addresses the wrong per-user domain and the plist is
	// removed while the job stays loaded, with nothing left on disk to unload it.
	// Install and WriteConfig defend against exactly this; removal must too.
	m.bootoutStaleDomains(domain)

	if err := m.fs.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing plist file: %w", err)
	}

	return nil
}

func (m *launchdManager) Start() error {
	if m.label == "" {
		return errors.New("service label not set")
	}
	target, err := m.serviceTarget()
	if err != nil {
		return err
	}
	if out, err := m.executor.Run("launchctl", "kickstart", "-k", target); err != nil {
		return runErr("starting service", out, err)
	}
	return nil
}

func (m *launchdManager) Stop() error {
	if m.label == "" {
		return errors.New("service label not set")
	}
	target, err := m.serviceTarget()
	if err != nil {
		return err
	}
	if out, err := m.executor.Run("launchctl", "kill", "SIGTERM", target); err != nil {
		return runErr("stopping service", out, err)
	}
	return nil
}

func (m *launchdManager) Enable() error {
	return ErrEnableNotSupported
}

func (m *launchdManager) Disable() error {
	return ErrEnableNotSupported
}

func (m *launchdManager) GetManifestPath() string {
	path := m.plistPath()
	return path
}

func (m *launchdManager) GenerateConfig(config ServiceConfig) (string, error) {
	m.label = launchdLabel(config.Name)
	m.scope = config.Scope
	logDir, err := resolveLaunchdLogDir(config, m.homeFunc)
	if err != nil {
		return "", err
	}
	config.LogDir = logDir
	m.logDir = logDir
	m.rememberLogOwner(config)
	return GeneratePlistFile(config)
}

func (m *launchdManager) ReadInstalledConfig() (string, error) {
	path, err := m.plistPathErr()
	if err != nil {
		return "", err
	}
	data, err := m.fs.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading plist file: %w", err)
	}
	return string(data), nil
}

// The rollback variant is reached by type assertion, so a signature drift here
// would silently fall back to WriteConfig rather than fail the build.
var _ ConfigRollbackWriter = (*launchdManager)(nil)

func (m *launchdManager) WriteConfig(content string) error {
	return m.writeConfig(content, false)
}

// WriteConfigForRollback writes the plist without restoring or reloading the
// one it overwrites when the bootstrap fails — see ConfigRollbackWriter.
func (m *launchdManager) WriteConfigForRollback(content string) error {
	return m.writeConfig(content, true)
}

func (m *launchdManager) CanRestartWatchdog() bool {
	return true
}

func (m *launchdManager) SupportsEnable() bool { return false }

func (m *launchdManager) SupportsStart() bool  { return true }
func (m *launchdManager) SupportsStatus() bool { return true }

// SupportsScope reports true for both scopes: user scope installs a LaunchAgent
// under ~/Library/LaunchAgents, bootstrapped into gui/<uid> or user/<uid>.
//
// launchd has no equivalent of systemd lingering: an agent in gui/<uid> is torn
// down at logout and does not start at boot before a graphical login.
func (m *launchdManager) SupportsScope(scope Scope) bool {
	return scope == ScopeSystem || scope == ScopeUser
}

func (m *launchdManager) Name() string {
	return "launchd"
}

// Status queries the resolved domain explicitly.
//
// `launchctl list <label>` is implicitly scoped to the caller's own domain, so
// asking about a healthy root LaunchDaemon as an unprivileged user answers
// "stopped" instead of "you cannot see that domain". `launchctl print
// <domain>/<label>` names the domain, so a permission failure is reported as an
// error and only a genuinely absent job reads as stopped.
func (m *launchdManager) Status() (ServiceStatus, error) {
	if m.label == "" {
		return ServiceStatusUnknown, errors.New("service label not set")
	}

	target, err := m.serviceTarget()
	if err != nil {
		return ServiceStatusUnknown, err
	}

	output, err := m.executor.Run("launchctl", "print", target)
	if err != nil {
		if launchdNotLoaded(string(output)) {
			return ServiceStatusStopped, nil
		}
		return ServiceStatusUnknown, runErr("querying service status", output, err)
	}

	return parseLaunchdPrint(string(output)), nil
}

// launchdNotLoaded reports whether a failed `launchctl print` failed because the
// job is not loaded, as opposed to the domain being unreachable or forbidden.
func launchdNotLoaded(output string) bool {
	lower := strings.ToLower(output)
	for _, marker := range []string{"could not find service", "no such process", "no such file or directory"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// parseLaunchdPrint maps the `state = ...` line of a successful `launchctl
// print` to a ServiceStatus, falling back to the pid line on the older output
// formats that omit the state. A job that is not running and last exited
// non-zero is reported as failed.
//
// Only keys of the job dictionary itself are read. `launchctl print` nests
// sub-dictionaries — `resource coalition = { ... state = active }` and
// `jetsam coalition` among them — that carry a `state` key of their own, and
// they come after the job's own `state = running`, so a scan that took the last
// match would report every healthy service as failed.
func parseLaunchdPrint(output string) ServiceStatus {
	var (
		state    string
		pid      string
		exitCode string
	)
	depth := 0
	for line := range strings.SplitSeq(output, "\n") {
		trimmed := strings.TrimSpace(line)
		opens := strings.Count(trimmed, "{")
		closes := strings.Count(trimmed, "}")

		// depth 0 covers output that is not wrapped in an outer dictionary;
		// depth 1 is the job dictionary. A line opening a sub-dictionary is
		// never a value line, so it is skipped rather than parsed.
		if depth <= 1 && opens == 0 {
			if key, value, ok := strings.Cut(trimmed, "="); ok {
				value = strings.TrimSpace(value)
				switch strings.TrimSpace(key) {
				case "state":
					state = value
				case "pid":
					pid = value
				case "last exit code":
					exitCode = value
				}
			}
		}

		depth += opens - closes
		if depth < 0 {
			depth = 0
		}
	}

	if state == "running" || (state == "" && pid != "" && pid != "0") {
		return ServiceStatusRunning
	}
	// The job is not running. Only a numeric non-zero code means failure:
	// launchd prints "(never exited)" for a job that has never run and
	// "(signal: 15)" for one that was stopped, neither of which is a failure.
	// This is read whether or not `state` was present — on the older output
	// formats the pid fallback exists for, a crashed job would otherwise be
	// reported as merely stopped.
	if code, err := strconv.Atoi(exitCode); err == nil && code != 0 {
		return ServiceStatusFailed
	}
	return ServiceStatusStopped
}

// The domain is read back by pkg/daemonpilot through a type assertion, so a
// signature drift here would silently stop pinning it rather than fail the build.
var _ DomainReporter = (*launchdManager)(nil)

// ResolvedDomain reports the launchd domain this manager acts in, once it has
// been resolved or restored from state. It is empty until then.
func (m *launchdManager) ResolvedDomain() string {
	return m.domain
}

func (m *launchdManager) writeConfig(content string, isRollback bool) error {
	if m.label == "" {
		return errors.New("service label not set")
	}
	path, err := m.plistPathErr()
	if err != nil {
		return err
	}
	domain, err := m.resolveDomain()
	if err != nil {
		return err
	}
	// Before the plist write, as in Install: a rewrite that failed on the log
	// directory would otherwise replace a working plist with one launchd cannot
	// spawn, and reload it at the next login.
	if err := m.ensureLogDir(); err != nil {
		return err
	}
	if err := m.fs.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating launchd directory: %w", err)
	}

	// Read before overwriting, exactly as Install does and for the same reason:
	// WriteConfig is the install path whenever ConfigPatchers is set, as well as
	// the update-config write path. A first patcher-based install whose
	// bootstrap fails writes no state file, so a plist left behind under
	// ~/Library/LaunchAgents would be auto-loaded at the next login with nothing
	// recording it, and uninstall would report the service as not installed.
	previousPlist, hadPlist, err := m.readPlistFile(path)
	if err != nil {
		return err
	}

	if err := m.fs.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing plist file: %w", err)
	}

	// Bootout first if already loaded; ignore errors (may not be loaded yet).
	// Stale per-user domains are booted out too — see Install: booting out only
	// the new target leaves a GUI-session job loaded against the same binary
	// with the state file now naming a domain it was never bootstrapped into.
	m.bootoutStaleDomains(domain)
	_, _ = m.executor.Run("launchctl", "bootout", domain+"/"+m.label)
	if out, err := m.executor.Run("launchctl", "bootstrap", domain, path); err != nil {
		if !isRollback {
			restored := m.rollbackPlist(path, previousPlist, hadPlist)
			m.reloadRolledBackPlist(m.rollbackDomain(domain), path, restored)
		}
		return runErr("bootstrapping service", out, err)
	}
	return nil
}

// rollbackDomain names the domain a restored plist has to go back into. That is
// the domain the previous install was bootstrapped into and which the untouched
// state file still records — not the one this session just resolved. Reloading
// into the newly resolved domain after a failed cross-session re-install would
// leave the job in user/<uid> while state says gui/<uid>, so every later
// start/stop/status would address an empty domain.
func (m *launchdManager) rollbackDomain(resolved string) string {
	if m.previousDomain != "" {
		return m.previousDomain
	}
	return resolved
}

// readPlistFile returns the plist currently on disk, so an install that
// overwrites one can put it back if the bootstrap then fails.
//
// Only a missing file counts as absence. Any other read error is returned
// rather than reported as "there was nothing here": rolling back on that
// assumption would delete the manifest of a service the state file still
// describes.
func (m *launchdManager) readPlistFile(path string) (content []byte, ok bool, err error) {
	data, err := m.fs.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading plist file: %w", err)
	}
	return data, true, nil
}

// reloadRolledBackPlist puts a re-install's previous job back under supervision
// after rollbackPlist has restored the plist it overwrote. The bootout above
// already unloaded it, so without this a transient bootstrap failure — the
// documented `Bootstrap failed: 5: Input/output error` race — leaves a working
// service unloaded with a state file still claiming it is installed, and no
// verb able to bring it back: start kickstarts a job that is no longer there.
//
// Best effort: the bootstrap that just failed may well fail again, and the
// install already reports its own error.
func (m *launchdManager) reloadRolledBackPlist(domain, path string, hadPlist bool) {
	if !hadPlist {
		return
	}
	if out, err := m.executor.Run("launchctl", "bootstrap", domain, path); err != nil {
		logDetectionWarn("bootstrap failed and the previous plist could not be reloaded: "+
			runErr("reloading the previous plist", out, err).Error(),
			"manager", "launchd", "label", m.label, "domain", domain, "path", path)
	}
}

// rollbackPlist undoes the plist write of a failed bootstrap. A plist this
// install created has to come back out for the same reason it is written only
// after the log directory: a failed install writes no state file, and launchd
// auto-loads every plist under ~/Library/LaunchAgents at the next login, so one
// left behind returns on its own with nothing recording it.
//
// A re-install is the other way round — the state file still describes the
// previous service, and the bootout above already unloaded it — so what was
// overwritten is restored rather than deleted, exactly as the crontab manifest
// rollback does. Deleting it there would destroy a working install over a
// transient bootstrap failure.
//
// WriteConfig calls this too: it is the install path whenever ConfigPatchers is
// set, so its first-install case has no state file either.
// It reports whether the previous plist is now the one on disk. A restore that
// fails leaves the rejected plist in place, so the caller must not reload it:
// bootstrapping it would load the very configuration that was just refused.
func (m *launchdManager) rollbackPlist(path string, previous []byte, had bool) (restored bool) {
	if had {
		if err := m.fs.WriteFile(path, previous, 0o644); err != nil {
			logDetectionWarn("bootstrap failed and the previous plist could not be restored: "+err.Error(),
				"manager", "launchd", "label", m.label, "path", path)
			return false
		}
		return true
	}
	if err := m.fs.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logDetectionWarn("bootstrap failed and the plist could not be removed: "+err.Error(),
			"manager", "launchd", "label", m.label, "path", path)
	}
	return false
}

// serviceName recovers the service name from the label, for the paths that only
// ever received the label (WriteConfig takes rendered content, not a config).
func (m *launchdManager) serviceName() string {
	return strings.TrimPrefix(m.label, "com.")
}

// rememberLogOwner records the account the plist drops privileges to, so
// ensureLogDir can give it the directory it has to write into. buildPlistData
// emits UserName/GroupName for system scope only, and this must match it.
func (m *launchdManager) rememberLogOwner(config ServiceConfig) {
	if config.Scope == ScopeUser {
		m.logUser, m.logGroup = "", ""
		return
	}
	m.logUser, m.logGroup = config.User, config.Group
}

// ensureLogDir creates the parent of StandardOutPath. launchd does not create it
// and refuses to spawn a job whose log parent is missing, so every path that
// writes a plist — Install and WriteConfig alike — has to call this.
//
// A system-scope job carrying UserName also needs the directory to belong to
// that account: launchd opens StandardOutPath *after* dropping privileges, and
// the directory is created here by root. This did not matter while the target
// was world-writable /tmp; under /var/log/<name> an unowned directory means the
// job never spawns.
//
// The chown is deliberately not unconditional. MkdirAll succeeds silently on an
// existing directory, so chowning whatever LogDir names would let an install
// configured with a shared path — /var/log itself, say — hand a root-owned
// system directory to an unprivileged service account. Only a directory this
// call created, or the per-service default that belongs to the service by
// construction, is given away.
func (m *launchdManager) ensureLogDir() error {
	logDir := m.logDir
	if logDir == "" {
		resolved, err := resolveLaunchdLogDir(
			ServiceConfig{Name: m.serviceName(), Scope: m.scope}, m.homeFunc)
		if err != nil {
			return err
		}
		logDir = resolved
	}
	existed, err := m.fs.DirExists(logDir)
	if err != nil {
		return fmt.Errorf("inspecting the log directory: %w", err)
	}
	if err := m.fs.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}
	if m.logUser == "" && m.logGroup == "" {
		return nil
	}
	if existed && filepath.Clean(logDir) != filepath.Join(launchdSystemLogRoot, m.serviceName()) {
		return nil
	}
	uid, gid, err := lookupOwner(m.logUser, m.logGroup)
	if err != nil {
		return fmt.Errorf("resolving the owner of the log directory: %w", err)
	}
	if err := m.fs.Chown(logDir, uid, gid); err != nil {
		return fmt.Errorf("setting the owner of the log directory: %w", err)
	}
	return nil
}

// lookupOwner resolves a user and group name to ids, returning -1 for whichever
// is unset — the value os.Chown reads as "leave unchanged".
func lookupOwner(userName, groupName string) (uid int, gid int, err error) {
	uid, gid = -1, -1
	if userName != "" {
		u, lookupErr := user.Lookup(userName)
		if lookupErr != nil {
			return 0, 0, fmt.Errorf("looking up user %q: %w", userName, lookupErr)
		}
		if uid, err = strconv.Atoi(u.Uid); err != nil {
			return 0, 0, fmt.Errorf("parsing uid of user %q: %w", userName, err)
		}
	}
	if groupName != "" {
		g, lookupErr := user.LookupGroup(groupName)
		if lookupErr != nil {
			return 0, 0, fmt.Errorf("looking up group %q: %w", groupName, lookupErr)
		}
		if gid, err = strconv.Atoi(g.Gid); err != nil {
			return 0, 0, fmt.Errorf("parsing gid of group %q: %w", groupName, err)
		}
	}
	return uid, gid, nil
}

// plistPath returns the plist path, or an empty string when the user-scope path
// cannot be resolved. Callers that can report an error use plistPathErr.
func (m *launchdManager) plistPath() string {
	path, err := m.plistPathErr()
	if err != nil {
		return ""
	}
	return path
}

func (m *launchdManager) plistPathErr() (string, error) {
	dir, err := m.plistDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, m.label+".plist"), nil
}

// plistDir resolves the directory holding the plist. Per-user agents live under
// $HOME alone: launchd loads LaunchAgents from the user's home directory, and
// XDG_* has no meaning to it.
func (m *launchdManager) plistDir() (string, error) {
	if m.scope != ScopeUser {
		return launchDaemonsDir, nil
	}

	home := m.homeFunc
	if home == nil {
		home = os.UserHomeDir
	}
	dir, err := home()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoHomeDir, err)
	}
	if dir == "" {
		return "", ErrNoHomeDir
	}
	return filepath.Join(dir, "Library", "LaunchAgents"), nil
}

// resolveDomain returns the domain target every launchctl verb is addressed to,
// resolving it once and caching it on the manager.
//
// A pinned domain — restored from the state file written at install — is used
// verbatim. Re-resolving on a host whose session state has changed since would
// address a different domain than the one the agent was bootstrapped into,
// leaving it loaded and orphaned.
//
// `gui/<uid>` is tried first because a LaunchAgent bootstrapped there has access
// to the graphical session, but on a headless Mac or a CI runner there is no
// such domain and `launchctl` refuses outright; `user/<uid>` exists whenever the
// user has any session at all.
func (m *launchdManager) resolveDomain() (string, error) {
	if m.scope != ScopeUser {
		return launchdSystemDomain, nil
	}
	if m.domain != "" {
		return m.domain, nil
	}

	uid := strconv.Itoa(m.euid())

	var lastOut []byte
	var lastErr error
	for _, candidate := range []string{"gui/" + uid, "user/" + uid} {
		out, err := m.executor.Run("launchctl", "print", candidate)
		if err == nil {
			m.domain = candidate
			return candidate, nil
		}
		lastOut, lastErr = out, err
	}
	return "", runErr(fmt.Sprintf("resolving launchd domain: neither gui/%s nor user/%s is reachable from this session", uid, uid),
		lastOut, lastErr)
}

func (m *launchdManager) euid() int {
	if m.geteuidFunc == nil {
		return os.Geteuid()
	}
	return m.geteuidFunc()
}

// bootoutStaleDomains unloads this job from every domain other than the one it
// is about to be bootstrapped into, so a re-install cannot orphan the instance a
// previous one left behind.
//
// The domain the previous install recorded is the obvious candidate, but it is
// not a reliable one: install reads it through readableState, which downgrades an
// unreadable or corrupt state file to "no previous install", and an install that
// failed before the state file was written never recorded a domain at all. In
// user scope both per-user domains are therefore booted out unconditionally —
// bootout of a job that is not loaded, or of a domain this session cannot reach,
// is a no-op whose error is ignored either way.
func (m *launchdManager) bootoutStaleDomains(domain string) {
	candidates := []string{m.previousDomain}
	if m.scope == ScopeUser {
		uid := strconv.Itoa(m.euid())
		candidates = append(candidates, "gui/"+uid, "user/"+uid)
	}

	seen := map[string]bool{domain: true}
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		_, _ = m.executor.Run("launchctl", "bootout", candidate+"/"+m.label)
	}
}

// serviceTarget builds the <domain>/<label> target launchctl verbs act on.
func (m *launchdManager) serviceTarget() (string, error) {
	domain, err := m.resolveDomain()
	if err != nil {
		return "", err
	}
	return domain + "/" + m.label, nil
}
