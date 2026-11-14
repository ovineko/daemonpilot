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
	"strconv"
	"strings"
	"text/template"
)

var execCommand = exec.Command

const systemdUnitTemplate = `[Unit]
Description={{.Description}}
{{- if .NetworkOrdering}}
After=network-online.target
Wants=network-online.target
{{- end}}
# Never let systemd permanently give up on the agent: with the default rate limit
# (5 starts / 10s) a transient crash-loop — e.g. the watchdog being OOM-killed
# during a memory spike on a small node — trips "start-limit-hit" and the service
# stays DEAD until a reboot. StartLimitIntervalSec=0 disables that limiter so
# Restart=always keeps retrying (with RestartSec backoff) until the node recovers.
StartLimitIntervalSec=0

[Service]
Type={{.ServiceType}}
ExecStart={{.ExecStart}}
Restart=always
RestartSec=5s
WorkingDirectory={{.WorkingDir}}
{{- if .User}}
User={{.User}}
{{- end}}
{{- if .Group}}
Group={{.Group}}
{{- end}}
{{- range .EnvironmentLines}}
Environment={{.}}
{{- end}}
{{- range .ExtraDirectives}}
{{.}}
{{- end}}

[Install]
WantedBy={{.WantedBy}}
`

type commandExecutor interface {
	Run(name string, args ...string) ([]byte, error)
}

// execCommandExecutor runs init-system commands as child processes. extraEnv
// carries variables a per-user manager needs (XDG_RUNTIME_DIR and, when the
// caller has none, DBUS_SESSION_BUS_ADDRESS); it lives here rather than on the
// commandExecutor signature, which all 11 managers and every test fake
// implement.
type execCommandExecutor struct {
	extraEnv []string
}

func (e *execCommandExecutor) Run(name string, args ...string) ([]byte, error) {
	cmd := execCommand(name, args...)
	if len(e.extraEnv) > 0 {
		cmd.Env = append(os.Environ(), e.extraEnv...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("running %s: %w", name, err)
	}
	return out, nil
}

type fileSystem interface {
	WriteFile(path string, data []byte, perm os.FileMode) error
	// ReadFile returns an error satisfying errors.Is(err, fs.ErrNotExist) when
	// the file is absent, so a caller can tell absence from a real read failure.
	ReadFile(path string) ([]byte, error)
	Remove(path string) error
	RemoveAll(path string) error
	MkdirAll(path string, perm os.FileMode) error
	// DirExists reports whether path is an existing directory. Absence is
	// reported as false with a nil error; anything else is a real failure.
	DirExists(path string) (bool, error)
	Chown(path string, uid int, gid int) error
}

type osFileSystem struct{}

func (f *osFileSystem) WriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.WriteFile(path, data, perm); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func (f *osFileSystem) ReadFile(path string) ([]byte, error) {
	// #nosec G304 -- the path is a manifest path this package derives itself
	// from the service name, not caller-supplied input.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return data, nil
}

func (f *osFileSystem) Remove(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

func (f *osFileSystem) RemoveAll(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("removing %s recursively: %w", path, err)
	}
	return nil
}

func (f *osFileSystem) MkdirAll(path string, perm os.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return fmt.Errorf("creating directory %s: %w", path, err)
	}
	return nil
}

func (f *osFileSystem) DirExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspecting %s: %w", path, err)
	}
	return info.IsDir(), nil
}

// Chown does not follow symlinks. Its only caller hands a log directory to an
// unprivileged service account while running as root, so traversing a symlink
// an unprivileged account pre-created under a writable parent would transfer
// ownership of whatever it points at.
func (f *osFileSystem) Chown(path string, uid int, gid int) error {
	if err := os.Lchown(path, uid, gid); err != nil {
		return fmt.Errorf("changing owner of %s: %w", path, err)
	}
	return nil
}

// systemdSystemUnitDir is where machine-wide units live. User units are
// resolved from $HOME alone — never from XDG_CONFIG_HOME, which the per-user
// systemd manager does not inherit from the login shell.
const systemdSystemUnitDir = "/etc/systemd/system"

// systemdLingerDir holds one file per user for whom logind keeps a manager
// running outside a login session. Its presence is the only reliable, offline
// way to answer "will this user's services survive logout and come back at
// boot?".
const systemdLingerDir = "/var/lib/systemd/linger"

type systemdManager struct {
	serviceName  string
	scope        Scope
	executor     commandExecutor
	fs           fileSystem
	statFunc     func(string) (os.FileInfo, error)
	getenvFunc   func(string) string
	geteuidFunc  func() int
	homeFunc     func() (string, error)
	userNameFunc func() (string, error)
}

func newSystemdManager() *systemdManager {
	return &systemdManager{
		executor:     &execCommandExecutor{},
		fs:           &osFileSystem{},
		statFunc:     os.Stat,
		getenvFunc:   os.Getenv,
		geteuidFunc:  os.Geteuid,
		homeFunc:     os.UserHomeDir,
		userNameFunc: currentUserName,
	}
}

func (m *systemdManager) Install(config ServiceConfig) error {
	m.serviceName = config.Name
	m.scope = config.Scope

	content, err := GenerateUnitFile(config)
	if err != nil {
		return fmt.Errorf("generating unit file: %w", err)
	}

	unitPath, err := m.unitFilePathErr()
	if err != nil {
		return err
	}

	if err := m.fs.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return fmt.Errorf("creating unit directory: %w", err)
	}

	if err := m.fs.WriteFile(unitPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing unit file: %w", err)
	}

	if out, err := m.systemctl("daemon-reload"); err != nil {
		return runErr("daemon-reload", out, err)
	}

	return nil
}

func sanitizeSystemdValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s)
	var encoded strings.Builder
	for i := range len(s) {
		if s[i] < 32 || s[i] == 127 {
			fmt.Fprintf(&encoded, `\x%02x`, s[i])
		} else {
			encoded.WriteByte(s[i])
		}
	}
	s = encoded.String()
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, "$", "$$")
	return s
}

// sanitizeSystemdSetting escapes a plain (non-command-line) setting value.
// Only newline stripping and specifier escaping apply: systemd does not
// perform variable substitution or C-style unescaping outside ExecStart-style
// command lines, so escaping `$` or `\` here would corrupt legal values such
// as the trailing `$` of a machine account name. A trailing `\` is dropped:
// it would make systemd fold the following directive into this line, and
// outside command lines there is no escape sequence that neutralizes it.
// Trailing whitespace is dropped along with it, because systemd strips a
// line's trailing whitespace before testing for the continuation backslash —
// so `\` followed by a space folds just as a bare `\` does.
func sanitizeSystemdSetting(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "%", "%%")
	return strings.TrimRight(s, " \t\r\v\f\\")
}

// systemdCommandDirectives lists the [Service] directives systemd parses with
// command-line rules — variable expansion and C-style unescaping — rather than
// as plain settings. Values for these keys need sanitizeSystemdValue.
var systemdCommandDirectives = map[string]bool{
	"ExecCondition": true,
	"ExecReload":    true,
	"ExecStart":     true,
	"ExecStartPost": true,
	"ExecStartPre":  true,
	"ExecStop":      true,
	"ExecStopPost":  true,
}

// systemdDirectiveKey is the key a SystemdDirectives entry is actually emitted
// under. Classify on it, not on the raw one: systemd strips a key's surrounding
// whitespace, so `ExecStart ` is an ExecStart command line and its value needs
// command-line escaping — and `User ` is a User= directive.
func systemdDirectiveKey(key string) string {
	return strings.TrimLeft(sanitizeSystemdSetting(key), " \t\r\v\f")
}

// sanitizeSystemdDirective escapes a SystemdDirectives value according to how
// systemd parses the directive it belongs to. Command-line directives keep the
// `$` and `\` escaping that plain settings must not have; the trailing
// backslash the escaping can leave behind is then dropped, because the config
// lexer tests for line continuation before any unescaping and would otherwise
// fold the following directive into this line.
func sanitizeSystemdDirective(key, value string) string {
	if !systemdCommandDirectives[key] {
		return sanitizeSystemdSetting(value)
	}
	return strings.TrimRight(sanitizeSystemdValue(value), " \t\r\v\f\\")
}

type systemdTemplateData struct {
	Description      string
	ServiceType      string
	ExecStart        string
	WorkingDir       string
	User             string
	Group            string
	EnvironmentLines []string
	ExtraDirectives  []string
	// WantedBy is multi-user.target for system units; the user manager has no
	// such target and uses default.target.
	WantedBy string
	// NetworkOrdering emits the network-online.target ordering, which exists
	// only in the system manager.
	NetworkOrdering bool
}

// quoteSystemdArg escapes and, where needed, quotes a single ExecStart word.
// A value whose escaped form ends in `\` is always quoted: systemd folds a
// line ending in a backslash into the next directive, so an unquoted trailing
// backslash would swallow the line that follows.
func quoteSystemdArg(s string) string {
	s = sanitizeSystemdValue(s)
	if s == "" || strings.ContainsAny(s, " \t\"'") || strings.HasSuffix(s, `\`) {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

func buildSystemdTemplateData(config ServiceConfig) systemdTemplateData {
	execStart := quoteSystemdArg(config.ExecPath)
	var execStartSb134 strings.Builder
	for _, arg := range config.Arguments {
		execStartSb134.WriteString(" ")
		execStartSb134.WriteString(quoteSystemdArg(arg))
	}
	execStart += execStartSb134.String()

	var envLines []string
	if len(config.Environment) > 0 {
		keys := make([]string, 0, len(config.Environment))
		for k := range config.Environment {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := config.Environment[k]
			// %q handles newlines and quotes, but systemd expands specifiers in
			// Environment= values: an unescaped % either rewrites the value or
			// makes the unit fail to load.
			pair := strings.ReplaceAll(k+"="+v, "%", "%%")
			envLines = append(envLines, fmt.Sprintf("%q", pair))
		}
	}

	var directives []string
	if len(config.SystemdDirectives) > 0 {
		keys := make([]string, 0, len(config.SystemdDirectives))
		for k := range config.SystemdDirectives {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sk := systemdDirectiveKey(k)
			directives = append(directives, sk+"="+sanitizeSystemdDirective(sk, config.SystemdDirectives[k]))
		}
	}

	serviceType := "simple"
	if config.NotifyReady {
		serviceType = "notify"
	}

	wantedBy := "multi-user.target"
	networkOrdering := true
	if config.Scope == ScopeUser {
		wantedBy = "default.target"
		networkOrdering = false
	}

	return systemdTemplateData{
		Description:      sanitizeSystemdSetting(config.Description),
		ServiceType:      serviceType,
		ExecStart:        execStart,
		WorkingDir:       sanitizeSystemdSetting(config.WorkingDir),
		User:             sanitizeSystemdSetting(config.User),
		Group:            sanitizeSystemdSetting(config.Group),
		EnvironmentLines: envLines,
		ExtraDirectives:  directives,
		WantedBy:         wantedBy,
		NetworkOrdering:  networkOrdering,
	}
}

// UserGroupDirective returns the emitted key of the first raw directive that
// sets User= or Group=, or "" when there is none. Matching is case-sensitive
// because systemd's own directive lookup is: `user=svc` is an unknown key it
// ignores, not an identity switch, and rejecting it would be a false refusal.
//
// It is exported so configuration validation can refuse the directive before
// install copies the binary, rather than at unit generation afterwards.
func UserGroupDirective(directives map[string]string) string {
	keys := make([]string, 0, len(directives))
	for k := range directives {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch sk := systemdDirectiveKey(k); sk {
		case "User", "Group":
			return sk
		}
	}
	return ""
}

// GenerateUnitFile renders the systemd unit for the configured service,
// including any custom directives from SystemdDirectives.
func GenerateUnitFile(config ServiceConfig) (string, error) {
	if err := validateServiceText(config, config.ExecPath, "systemd"); err != nil {
		return "", err
	}
	if config.Scope == ScopeUser {
		if config.User != "" || config.Group != "" {
			return "", fmt.Errorf("%w: systemd refuses to load a user unit carrying User= or Group=", ErrUserGroupInUserScope)
		}
		// The typed fields are not the only way in: a raw directive is emitted
		// verbatim, so `SystemdDirectives: {"User": "svc"}` would produce the
		// same unloadable unit — after install has already written the binary.
		if key := UserGroupDirective(config.SystemdDirectives); key != "" {
			return "", fmt.Errorf("%w: SystemdDirectives sets %s=, which systemd refuses to load in a user unit",
				ErrUserGroupInUserScope, key)
		}
	}

	data := buildSystemdTemplateData(config)

	tmpl, err := template.New("unit").Parse(systemdUnitTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing unit template: %w", err)
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, data)
	if err != nil {
		return "", fmt.Errorf("executing unit template: %w", err)
	}
	return buf.String(), nil
}

func (m *systemdManager) Uninstall() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}

	if out, err := m.systemctl("disable", m.serviceName); err != nil {
		return runErr("disabling service", out, err)
	}

	unitPath, err := m.unitFilePathErr()
	if err != nil {
		return err
	}

	if err := m.fs.Remove(unitPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing unit file: %w", err)
	}

	if out, err := m.systemctl("daemon-reload"); err != nil {
		return runErr("daemon-reload", out, err)
	}

	return nil
}

func (m *systemdManager) Start() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if out, err := m.systemctl("start", m.serviceName); err != nil {
		return runErr("starting service", out, err)
	}
	return nil
}

func (m *systemdManager) Stop() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if out, err := m.systemctl("stop", m.serviceName); err != nil {
		return runErr("stopping service", out, err)
	}
	return nil
}

func (m *systemdManager) Enable() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if out, err := m.systemctl("enable", m.serviceName); err != nil {
		return runErr("enabling service", out, err)
	}
	return nil
}

func (m *systemdManager) Disable() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	if out, err := m.systemctl("disable", m.serviceName); err != nil {
		return runErr("disabling service", out, err)
	}
	return nil
}

func (m *systemdManager) GetManifestPath() string {
	return m.unitFilePath()
}

func (m *systemdManager) GenerateConfig(config ServiceConfig) (string, error) {
	m.serviceName = config.Name
	m.scope = config.Scope
	return GenerateUnitFile(config)
}

func (m *systemdManager) ReadInstalledConfig() (string, error) {
	unitPath, err := m.unitFilePathErr()
	if err != nil {
		return "", err
	}
	//nolint:gosec // path is derived from the service name and scope, not user input
	data, err := os.ReadFile(unitPath)
	if err != nil {
		return "", fmt.Errorf("reading unit file: %w", err)
	}
	return string(data), nil
}

func (m *systemdManager) WriteConfig(content string) error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}
	unitPath, err := m.unitFilePathErr()
	if err != nil {
		return err
	}
	if err := m.fs.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return fmt.Errorf("creating unit directory: %w", err)
	}
	if err := m.fs.WriteFile(unitPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing unit file: %w", err)
	}
	if out, err := m.systemctl("daemon-reload"); err != nil {
		return runErr("daemon-reload", out, err)
	}
	return nil
}

func (m *systemdManager) CanRestartWatchdog() bool {
	return true
}

func (m *systemdManager) SupportsEnable() bool { return true }

func (m *systemdManager) SupportsStart() bool  { return true }
func (m *systemdManager) SupportsStatus() bool { return true }

// SupportsScope reports true for both scopes: systemd is the one init system
// with a per-user manager wired up in this phase.
func (m *systemdManager) SupportsScope(scope Scope) bool {
	return scope == ScopeSystem || scope == ScopeUser
}

func (m *systemdManager) Name() string {
	return "systemd"
}

func (m *systemdManager) Status() (ServiceStatus, error) {
	if m.serviceName == "" {
		return ServiceStatusUnknown, errors.New("service name not set")
	}

	output, err := m.systemctl("is-active", m.serviceName)
	status := strings.TrimSpace(string(output))

	switch status {
	case "active":
		return ServiceStatusRunning, nil
	case "inactive":
		return ServiceStatusStopped, nil
	case "failed":
		return ServiceStatusFailed, nil
	default:
		if err != nil {
			return ServiceStatusUnknown, runErr("checking service status", output, err)
		}
		return ServiceStatusUnknown, nil
	}
}

// Both gates are reached by type assertion from pkg/daemonpilot, so a signature
// drift here would not fail the build — it would silently skip the lingering
// check and the registration check at install time.
var (
	_ LingerChecker        = (*systemdManager)(nil)
	_ RegistrationVerifier = (*systemdManager)(nil)
)

// LingeringEnabled reports whether the invoking user has lingering enabled,
// alongside the user name a remedy command has to name. Without lingering the
// per-user manager is torn down at logout and is not started at boot, so a
// user-scope service silently stops existing — the opposite of what installing
// a supervisor is meant to guarantee.
//
// The name comes from user.Current rather than $USER, which under `sudo -u` is
// often still root.
func (m *systemdManager) LingeringEnabled() (bool, string, error) {
	name, err := m.userName()
	if err != nil {
		return false, "", err
	}

	stat := m.statFunc
	if stat == nil {
		stat = os.Stat
	}
	_, err = stat(filepath.Join(systemdLingerDir, name))
	return err == nil, name, nil
}

// VerifyRegistered asks the per-user manager where it believes the unit lives
// and fails when that is not where the unit was written. It costs one
// systemctl call and catches the entire class of "the manager looks for the
// unit somewhere other than where we put it" at install time, rather than at
// the next boot when nothing comes back.
func (m *systemdManager) VerifyRegistered() error {
	if m.scope != ScopeUser {
		return nil
	}
	if m.serviceName == "" {
		return errors.New("service name not set")
	}

	want, err := m.unitFilePathErr()
	if err != nil {
		return err
	}

	out, err := m.systemctl("show", "-p", "FragmentPath", m.serviceName)
	if err != nil {
		return runErr("verifying unit registration", out, err)
	}

	// Take the FragmentPath line rather than the whole output: systemctl writes
	// warnings to the same combined stream, and folding one into the path turns a
	// correct install into a garbled mismatch error.
	got := fragmentPath(out)
	if got == "" {
		return fmt.Errorf("systemd does not know unit %s.service after writing %s; the user manager is not reading that directory",
			m.serviceName, want)
	}
	if resolvePath(got) != resolvePath(want) {
		return fmt.Errorf("systemd loads unit %s.service from %s, but it was written to %s", m.serviceName, got, want)
	}
	return nil
}

// resolvePath normalizes a path for comparison, following symlinks when it can.
// systemd reports FragmentPath from the home directory in the user record while
// the install writes below $HOME; on hosts where one of the two crosses a
// symlink (/home -> /var/home on rpm-ostree, bind-mounted homes) the two strings
// name the same file. A path that cannot be resolved — it may not exist yet —
// falls back to lexical cleaning.
func resolvePath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// fragmentPath extracts the unit path from `systemctl show -p FragmentPath`
// output, ignoring any other line the command wrote to the combined stream. A
// bare path with no property prefix is still accepted, but only when it is the
// whole output — otherwise a warning line would be read as the unit path.
func fragmentPath(out []byte) string {
	var lines []string
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "FragmentPath="); ok {
			return strings.TrimSpace(v)
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 1 && !strings.Contains(lines[0], "=") {
		return lines[0]
	}
	return ""
}

// systemctl runs a systemctl verb in the manager's scope, inserting --user and
// the environment the per-user manager needs when the scope calls for it.
//
// The scope can change over a manager's lifetime — Install and GenerateConfig
// both adopt the scope of the config they are given — so the executor's extra
// environment is set on every call and cleared for system scope, rather than
// left behind from an earlier user-scope call.
//
// needs both to explain why systemctl failed
//
//nolint:wrapcheck // callers pass the raw error and output to runErr, which
func (m *systemdManager) systemctl(args ...string) ([]byte, error) {
	if e, ok := m.executor.(*execCommandExecutor); ok {
		if m.scope == ScopeUser {
			e.extraEnv = m.userManagerEnv()
		} else {
			e.extraEnv = nil
		}
	}
	if m.scope != ScopeUser {
		return m.executor.Run("systemctl", args...)
	}
	return m.executor.Run("systemctl", append([]string{"--user"}, args...)...)
}

// userManagerEnv builds the environment `systemctl --user` needs to find the
// per-user manager's bus. XDG_RUNTIME_DIR is read here as a connection detail
// only, never to place artifacts.
func (m *systemdManager) userManagerEnv() []string {
	getenv := m.getenvFunc
	if getenv == nil {
		getenv = os.Getenv
	}
	geteuid := m.geteuidFunc
	if geteuid == nil {
		geteuid = os.Geteuid
	}

	runtimeDir := getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = filepath.Join("/run/user", strconv.Itoa(geteuid()))
	}

	env := []string{"XDG_RUNTIME_DIR=" + runtimeDir}
	if getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		env = append(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(runtimeDir, "bus"))
	}
	return env
}

func (m *systemdManager) userName() (string, error) {
	if m.userNameFunc != nil {
		return m.userNameFunc()
	}
	return currentUserName()
}

// userManagerReachable reports whether this session can talk to the per-user
// systemd manager, by probing its private socket. $XDG_RUNTIME_DIR is read only
// as a detection predicate here — never to place artifacts, since the user
// manager does not inherit the login shell's environment.
func (m *systemdManager) userManagerReachable() bool {
	stat := m.statFunc
	if stat == nil {
		stat = os.Stat
	}

	getenv := m.getenvFunc
	if getenv == nil {
		getenv = os.Getenv
	}
	if dir := getenv("XDG_RUNTIME_DIR"); dir != "" {
		path := filepath.Join(dir, "systemd", "private")
		if _, err := stat(path); err == nil {
			getDetectionLog().Debug("init detection: systemd: user manager reachable at "+path, "detector", "systemd", "path", path)
			return true
		}
	}

	geteuid := m.geteuidFunc
	if geteuid == nil {
		geteuid = os.Geteuid
	}
	path := filepath.Join("/run/user", strconv.Itoa(geteuid()), "systemd", "private")
	if _, err := stat(path); err == nil {
		getDetectionLog().Debug("init detection: systemd: user manager reachable at "+path, "detector", "systemd", "path", path)
		return true
	}

	getDetectionLog().Debug("init detection: systemd: user manager unreachable", "detector", "systemd")
	return false
}

// unitFilePath returns the unit path, or an empty string when the user-scope
// path cannot be resolved. Callers that can report an error use
// unitFilePathErr instead.
func (m *systemdManager) unitFilePath() string {
	path, err := m.unitFilePathErr()
	if err != nil {
		return ""
	}
	return path
}

func (m *systemdManager) unitFilePathErr() (string, error) {
	dir, err := m.unitDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, m.serviceName+".service"), nil
}

// unitDir resolves the directory holding the unit file. User units are placed
// under $HOME alone: the per-user systemd manager runs with its own environment
// and would not find a unit written relative to XDG_CONFIG_HOME.
func (m *systemdManager) unitDir() (string, error) {
	if m.scope != ScopeUser {
		return systemdSystemUnitDir, nil
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
	return filepath.Join(dir, ".config", "systemd", "user"), nil
}
