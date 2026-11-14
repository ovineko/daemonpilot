//go:build linux

package daemon

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

// These tests EXECUTE the generated BusyBox init script against a fake daemon to
// validate the respawn supervisor logic end-to-end (the content-assertion tests
// can't catch a shell bug, and this script supervises every BusyBox agent). The
// script hardcodes /var/run; we rewrite that to a writable tmp dir so the logic
// can run unprivileged — the respawn behavior is path-independent.

// initGenerator produces an init script for a service whose daemon is execPath.
type initGenerator func(execPath string) (string, error)

// Both generators run the fake daemon through /bin/sh, whose executable identity
// is the shell on native hosts and the binfmt interpreter under qemu-user.
func busyboxGen(script string) (string, error) {
	return GenerateBusyboxInitScript(ServiceConfig{Name: "respawnsvc", Arguments: []string{script}}, "/bin/sh")
}

func sysvinitGen(script string) (string, error) {
	return GenerateSysvinitInitScript(ServiceConfig{Name: "respawnsvc", ExecPath: "/bin/sh", Arguments: []string{script}})
}

// writeRespawnHarness generates the init script pointed at a fake daemon with the
// given body, rewires /var/run to tmp, and returns the script path + the counter
// file the fake daemon appends to on each launch.
func writeRespawnHarness(t *testing.T, gen initGenerator, daemonBody string) (scriptPath, counterPath string) {
	t.Helper()
	tmp := t.TempDir()
	counterPath = filepath.Join(tmp, "runs")
	if err := os.WriteFile(counterPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	daemonPath := filepath.Join(tmp, "faked")
	daemon := "#!/bin/sh\nprintf 'x' >> " + counterPath + "\n" + daemonBody + "\n"
	if err := os.WriteFile(daemonPath, []byte(daemon), 0o700); err != nil {
		t.Fatal(err)
	}

	script, err := gen(daemonPath)
	if err != nil {
		t.Fatal(err)
	}
	// Rewire the hardcoded run dir to a writable tmp path so PIDFILE/STOPFILE work
	// unprivileged; the respawn logic under test is unaffected.
	script = strings.ReplaceAll(script, "/var/run/", tmp+"/")
	if testsupport.Emulated() {
		// The guest shell is a QEMU process in the host's /proc. Measure a separate
		// shell's kernel identity; retain the generated stop path's equality check.
		identity := shellKernelExecutable(t)
		script = strings.ReplaceAll(script, `readlink -f "$DAEMON"`, `printf '%s\n' `+shellQuote(identity))
	}
	scriptPath = filepath.Join(tmp, "init.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return scriptPath, counterPath
}

func runInit(t *testing.T, scriptPath, verb string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(40*time.Second))
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", scriptPath, verb)
	cmd.WaitDelay = testsupport.Scale(200 * time.Millisecond)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s failed: %v: %s", scriptPath, verb, err, out)
	}
}

func TestInitStartDetachesOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		gen  initGenerator
	}{{"busybox", busyboxGen}, {"sysvinit", sysvinitGen}} {
		t.Run(tc.name, func(t *testing.T) {
			path, counter := writeRespawnHarness(t, tc.gen, "while :; do sleep 0.1; done")
			t.Cleanup(func() { runInit(t, path, "stop") })
			runInit(t, path, "start")
			deadline := time.Now().Add(testsupport.Scale(3 * time.Second))
			for runCount(t, counter) == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if runCount(t, counter) != 1 {
				t.Fatal("start returned without launching service")
			}
		})
	}
}

func runCount(t *testing.T, counterPath string) int {
	t.Helper()
	b, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

// TestInitSupervisorRespawn validates the respawn supervisor for every run-once
// init whose generated script includes it, by actually executing the shell.
func TestInitSupervisorRespawn(t *testing.T) {
	inits := []struct {
		name string
		gen  initGenerator
	}{
		{"busybox", busyboxGen},
		{"sysvinit", sysvinitGen},
	}
	for _, tc := range inits {
		t.Run(tc.name, func(t *testing.T) {
			// The fake daemons live ~2s before exiting so sysvinit's start()
			// 1-second liveness check sees a running process (start returns 0),
			// then the exit exercises the respawn decision.

			// A daemon that keeps crashing (abnormal, non-zero exit) is respawned,
			// and stop() halts the respawn.
			t.Run("respawns-on-crash", func(t *testing.T) {
				scriptPath, counter := writeRespawnHarness(t, tc.gen, "sleep 2\nexit 1")
				runInit(t, scriptPath, "start")
				t.Cleanup(func() { _ = exec.Command("/bin/sh", scriptPath, "stop").Run() })

				time.Sleep(testsupport.Scale(10 * time.Second)) // ~2s run + 2s backoff per cycle → several launches
				if n := runCount(t, counter); n < 2 {
					t.Fatalf("expected the crashing daemon to be respawned (>=2 launches), got %d", n)
				}
				runInit(t, scriptPath, "stop")
				settled := runCount(t, counter)
				time.Sleep(testsupport.Scale(5 * time.Second))
				if n := runCount(t, counter); n != settled {
					t.Fatalf("daemon respawned AFTER stop: %d -> %d", settled, n)
				}
			})

			// A clean exit (0 = intentional permanent stop, e.g. delete-agent) is
			// NOT respawned.
			t.Run("no-respawn-on-clean-exit", func(t *testing.T) {
				scriptPath, counter := writeRespawnHarness(t, tc.gen, "sleep 2\nexit 0")
				runInit(t, scriptPath, "start")
				t.Cleanup(func() { _ = exec.Command("/bin/sh", scriptPath, "stop").Run() })

				time.Sleep(testsupport.Scale(7 * time.Second))
				if n := runCount(t, counter); n != 1 {
					t.Fatalf("clean exit must not respawn; expected exactly 1 launch, got %d", n)
				}
			})

			// stop() terminates a healthy long-running daemon and prevents respawn.
			t.Run("stop-halts-respawn", func(t *testing.T) {
				scriptPath, counter := writeRespawnHarness(t, tc.gen, "sleep 30")
				runInit(t, scriptPath, "start")
				t.Cleanup(func() { _ = exec.Command("/bin/sh", scriptPath, "stop").Run() })

				time.Sleep(testsupport.Scale(2 * time.Second))
				if n := runCount(t, counter); n != 1 {
					t.Fatalf("expected 1 launch of the long-running daemon, got %d", n)
				}
				runInit(t, scriptPath, "stop")
				time.Sleep(testsupport.Scale(3 * time.Second))
				if n := runCount(t, counter); n != 1 {
					t.Fatalf("daemon respawned after stop: got %d launches", n)
				}
			})
		})
	}
}

func shellKernelExecutable(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testsupport.Scale(5*time.Second))
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "echo ready; read answer")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = cmd.Wait() }()
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("shell readiness: %q (%v)", line, err)
	}
	path, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInitHarnessKernelIdentity(t *testing.T) {
	modes := []string{"1"}
	if !testsupport.Emulated() {
		modes = append(modes, "0")
	}
	for _, emulated := range modes {
		t.Run(emulated, func(t *testing.T) {
			t.Setenv("DAEMONPILOT_TEST_EMULATED", emulated)
			path, counter := writeRespawnHarness(t, sysvinitGen, "while :; do sleep 0.1; done")
			t.Cleanup(func() { runInit(t, path, "stop") })
			runInit(t, path, "start")
			if runCount(t, counter) != 1 {
				t.Fatal("shell did not start")
			}
		})
	}
}
