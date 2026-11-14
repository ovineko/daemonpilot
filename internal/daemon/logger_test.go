package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestNoopDetectionLogger(t *testing.T) {
	l := noopDetectionLogger{}
	l.Debug("should not panic", "key", "value")
}

func TestSetDetectionLogger(t *testing.T) {
	original := getDetectionLog()
	defer SetDetectionLogger(original)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	SetDetectionLogger(logger)
	getDetectionLog().Debug("test message", "key", "val")

	if !strings.Contains(buf.String(), "test message") {
		t.Errorf("expected log output to contain 'test message', got %q", buf.String())
	}
}

func TestSetDetectionLogger_Nil(t *testing.T) {
	original := getDetectionLog()
	defer SetDetectionLogger(original)

	SetDetectionLogger(nil)
	getDetectionLog().Debug("should not panic")

	if _, ok := getDetectionLog().(noopDetectionLogger); !ok {
		t.Errorf("expected noopDetectionLogger after setting nil, got %T", getDetectionLog())
	}
}

func TestNewDetectionLogger_TraceLevel(t *testing.T) {
	t.Setenv("LOG_LEVEL", "trace")
	l := NewDetectionLogger()
	if l == nil {
		t.Fatal("expected non-nil logger when LOG_LEVEL=trace")
	}
}

func TestNewDetectionLogger_DebugLevel(t *testing.T) {
	t.Setenv("LOG_LEVEL", "debug")
	l := NewDetectionLogger()
	if l == nil {
		t.Fatal("expected non-nil logger when LOG_LEVEL=debug")
	}
}

func TestNewDetectionLogger_CaseInsensitive(t *testing.T) {
	t.Setenv("LOG_LEVEL", "TRACE")
	l := NewDetectionLogger()
	if l == nil {
		t.Fatal("expected non-nil logger when LOG_LEVEL=TRACE")
	}
}

// Below trace/debug the detection trace is off, but warnings survive: they carry
// outcomes that change what the install did, and a noop logger made them
// unreachable in the configuration nearly every install runs in.
func TestNewDetectionLogger_QuietLevelsDropDebugButKeepWarn(t *testing.T) {
	for _, level := range []string{"", "info", "warn", "error", "nonsense"} {
		t.Run("LOG_LEVEL="+level, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", level)
			l, ok := NewDetectionLogger().(*slog.Logger)
			if !ok {
				t.Fatalf("NewDetectionLogger() = %T, want *slog.Logger", NewDetectionLogger())
			}
			if l.Enabled(context.Background(), slog.LevelDebug) {
				t.Error("detection trace is on at a non-debug LOG_LEVEL")
			}
			if !l.Enabled(context.Background(), slog.LevelWarn) {
				t.Error("warnings are dropped, so logDetectionWarn never reaches the operator")
			}
		})
	}
}

func TestInitDetectionLogging_EnablesLogger(t *testing.T) {
	original := getDetectionLog()
	defer SetDetectionLogger(original)

	t.Setenv("LOG_LEVEL", "trace")
	SetDetectionLogger(nil)
	InitDetectionLogging()

	if _, ok := getDetectionLog().(noopDetectionLogger); ok {
		t.Error("expected real logger after InitDetectionLogging with LOG_LEVEL=trace")
	}
}

func TestInitDetectionLogging_KeepsWarningsWhenTraceIsOff(t *testing.T) {
	original := getDetectionLog()
	defer SetDetectionLogger(original)

	t.Setenv("LOG_LEVEL", "info")
	SetDetectionLogger(nil)
	InitDetectionLogging()

	l, ok := getDetectionLog().(*slog.Logger)
	if !ok {
		t.Fatalf("getDetectionLog() = %T, want *slog.Logger", getDetectionLog())
	}
	if !l.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("InitDetectionLogging installed a logger that drops warnings")
	}
}

// Before InitDetectionLogging runs — and after SetDetectionLogger(nil) — the
// package must stay silent rather than write to a caller's stderr uninvited.
func TestDetectionLogDefaultIsNoop(t *testing.T) {
	original := getDetectionLog()
	defer SetDetectionLogger(original)

	SetDetectionLogger(nil)
	if _, ok := getDetectionLog().(noopDetectionLogger); !ok {
		t.Fatalf("getDetectionLog() = %T, want noopDetectionLogger", getDetectionLog())
	}
	getDetectionLog().Debug("should not panic with default logger")
	getDetectionLog().Warn("should not panic with default logger")
}
