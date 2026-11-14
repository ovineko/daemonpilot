//go:build e2e_docker

package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/e2e/docker/internal/wire"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

type expectation struct {
	doc, contract, mutation string
	run                     func(*rig, wire.Snapshot)
}

var expectations = map[string]expectation{
	"lifecycle-crash-restart":            {"architecture.md#the-restart-loop", "exit 3 restarts distinct workers after 1s then 2s backoff", "Remove the ordinary-failure restart policy in watchdog.New", crashRestart},
	"lifecycle-readiness-resets-backoff": {"readiness.md#what-it-changes", "readiness resets an accumulated backoff; next crash waits 1s", "Remove supervisor.MarkStable in the ready handler", readinessReset},
	"lifecycle-health-restart":           {"health-checks.md#how-it-runs", "three failed checks trigger restart; dump precedes replacement", "Remove supervisor.Restart(false) in runHealthChecks", healthRestart},
	"lifecycle-exit-permanent":           {"watchdog-worker-protocol.md#exit-codes-are-requests", "exit 100 stops watchdog with code 0 and exactly one worker launch", "Remove ExitPermanent from PreventExitCodes", permanentExit},
	"lifecycle-exit-respawn":             {"watchdog-worker-protocol.md#exit-codes-are-requests", "exit 101 loads the disk image, retaining Unix PID but changing watchdog start time", "Remove needsExecRestart.Store(true) from the exit-101 branch", respawnExit},
	"lifecycle-docker-stop":              {"architecture.md#worker-process-trees", "SIGTERM completes worker and grandchild cleanup inside grace, exit 0", "Remove cancel() in the signal case of process.SetupSignalHandler", gracefulStop},
	"lifecycle-watchdog-sigkill":         {"architecture.md#orphan-prevention", "kernel parent-death SIGTERM stops the worker; an unmonitored grandchild survives", "Ignore SIGTERM in process.SetupSignalHandler", watchdogKill},
	"update-fast":                        {"self-update.md#fast-path-and-staged-path", "v2 worker, same resident watchdog, restart resets accumulated backoff", "Change supervisor.Restart(true) to Restart(false) in applyOfferedUpdate", fastUpdate},
	"update-staged":                      {"self-update.md#rollback", "new watchdog runs v2 and commits the verified rollback after stability", "Re-add the ExecThrough exclusion in stagedProbationPath", stagedUpdate},
	"update-staged-rollback":             {"self-update.md#rollback-integrity", "unstable worker fails probation; verified v1 is restored within the rollback window", "Skip stopRun in the exec-through branch of rollbackStaged", stagedRollback},
	"update-bad-signature":               {"http-releases.md#signing-and-keys", "wrong signing key is rejected, v1 survives, three failures exhaust budget", "Bypass VerifySignature failure in updater.DownloadAndVerify", badSignature},
	"update-selftest-fails":              {"self-update.md#retry-budget", "unsuccessful selftest rejects the build and leaves v1 running", "Bypass the !selftestResult.OK branch in applyOfferedUpdate", selftestFailure},
	"update-retry-budget":                {"self-update.md#retry-budget", "exactly three failed downloads despite further polls; a newer good offer still applies", "Remove the attempt/time gate in performUpdateCheck", retryBudget},
	"update-accept-version":              {"self-update.md#application-update-policy", "policy is exercised across repeated offers with zero downloads", "Bypass AcceptVersion in performUpdateCheck", acceptVersion},
}

func scenarioIDs() []string {
	ids := make([]string, 0, len(expectations))
	for id := range expectations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func TestExpectationRegistry(t *testing.T) {
	guide, err := os.ReadFile("../../docs/guides/testing.md")
	check(t, err)
	for id, e := range expectations {
		if e.run == nil || e.contract == "" || e.mutation == "" {
			t.Errorf("incomplete expectation %s", id)
		}
		if !strings.Contains(string(guide), "`"+id+"`") {
			t.Errorf("testing guide omits %s", id)
		}
		parts := strings.Split(e.doc, "#")
		data, err := os.ReadFile(filepath.Join("../../docs/guides", parts[0]))
		check(t, err)
		anchorFound := false
		for line := range strings.SplitSeq(string(data), "\n") {
			if strings.HasPrefix(line, "#") {
				anchor := strings.ReplaceAll(strings.ToLower(strings.TrimLeft(line, "# ")), " ", "-")
				if len(parts) == 2 && anchor == parts[1] {
					anchorFound = true
				}
			}
		}
		if !anchorFound {
			t.Errorf("%s: missing contract heading %s", id, e.doc)
		}
	}
}

func runScenario(r *rig, id string) {
	r.t.Helper()
	e := expectations[id]
	r.t.Logf("%s: %s; contract docs/guides/%s; negative control: %s", id, e.contract, e.doc, e.mutation)
	initial := r.workerAfter(0, variant(r.t, "v1").Version)
	r.t.Logf("runtime: init_system=%q auto_restart=%t watchdog_pid=%d", initial.Runtime.InitSystem, initial.Runtime.CanAutoRestart, initial.Runtime.WatchdogPID)
	if initial.Runtime.WatchdogPID <= 0 {
		r.t.Fatal("missing watchdog IPC identity")
	}
	if r.container && r.noInit {
		wantPID1 := id != "lifecycle-watchdog-sigkill"
		if (initial.Runtime.WatchdogPID == 1) != wantPID1 || initial.Runtime.CanAutoRestart {
			r.t.Fatalf("unexpected container supervision: %+v", initial.Runtime)
		}
	}
	e.run(r, initial)
}

func crash(r *rig, s wire.Snapshot, want time.Duration) wire.Snapshot {
	r.t.Helper()
	r.command(s.PID, "exit", 3)
	next := r.workerAfter(s.PID, s.Version)
	if next.Runtime.CurrentBackoff != want || next.Runtime.RestartCount != s.Runtime.RestartCount+1 {
		r.t.Fatalf("restart: backoff/count = %s/%d, want %s/%d", next.Runtime.CurrentBackoff, next.Runtime.RestartCount, want, s.Runtime.RestartCount+1)
	}
	exits, starts := r.records("exit-request"), r.records("worker-start")
	if len(exits) == 0 || len(starts) < 2 {
		r.t.Fatal("missing crash observations")
	}
	delay := starts[len(starts)-1].At.Sub(exits[len(exits)-1].At)
	if delay < want-100*time.Millisecond {
		r.t.Fatalf("restart delay %s < expected backoff %s", delay, want)
	}
	if next.Runtime.WatchdogPID != s.Runtime.WatchdogPID {
		r.t.Fatal("worker crash replaced watchdog")
	}
	return next
}

func crashRestart(r *rig, s wire.Snapshot) { s = crash(r, s, time.Second); crash(r, s, 2*time.Second) }

func readinessReset(r *rig, s wire.Snapshot) {
	s = crash(r, s, time.Second)
	s = crash(r, s, 2*time.Second)
	r.command(s.PID, "ready", 0)
	await(r.t, "readiness consumed by watchdog", wire.Deadline, func() bool {
		data, _ := os.ReadFile(filepath.Join(r.dir, "readiness-count"))
		return len(r.records("ready")) == 1 && string(data) == "1" && r.logContains("worker signaled readiness")
	})
	s = r.snapshot()
	data, err := os.ReadFile(filepath.Join(r.dir, "readiness-count"))
	check(r.t, err)
	if string(data) != "1" {
		r.t.Fatalf("readiness count %q, want 1", data)
	}
	crash(r, s, time.Second)
}

func healthRestart(r *rig, s wire.Snapshot) {
	check(r.t, wire.Write(filepath.Join(r.dir, "health-hang"), []byte("hang")))
	next := r.workerAfter(s.PID, s.Version)
	check(r.t, os.Remove(filepath.Join(r.dir, "health-hang")))
	dumps := r.records("dump")
	starts := r.records("worker-start")
	if len(dumps) != 1 || dumps[0].PID != s.PID || !dumps[0].At.Before(starts[len(starts)-1].At) {
		r.t.Fatalf("dump before restart: %+v", dumps)
	}
	data, err := os.ReadFile(filepath.Join(r.dir, dumps[0].Value))
	check(r.t, err)
	if !strings.HasPrefix(dumps[0].Value, "goroutine-dump-") || !strings.Contains(string(data), "goroutine") {
		r.t.Fatal("missing worker goroutine dump")
	}
	if !r.logContains(`"failures":3`) || next.Runtime.CurrentBackoff != time.Second {
		r.t.Fatalf("health threshold/backoff incorrect: %+v", next)
	}
}

func permanentExit(r *rig, s wire.Snapshot) {
	r.command(s.PID, "exit", 100)
	r.assertExit(0)
	if len(r.records("worker-start")) != 1 {
		r.t.Fatal("permanent stop restarted a worker")
	}
}

func respawnExit(r *rig, s wire.Snapshot) {
	copyBinary(r, "fast")
	before := r.records("watchdog-start")
	r.command(s.PID, "exit", 101)
	next := r.workerAfter(s.PID, variant(r.t, "fast").Version)
	after := r.records("watchdog-start")
	if len(before) != 1 || len(after) != 2 || after[1].PID != before[0].PID || !after[1].At.After(before[0].At) || next.Runtime.WatchdogPID != s.Runtime.WatchdogPID {
		r.t.Fatalf("Unix exec identity: before=%+v after=%+v worker=%+v", before, after, next)
	}
}

func copyBinary(r *rig, id string) {
	data, err := os.ReadFile(filepath.Join(r.artifacts, id))
	check(r.t, err)
	path := filepath.Join(r.dir, wire.Name+".replacement")
	check(r.t, os.WriteFile(path, data, 0o700))
	check(r.t, os.Rename(path, filepath.Join(r.dir, wire.Name)))
}

func spawnGrandchild(r *rig, s wire.Snapshot) int {
	r.command(s.PID, "grandchild", 0)
	await(r.t, "grandchild ready", wire.Deadline, func() bool { return len(r.records("grandchild-start")) == 1 })
	child := r.records("grandchild-start")[0]
	if child.Parent != s.PID || child.PID == s.PID || !r.alive(child.PID) {
		r.t.Fatalf("grandchild identity: %+v", child)
	}
	return child.PID
}

func gracefulStop(r *rig, s wire.Snapshot) {
	child := spawnGrandchild(r, s)
	started := time.Now()
	r.stop()
	r.assertExit(0)
	if elapsed := time.Since(started); elapsed >= 6*time.Second {
		r.t.Fatalf("shutdown reached Docker kill timeout: %s", elapsed)
	}
	for event, pid := range map[string]int{"worker-stop": s.PID, "grandchild-stop": child} {
		records := r.records(event)
		if len(records) != 1 || records[0].PID != pid || records[0].At.Sub(started) > wire.Grace {
			r.t.Fatalf("%s must complete within %s: %+v", event, wire.Grace, records)
		}
	}
	if len(r.records("worker-start")) != 1 {
		r.t.Fatal("shutdown restarted worker")
	}
}

func watchdogKill(r *rig, s wire.Snapshot) {
	child := spawnGrandchild(r, s)
	r.kill(s.Runtime.WatchdogPID)
	await(r.t, "kernel parent-death stops worker", 2*time.Second, func() bool { return !r.alive(s.PID) })
	stops := r.records("worker-stop")
	if len(stops) != 1 || stops[0].PID != s.PID || !r.alive(child) || len(r.records("grandchild-stop")) != 0 {
		r.t.Fatalf("parent-death evidence: worker=%+v grandchild=%d alive=%t", stops, child, r.alive(child))
	}
	r.kill(child)
}

func fastUpdate(r *rig, s wire.Snapshot) {
	s = crash(r, s, time.Second)
	s = crash(r, s, 2*time.Second)
	r.offer("fast", false)
	next := r.workerAfter(s.PID, variant(r.t, "fast").Version)
	if next.Runtime.WatchdogPID != s.Runtime.WatchdogPID || len(r.records("watchdog-start")) != 1 || next.Runtime.CurrentBackoff != 0 {
		r.t.Fatalf("fast update did not preserve watchdog/reset backoff: %+v", next)
	}
}

func verifiedRollback(r *rig) {
	path := filepath.Join(r.dir, wire.Name+".rollback")
	await(r.t, "recorded rollback digest", wire.Deadline, func() bool { return daemonpilot.VerifyRollbackBinary(r.dir, wire.Name, path) == nil })
	data, err := os.ReadFile(path)
	check(r.t, err)
	want, err := os.ReadFile(filepath.Join(r.artifacts, "v1"))
	check(r.t, err)
	if sha256.Sum256(data) != sha256.Sum256(want) {
		r.t.Fatal("rollback does not contain displaced v1")
	}
}

func stagedUpdate(r *rig, s wire.Snapshot) {
	r.offer("staged", false)
	verifiedRollback(r)
	next := r.workerAfter(s.PID, variant(r.t, "staged").Version)
	if s.Runtime.CanAutoRestart {
		starts := r.records("watchdog-start")
		if next.Runtime.WatchdogPID != s.Runtime.WatchdogPID || next.Runtime.IsExecThrough || len(starts) != 2 || !starts[1].At.After(starts[0].At) {
			r.t.Fatalf("expected direct exec on restart-capable native host: %+v starts=%+v", next, starts)
		}
	} else if !next.Runtime.IsExecThrough || next.Runtime.WatchdogPID == s.Runtime.WatchdogPID {
		r.t.Fatalf("expected new exec-through watchdog: %+v", next)
	}
	if next.Runtime.WatchdogVersion != "2" {
		r.t.Fatalf("wrong resident version: %+v", next)
	}
	if !r.alive(s.Runtime.WatchdogPID) {
		r.t.Fatal("proxy watchdog exited")
	}
	r.command(next.PID, "ready", 0)
	await(r.t, "staged stability commit removes rollback", wire.StagedStable+3*time.Second, func() bool { _, err := os.Stat(filepath.Join(r.dir, wire.Name+".rollback")); return os.IsNotExist(err) })
	current := r.snapshot()
	if current.PID != next.PID || current.Version != next.Version || time.Since(current.At) >= time.Second || !r.alive(next.PID) {
		r.t.Fatalf("commit must retain the healthy updated worker: %+v", current)
	}
	installed, err := os.ReadFile(filepath.Join(r.dir, wire.Name))
	check(r.t, err)
	staged, err := os.ReadFile(filepath.Join(r.artifacts, "staged"))
	check(r.t, err)
	if sha256.Sum256(installed) != sha256.Sum256(staged) {
		r.t.Fatal("rollback removal did not commit the staged binary")
	}
	state, err := daemonpilot.LoadState(r.dir, wire.Name)
	check(r.t, err)
	if state.RollbackDigest != "" {
		r.t.Fatal("commit retained rollback digest")
	}
}

func stagedRollback(r *rig, s wire.Snapshot) {
	r.offer("unstable", false)
	verifiedRollback(r)
	await(r.t, "unstable worker launched", wire.Deadline, func() bool {
		for _, record := range r.records("worker-start") {
			if record.Version == variant(r.t, "unstable").Version {
				return true
			}
		}
		return false
	})
	started := time.Now()
	r.offer("", false)
	var next wire.Snapshot
	await(r.t, "rollback restores a running v1", wire.RollbackWindow+2*time.Second, func() bool {
		next = r.snapshot()
		return next.PID != s.PID && next.Version == s.Version && time.Since(next.At) < time.Second
	})
	if time.Since(started) > wire.RollbackWindow+2*time.Second {
		r.t.Fatal("rollback missed deadline")
	}
	data, err := os.ReadFile(filepath.Join(r.dir, wire.Name))
	check(r.t, err)
	want, err := os.ReadFile(filepath.Join(r.artifacts, "v1"))
	check(r.t, err)
	actualHash, wantHash := sha256.Sum256(data), sha256.Sum256(want)
	if actualHash != wantHash {
		r.t.Fatalf("restored digest %s, want %s", hex.EncodeToString(actualHash[:]), hex.EncodeToString(wantHash[:]))
	}
	if !r.logContains("restored previous binary") {
		r.t.Fatal("missing verified rollback outcome")
	}
}

func rejected(r *rig, s wire.Snapshot, id, reason string, wrong bool) {
	r.offer(id, wrong)
	await(r.t, "failed offers exhaust retry budget", wire.Deadline, func() bool {
		if current := r.snapshot(); current.PID != s.PID || current.Version != s.Version {
			r.t.Fatalf("rejected offer changed worker: %+v", current)
		}
		return r.logContains("update retry budget exhausted")
	})
	if !r.logContains(reason) {
		r.t.Fatalf("missing rejection reason %q", reason)
	}
	c := r.counts()
	if c.Downloads[id] != wire.RetryAttempts {
		r.t.Fatalf("download attempts %d, want %d", c.Downloads[id], wire.RetryAttempts)
	}
	before := c.Manifests[id]
	await(r.t, "continued polling without more downloads", wire.Deadline, func() bool {
		c = r.counts()
		if c.Downloads[id] != wire.RetryAttempts {
			r.t.Fatalf("retry budget exceeded: %+v", c)
		}
		if current := r.snapshot(); current.PID != s.PID || current.Version != s.Version {
			r.t.Fatalf("rejected offer changed worker: %+v", current)
		}
		return c.Manifests[id] >= before+8
	})
}

func badSignature(r *rig, s wire.Snapshot) {
	rejected(r, s, "fast", "signature verification failed", true)
}
func selftestFailure(r *rig, s wire.Snapshot) { rejected(r, s, "broken", "selftest failed", false) }
func retryBudget(r *rig, s wire.Snapshot) {
	rejected(r, s, "broken", "selftest failed", false)
	r.offer("newer", false)
	next := r.workerAfter(s.PID, variant(r.t, "newer").Version)
	if next.Runtime.WatchdogPID != s.Runtime.WatchdogPID {
		r.t.Fatal("new version did not use resident retry state")
	}
}

func acceptVersion(r *rig, s wire.Snapshot) {
	r.offer("fast", false)
	await(r.t, "policy rejection after repeated discovery", wire.Deadline, func() bool {
		c := r.counts()
		if c.Downloads["fast"] != 0 {
			r.t.Fatalf("policy rejection downloaded %d binaries", c.Downloads["fast"])
		}
		return c.Manifests["fast"] >= 8 && r.logContains("fixture version policy")
	})
	if current := r.snapshot(); current.PID != s.PID || current.Version != s.Version {
		r.t.Fatalf("policy changed worker: %+v", current)
	}
	if r.logContains("update retry budget exhausted") {
		r.t.Fatal("policy rejection consumed retry budget")
	}
}
