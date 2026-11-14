//go:build e2e_docker

// Package wire owns fixture inputs, build variants, timings, and observations.
package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

// Fixture identity and timing bounds shared by runners and subprocesses.
const (
	Name           = "daemonpilot-e2e"
	EnvironmentKey = "GO_DAEMONPILOT_E2E_VALUE"
	ConfigEnv      = "GO_DAEMONPILOT_E2E_CONFIG"
	Grace          = 2 * time.Second
	Deadline       = 25 * time.Second
	Poll           = 50 * time.Millisecond
	UpdateInterval = 250 * time.Millisecond
	RetryAttempts  = 3
	HealthFailures = 3
	RollbackWindow = 9 * time.Second
	StagedStable   = 3 * time.Second
)

// Variant describes an immutable build, including intentionally unhealthy builds.
type Variant struct {
	ID, Version, WatchdogVersion string
	Broken, Unstable             bool
}

// Variants is the single build matrix used by fixture, server, and orchestrator.
func Variants() []Variant {
	return []Variant{
		{ID: "v1", Version: "1.0.0", WatchdogVersion: "1"},
		{ID: "fast", Version: "2.0.0", WatchdogVersion: "1"},
		{ID: "staged", Version: "2.1.0", WatchdogVersion: "2"},
		{ID: "unstable", Version: "2.2.0", WatchdogVersion: "2", Unstable: true},
		{ID: "broken", Version: "2.3.0", WatchdogVersion: "1", Broken: true},
		{ID: "newer", Version: "3.0.0", WatchdogVersion: "1"},
	}
}

// FindVariant rejects unknown release selectors.
func FindVariant(id string) (Variant, bool) {
	for _, v := range Variants() {
		if v.ID == id {
			return v, true
		}
	}
	return Variant{}, false
}

// Config is serialized by the host, and inherited across worker and watchdog launches.
type Config struct {
	Dir                  string            `json:"Dir"`
	ReleaseURL           string            `json:"ReleaseURL"`
	PublicKey            string            `json:"PublicKey"`
	RejectVersion        string            `json:"RejectVersion"`
	NativeReleaseDir     string            `json:"NativeReleaseDir"`
	NativeArtifacts      string            `json:"NativeArtifacts"`
	Stable               time.Duration     `json:"Stable"`
	DisableParentMonitor bool              `json:"DisableParentMonitor"`
	ServiceArguments     []string          `json:"ServiceArguments"`
	ServiceEnvironment   map[string]string `json:"ServiceEnvironment"`
	FailConfigApply      bool              `json:"FailConfigApply"`
}

// Load reads the sole fixture environment input.
func Load() (Config, error) {
	var c Config
	data, err := os.ReadFile(os.Getenv(ConfigEnv)) //nolint:gosec // Test-owned config path.
	if err != nil {
		return c, fmt.Errorf("read fixture config: %w", err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("decode fixture config: %w", err)
	}
	if !filepath.IsAbs(c.Dir) || c.Stable <= 0 {
		return c, errors.New("invalid fixture directory or stability duration")
	}
	return c, nil
}

// EnvironmentValue observes the value inherited from the real service manifest.
func EnvironmentValue() string { return os.Getenv(EnvironmentKey) }

// RequireDocker turns a missing Docker facility into a failure for explicit runs.
func RequireDocker() bool { return os.Getenv("DAEMONPILOT_E2E_REQUIRE") == "1" }

// Record identifies an externally observed process event.
type Record struct {
	Arguments       []string  `json:"arguments,omitempty"`
	Directory       string    `json:"directory,omitempty"`
	Event           string    `json:"event"`
	PID             int       `json:"pid"`
	Parent          int       `json:"parent"`
	At              time.Time `json:"at"`
	Version         string    `json:"version,omitempty"`
	WatchdogVersion string    `json:"watchdog_version,omitempty"`
	Value           string    `json:"value,omitempty"`
}

// Snapshot combines the actual build version and the watchdog's IPC state.
type Snapshot struct {
	Version     string
	Environment string
	PID         int
	At          time.Time
	Runtime     daemonpilot.RuntimeInfo
}

// Command targets a single worker, so a restart cannot consume an old request.
type Command struct {
	PID    int    `json:"PID"`
	Action string `json:"Action"`
	Code   int    `json:"Code"`
}

// Write atomically replaces a test-owned file.
func Write(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return fmt.Errorf("create marker: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("make test evidence readable: %w", err)
	}
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil {
		return fmt.Errorf("write marker: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close marker: %w", closeErr)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("publish marker: %w", err)
	}
	return nil
}

// WriteJSON publishes a complete JSON observation.
func WriteJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal marker: %w", err)
	}
	return Write(path, data)
}
