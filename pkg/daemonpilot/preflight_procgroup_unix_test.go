//go:build !windows

package daemonpilot

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

// writeProbeScript drops an executable shell script and returns its path.
func writeProbeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	return path
}

// TestRunVersionProbe_RunsInOwnProcessGroup pins that the probe does not share
// the CLI's process group: the kill on timeout is addressed to the group, so a
// shared one would signal the installing process itself.
func TestRunVersionProbe_RunsInOwnProcessGroup(t *testing.T) {
	out := filepath.Join(t.TempDir(), "pgid")
	t.Setenv(testsupport.ProbeGroupOutputKey, out)
	script := testsupport.Executable()

	if err := runVersionProbe(script, []string{"-test.run=^TestProbeProcessGroupChild$", "--"}); err != nil {
		t.Fatalf("runVersionProbe() error: %v", err)
	}

	recorded, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	own := syscall.Getpgrp()
	if got := string(recorded); got == "" {
		t.Fatal("probe recorded no process group")
	} else if pgid := parsePGID(t, got); pgid == own {
		t.Errorf("probe pgid = %d, want a group of its own (test pgid %d)", pgid, own)
	}
}

// TestRunVersionProbe_TimeoutKillsGrandchildren pins that a probe which hangs
// takes its children with it. A host binary that does not route
// watchdog-version may instead start doing its real work and spawn a worker;
// killing only the direct child would leave that worker orphaned by an install.
func TestRunVersionProbe_TimeoutKillsGrandchildren(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "grandchild-alive")
	pidFile := filepath.Join(dir, "grandchild.pid")
	script := writeProbeScript(t,
		"( touch "+marker+"; sleep 60 ) &\n"+
			"echo $! > "+pidFile+"\n"+
			"sleep 60\n")

	restore := execProbeTimeout
	// Long enough that a loaded host still reaches the `echo $! > pidFile` line
	// before the group kill lands: a shorter window races the shell and fails
	// with "grandchild.pid was never written" rather than a real regression.
	execProbeTimeout = testsupport.Scale(2 * time.Second)
	t.Cleanup(func() { execProbeTimeout = restore })

	if err := runVersionProbe(script, nil); err == nil {
		t.Fatal("runVersionProbe() error = nil, want the timeout kill")
	}

	pid := parsePGID(t, readEventually(t, pidFile))
	// Signal 0 only probes for existence. The grandchild is not our child, so it
	// is never reaped by this process and cannot be mistaken for a zombie here.
	for deadline := time.Now().Add(testsupport.Scale(2 * time.Second)); time.Now().Before(deadline); {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("grandchild %d survived the probe timeout", pid)
}

func readEventually(t *testing.T, path string) string {
	t.Helper()
	for deadline := time.Now().Add(testsupport.Scale(2 * time.Second)); time.Now().Before(deadline); {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return string(data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s was never written", path)
	return ""
}

func parsePGID(t *testing.T, s string) int {
	t.Helper()
	var n int
	var seen bool
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
			seen = true
			continue
		}
		if seen {
			break
		}
	}
	if !seen {
		t.Fatalf("no pid in %q", s)
	}
	return n
}

func TestProbeProcessGroupChild(t *testing.T) {
	output := testsupport.ProbeGroupOutput()
	if output == "" {
		return
	}
	if err := os.WriteFile(output, fmt.Appendf(nil, "%d", syscall.Getpgrp()), 0o600); err != nil {
		t.Fatal(err)
	}
}
