package daemonpilot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrLivenessLapsed means a parseable worker deadline has passed.
var ErrLivenessLapsed = errors.New("worker liveness deadline lapsed")

// Liveness shares a worker deadline with its watchdog through a file.
type Liveness struct{ path string }

// NewLiveness uses path for the shared deadline. Its parent directory must exist.
func NewLiveness(path string) *Liveness { return &Liveness{path: path} }

// Path returns the shared deadline file's path.
func (l *Liveness) Path() string { return l.path }

// Mark atomically writes a decimal UnixNano deadline, with no newline, using mode 0600.
// This format is a compatibility contract with already deployed watchdogs.
func (l *Liveness) Mark(grace time.Duration) error {
	deadline := strconv.FormatInt(time.Now().Add(grace).UnixNano(), 10)
	f, err := os.CreateTemp(filepath.Dir(l.path), ".daemonpilot-liveness-*")
	if err != nil {
		return fmt.Errorf("creating liveness file: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(deadline); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing liveness deadline: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing liveness file: %w", err)
	}
	if err := os.Rename(f.Name(), l.path); err != nil {
		return fmt.Errorf("replacing liveness file: %w", err)
	}
	return nil
}

// Check reports ErrLivenessLapsed only for a readable, parseable, expired deadline.
// Missing, unreadable, and malformed files are healthy to avoid spurious restarts.
func (l *Liveness) Check() error {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return nil //nolint:nilerr // Missing or unreadable liveness files are healthy by contract.
	}
	nanos, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return nil //nolint:nilerr // Malformed liveness deadlines are healthy by contract.
	}
	elapsed := time.Since(time.Unix(0, nanos))
	if elapsed > 0 {
		return fmt.Errorf("%w %s ago", ErrLivenessLapsed, elapsed.Round(time.Second))
	}
	return nil
}

// HealthFunc adapts Check for Config.HealthFunc.
func (l *Liveness) HealthFunc() func(context.Context) error {
	return func(context.Context) error { return l.Check() }
}
