// Package testsupport owns portable test fixtures and timing controls.
package testsupport

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// Scale multiplies a wall-clock budget and rejects invalid settings and overflow.
func Scale(d time.Duration) time.Duration {
	raw := os.Getenv("DAEMONPILOT_TEST_TIME_SCALE")
	if raw == "" {
		raw = "1"
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 || d < 0 || (d > 0 && n > int64(1<<63-1)/int64(d)) {
		panic("invalid DAEMONPILOT_TEST_TIME_SCALE or duration overflow")
	}
	return d * time.Duration(n)
}

// SkipAddressSpace excludes explicitly declared emulation from RLIMIT_AS experiments.
func SkipAddressSpace(t *testing.T) {
	t.Helper()
	if Emulated() {
		t.Skip("runner declared emulation: RLIMIT_AS constrains the emulator address space")
	}
}

// Executable returns the absolute test binary path, including after a chdir.
func Executable() string {
	path, err := os.Executable()
	if err != nil {
		panic(fmt.Sprintf("locate test executable: %v", err))
	}
	return path
}

// Fixture copies a prebuilt fixture or builds one when no bundle is configured.
func Fixture(t *testing.T, name, source string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if dir := os.Getenv("DAEMONPILOT_TEST_FIXTURES"); dir != "" {
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // The test runner explicitly supplies its trusted fixture directory.
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o700); err != nil { //nolint:gosec // A private test fixture must be executable.
			t.Fatal(err)
		}
		return path
	}
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", path, source) //nolint:gosec // Source is a test-owned package path, never application input.
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, out)
	}
	return path
}

const (
	// WorkerArgumentOutputKey marks the watchdog's argument-contract child.
	WorkerArgumentOutputKey = "WORKER_ARGUMENT_OUTPUT"
	// ProbeGroupOutputKey marks a process-group probe child.
	ProbeGroupOutputKey = "DAEMONPILOT_TEST_PGID_OUTPUT"
	// RenderProbeKey marks the manifest argv/environment probe.
	RenderProbeKey = "GO_DAEMONPILOT_RENDER_PROBE"
	// RenderOutputKey names the probe's JSON observation file.
	RenderOutputKey = "GO_DAEMONPILOT_RENDER_OUTPUT"
)

// WorkerArgumentOutput returns the argument-contract output path.
func WorkerArgumentOutput() string { return os.Getenv(WorkerArgumentOutputKey) }

// ProbeGroupOutput returns the process-group probe output path.
func ProbeGroupOutput() string { return os.Getenv(ProbeGroupOutputKey) }

// RenderProbe reports whether the current process is a manifest probe.
func RenderProbe() bool { return os.Getenv(RenderProbeKey) == "1" }

// RenderOutput returns the manifest probe's observation path.
func RenderOutput() string { return os.Getenv(RenderOutputKey) }

// RenderValues returns the two environment payloads used by manifest probes.
func RenderValues() (string, string) { return os.Getenv("PROBE_VALUE"), os.Getenv("SECOND_VALUE") }

// Emulated reports the runner's explicit qemu-user declaration.
func Emulated() bool {
	switch os.Getenv("DAEMONPILOT_TEST_EMULATED") {
	case "1":
		return true
	case "", "0":
		return false
	default:
		panic("DAEMONPILOT_TEST_EMULATED must be 0 or 1")
	}
}
