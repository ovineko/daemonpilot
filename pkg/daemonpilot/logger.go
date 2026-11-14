package daemonpilot

import (
	"log/slog"
	"os"
)

// Logger is the logging interface used by daemonpilot. It is compatible with
// stdlib log/slog: a *slog.Logger can be passed directly.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// LogLevel represents the logging level for the daemonpilot logger.
type LogLevel = slog.Level

// Predefined log levels matching [log/slog] levels.
const (
	LogLevelDebug LogLevel = slog.LevelDebug
	LogLevelInfo  LogLevel = slog.LevelInfo
	LogLevelWarn  LogLevel = slog.LevelWarn
	LogLevelError LogLevel = slog.LevelError
)

// NewDefaultLogger creates a JSON logger writing to stderr at the given level.
func NewDefaultLogger(level LogLevel) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	}))
}

// noopLogger is a Logger that discards all output.
type noopLogger struct{}

func (noopLogger) Debug(string, ...any) {}
func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

// NoopLogger returns a Logger that silently discards all output.
func NoopLogger() Logger {
	return noopLogger{}
}

// IsEmbeddedSystem reports whether the given init system typically runs on
// embedded hardware with flash storage (SD cards, eMMC) that has limited
// write cycles. Passing an empty string returns false.
func IsEmbeddedSystem(initSystem string) bool {
	switch initSystem {
	case "busybox", "procd", "s6", "runit":
		return true
	default:
		return false
	}
}

// RecommendedLogLevel returns "silent" for embedded init systems (to protect
// flash storage) and "normal" for all others.
func RecommendedLogLevel(initSystem string) string {
	if IsEmbeddedSystem(initSystem) {
		return "silent"
	}
	return "normal"
}

// SystemInfo provides information about the detected init system and its
// characteristics, helping users decide their own logging and operational
// strategy.
type SystemInfo struct {
	InitSystem       string
	IsEmbedded       bool
	RecommendedLevel string
	StorageWarning   string
}

// GetSystemInfo returns a SystemInfo describing the given init system's
// characteristics. Users can call this to decide their own logging strategy
// rather than relying on daemonpilot's built-in embedded detection.
func GetSystemInfo(initSystem string) SystemInfo {
	embedded := IsEmbeddedSystem(initSystem)
	info := SystemInfo{
		InitSystem:       initSystem,
		IsEmbedded:       embedded,
		RecommendedLevel: RecommendedLogLevel(initSystem),
	}
	if embedded {
		info.StorageWarning = "This init system typically runs on flash storage (SD cards, eMMC) " +
			"with limited write cycles. Excessive logging may degrade storage lifespan. " +
			"Consider logging only critical errors or using a tmpfs-backed log directory."
	}
	return info
}

// embeddedFilterLogger wraps a Logger and only passes through Error-level
// calls. Used on embedded systems to reduce flash storage writes.
type embeddedFilterLogger struct {
	inner Logger
}

func (l embeddedFilterLogger) Debug(string, ...any) {}
func (l embeddedFilterLogger) Info(string, ...any)  {}
func (l embeddedFilterLogger) Warn(string, ...any)  {}
func (l embeddedFilterLogger) Error(msg string, args ...any) {
	l.inner.Error(msg, args...)
}

// NewEmbeddedFilterLogger wraps a Logger so that only Error-level messages
// are forwarded. Debug, Info, and Warn calls are silently discarded.
func NewEmbeddedFilterLogger(inner Logger) Logger {
	return embeddedFilterLogger{inner: inner}
}

func (c *Config) initLogger() {
	if c.Logger == nil {
		c.Logger = noopLogger{}
	}
}

// ApplyEmbeddedLogFilter wraps the configured Logger with an embedded filter
// if the init system is embedded and VerboseLogging is not enabled. This
// should be called after InitSystemName is set (during run command init).
func (c *Config) ApplyEmbeddedLogFilter() {
	if c.Logger == nil {
		return
	}
	if _, ok := c.Logger.(noopLogger); ok {
		return
	}
	if IsEmbeddedSystem(c.InitSystemName) && !c.VerboseLogging {
		c.Logger = NewEmbeddedFilterLogger(c.Logger)
	}
}
