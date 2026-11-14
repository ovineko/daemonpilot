package daemon

import (
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
)

// DetectionLogger defines the logging interface for init system detection
// diagnostics. Compatible with *slog.Logger.
type DetectionLogger interface {
	Debug(msg string, args ...any)
	Warn(msg string, args ...any)
}

type noopDetectionLogger struct{}

func (noopDetectionLogger) Debug(string, ...any) {}
func (noopDetectionLogger) Warn(string, ...any)  {}

type loggerHolder struct {
	logger DetectionLogger
}

var detectionLogValue atomic.Value

func init() {
	detectionLogValue.Store(loggerHolder{logger: noopDetectionLogger{}})
}

func getDetectionLog() DetectionLogger {
	holder, ok := detectionLogValue.Load().(loggerHolder)
	if !ok {
		return noopDetectionLogger{}
	}
	return holder.logger
}

// logDetectionWarn emits msg at warning level. Unlike the Debug diagnostics
// around it, this reaches the operator at default log levels: NewDetectionLogger
// keeps a warn-level logger when detection debugging is off, because the
// outcomes routed here — the fallback detector winning, a launchd domain that
// cannot be resolved — change what the install actually did.
func logDetectionWarn(msg string, args ...any) {
	getDetectionLog().Warn(msg, args...)
}

// SetDetectionLogger configures the package-level detection logger.
// Passing nil resets to a no-op logger.
func SetDetectionLogger(l DetectionLogger) {
	if l == nil {
		detectionLogValue.Store(loggerHolder{logger: noopDetectionLogger{}})
		return
	}
	detectionLogValue.Store(loggerHolder{logger: l})
}

// NewDetectionLogger creates a detection logger based on the LOG_LEVEL
// environment variable. LOG_LEVEL "trace" or "debug" enables the full detection
// trace; any other value still emits warnings, because logDetectionWarn carries
// outcomes an operator has to see and gating them behind a debug level would
// make them unreachable in the only configuration most installs ever run in.
func NewDetectionLogger() DetectionLogger {
	level := slog.LevelWarn
	if l := strings.ToLower(os.Getenv("LOG_LEVEL")); l == "trace" || l == "debug" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// InitDetectionLogging sets up detection logging from the LOG_LEVEL
// environment variable. Call this before DetectServiceManager to enable
// diagnostic output.
func InitDetectionLogging() {
	SetDetectionLogger(NewDetectionLogger())
}
