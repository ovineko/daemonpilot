//go:build e2e_docker

package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/e2e/docker/internal/wire"
)

type initScenario struct {
	id, contract, mutation string
	run                    func(*initRig, wire.Snapshot)
}

func initScenarios() []initScenario {
	return []initScenario{
		{"init-manifest-values", "manifest preserves watchdog arguments and working directory, and worker environment", "Restore the Finit cd wrapper from Q2 (cd plus a quoted literal path); restore unquoted Finit environment values or separate procd env calls", initManifestValues},
		{"init-lifecycle", "detected init installs and controls a real worker; status follows stop/start/restart", "In commands.go handleStop return nil before mgr.Stop", initLifecycle},
		{"init-persistence", "disable survives reboot; enable restores boot startup", "In busybox.go Enable omit the K95 to S95 rename; in sysvinit.go enableDebian omit update-rc.d", initPersistence},
		{"init-watchdog-sigkill", "the advertised supervisor replaces a SIGKILLed watchdog without operator intervention", "Remove Restart=always from systemdUnitTemplate; remove supervisor=supervise-daemon from openrcInitScriptTemplate", initWatchdogKill},
		{"init-staged-commit", "real-init staged update retains healthy v2 and consumes verified rollback state", "Remove the commit branch in watchdog.runProbation", initStagedCommit},
		{"init-staged-rollback", "unstable staged update restores and executes the verified v1", "Return without restoring in watchdog.rollbackStaged", initStagedRollback},
		{"init-uninstall", "uninstall removes manifest, registration, all service processes and named artifacts across reboot", "In systemd.Uninstall omit manifest removal; in commands.go omit removeInstallArtifacts; in runit.Stop restore asynchronous sv down", initUninstall},
		{"init-update-config", "new manifest environment reaches a replacement worker", "In commands.go handleUpdateConfig pass oldContent to mgr.WriteConfig", initUpdateConfig},
		{"init-config-rollback", "a synchronous failed apply restores the previous manifest and running environment", "In commands.go replace writeConfigForRollback with a no-op; for crontab omit backup removal after failed WriteConfig", initConfigRollback},
	}
}

func initLifecycle(r *initRig, s wire.Snapshot) {
	caps := r.inspect().Capabilities
	out := r.cli("status", caps.Status)
	if caps.Status && !strings.Contains(out.Output, "Status: running") {
		r.t.Fatal("status did not report running")
	}
	r.cli("stop", caps.Start)
	if !caps.Start {
		r.cli("start", false)
		r.cli("restart", false)
		return
	}
	r.await("all service processes stopped", wire.Deadline, func() bool { return len(r.inspect().Processes) == 0 })
	r.absentFor("stopped", 3*time.Second)
	out = r.cli("status", caps.Status)
	if caps.Status && !strings.Contains(out.Output, "Status: stopped") {
		r.t.Fatal("status did not report stopped")
	}
	started := time.Now()
	r.cli("start", true)
	next := r.freshWorker(started, s.Version)
	if next.PID == s.PID {
		r.t.Fatal("start did not replace worker")
	}
	started = time.Now()
	r.cli("restart", true)
	last := r.replacementWorker(started, next, "before")
	if last.PID == next.PID || r.alive(next.PID) {
		r.t.Fatal("restart left the previous worker alive")
	}
}

func initPersistence(r *initRig, s wire.Snapshot) {
	caps := r.inspect().Capabilities
	r.cli("disable", caps.Enable)
	if !caps.Enable {
		r.cli("enable", false)
		return
	}
	r.reboot()
	r.absentFor("disabled-after-reboot", 10*time.Second)
	r.cli("enable", true)
	r.freshWorker(r.reboot(), s.Version)
}

func initWatchdogKill(r *initRig, s wire.Snapshot) {
	if !r.inspect().Capabilities.RestartWatchdog {
		reason := "backend advertises CanRestartWatchdog=false; no init restart guarantee (shell respawn is not init supervision)"
		r.evidence("watchdog-restart-gap", reason)
		r.t.Skip(reason)
	}
	started := time.Now()
	r.kill(s.Runtime.WatchdogPID)
	r.evidence("SIGKILL", map[string]int{"watchdog_pid": s.Runtime.WatchdogPID})
	var next wire.Snapshot
	r.await("init restarted watchdog", wire.Deadline, func() bool {
		next = r.snapshot()
		return next.At.After(started) && next.Runtime.WatchdogPID != s.Runtime.WatchdogPID && next.PID != s.PID && time.Since(next.At) < time.Second
	})
	if next.Runtime.IsExecThrough || !r.alive(next.Runtime.WatchdogPID) || r.alive(s.Runtime.WatchdogPID) {
		r.t.Fatalf("wrong restart owner: %+v", next)
	}
	r.await("old worker gone", wire.Deadline, func() bool { return !r.alive(s.PID) })
	r.evidence("init-restarted-watchdog", next)
}

func (r *initRig) offerStaged(s wire.Snapshot, id string) {
	r.t.Helper()
	r.offer(id, false)
	r.await("verified displaced binary", wire.Deadline, func() bool { return r.inspect().RollbackVerified })
	r.observe("verified-rollback")
	r.await("updated watchdog launch", wire.Deadline, func() bool {
		for _, record := range r.records("watchdog-start") {
			if record.Version == variant(r.t, id).Version {
				return true
			}
		}
		return false
	})
	r.offer("", false)
	starts := r.records("watchdog-start")
	r.evidence("staged-watchdog-launches", starts)
	last := starts[len(starts)-1]
	if r.profile.restart && last.PID != s.Runtime.WatchdogPID {
		r.t.Fatalf("current restart-capable path must direct-exec with same PID: %+v", starts)
	}
	if !r.profile.restart && last.PID == s.Runtime.WatchdogPID {
		r.t.Fatalf("fallback must launch exec-through child: %+v", starts)
	}
}

func initStagedCommit(r *initRig, s wire.Snapshot) {
	started := time.Now()
	r.offerStaged(s, "staged")
	next := r.freshWorker(started, variant(r.t, "staged").Version)
	if next.Runtime.IsExecThrough == r.profile.restart || next.Runtime.WatchdogVersion != "2" {
		r.t.Fatalf("wrong staged supervision: %+v", next)
	}
	r.await("commit consumes copy and digest", wire.StagedStable+3*time.Second, func() bool {
		o := r.inspect()
		return !o.Files["/state/"+wire.Name+".rollback"] && o.State != nil && o.State.RollbackDigest == ""
	})
	r.assertBinary("staged")
	current := r.freshWorker(time.Now(), next.Version)
	if current.PID != next.PID {
		r.t.Fatal("commit replaced healthy worker")
	}
	r.observe("committed")
}

func initStagedRollback(r *initRig, s wire.Snapshot) {
	started := time.Now()
	r.offerStaged(s, "unstable")
	r.await("unstable worker actually launched", wire.Deadline, func() bool {
		for _, record := range r.records("worker-start") {
			if record.Version == variant(r.t, "unstable").Version {
				return true
			}
		}
		return false
	})
	var next wire.Snapshot
	r.await("verified v1 rollback executes", wire.RollbackWindow+3*time.Second, func() bool {
		next = r.snapshot()
		return next.At.After(started) && next.PID != s.PID && next.Version == s.Version && time.Since(next.At) < time.Second
	})
	r.evidence("restored-worker", next)
	r.assertBinary("v1")
	o := r.observe("rolled-back")
	if !r.alive(next.PID) || o.Files["/state/"+wire.Name+".rollback"] || o.State == nil || o.State.RollbackDigest != "" {
		r.t.Fatal("rollback did not consume copy/digest and retain a live worker")
	}
}

func (r *initRig) assertBinary(id string) {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.artifacts, id))
	check(r.t, err)
	sum := sha256.Sum256(data)
	o := r.inspect()
	if o.BinaryDigest != hex.EncodeToString(sum[:]) {
		r.t.Fatalf("installed binary differs from %s: %+v", id, o)
	}
}

func initUninstall(r *initRig, _ wire.Snapshot) {
	// Seed leftovers only after startup cleanup has finished.
	r.cli("stop", true)
	r.await("stopped before seeding leftovers", wire.Deadline, func() bool { return len(r.inspect().Processes) == 0 })
	r.cli("start", true)
	r.freshWorker(time.Now(), "1.0.0")
	for _, suffix := range []string{".new", ".restore", ".rollback", ".rollback.invalid"} {
		check(r.t, os.WriteFile(filepath.Join(r.dir, wire.Name+suffix), []byte("stale"), 0o600))
	}
	manifest := r.inspect().Manifest
	r.exec("cp", manifest, manifest+".backup")
	check(r.t, os.WriteFile(filepath.Join(r.dir, "unrelated.keep"), []byte("keep"), 0o600))
	r.observe("leftovers-seeded")
	r.cli("uninstall", true)
	r.await("uninstall stopped processes", wire.Deadline, func() bool { return len(r.inspect().Processes) == 0 })
	r.assertUninstalled()
	r.reboot()
	r.absentFor("uninstalled-after-reboot", 10*time.Second)
	r.assertUninstalled()
}

func (r *initRig) assertUninstalled() {
	r.t.Helper()
	o := r.observe("uninstalled")
	for path, present := range o.Files {
		if present {
			r.t.Fatalf("uninstall left %s: %+v", path, o)
		}
	}
	if o.State != nil || len(o.Processes) != 0 {
		r.t.Fatalf("uninstall left service state/processes: %+v", o)
	}
	data, err := os.ReadFile(filepath.Join(r.dir, "unrelated.keep"))
	check(r.t, err)
	if string(data) != "keep" {
		r.t.Fatal("uninstall damaged unrelated file")
	}
	var result commandResult
	defer func() { r.evidence("init-registration-absent", result) }()
	r.await("init registration removed", wire.Deadline, func() bool {
		result = dockerResult(r.t, 5*time.Second, "exec", r.containerName, "/bin/sh", "-c", registrationQuery(r.profile.name))
		return result.Exit == 0
	})
}

func registrationQuery(name string) string {
	switch name {
	case "systemd":
		return `test "$(systemctl show -p LoadState --value daemonpilot-e2e.service)" = not-found`
	case "openrc":
		return `! rc-service --exists daemonpilot-e2e`
	case "sysvinit":
		return `! service --status-all 2>&1 | grep -F daemonpilot-e2e`
	case "runit":
		return `! sv status daemonpilot-e2e`
	case "s6":
		return `! s6-svstat /var/service/daemonpilot-e2e`
	case "dinit":
		return `out=$(dinitctl list) || exit 98; ! printf '%s\n' "$out" | grep -F daemonpilot-e2e`
	case "finit":
		return `out=$(initctl status) || exit 98; ! printf '%s\n' "$out" | grep -F daemonpilot-e2e`
	case "procd":
		return `out=$(ubus call service list) || exit 98; ! printf '%s\n' "$out" | grep -F daemonpilot-e2e`
	case "busybox":
		return `test ! -e /etc/init.d/S95daemonpilot-e2e && test ! -e /etc/init.d/K95daemonpilot-e2e && test ! -e /etc/init.d/daemonpilot-e2e.disabled && test ! -e /var/run/daemonpilot-e2e.pid`
	case "crontab":
		return `! crontab -l | grep -F daemonpilot-e2e`
	default:
		return "exit 99"
	}
}

func initUpdateConfig(r *initRig, s wire.Snapshot) {
	r.config.ServiceEnvironment[wire.EnvironmentKey] = "after"
	check(r.t, wire.WriteJSON(filepath.Join(r.dir, "config.json"), r.config))
	started := time.Now()
	r.cli("update-config", true)
	next := r.replacementWorker(started, s, "after")
	if next.PID == s.PID || next.Environment != "after" {
		r.t.Fatalf("new environment not applied: %+v", next)
	}
	if r.inspect().Files[r.inspect().Manifest+".backup"] {
		r.t.Fatal("successful apply retained backup")
	}
	r.observe("config-applied")
}

func initConfigRollback(r *initRig, s wire.Snapshot) {
	if r.profile.rollbackGap != "" {
		r.evidence("config-rollback-gap", r.profile.rollbackGap)
		r.t.Skip(r.profile.rollbackGap)
	}
	before := r.inspect()
	r.config.ServiceEnvironment[wire.EnvironmentKey] = "rejected"
	r.config.FailConfigApply = true
	check(r.t, wire.WriteJSON(filepath.Join(r.dir, "config.json"), r.config))
	started := time.Now()
	r.step = "failed-config-apply"
	result := dockerResult(r.t, 45*time.Second, "exec", r.containerName, "/artifacts/v1", "update-config")
	r.lastCommand = result
	r.evidence("failed-config-apply", result)
	if result.Exit == 0 || !strings.Contains(result.Output, "71") && !strings.Contains(strings.ToLower(result.Output), "config") {
		r.t.Fatalf("expected synchronous apply rejection: %+v", result)
	}
	after := r.observe("config-restored")
	if after.Content != before.Content || after.ManifestContent != before.ManifestContent || after.Files[after.Manifest+".backup"] {
		r.t.Fatal("failed apply did not restore manifest and remove backup")
	}
	next := r.replacementWorker(started, s, "before")
	if next.PID == s.PID || next.Environment != "before" {
		r.t.Fatalf("failed apply did not restore running environment: %+v", next)
	}
}

func TestInitRegistry(t *testing.T) {
	guide, err := os.ReadFile("../../docs/guides/testing.md")
	check(t, err)
	for _, s := range initScenarios() {
		if s.contract == "" || s.mutation == "" || s.run == nil || !strings.Contains(string(guide), "`"+s.id+"`") {
			t.Errorf("incomplete init contract %s", s.id)
		}
	}
	for _, p := range initProfiles() {
		data, err := os.ReadFile(filepath.Join("images", p.name, "Dockerfile"))
		check(t, err)
		for line := range strings.SplitSeq(string(data), "\n") {
			if strings.HasPrefix(line, "FROM ") && (!strings.Contains(line, ":") || strings.Contains(line, ":latest")) {
				t.Errorf("%s: unpinned base %s", p.name, line)
			}
		}
	}
}

func initManifestValues(r *initRig, s wire.Snapshot) {
	value := "  quotes '\" $HOME `printf injected` \\ unicode \u754c  "
	if r.profile.name != "crontab" {
		value += "%n"
	}
	r.config.ServiceArguments = []string{"", "hello world", "--", `quote'"$HOME`, "tail"}
	r.config.ServiceEnvironment[wire.EnvironmentKey] = value
	check(r.t, wire.WriteJSON(filepath.Join(r.dir, "config.json"), r.config))
	started := time.Now()
	r.cli("update-config", true)
	r.replacementWorker(started, s, value)

	want := append([]string{"run"}, r.config.ServiceArguments...)
	found := false
	for _, record := range r.records("watchdog-start") {
		if !record.At.After(started) {
			continue
		}
		found = true
		if record.Directory != r.config.Dir || !slices.Equal(record.Arguments, want) {
			r.t.Fatalf("manifest changed watchdog launch: got cwd=%q argv=%q; want cwd=%q argv=%q", record.Directory, record.Arguments, r.config.Dir, want)
		}
	}
	if !found {
		r.t.Fatal("missing replacement watchdog launch evidence")
	}
	r.observe("manifest-values-preserved")
}
