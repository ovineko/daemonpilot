//go:build e2e_docker

package docker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/e2e/docker/internal/releasehost"
	"github.com/ovineko/daemonpilot/e2e/docker/internal/wire"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

type rig struct {
	t                        *testing.T
	dir, releases, artifacts string
	config                   wire.Config
	containerName            string
	container                bool
	noInit                   bool
	exec                     func(...string) string
	stop                     func()
	exit                     func() (int, bool)
	kill                     func(int)
	alive                    func(int) bool
}

func build(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, v := range wire.Variants() {
		flags := "-X github.com/ovineko/daemonpilot/pkg/daemonpilot.Version=" + v.Version + " -X github.com/ovineko/daemonpilot/pkg/daemonpilot.WatchdogVersion=" + v.WatchdogVersion
		buildCommand(t, "-ldflags", flags, "-o", filepath.Join(dir, v.ID), "./e2e/docker/fixture")
	}
	buildCommand(t, "-o", filepath.Join(dir, "relserver"), "./e2e/docker/relserver")
	return dir
}

func buildCommand(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testsupport.Scale(2*time.Minute))
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", append([]string{"build", "-tags", "e2e_docker"}, args...)...)
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
}

func newRig(t *testing.T, artifacts, id string) *rig {
	t.Helper()
	r := &rig{t: t, dir: t.TempDir(), releases: t.TempDir(), artifacts: artifacts}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	check(t, err)
	_, wrong, err := ed25519.GenerateKey(rand.Reader)
	check(t, err)
	r.config = wire.Config{Dir: r.dir, PublicKey: hex.EncodeToString(pub), Stable: time.Minute, DisableParentMonitor: id == "lifecycle-watchdog-sigkill"}
	if strings.HasPrefix(id, "update-staged") {
		r.config.Stable = wire.StagedStable
	}
	if id == "update-accept-version" {
		r.config.RejectVersion = variant(t, "fast").Version
	}
	data, err := os.ReadFile(filepath.Join(artifacts, "v1"))
	check(t, err)
	check(t, os.WriteFile(filepath.Join(r.dir, wire.Name), data, 0o700))
	check(t, os.WriteFile(filepath.Join(r.releases, "key"), key.Seed(), 0o600))
	check(t, os.WriteFile(filepath.Join(r.releases, "wrong-key"), wrong.Seed(), 0o600))
	r.offer("", false)
	t.Cleanup(func() {
		if t.Failed() {
			for _, name := range []string{"events.jsonl", "logs.jsonl"} {
				data, _ := os.ReadFile(filepath.Join(r.dir, name))
				t.Logf("%s:\n%s", name, data)
			}
			data, _ := os.ReadFile(filepath.Join(r.releases, "counts.json"))
			t.Logf("release requests: %s", data)
		}
	})
	return r
}

func variant(t *testing.T, id string) wire.Variant {
	t.Helper()
	v, ok := wire.FindVariant(id)
	if !ok {
		t.Fatalf("unknown variant %q", id)
	}
	return v
}

func (r *rig) offer(id string, wrong bool) {
	r.t.Helper()
	check(r.t, wire.WriteJSON(filepath.Join(r.releases, "offer.json"), releasehost.Offer{ID: id, WrongKey: wrong}))
}

func (r *rig) command(pid int, action string, code int) {
	r.t.Helper()
	check(r.t, wire.WriteJSON(filepath.Join(r.dir, "command.json"), wire.Command{PID: pid, Action: action, Code: code}))
}

func (r *rig) snapshot() wire.Snapshot {
	var s wire.Snapshot
	readJSON(r.t, filepath.Join(r.dir, "worker.json"), &s)
	return s
}

func (r *rig) workerAfter(pid int, version string) wire.Snapshot {
	r.t.Helper()
	var s wire.Snapshot
	await(r.t, "fresh worker "+version, wire.Deadline, func() bool {
		s = r.snapshot()
		return s.PID > 0 && s.PID != pid && s.Version == version && s.Runtime.WorkerPID == s.PID && time.Since(s.At) < time.Second
	})
	return s
}

func (r *rig) records(event string) []wire.Record {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, "events.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	check(r.t, err)
	var records []wire.Record
	lines := strings.Split(string(data), "\n")
	for _, line := range lines[:len(lines)-1] {
		var record wire.Record
		check(r.t, json.Unmarshal([]byte(line), &record))
		if record.Event == event {
			records = append(records, record)
		}
	}
	return records
}

func (r *rig) counts() releasehost.Counts {
	var c releasehost.Counts
	readJSON(r.t, filepath.Join(r.releases, "counts.json"), &c)
	return c
}

func (r *rig) logContains(value string) bool {
	data, err := os.ReadFile(filepath.Join(r.dir, "logs.jsonl"))
	if os.IsNotExist(err) {
		return false
	}
	check(r.t, err)
	return strings.Contains(string(data), value)
}

func (r *rig) assertExit(code int) {
	r.t.Helper()
	await(r.t, fmt.Sprintf("exit %d", code), wire.Deadline, func() bool {
		actual, exited := r.exit()
		if exited && actual != code {
			r.t.Fatalf("exit code: got %d, want %d", actual, code)
		}
		return exited
	})
}

func readJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	check(t, err)
	check(t, json.Unmarshal(data, value))
}

func await(t *testing.T, description string, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(testsupport.Scale(timeout))
	defer deadline.Stop()
	ticker := time.NewTicker(wire.Poll)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-deadline.C:
			t.Fatalf("deadline %s waiting for %s", timeout, description)
		case <-ticker.C:
		}
	}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
