//go:build e2e_docker && linux

package docker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/e2e/docker/internal/wire"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestNative(t *testing.T) {
	artifacts := build(t)
	for _, id := range scenarioIDs() {
		t.Run(id, func(t *testing.T) {
			r := newRig(t, artifacts, id)
			startNative(r)
			runScenario(r, id)
		})
	}
}

func startNative(r *rig) {
	t := r.t
	r.config.ReleaseURL = "http://releases.test"
	r.config.NativeReleaseDir, r.config.NativeArtifacts = r.releases, r.artifacts
	configPath := filepath.Join(r.dir, "config.json")
	check(t, wire.WriteJSON(configPath, r.config))
	output, err := os.Create(filepath.Join(r.dir, "console.log"))
	check(t, err)
	t.Cleanup(func() { check(t, output.Close()) })
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, filepath.Join(r.dir, wire.Name), "run")
	cmd.Env = append(os.Environ(), wire.ConfigEnv+"="+configPath)
	cmd.Stdout, cmd.Stderr = output, output
	check(t, cmd.Start())
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	r.exit = func() (int, bool) {
		select {
		case <-done:
			return cmd.ProcessState.ExitCode(), true
		default:
			return 0, false
		}
	}
	r.stop = func() { check(t, cmd.Process.Signal(syscall.SIGTERM)) }
	r.kill = func(pid int) { check(t, syscall.Kill(pid, syscall.SIGKILL)) }
	r.alive = func(pid int) bool {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if os.IsNotExist(err) {
			return false
		}
		check(t, err)
		return liveStat(t, string(data))
	}
	t.Cleanup(func() {
		if _, exited := r.exit(); !exited {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(testsupport.Scale(wire.Grace + time.Second)):
				cancel()
				<-done
			}
		}
		cancel()
		// Native descendants have no private PID namespace to tear down on failure.
		for _, event := range []string{"worker-start", "grandchild-start", "watchdog-start"} {
			for _, record := range r.records(event) {
				if r.alive(record.PID) {
					_ = syscall.Kill(record.PID, syscall.SIGKILL)
				}
			}
		}
	})
}
