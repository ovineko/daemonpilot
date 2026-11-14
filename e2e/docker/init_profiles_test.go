//go:build e2e_docker

package docker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/e2e/docker/internal/wire"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

type initProfile struct {
	name, pid1, ready string
	restart           bool
	flags             []string
	rollbackGap       string
}

func initProfiles() []initProfile {
	return []initProfile{
		{name: "systemd", pid1: "systemd", restart: true, ready: `s=$(systemctl is-system-running); [ "$s" = running ] || [ "$s" = degraded ]`, flags: []string{"--tmpfs", "/run", "--tmpfs", "/run/lock", "--cgroupns=host", "--volume", "/sys/fs/cgroup:/sys/fs/cgroup:rw", "--env", "container=docker"}},
		{name: "openrc", pid1: "busybox", restart: true, ready: `[ "$(cat /run/openrc/softlevel)" = default ]`, flags: []string{"--tmpfs", "/run"}},
		{name: "sysvinit", pid1: "init", ready: `runlevel | grep -q ' 2$'`, flags: []string{"--tmpfs", "/run"}},
		{name: "runit", pid1: "runit", restart: true, ready: `pgrep -x runsvdir`, rollbackGap: "sv up acknowledges a supervision request; it does not reject an invalid run script synchronously"},
		{name: "s6", pid1: "s6-svscan", restart: true, ready: `test -p /var/service/.s6-svscan/control`, rollbackGap: "s6-svc -u acknowledges a supervision request; it does not reject an invalid run script synchronously"},
		{name: "dinit", pid1: "dinit", restart: true, ready: `dinitctl list`},
		{name: "finit", pid1: "finit", restart: true, ready: `initctl runlevel | grep -q ' 2$'`, rollbackGap: "Finit reload acknowledges a configuration rescan; it does not guarantee synchronous rejection of a bad service stanza"},
		{name: "procd", pid1: "procd", restart: true, ready: `ubus call system board`, rollbackGap: "procd start accepts an asynchronous request; worker failure is not a synchronous apply error"},
		{name: "busybox", pid1: "busybox", ready: `test -d /proc/1`, flags: []string{"--tmpfs", "/run"}},
		{name: "crontab", pid1: "cron", ready: `test -f /var/run/crond.pid`, flags: []string{"--tmpfs", "/run"}},
	}
}

type initRig struct {
	*rig

	profile     initProfile
	step        string
	lastCommand commandResult
}

type commandResult struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
	Exit   int    `json:"exit"`
}

func dockerResult(t *testing.T, timeout time.Duration, args ...string) commandResult {
	t.Helper()
	return dockerCommand(t.Context(), timeout, args...)
}

func dockerCommand(parent context.Context, timeout time.Duration, args ...string) commandResult {
	ctx, cancel := context.WithTimeout(parent, testsupport.Scale(timeout))
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.WaitDelay = testsupport.Scale(time.Second)
	out, err := cmd.CombinedOutput()
	result := commandResult{Output: strings.TrimSpace(string(out))}
	if err != nil {
		result.Error = err.Error()
		result.Exit = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.Exit = exitErr.ExitCode()
		}
	}
	return result
}

func (r *initRig) evidence(step string, value any) {
	r.t.Helper()
	data, err := json.Marshal(map[string]any{"step": step, "profile": r.profile.name, "at": time.Now().UTC(), "evidence": value})
	check(r.t, err)
	r.t.Log(string(data))
	f, err := os.OpenFile(filepath.Join(r.dir, "init-evidence.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	check(r.t, err)
	_, err = f.Write(append(data, '\n'))
	check(r.t, err)
	check(r.t, f.Close())
}

func (r *initRig) cli(verb string, supported bool) commandResult {
	r.t.Helper()
	r.step = verb
	result := dockerResult(r.t, 45*time.Second, "exec", r.containerName, "/artifacts/v1", verb)
	r.lastCommand = result
	r.evidence(verb, result)
	if supported {
		if result.Exit != 0 {
			r.t.Fatalf("%s failed: %+v", verb, result)
		}
	} else if result.Exit == 0 || !strings.Contains(result.Output, "not supported") {
		r.t.Fatalf("%s must report not supported: %+v", verb, result)
	}
	return result
}

func (r *initRig) inspect() wire.InitObservation {
	r.t.Helper()
	var o wire.InitObservation
	check(r.t, json.Unmarshal([]byte(r.exec("/artifacts/v1", "inspect-init")), &o))
	return o
}

func (r *initRig) observe(step string) wire.InitObservation {
	r.t.Helper()
	r.step = step
	o := r.inspect()
	r.evidence(step, o)
	return o
}

func buildInitImage(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("images", name, "Dockerfile"))
	check(t, err)
	sum := sha256.Sum256(data)
	tag := fmt.Sprintf("daemonpilot-e2e/%s:%x", name, sum)
	contextDir := t.TempDir()
	check(t, os.WriteFile(filepath.Join(contextDir, "Dockerfile"), data, 0o600))
	result := dockerResult(t, 8*time.Minute, "build", "--tag", tag, contextDir)
	t.Logf("image=%s build=%s", tag, result.Output)
	if result.Exit != 0 {
		t.Fatalf("build %s: %+v", name, result)
	}
	return tag
}

func TestInitProfiles(t *testing.T) {
	requireDocker(t)
	artifacts := build(t)
	for _, p := range initProfiles() {
		t.Run(p.name, func(t *testing.T) {
			if p.name == "procd" && runtime.GOARCH != "amd64" {
				t.Skip("procd profile pins OpenWrt x86-64; other architectures are an explicit gap")
			}
			image := buildInitImage(t, p.name)
			for _, scenario := range initScenarios() {
				t.Run(scenario.id, func(t *testing.T) {
					r := &initRig{rig: newRig(t, artifacts, "update-staged"), profile: p}
					r.config.ServiceEnvironment = map[string]string{wire.ConfigEnv: "/state/config.json", wire.EnvironmentKey: "before"}
					startDocker(r.rig, profile{name: p.name, image: image, user: "0:0", imageEntrypoint: true, flags: p.flags}, scenario.id)
					t.Cleanup(func() {
						if t.Failed() {
							r.failureEvidence()
						}
					})
					r.bootReady()
					r.assertDetection()
					r.evidence("contract", map[string]string{"id": scenario.id, "contract": scenario.contract, "mutation": scenario.mutation})
					r.cli("install", true)
					s := r.freshWorker(time.Now().Add(-testsupport.Scale(wire.Deadline)), "1.0.0")
					r.assertInstalled(s)
					scenario.run(r, s)
				})
			}
		})
	}
}

func (r *initRig) bootReady() {
	r.t.Helper()
	r.step = "boot readiness"
	deadline := time.Now().Add(testsupport.Scale(30 * time.Second))
	var result commandResult
	for time.Now().Before(deadline) {
		result = dockerResult(r.t, 3*time.Second, "exec", r.containerName, "/bin/sh", "-c", r.profile.ready)
		if result.Exit == 0 {
			r.evidence("boot-ready", result)
			return
		}
		state := dockerResult(r.t, 3*time.Second, "inspect", "--format", "{{.State.Status}}", r.containerName)
		if state.Exit == 0 && (state.Output == "exited" || state.Output == "dead") {
			break
		}
		select {
		case <-r.t.Context().Done():
			r.t.Fatal(r.t.Context().Err())
		case <-time.After(testsupport.Scale(200 * time.Millisecond)):
		}
	}
	logs := dockerCommand(context.Background(), 5*time.Second, "logs", "--timestamps", r.containerName)
	state := dockerCommand(context.Background(), 5*time.Second, "inspect", "--format", "{{json .State}}", r.containerName)
	r.evidence("boot-failed", map[string]any{"probe": result, "logs": logs, "state": state})
	if reason := initFacilityGap(r.profile.name, logs.Output); reason != "" {
		r.t.Skip(reason)
	}
	r.t.Fatalf("init did not become ready: %+v; logs: %s", result, logs.Output)
}

func initFacilityGap(name, output string) string {
	if name != "systemd" && name != "finit" && name != "procd" {
		return ""
	}
	for line := range strings.SplitSeq(output, "\n") {
		lower := strings.ToLower(line)
		if (strings.Contains(lower, "cgroup") || strings.Contains(lower, "control group") || strings.Contains(lower, "mount")) &&
			(strings.Contains(lower, "permission denied") || strings.Contains(lower, "operation not permitted") || strings.Contains(lower, "read-only file system")) {
			return "host cannot boot " + name + " with the unprivileged profile: " + line + "; no --privileged retry is allowed"
		}
	}
	return ""
}

func (r *initRig) assertDetection() {
	r.t.Helper()
	o := r.observe("detection-capabilities")
	want := wire.Capabilities{Enable: true, Start: true, Status: true, RestartWatchdog: r.profile.restart}
	if o.UID != 0 || o.Name != r.profile.name || o.Capabilities != want || filepath.Base(o.PID1) != r.profile.pid1 {
		r.t.Fatalf("detection/PID 1/capabilities mismatch: got %+v want %s %+v PID1=%s", o, r.profile.name, want, r.profile.pid1)
	}
	if r.profile.name == "openrc" || r.profile.name == "busybox" {
		if !strings.Contains(o.PID1Command, "init") {
			r.t.Fatalf("BusyBox PID 1 must be the init applet: %+v", o)
		}
	}
}

func (r *initRig) freshWorker(after time.Time, version string) wire.Snapshot {
	r.t.Helper()
	var s wire.Snapshot
	r.await("fresh live worker "+version, wire.Deadline, func() bool {
		s = r.snapshot()
		return s.At.After(after) && time.Since(s.At) < time.Second && s.Version == version && s.PID > 1 && s.Runtime.WorkerPID == s.PID && r.alive(s.PID)
	})
	r.evidence("worker", s)
	return s
}

func (r *initRig) assertInstalled(s wire.Snapshot) {
	r.t.Helper()
	o := r.observe("installed")
	if o.State == nil || o.State.InitSystem != r.profile.name || o.State.ManifestPath != o.Manifest || !o.Files[o.Manifest] || s.Environment != "before" || s.Runtime.InitSystem != r.profile.name || s.Runtime.CanAutoRestart != r.profile.restart || s.Runtime.WatchdogPID <= 1 || s.Runtime.IsExecThrough {
		r.t.Fatalf("installed state/runtime/environment mismatch: %+v worker=%+v", o, s)
	}
}

func (r *initRig) reboot() time.Time {
	r.t.Helper()
	before := dockerCLI(r.t, "inspect", "--format", "{{.State.StartedAt}}", r.containerName)
	started := time.Now()
	result := dockerResult(r.t, 30*time.Second, "restart", "--time", "5", r.containerName)
	r.evidence("docker-restart", result)
	if result.Exit != 0 {
		r.t.Fatalf("reboot: %+v", result)
	}
	r.bootReady()
	after := dockerCLI(r.t, "inspect", "--format", "{{.State.StartedAt}}", r.containerName)
	r.evidence("reboot-generation", map[string]string{"before": before, "after": after})
	if before == after {
		r.t.Fatal("container did not reboot")
	}
	r.assertDetection()
	return started
}

func (r *initRig) absentFor(step string, duration time.Duration) {
	r.t.Helper()
	r.step = step
	deadline := time.Now().Add(testsupport.Scale(duration))
	for {
		o := r.inspect()
		if len(o.Processes) != 0 {
			r.evidence(step, o)
			r.t.Fatalf("service returned during %s: %v", step, o.Processes)
		}
		if time.Now().After(deadline) {
			r.evidence(step, map[string]any{"window": duration.String(), "observation": o})
			return
		}
		select {
		case <-r.t.Context().Done():
			r.t.Fatal(r.t.Context().Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (r *initRig) replacementWorker(after time.Time, previous wire.Snapshot, environment string) wire.Snapshot {
	r.t.Helper()
	var next wire.Snapshot
	r.await("replacement worker with environment "+environment, wire.Deadline, func() bool {
		next = r.snapshot()
		return next.At.After(after) && time.Since(next.At) < time.Second && next.PID != previous.PID && next.Version == previous.Version && next.Environment == environment && next.Runtime.WorkerPID == next.PID && r.alive(next.PID) && !r.alive(previous.PID)
	})
	r.evidence("replacement-worker", next)
	return next
}
