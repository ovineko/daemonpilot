package daemonpilot

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestNewDefaultLogger(t *testing.T) {
	logger := NewDefaultLogger(LogLevelInfo)
	if logger == nil {
		t.Fatal("NewDefaultLogger returned nil")
	}

	handler := logger.Handler()
	if handler == nil {
		t.Fatal("logger has nil handler")
	}

	if !handler.Enabled(context.TODO(), slog.LevelInfo) {
		t.Error("info level should be enabled")
	}
	if handler.Enabled(context.TODO(), slog.LevelDebug) {
		t.Error("debug level should not be enabled at info level")
	}
}

func TestNewDefaultLoggerDebugLevel(t *testing.T) {
	logger := NewDefaultLogger(LogLevelDebug)

	handler := logger.Handler()
	if !handler.Enabled(context.TODO(), slog.LevelDebug) {
		t.Error("debug level should be enabled at debug level")
	}
}

func TestNewDefaultLoggerErrorLevel(t *testing.T) {
	logger := NewDefaultLogger(LogLevelError)

	handler := logger.Handler()
	if handler.Enabled(context.TODO(), slog.LevelWarn) {
		t.Error("warn level should not be enabled at error level")
	}
	if !handler.Enabled(context.TODO(), slog.LevelError) {
		t.Error("error level should be enabled at error level")
	}
}

func TestLogOutputIsJSON(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(handler)

	logger.Info("hello", slog.String("key", "value"))

	var logEntry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &logEntry); err != nil {
		t.Fatalf("log output is not valid JSON: %v\noutput: %s", err, buf.String())
	}

	if logEntry["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", logEntry["msg"])
	}
	if logEntry["key"] != "value" {
		t.Errorf("key = %v, want value", logEntry["key"])
	}
	if _, ok := logEntry["time"]; !ok {
		t.Error("missing time field in JSON output")
	}
}

func TestInitLoggerSetsNoopWhenNil(t *testing.T) {
	c := &Config{}
	c.initLogger()

	if c.Logger == nil {
		t.Fatal("initLogger should set Logger when nil")
	}

	if _, ok := c.Logger.(noopLogger); !ok {
		t.Error("initLogger should set noopLogger when Logger is nil")
	}
}

func TestInitLoggerPreservesExisting(t *testing.T) {
	var buf bytes.Buffer
	custom := slog.New(slog.NewJSONHandler(&buf, nil))

	c := &Config{Logger: custom}
	c.initLogger()

	if c.Logger != custom {
		t.Error("initLogger should not replace existing logger")
	}
}

func TestSetDefaultsInitializesLogger(t *testing.T) {
	c := &Config{
		Name:       "test",
		WorkerFunc: func(ctx context.Context) error { return nil },
	}
	if err := c.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}

	if c.Logger == nil {
		t.Fatal("SetDefaults should initialize Logger")
	}
}

func TestLogLevelConstants(t *testing.T) {
	if LogLevelDebug != slog.LevelDebug {
		t.Error("LogLevelDebug mismatch")
	}
	if LogLevelInfo != slog.LevelInfo {
		t.Error("LogLevelInfo mismatch")
	}
	if LogLevelWarn != slog.LevelWarn {
		t.Error("LogLevelWarn mismatch")
	}
	if LogLevelError != slog.LevelError {
		t.Error("LogLevelError mismatch")
	}
}

func TestLoggerInterfaceCompatibleWithSlog(t *testing.T) {
	var l Logger = slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	l.Info("test message")
	l.Debug("debug message")
	l.Warn("warn message")
	l.Error("error message")
}

func TestNoopLoggerDoesNotPanic(t *testing.T) {
	var l Logger = noopLogger{}
	l.Debug("debug")
	l.Info("info")
	l.Warn("warn")
	l.Error("error")
}

func TestSlogLoggerSatisfiesInterface(t *testing.T) {
	var buf bytes.Buffer
	slogLogger := slog.New(slog.NewJSONHandler(&buf, nil))

	var l Logger = slogLogger
	l.Info("hello", "key", "value")

	var logEntry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &logEntry); err != nil {
		t.Fatalf("slog logger output is not valid JSON: %v", err)
	}
	if logEntry["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", logEntry["msg"])
	}
}

func TestNilLoggerBecomesNoop(t *testing.T) {
	cfg := Config{Name: "test"}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}

	cfg.Logger.Info("should not panic")
	cfg.Logger.Debug("should not panic")
	cfg.Logger.Warn("should not panic")
	cfg.Logger.Error("should not panic")
}

func TestUserProvidedLoggerPreserved(t *testing.T) {
	var buf bytes.Buffer
	userLogger := slog.New(slog.NewJSONHandler(&buf, nil))

	cfg := Config{Name: "test", Logger: userLogger}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}

	cfg.Logger.Info("test")

	if buf.Len() == 0 {
		t.Error("user-provided logger should have received log output")
	}
}

func TestVerboseLoggingDefaultFalse(t *testing.T) {
	var cfg Config
	if cfg.VerboseLogging {
		t.Error("VerboseLogging should default to false")
	}
}

func TestIsEmbeddedSystem(t *testing.T) {
	tests := []struct {
		initSystem string
		want       bool
	}{
		{"busybox", true},
		{"procd", true},
		{"s6", true},
		{"runit", true},
		{"systemd", false},
		{"launchd", false},
		{"openrc", false},
		{"crontab", false},
		{"", false},
		{"unknown", false},
	}
	for _, tt := range tests {
		t.Run(tt.initSystem, func(t *testing.T) {
			got := IsEmbeddedSystem(tt.initSystem)
			if got != tt.want {
				t.Errorf("IsEmbeddedSystem(%q) = %v, want %v", tt.initSystem, got, tt.want)
			}
		})
	}
}

func TestRecommendedLogLevel(t *testing.T) {
	tests := []struct {
		initSystem string
		want       string
	}{
		{"busybox", "silent"},
		{"procd", "silent"},
		{"s6", "silent"},
		{"runit", "silent"},
		{"systemd", "normal"},
		{"launchd", "normal"},
		{"openrc", "normal"},
		{"crontab", "normal"},
		{"", "normal"},
		{"unknown", "normal"},
	}
	for _, tt := range tests {
		t.Run(tt.initSystem, func(t *testing.T) {
			got := RecommendedLogLevel(tt.initSystem)
			if got != tt.want {
				t.Errorf("RecommendedLogLevel(%q) = %q, want %q", tt.initSystem, got, tt.want)
			}
		})
	}
}

type recordingLogger struct {
	calls []logCall
}

type logCall struct {
	level string
	msg   string
	args  []any
}

func (r *recordingLogger) Debug(msg string, args ...any) {
	r.calls = append(r.calls, logCall{"debug", msg, args})
}

func (r *recordingLogger) Info(msg string, args ...any) {
	r.calls = append(r.calls, logCall{"info", msg, args})
}

func (r *recordingLogger) Warn(msg string, args ...any) {
	r.calls = append(r.calls, logCall{"warn", msg, args})
}

func (r *recordingLogger) Error(msg string, args ...any) {
	r.calls = append(r.calls, logCall{"error", msg, args})
}

func TestEmbeddedFilterLoggerOnlyPassesErrors(t *testing.T) {
	inner := &recordingLogger{}
	filtered := NewEmbeddedFilterLogger(inner)

	filtered.Debug("debug msg", "k", "v")
	filtered.Info("info msg")
	filtered.Warn("warn msg")
	filtered.Error("error msg", "err", "boom")

	if len(inner.calls) != 1 {
		t.Fatalf("expected 1 call to inner logger, got %d", len(inner.calls))
	}
	if inner.calls[0].level != "error" {
		t.Errorf("expected error level, got %s", inner.calls[0].level)
	}
	if inner.calls[0].msg != "error msg" {
		t.Errorf("expected 'error msg', got %q", inner.calls[0].msg)
	}
	if len(inner.calls[0].args) != 2 || inner.calls[0].args[0] != "err" {
		t.Errorf("expected args [err boom], got %v", inner.calls[0].args)
	}
}

func TestEmbeddedFilterLoggerDoesNotPanic(t *testing.T) {
	filtered := NewEmbeddedFilterLogger(NoopLogger())
	filtered.Debug("debug")
	filtered.Info("info")
	filtered.Warn("warn")
	filtered.Error("error")
}

func TestApplyEmbeddedLogFilter(t *testing.T) {
	tests := []struct {
		name           string
		initSystem     string
		verboseLogging bool
		loggerNil      bool
		wantFiltered   bool
	}{
		{"embedded system filters non-error", "busybox", false, false, true},
		{"embedded procd filters", "procd", false, false, true},
		{"embedded s6 filters", "s6", false, false, true},
		{"embedded runit filters", "runit", false, false, true},
		{"non-embedded systemd no filter", "systemd", false, false, false},
		{"non-embedded launchd no filter", "launchd", false, false, false},
		{"embedded with verbose no filter", "busybox", true, false, false},
		{"nil logger stays nil-like", "", false, true, false},
		{"empty init system no filter", "", false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				InitSystemName: tt.initSystem,
				VerboseLogging: tt.verboseLogging,
			}
			if tt.loggerNil {
				cfg.Logger = nil
			} else {
				cfg.Logger = &recordingLogger{}
			}

			cfg.ApplyEmbeddedLogFilter()

			if tt.loggerNil {
				if cfg.Logger != nil {
					t.Error("nil logger should remain nil")
				}
				return
			}

			_, isFiltered := cfg.Logger.(embeddedFilterLogger)
			if isFiltered != tt.wantFiltered {
				t.Errorf("filtered = %v, want %v", isFiltered, tt.wantFiltered)
			}
		})
	}
}

func TestApplyEmbeddedLogFilterPreservesNoop(t *testing.T) {
	cfg := &Config{
		InitSystemName: "busybox",
		Logger:         NoopLogger(),
	}
	cfg.ApplyEmbeddedLogFilter()

	if _, ok := cfg.Logger.(noopLogger); !ok {
		t.Error("noop logger should not be wrapped")
	}
}

func TestEmbeddedFilterIntegration(t *testing.T) {
	inner := &recordingLogger{}
	cfg := &Config{
		Name:           "test",
		InitSystemName: "busybox",
		Logger:         inner,
		WorkerFunc:     func(ctx context.Context) error { return nil },
	}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}
	cfg.ApplyEmbeddedLogFilter()

	cfg.Logger.Info("should be filtered")
	cfg.Logger.Warn("should be filtered")
	cfg.Logger.Debug("should be filtered")
	cfg.Logger.Error("should pass through")

	if len(inner.calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %v", len(inner.calls), inner.calls)
	}
	if inner.calls[0].level != "error" {
		t.Errorf("expected error, got %s", inner.calls[0].level)
	}
}

func TestVerboseLoggingOverridesEmbeddedFilter(t *testing.T) {
	inner := &recordingLogger{}
	cfg := &Config{
		Name:           "test",
		InitSystemName: "procd",
		VerboseLogging: true,
		Logger:         inner,
		WorkerFunc:     func(ctx context.Context) error { return nil },
	}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}
	cfg.ApplyEmbeddedLogFilter()

	cfg.Logger.Info("should pass through")
	cfg.Logger.Error("should pass through")

	if len(inner.calls) != 2 {
		t.Fatalf("expected 2 calls with VerboseLogging, got %d", len(inner.calls))
	}
}

func TestGetSystemInfo(t *testing.T) {
	tests := []struct {
		initSystem       string
		wantEmbedded     bool
		wantLevel        string
		wantWarningEmpty bool
	}{
		{"busybox", true, "silent", false},
		{"procd", true, "silent", false},
		{"s6", true, "silent", false},
		{"runit", true, "silent", false},
		{"systemd", false, "normal", true},
		{"launchd", false, "normal", true},
		{"openrc", false, "normal", true},
		{"crontab", false, "normal", true},
		{"", false, "normal", true},
		{"unknown", false, "normal", true},
	}
	for _, tt := range tests {
		t.Run(tt.initSystem, func(t *testing.T) {
			info := GetSystemInfo(tt.initSystem)
			if info.InitSystem != tt.initSystem {
				t.Errorf("InitSystem = %q, want %q", info.InitSystem, tt.initSystem)
			}
			if info.IsEmbedded != tt.wantEmbedded {
				t.Errorf("IsEmbedded = %v, want %v", info.IsEmbedded, tt.wantEmbedded)
			}
			if info.RecommendedLevel != tt.wantLevel {
				t.Errorf("RecommendedLevel = %q, want %q", info.RecommendedLevel, tt.wantLevel)
			}
			if tt.wantWarningEmpty && info.StorageWarning != "" {
				t.Errorf("StorageWarning should be empty for %q, got %q", tt.initSystem, info.StorageWarning)
			}
			if !tt.wantWarningEmpty && info.StorageWarning == "" {
				t.Errorf("StorageWarning should not be empty for %q", tt.initSystem)
			}
		})
	}
}

func TestGetSystemInfoStorageWarningContent(t *testing.T) {
	info := GetSystemInfo("busybox")
	if info.StorageWarning == "" {
		t.Fatal("expected non-empty StorageWarning for embedded system")
	}
	if !contains(info.StorageWarning, "flash storage") {
		t.Error("StorageWarning should mention flash storage")
	}
	if !contains(info.StorageWarning, "write cycles") {
		t.Error("StorageWarning should mention write cycles")
	}
}

func TestNonEmbeddedSystemNormalLogging(t *testing.T) {
	inner := &recordingLogger{}
	cfg := &Config{
		Name:           "test",
		InitSystemName: "systemd",
		Logger:         inner,
		WorkerFunc:     func(ctx context.Context) error { return nil },
	}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}
	cfg.ApplyEmbeddedLogFilter()

	cfg.Logger.Info("info")
	cfg.Logger.Warn("warn")
	cfg.Logger.Error("error")

	if len(inner.calls) != 3 {
		t.Fatalf("expected 3 calls on non-embedded, got %d", len(inner.calls))
	}
}
