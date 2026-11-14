package daemonpilot

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ovineko/daemonpilot/internal/daemon"
)

// Validate reports whether the configuration is usable, checking the fields a
// service cannot start without.
func (c *Config) Validate() error {
	if c.Name == "" {
		return errors.New("daemonpilot: Config.Name is required")
	}
	if !isValidServiceName(c.Name) {
		return fmt.Errorf("daemonpilot: Config.Name must match [a-z0-9_-]+ (lowercase only), got %q", c.Name)
	}
	if c.WorkerFunc == nil {
		return errors.New("daemonpilot: Config.WorkerFunc is required")
	}
	if c.HealthInterval < 0 {
		return fmt.Errorf("daemonpilot: Config.HealthInterval must not be negative, got %v", c.HealthInterval)
	}
	if c.HealthFailures < 0 {
		return fmt.Errorf("daemonpilot: Config.HealthFailures must not be negative, got %d", c.HealthFailures)
	}
	if c.WatchdogLogBufferBytes < 0 {
		return fmt.Errorf("daemonpilot: Config.WatchdogLogBufferBytes must not be negative, got %d", c.WatchdogLogBufferBytes)
	}
	if err := c.validateCommandPrefix(); err != nil {
		return err
	}
	if err := c.validateScope(); err != nil {
		return err
	}
	if err := c.validateLogDir(); err != nil {
		return err
	}
	if c.MaxMemoryBytes < 0 {
		return fmt.Errorf("daemonpilot: Config.MaxMemoryBytes must not be negative, got %d", c.MaxMemoryBytes)
	}
	if c.WorkerCgroupMemoryMax < 0 {
		return fmt.Errorf("daemonpilot: Config.WorkerCgroupMemoryMax must not be negative, got %d", c.WorkerCgroupMemoryMax)
	}
	if (c.WorkerCgroupMemoryMax != 0 || c.WorkerCgroupParent != "") && runtime.GOOS != "linux" {
		return fmt.Errorf("daemonpilot: Config.WorkerCgroupMemoryMax/WorkerCgroupParent require Linux, running on %s", runtime.GOOS)
	}
	if c.OOMScoreAdj != nil {
		v := *c.OOMScoreAdj
		if v < -1000 || v > 1000 {
			return fmt.Errorf("daemonpilot: Config.OOMScoreAdj must be between -1000 and 1000, got %d", v)
		}
	}
	if c.Update != nil {
		if err := c.Update.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// SetDefaults fills in the optional fields left zero, so the rest of the
// library can rely on concrete values. It returns [ErrNoHomeDir] when
// [ScopeUser] is requested and the home directory cannot be resolved, because
// user-scope paths are derived from $HOME alone.
func (c *Config) SetDefaults() error {
	if c.Version == "" {
		c.Version = Version
	}
	if c.WatchdogVersion == "" {
		c.WatchdogVersion = WatchdogVersion
	}
	if c.Scope == "" {
		c.Scope = ScopeSystem
	}
	// User scope needs $HOME even when InstallDir is set explicitly: the unit
	// file itself is always placed under $HOME, so an unresolvable home yields an
	// empty manifest path rather than an error at install time.
	var home string
	if c.Scope == ScopeUser {
		h, err := userHomeDir()
		if err != nil || h == "" {
			return ErrNoHomeDir
		}
		home = h
	}
	if c.InstallDir == "" {
		c.InstallDir = defaultInstallDir(runtime.GOOS, c.Name, c.Scope, home)
	}
	if c.LogDir == "" {
		c.LogDir = defaultLogDir(runtime.GOOS, c.Name, c.Scope, home)
	}
	if c.GracefulTimeout == 0 {
		c.GracefulTimeout = 30 * time.Second
	}
	if c.StableThreshold == 0 {
		c.StableThreshold = 5 * time.Minute
	}
	if c.MaxRestartDelay == 0 {
		c.MaxRestartDelay = 60 * time.Second
	}
	if c.CollectWatchdogLogs && c.WatchdogLogBufferBytes == 0 {
		c.WatchdogLogBufferBytes = DefaultWatchdogLogBufferBytes
	}
	if c.HealthFunc != nil {
		if c.HealthInterval == 0 {
			c.HealthInterval = 30 * time.Second
		}
		if c.HealthFailures == 0 {
			c.HealthFailures = 3
		}
	}
	c.initLogger()
	if c.Update != nil {
		c.Update.setDefaults()
	}
	return nil
}

// Validate reports whether the self-update configuration is usable: an update
// source and a signing key are both required.
func (uc *UpdateConfig) Validate() error {
	if uc.RetryMaxAttempts < 0 {
		return fmt.Errorf("daemonpilot: UpdateConfig.RetryMaxAttempts must not be negative, got %d", uc.RetryMaxAttempts)
	}
	if uc.RetryBaseBackoff < 0 {
		return fmt.Errorf("daemonpilot: UpdateConfig.RetryBaseBackoff must not be negative, got %v", uc.RetryBaseBackoff)
	}
	if uc.RetryMaxBackoff < 0 {
		return fmt.Errorf("daemonpilot: UpdateConfig.RetryMaxBackoff must not be negative, got %v", uc.RetryMaxBackoff)
	}
	if uc.Source == nil {
		return errors.New("daemonpilot: UpdateConfig.Source is required")
	}
	if uc.CheckInterval < 0 {
		return fmt.Errorf("daemonpilot: UpdateConfig.CheckInterval must not be negative, got %v", uc.CheckInterval)
	}
	if uc.SelftestTimeout < 0 {
		return fmt.Errorf("daemonpilot: UpdateConfig.SelftestTimeout must not be negative, got %v", uc.SelftestTimeout)
	}
	if uc.RollbackTimeout < 0 {
		return fmt.Errorf("daemonpilot: UpdateConfig.RollbackTimeout must not be negative, got %v", uc.RollbackTimeout)
	}
	if uc.VerifyFunc != nil && uc.VerifyMessage != nil {
		return errors.New("daemonpilot: UpdateConfig.VerifyFunc and VerifyMessage are mutually exclusive; choose one verification point")
	}
	// A signing key is required only for the built-in Ed25519 verification.
	// Custom verifiers carry their keys through closures; their format and
	// strength are the application's responsibility.
	if uc.VerifyFunc == nil && uc.VerifyMessage == nil {
		if len(uc.PublicKey) == 0 {
			return errors.New("daemonpilot: UpdateConfig.PublicKey is required")
		}
		if len(uc.PublicKey) != ed25519.PublicKeySize {
			return fmt.Errorf("daemonpilot: UpdateConfig.PublicKey must be %d bytes (ed25519)", ed25519.PublicKeySize)
		}
	}
	return nil
}

func (c *Config) validateCommandPrefix() error {
	for i, word := range c.CommandPrefix {
		if strings.ContainsRune(word, 0) {
			return fmt.Errorf("daemonpilot: Config.CommandPrefix[%d] contains NUL", i)
		}
		if word == "" || strings.TrimSpace(word) != word {
			return fmt.Errorf("daemonpilot: Config.CommandPrefix[%d] must be a single word without surrounding space, got %q", i, word)
		}
		if strings.HasPrefix(word, "-") {
			return fmt.Errorf("daemonpilot: Config.CommandPrefix[%d] must be a command word, not a flag, got %q", i, word)
		}
	}
	return nil
}

func (c *Config) validateScope() error {
	if c.Scope != "" && c.Scope != ScopeSystem && c.Scope != ScopeUser {
		return fmt.Errorf("daemonpilot: Config.Scope must be %q or %q, got %q", ScopeSystem, ScopeUser, c.Scope)
	}
	if c.Scope == ScopeUser {
		if c.ServiceUser != "" {
			return errors.New("daemonpilot: Config.ServiceUser is not supported with Config.Scope=user: " +
				"setting the service user requires root, and a user-scope service already runs as the invoking user")
		}
		if c.ServiceGroup != "" {
			return errors.New("daemonpilot: Config.ServiceGroup is not supported with Config.Scope=user: " +
				"setting the service group requires root, and a user-scope service already runs as the invoking user")
		}
		// A raw directive is emitted verbatim, so it reaches the unit by a second
		// route the typed fields do not cover. Unit generation refuses it too, but
		// only after install has copied the binary — and the failed install leaves
		// no manifest and no state file, so uninstall then reports the service as
		// not installed and the copy is orphaned. Refuse here instead.
		if key := daemon.UserGroupDirective(c.SystemdDirectives); key != "" {
			return fmt.Errorf("daemonpilot: Config.SystemdDirectives sets %s=, which is not supported with "+
				"Config.Scope=user: systemd refuses to load a user unit carrying User= or Group=", key)
		}
	}
	return nil
}

// validateLogDir rejects a relative log directory. Unlike the crontab-specific
// rules below this one applies to every init system: a relative path would be
// created next to whatever directory the install was invoked from, while the
// manifest points a launchd job or a cron redirect at a path resolved somewhere
// else entirely.
func (c *Config) validateLogDir() error {
	if c.LogDir != "" && !filepath.IsAbs(c.LogDir) {
		return fmt.Errorf("daemonpilot: Config.LogDir must be an absolute path, got %q", c.LogDir)
	}
	return nil
}

// validateCrontabFields refuses the characters cron acts on in every value that
// reaches a crontab command field.
//
// None of them stays inside the field it is written into: the crontab manager
// renders each one into a command line cron splits on newlines before any shell
// quoting applies, and cron rewrites an unescaped % into a newline of its own —
// so either could append an entry uninstall would never find again, or silently
// truncate the command. The crontab manager refuses them as well
// (validateCrontabLogDir, validateCrontabCommandFields and validateEnvKeys), but
// only from Install, which runs after the binary has been copied: the failed
// install leaves no manifest and no state file, so uninstall then reports the
// service as not installed and the copy is orphaned. Refuse before the copy
// instead. InstallDir stands in for the ExecPath and WorkingDir the manager
// checks, since both derive from it and Name is already restricted to
// [a-z0-9_-].
//
// This is called from handleInstall once the manager is known to be crontab,
// not from Validate: % is a cron hazard alone, and Validate runs from New on
// every process start, so rejecting it unconditionally would stop an existing
// systemd or launchd service whose environment carries a percent-encoded value
// from booting at all.
func (c *Config) validateCrontabFields() error {
	if err := daemon.ValidateEnvKeys(c.ServiceEnvironment); err != nil {
		return fmt.Errorf("daemonpilot: Config.ServiceEnvironment: %w", err)
	}
	// The schedule is the one field of the entry that is not a command field,
	// but it needs the same hoist: the manager validates it from Install, after
	// the binary has been copied, so a rejected schedule orphans the copy the
	// same way a rejected LogDir would.
	if c.CrontabSchedule != "" {
		if err := daemon.ValidateCrontabSchedule(c.CrontabSchedule); err != nil {
			return fmt.Errorf("daemonpilot: Config.CrontabSchedule is invalid: %w", err)
		}
	}
	if err := checkCrontabSafe("Config.LogDir", c.LogDir); err != nil {
		return err
	}
	if err := checkCrontabSafe("Config.InstallDir", c.InstallDir); err != nil {
		return err
	}
	for i, arg := range c.ServiceArguments {
		if err := checkCrontabSafe(fmt.Sprintf("Config.ServiceArguments[%d]", i), arg); err != nil {
			return err
		}
	}
	for i, word := range c.CommandPrefix {
		if err := checkCrontabSafe(fmt.Sprintf("Config.CommandPrefix[%d]", i), word); err != nil {
			return err
		}
	}
	for _, k := range sortedEnvKeys(c.ServiceEnvironment) {
		if err := checkCrontabSafeRedacted("Config.ServiceEnvironment["+k+"]", c.ServiceEnvironment[k]); err != nil {
			return err
		}
	}
	return nil
}

// checkCrontabSafe rejects the characters cron acts on before any quoting the
// crontab manager applies: a newline installs a second live line, and % becomes
// one. field is the caller-facing name of the value being checked.
func checkCrontabSafe(field, value string) error {
	return crontabSafetyError(field, value, false)
}

// checkCrontabSafeRedacted is checkCrontabSafe for values that carry secrets.
// Environment values are where tokens and passwords live, and a percent-encoded
// secret (pass%40word) trips this check, so the value is named but not printed:
// install output ends up in CI logs and provisioning transcripts.
func checkCrontabSafeRedacted(field, value string) error {
	return crontabSafetyError(field, value, true)
}

func crontabSafetyError(field, value string, redact bool) error {
	i := strings.IndexAny(value, "\n\r%")
	if i < 0 {
		return nil
	}
	if redact {
		return fmt.Errorf("daemonpilot: %s must not contain %q, which cron reads as a newline",
			field, value[i])
	}
	return fmt.Errorf("daemonpilot: %s must not contain %q, which cron reads as a newline: %q",
		field, value[i], value)
}

func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var validServiceNameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

func isValidServiceName(name string) bool {
	return validServiceNameRe.MatchString(name)
}

func (uc *UpdateConfig) setDefaults() {
	if uc.RetryMaxAttempts == 0 {
		uc.RetryMaxAttempts = 6
	}
	if uc.RetryBaseBackoff == 0 {
		uc.RetryBaseBackoff = 30 * time.Second
	}
	if uc.RetryMaxBackoff == 0 {
		uc.RetryMaxBackoff = 15 * time.Minute
	}
	if uc.CheckInterval == 0 {
		uc.CheckInterval = 5 * time.Minute
	}
	if uc.SelftestTimeout == 0 {
		uc.SelftestTimeout = 60 * time.Second
	}
	if uc.RollbackTimeout == 0 {
		uc.RollbackTimeout = 5 * time.Minute
	}
}
