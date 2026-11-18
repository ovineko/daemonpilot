package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// shellSafeRe matches the strings a POSIX shell reads identically quoted or
// bare.
var shellSafeRe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuoteIfNeeded quotes only what the shell would otherwise reinterpret.
// The redirect target goes through this rather than shellQuote so a system-scope
// entry stays byte-identical to the one earlier releases wrote, while a log
// directory carrying spaces or quotes is still safe.
func shellQuoteIfNeeded(s string) string {
	if shellSafeRe.MatchString(s) {
		return s
	}
	return shellQuote(s)
}

var validEnvKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func isValidEnvKey(key string) bool {
	return validEnvKeyRe.MatchString(key)
}

// ValidateEnvKeys exposes the environment key check to pkg/daemonpilot, so the
// crontab install path can reject an unusable key before the binary is copied
// rather than from Install, which runs after the copy and orphans it.
func ValidateEnvKeys(env map[string]string) error {
	return validateEnvKeys(env)
}

func validateEnvKeys(env map[string]string) error {
	for k := range env {
		if !isValidEnvKey(k) {
			return fmt.Errorf("invalid environment variable key: %q", k)
		}
	}
	return nil
}

func shellUnquote(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = s[1 : len(s)-1]
	}
	return strings.ReplaceAll(s, "'\\''", "'")
}

func skipShellQuoted(s string) int {
	if len(s) == 0 || s[0] != '\'' {
		return -1
	}
	i := 1
	for i < len(s) {
		if s[i] == '\'' {
			if i+3 < len(s) && s[i+1] == '\\' && s[i+2] == '\'' && s[i+3] == '\'' {
				i += 4
				continue
			}
			return i + 1
		}
		i++
	}
	return -1
}

const crontabEntryPrefix = "# daemonpilot:"

// ErrCrontabEntryNotFound reports that the crontab holds no entry for the
// service, so an uninstall had nothing to remove. It is a distinct error rather
// than a silent success so the caller can say "not installed" instead of
// claiming it removed something.
var ErrCrontabEntryNotFound = errors.New("no crontab entry found for service")

// crontabSystemLogRoot is where a system-scope entry redirects its output,
// preserved from the releases that hardcoded it.
const crontabSystemLogRoot = "/var/log"

type crontabManager struct {
	serviceName string
	installDir  string
	scope       Scope
	// logDir is the resolved redirect parent, remembered from Install or
	// GenerateConfig so WriteConfig can create it too.
	logDir       string
	executor     commandExecutor
	lookPathFunc func(string) (string, error)
	homeFunc     func() (string, error)
	mkdirAllFunc func(string, os.FileMode) error
}

func newCrontabManager() *crontabManager {
	return &crontabManager{
		executor:     &execCommandExecutor{},
		lookPathFunc: exec.LookPath,
		homeFunc:     os.UserHomeDir,
		mkdirAllFunc: os.MkdirAll,
	}
}

// resolveCrontabLogDir returns the directory the entry appends its output to. An
// explicit ServiceConfig.LogDir always wins; otherwise system scope keeps
// /var/log and user scope derives ~/.local/state/<name> — ~/Library/Logs/<name>
// on macOS — from $HOME alone. pkg/daemonpilot resolves LogDir for every user-scope
// install, so the user-scope branch here is only a fallback for a directly
// constructed manager; it follows the same per-OS convention so the two cannot
// disagree about the same scope.
func resolveCrontabLogDir(config ServiceConfig, homeFunc func() (string, error)) (string, error) {
	if config.LogDir != "" {
		return config.LogDir, nil
	}
	if config.Scope != ScopeUser {
		return crontabSystemLogRoot, nil
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
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Logs", config.Name), nil
	}
	return filepath.Join(home, ".local", "state", config.Name), nil
}

// validateCrontabLogDir rejects the characters a crontab command field cannot
// carry. cron splits the file on newlines before any shell sees it, so a
// newline in the redirect target would append a second, attacker-chosen crontab
// line that shell quoting cannot contain — and would desynchronise the
// two-line removal ParseCrontabEntries performs on uninstall. cron also
// rewrites an unescaped `%` into a newline and feeds the remainder to the job's
// stdin, silently truncating the command.
func validateCrontabLogDir(dir string) error {
	if strings.ContainsAny(dir, "\n\r") {
		return fmt.Errorf("crontab log directory must not contain newline characters: %q", dir)
	}
	if strings.Contains(dir, "%") {
		return fmt.Errorf("crontab log directory must not contain %%, which cron reads as a newline: %q", dir)
	}
	return nil
}

// validateCrontabCommandFields applies the same rejection to every value that
// reaches the command field of the entry. Shell quoting is applied *after* cron
// has already split the file on newlines and rewritten `%`, so a newline in an
// argument, an environment value, the executable path or the working directory
// appends a second, caller-chosen crontab line that uninstall then cannot find
// — ParseCrontabEntries removes the marker plus exactly one line — and a `%`
// truncates the command at that point.
func validateCrontabCommandFields(config ServiceConfig) error {
	type field struct {
		name string
		// value is omitted from the error when it may carry a secret.
		value  string
		redact bool
	}

	fields := make([]field, 0, 2+len(config.Arguments)+len(config.Environment))
	fields = append(fields,
		field{name: "executable path", value: config.ExecPath},
		field{name: "working directory", value: config.WorkingDir})
	for _, arg := range config.Arguments {
		fields = append(fields, field{name: "argument", value: arg})
	}
	// Environment values are where tokens and passwords live, and a
	// percent-encoded secret (pass%40word) trips this check, so the field is
	// named but the value is not printed: install output ends up in CI logs and
	// provisioning transcripts.
	for _, k := range sortedKeys(config.Environment) {
		fields = append(fields, field{name: "environment value for " + k, value: config.Environment[k], redact: true})
	}

	for _, f := range fields {
		bad := strings.ContainsAny(f.value, "\n\r")
		reason := "must not contain newline characters"
		if !bad && strings.Contains(f.value, "%") {
			bad = true
			reason = "must not contain %, which cron reads as a newline"
		}
		if !bad {
			continue
		}
		if f.redact {
			return fmt.Errorf("crontab %s %s", f.name, reason)
		}
		return fmt.Errorf("crontab %s %s: %q", f.name, reason, f.value)
	}
	return nil
}

// validateCrontabEntryContent re-checks the rendered entry on the write path.
// validateCrontabCommandFields guards the *inputs* to FormatCrontabEntry, but
// WriteConfig receives an already-rendered string that ConfigPatchers may have
// rewritten, so the same hazard returns through a route that never sees a
// ServiceConfig. The invariant uninstall depends on is exactly the shape
// FormatCrontabEntry produces: the marker comment plus exactly one entry line,
// because ParseCrontabEntries removes the marker plus exactly one line. A
// patcher that appends a third line leaves it behind as an unmarked, live cron
// entry that nothing can ever find again.
func validateCrontabEntryContent(content, serviceName string) error {
	if strings.Contains(content, "\r") {
		return errors.New("crontab entry must not contain carriage returns")
	}
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) != 2 {
		return fmt.Errorf("crontab entry must be the marker comment plus exactly one line, got %d line(s)", len(lines))
	}
	marker := crontabEntryPrefix + " " + serviceName
	if lines[0] != marker {
		return fmt.Errorf("crontab entry must start with %q, got %q", marker, lines[0])
	}
	// The marker is a comment, so cron never expands `%` there; the command line
	// is where it becomes a newline.
	if strings.Contains(lines[1], "%") {
		return errors.New("crontab entry must not contain %, which cron reads as a newline")
	}
	return nil
}

// crontabLogPath is the file the entry redirects into, once LogDir has been
// resolved by the caller.
func crontabLogPath(config ServiceConfig) string {
	dir := config.LogDir
	if dir == "" {
		dir = crontabSystemLogRoot
	}
	return filepath.Join(dir, config.Name+".log")
}

func (m *crontabManager) Install(config ServiceConfig) error {
	if err := validateServiceText(config, config.ExecPath, "crontab"); err != nil {
		return err
	}
	if err := validateEnvKeys(config.Environment); err != nil {
		return fmt.Errorf("crontab: %w", err)
	}
	if err := validateCrontabCommandFields(config); err != nil {
		return err
	}
	if config.CrontabSchedule != "" {
		if err := ValidateCrontabSchedule(config.CrontabSchedule); err != nil {
			return fmt.Errorf("crontab: %w", err)
		}
	}
	m.serviceName = config.Name
	m.installDir = config.InstallDir
	m.scope = config.Scope

	logDir, err := resolveCrontabLogDir(config, m.homeFunc)
	if err != nil {
		return err
	}
	if err := validateCrontabLogDir(logDir); err != nil {
		return fmt.Errorf("crontab: %w", err)
	}
	config.LogDir = logDir
	m.logDir = logDir

	existing, err := m.readCrontab()
	if err != nil {
		return err
	}

	cleaned, _ := ParseCrontabEntries(existing, config.Name)
	cleaned = strings.TrimRight(cleaned, "\n")

	entry := FormatCrontabEntry(config)
	var newContent string
	if cleaned == "" {
		newContent = entry + "\n"
	} else {
		newContent = cleaned + "\n" + entry + "\n"
	}

	if err := m.ensureLogDir(); err != nil {
		return err
	}

	// The manifest is written before the crontab is replaced: a manifest write
	// that fails afterwards would leave an installed entry with nothing to roll
	// back from. Rolling back therefore has to restore whatever a re-install
	// just overwrote, not simply delete the file — the previous entry is still
	// in the crontab and still needs a manifest describing it.
	previousManifest, hadManifest, err := m.readManifestFile()
	if err != nil {
		return err
	}
	if err := m.writeManifestFile(entry); err != nil {
		return err
	}

	if err := m.writeCrontab(newContent); err != nil {
		if hadManifest {
			_ = m.writeManifestFile(previousManifest)
		} else {
			m.removeManifestFile()
		}
		return err
	}

	return nil
}

// CrontabRebootScheduleCaveat returns the warning text for the @reboot default,
// or an empty string when schedule is time-based and the caveat does not apply.
// pkg/daemonpilot emits it through the caller's own Logger, which is where an
// operator-facing message belongs; Install deliberately does not repeat it on
// the detection logger, since both channels reach the same terminal.
//
// BusyBox crond honors @reboot only when built with FEATURE_CROND_SPECIAL_TIMES,
// and containers and WSL frequently run no cron daemon at all — on those hosts
// the service simply never starts.
func CrontabRebootScheduleCaveat(schedule string) string {
	if schedule != "" && strings.TrimSpace(schedule) != "@reboot" {
		return ""
	}
	return "crontab @reboot is not universally supported: BusyBox crond honors it only when built with " +
		"FEATURE_CROND_SPECIAL_TIMES, and most containers and WSL run no cron daemon at all; " +
		"set Config.CrontabSchedule to a time-based schedule if this host is one of them"
}

var (
	validCrontabKeyword = regexp.MustCompile(`^@(reboot|yearly|annually|monthly|weekly|daily|midnight|hourly)$`)
	validCrontabFields  = regexp.MustCompile(`^([0-9*/,\-]+\s+){4}[0-9*/,\-]+$`)
)

// ValidateCrontabSchedule reports whether schedule is a crontab expression the
// manager can install, accepting both @-shorthands and five-field schedules.
func ValidateCrontabSchedule(schedule string) error {
	if strings.ContainsAny(schedule, "\n\r") {
		return errors.New("crontab schedule must not contain newline characters")
	}
	s := strings.TrimSpace(schedule)
	if s == "" {
		return errors.New("crontab schedule must not be empty")
	}
	if validCrontabKeyword.MatchString(s) {
		return nil
	}
	if validCrontabFields.MatchString(s) {
		return nil
	}
	return fmt.Errorf("invalid crontab schedule: %q", schedule)
}

// FormatCrontabEntry renders the crontab line that starts the service, wrapped
// in the marker comments used to find it again on uninstall.
func FormatCrontabEntry(config ServiceConfig) string {
	schedule := "@reboot"
	if config.CrontabSchedule != "" {
		schedule = config.CrontabSchedule
	}

	runCmd := shellQuote(config.ExecPath)
	var runCmdSb124 strings.Builder
	for _, arg := range config.Arguments {
		runCmdSb124.WriteString(" ")
		runCmdSb124.WriteString(shellQuote(arg))
	}
	runCmd += runCmdSb124.String()

	var envPrefix string
	if len(config.Environment) > 0 {
		keys := sortedKeys(config.Environment)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+shellQuote(config.Environment[k]))
		}
		envPrefix = "export " + strings.Join(parts, " ") + "; "
	}

	return fmt.Sprintf("%s %s\n%s %scd %s && %s >> %s 2>&1",
		crontabEntryPrefix, config.Name, schedule, envPrefix, shellQuote(config.WorkingDir), runCmd,
		shellQuoteIfNeeded(crontabLogPath(config)))
}

// ParseCrontabEntries strips the entries belonging to serviceName from content,
// returning the remaining crontab and whether an entry was found.
func ParseCrontabEntries(content string, serviceName string) (kept string, found bool) {
	marker := crontabEntryPrefix + " " + serviceName
	lines := strings.Split(content, "\n")
	var result []string
	skipNext := false
	for _, line := range lines {
		if skipNext {
			skipNext = false
			found = true
			continue
		}
		if strings.TrimSpace(line) == marker {
			skipNext = true
			found = true
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n"), found
}

func (m *crontabManager) Uninstall() error {
	if m.serviceName == "" {
		return errors.New("service name not set; call Install first or set service name")
	}

	existing, err := m.readCrontab()
	if err != nil {
		return err
	}

	cleaned, found := ParseCrontabEntries(existing, m.serviceName)
	if !found {
		m.removeManifestFile()
		return fmt.Errorf("%w: %s", ErrCrontabEntryNotFound, m.serviceName)
	}

	if err := m.writeCrontab(cleaned); err != nil {
		return err
	}

	m.removeManifestFile()
	return nil
}

func (m *crontabManager) Enable() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}

	existing, err := m.readCrontab()
	if err != nil {
		return err
	}

	marker := crontabEntryPrefix + " " + m.serviceName
	lines := strings.Split(existing, "\n")
	var changed bool
	for i, line := range lines {
		if strings.TrimSpace(line) == marker {
			if i+1 < len(lines) && strings.HasPrefix(lines[i+1], "#") {
				lines[i+1] = lines[i+1][1:]
				changed = true
			}
			break
		}
	}

	if !changed {
		return nil
	}

	return m.writeCrontab(strings.Join(lines, "\n"))
}

func (m *crontabManager) Disable() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}

	existing, err := m.readCrontab()
	if err != nil {
		return err
	}

	marker := crontabEntryPrefix + " " + m.serviceName
	lines := strings.Split(existing, "\n")
	var changed bool
	for i, line := range lines {
		if strings.TrimSpace(line) == marker {
			if i+1 < len(lines) && !strings.HasPrefix(lines[i+1], "#") {
				lines[i+1] = "#" + lines[i+1]
				changed = true
			}
			break
		}
	}

	if !changed {
		return nil
	}

	return m.writeCrontab(strings.Join(lines, "\n"))
}

func (m *crontabManager) GetManifestPath() string {
	return m.manifestFilePath()
}

func (m *crontabManager) Start() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}

	status, err := m.Status()
	if err == nil && status == ServiceStatusRunning {
		return nil
	}

	existing, err := m.readCrontab()
	if err != nil {
		return err
	}

	marker := crontabEntryPrefix + " " + m.serviceName
	lines := strings.Split(existing, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == marker {
			if i+1 < len(lines) {
				entryLine := strings.TrimPrefix(lines[i+1], "#")
				cmdPart, err := extractCrontabCommand(entryLine)
				if err != nil {
					return fmt.Errorf("malformed crontab entry for %s: %w", m.serviceName, err)
				}
				// Redirect both shells so OpenBSD background jobs cannot retain CombinedOutput pipes.
				out, err := m.executor.Run("sh", "-c", "exec </dev/null >/dev/null 2>&1; ("+cmdPart+") </dev/null >/dev/null 2>&1 &")
				if err != nil {
					return runErr("starting service", out, err)
				}
				return nil
			}
			return fmt.Errorf("malformed crontab entry for %s", m.serviceName)
		}
	}

	return fmt.Errorf("service %s not found in crontab", m.serviceName)
}

func (m *crontabManager) Stop() error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}

	pattern, err := m.processPattern()
	if err != nil {
		return fmt.Errorf("stopping service: %w", err)
	}

	output, err := m.executor.Run("pgrep", "-f", pattern)
	if err != nil {
		return nil //nolint:nilerr // pgrep exits non-zero when nothing matches: nothing to stop
	}

	pids := strings.Fields(strings.TrimSpace(string(output)))
	if len(pids) == 0 {
		return nil
	}

	if out, err := m.executor.Run("pkill", "-f", pattern); err != nil {
		return runErr("stopping service", out, err)
	}
	return nil
}

func (m *crontabManager) ReadInstalledConfig() (string, error) {
	if m.serviceName == "" {
		return "", errors.New("service name not set")
	}
	existing, err := m.readCrontab()
	if err != nil {
		return "", err
	}
	marker := crontabEntryPrefix + " " + m.serviceName
	lines := strings.Split(existing, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == marker && i+1 < len(lines) {
			return marker + "\n" + lines[i+1], nil
		}
	}
	return "", fmt.Errorf("service %s not found in crontab", m.serviceName)
}

func (m *crontabManager) GenerateConfig(config ServiceConfig) (string, error) {
	if err := validateServiceText(config, config.ExecPath, "crontab"); err != nil {
		return "", err
	}
	if err := validateEnvKeys(config.Environment); err != nil {
		return "", fmt.Errorf("crontab: %w", err)
	}
	if err := validateCrontabCommandFields(config); err != nil {
		return "", err
	}
	if config.CrontabSchedule != "" {
		if err := ValidateCrontabSchedule(config.CrontabSchedule); err != nil {
			return "", fmt.Errorf("crontab: %w", err)
		}
	}
	m.serviceName = config.Name
	m.installDir = config.InstallDir
	m.scope = config.Scope
	logDir, err := resolveCrontabLogDir(config, m.homeFunc)
	if err != nil {
		return "", err
	}
	if err := validateCrontabLogDir(logDir); err != nil {
		return "", fmt.Errorf("crontab: %w", err)
	}
	config.LogDir = logDir
	m.logDir = logDir
	return FormatCrontabEntry(config), nil
}

// The rollback variant is reached by type assertion, so a signature drift here
// would silently fall back to WriteConfig rather than fail the build.
var _ ConfigRollbackWriter = (*crontabManager)(nil)

func (m *crontabManager) WriteConfig(content string) error {
	return m.writeConfig(content, false)
}

// WriteConfigForRollback writes the entry without restoring the manifest it
// overwrites when the crontab write fails — see ConfigRollbackWriter.
func (m *crontabManager) WriteConfigForRollback(content string) error {
	return m.writeConfig(content, true)
}

func extractCrontabCommand(entryLine string) (string, error) {
	if strings.ContainsAny(entryLine, "\r\n\x00") {
		return "", errors.New("malformed crontab entry: control character")
	}
	entryLine = strings.TrimLeft(entryLine, " \t")
	fields := 5
	if strings.HasPrefix(entryLine, "@") {
		fields = 1
	}
	pos := 0
	for range fields {
		for pos < len(entryLine) && (entryLine[pos] == ' ' || entryLine[pos] == '\t') {
			pos++
		}
		start := pos
		for pos < len(entryLine) && entryLine[pos] != ' ' && entryLine[pos] != '\t' {
			pos++
		}
		if start == pos {
			return "", errors.New("malformed crontab entry: missing schedule field")
		}
	}
	if err := ValidateCrontabSchedule(entryLine[:pos]); err != nil {
		return "", fmt.Errorf("malformed crontab entry: %w", err)
	}
	command := strings.TrimLeft(entryLine[pos:], " \t")
	if command == "" {
		return "", errors.New("malformed crontab entry: no command after schedule")
	}
	return command, nil
}

func (m *crontabManager) CanRestartWatchdog() bool {
	return false
}

func (m *crontabManager) SupportsEnable() bool { return true }

func (m *crontabManager) SupportsStart() bool { return true }

func (m *crontabManager) SupportsStatus() bool { return true }

// SupportsScope reports true for both scopes: the manager already edits the
// invoking user's crontab (`crontab -l` / `crontab <file>`, never `crontab -u`),
// so user scope only changes where the log redirect points.
func (m *crontabManager) SupportsScope(scope Scope) bool {
	return scope == ScopeSystem || scope == ScopeUser
}

func (m *crontabManager) Name() string {
	return "crontab"
}

func (m *crontabManager) Status() (ServiceStatus, error) {
	if m.serviceName == "" {
		return ServiceStatusUnknown, errors.New("service name not set")
	}

	pattern, err := m.processPattern()
	if err != nil {
		return ServiceStatusUnknown, fmt.Errorf("checking status: %w", err)
	}

	output, err := m.executor.Run("pgrep", "-f", pattern)
	if err != nil {
		return ServiceStatusStopped, nil //nolint:nilerr // pgrep exits non-zero when nothing matches, so the service is stopped
	}
	if strings.TrimSpace(string(output)) != "" {
		return ServiceStatusRunning, nil
	}
	return ServiceStatusStopped, nil
}

func (m *crontabManager) writeConfig(content string, isRollback bool) error {
	if m.serviceName == "" {
		return errors.New("service name not set")
	}

	// Ahead of ensureLogDir, so a rejected entry mutates nothing.
	if err := validateCrontabEntryContent(content, m.serviceName); err != nil {
		return err
	}

	if err := m.ensureLogDir(); err != nil {
		return err
	}

	existing, err := m.readCrontab()
	if err != nil {
		return err
	}

	cleaned, _ := ParseCrontabEntries(existing, m.serviceName)
	cleaned = strings.TrimRight(cleaned, "\n")

	var newContent string
	if cleaned == "" {
		newContent = content + "\n"
	} else {
		newContent = cleaned + "\n" + content + "\n"
	}

	// Same ordering as Install, and for the same reason: WriteConfig is the
	// install path whenever ConfigPatchers is set, as well as the update-config
	// write path. A manifest write that failed after the crontab was replaced
	// would leave a live entry with nothing describing it.
	previousManifest, hadManifest, err := m.readManifestFile()
	if err != nil {
		return err
	}
	if err := m.writeManifestFile(content); err != nil {
		return err
	}

	if err := m.writeCrontab(newContent); err != nil {
		if !isRollback {
			if hadManifest {
				_ = m.writeManifestFile(previousManifest)
			} else {
				m.removeManifestFile()
			}
		}
		return err
	}

	return nil
}

// ensureLogDir creates the parent of the redirect target. cron does not create
// it, and a `>>` into a missing directory fails the whole command, so every path
// that installs an entry — Install and WriteConfig alike — has to call this.
func (m *crontabManager) ensureLogDir() error {
	logDir := m.logDir
	if logDir == "" {
		resolved, err := resolveCrontabLogDir(
			ServiceConfig{Name: m.serviceName, Scope: m.scope}, m.homeFunc)
		if err != nil {
			return err
		}
		logDir = resolved
	}
	if err := m.mkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}
	return nil
}

func (m *crontabManager) mkdirAll(path string, perm os.FileMode) error {
	mkdirAll := m.mkdirAllFunc
	if mkdirAll == nil {
		mkdirAll = os.MkdirAll
	}
	return mkdirAll(path, perm)
}

func (m *crontabManager) detect() bool {
	getDetectionLog().Debug("init detection: crontab: checking crontab detection", "detector", "crontab")
	path, err := m.lookPathFunc("crontab")
	if err == nil {
		getDetectionLog().Debug("init detection: crontab: lookpath crontab -> "+path, "detector", "crontab", "path", path)
		// A warning, not a debug line: crontab is the last detector to run, so
		// winning here means the service will be supervised by whatever cron
		// daemon — if any — is running rather than by an init system.
		//
		// It does not say why no earlier detector won: under DetectForScope a
		// real init system may well have been recognised and then skipped for
		// not supporting the scope, and DetectForScope reports that itself.
		// Claiming "no init system was recognised" here would contradict it.
		logDetectionWarn("init detection: crontab: detected=true reason=crontab found in PATH (fallback detector: no other detector was selected)", "detector", "crontab", "detected", true)
		return true
	}
	getDetectionLog().Debug("init detection: crontab: lookpath crontab -> "+err.Error(), "detector", "crontab", "error", err)
	getDetectionLog().Debug("init detection: crontab: detected=false reason=crontab not found", "detector", "crontab", "detected", false)
	return false
}

func (m *crontabManager) readCrontab() (string, error) {
	output, err := m.executor.Run("crontab", "-l")
	if err != nil {
		errMsg := string(output)
		if strings.Contains(errMsg, "no crontab for") {
			return "", nil
		}
		return "", runErr("reading crontab", output, err)
	}
	return string(output), nil
}

func (m *crontabManager) writeCrontab(content string) error {
	f, err := os.CreateTemp("", "daemonpilot-crontab-")
	if err != nil {
		return fmt.Errorf("creating temp crontab file: %w", err)
	}
	tmpFile := f.Name()
	_, writeErr := f.WriteString(content)
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("writing temp crontab file: %w", writeErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("closing temp crontab file: %w", closeErr)
	}
	defer func() { _ = os.Remove(tmpFile) }()

	if out, err := m.executor.Run("crontab", tmpFile); err != nil {
		return runErr("installing crontab", out, err)
	}
	return nil
}

func (m *crontabManager) manifestFilePath() string {
	if m.installDir == "" {
		return fmt.Sprintf(".daemonpilot-crontab-%s.entry", m.serviceName)
	}
	return filepath.Join(m.installDir, fmt.Sprintf(".daemonpilot-crontab-%s.entry", m.serviceName))
}

// removeManifestFile deletes the manifest, if there is one to delete. Without an
// install dir manifestFilePath returns a bare relative name, so an unguarded
// Remove would delete a dot-file from whatever directory the CLI was invoked in
// — the same reason readManifestFile and writeManifestFile short-circuit.
func (m *crontabManager) removeManifestFile() {
	if m.installDir == "" {
		return
	}
	_ = os.Remove(m.manifestFilePath())
}

// readManifestFile returns the manifest currently on disk, so an install that
// overwrites one can put it back if the crontab write then fails.
//
// Only a missing file counts as "no manifest". Any other read error is
// returned rather than reported as absence: rolling back on that assumption
// would delete a manifest describing an entry that is still live in the
// crontab, which is precisely what writing the manifest first exists to avoid.
// The read happens before anything is mutated, so failing here is free.
func (m *crontabManager) readManifestFile() (content string, ok bool, err error) {
	if m.installDir == "" {
		return "", false, nil
	}

	data, err := os.ReadFile(m.manifestFilePath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading crontab manifest: %w", err)
	}
	return string(data), true, nil
}

func (m *crontabManager) writeManifestFile(content string) error {
	if m.installDir == "" {
		return nil
	}
	if _, err := os.Stat(m.installDir); err != nil {
		return nil //nolint:nilerr // without an install dir there is no manifest to write
	}
	if err := os.WriteFile(m.manifestFilePath(), []byte(content), 0o600); err != nil {
		return fmt.Errorf("writing crontab manifest: %w", err)
	}
	return nil
}

func (m *crontabManager) processPattern() (string, error) {
	existing, err := m.readCrontab()
	if err != nil {
		return "", fmt.Errorf("cannot determine process pattern: %w", err)
	}

	marker := crontabEntryPrefix + " " + m.serviceName
	lines := strings.Split(existing, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == marker && i+1 < len(lines) {
			entryLine := strings.TrimPrefix(lines[i+1], "#")
			_, after0, ok := strings.Cut(entryLine, " cd ")
			if !ok {
				continue
			}
			rest := after0
			end := skipShellQuoted(rest)
			if end == -1 || !strings.HasPrefix(rest[end:], " && ") {
				continue
			}
			rest = rest[end+4:]
			binEnd := skipShellQuoted(rest)
			if binEnd == -1 {
				spaceIdx := strings.Index(rest, " ")
				if spaceIdx == -1 {
					continue
				}
				binEnd = spaceIdx
			}
			binPath := shellUnquote(rest[:binEnd])
			after := rest[binEnd:]
			if strings.HasPrefix(after, " 'run'") || strings.HasPrefix(after, " run") {
				return regexp.QuoteMeta(binPath + " run"), nil
			}
		}
	}
	return "", fmt.Errorf("service %s not found in crontab", m.serviceName)
}
