//go:build e2e_docker

package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/e2e/docker/internal/wire"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

type profile struct {
	name, image      string
	flags            []string
	entrypoint, user string
	args, scenarios  []string
	prepare          func(*rig)
	imageEntrypoint  bool
}

func TestProfiles(t *testing.T) {
	requireDocker(t)
	artifacts := build(t)
	for _, p := range profiles() {
		t.Run(p.name, func(t *testing.T) {
			ids := p.scenarios
			if len(ids) == 0 {
				ids = scenarioIDs()
			}
			for _, id := range ids {
				t.Run(id, func(t *testing.T) {
					r := newRig(t, artifacts, id)
					startDocker(r, p, id)
					runScenario(r, id)
				})
			}
		})
	}
}

func profiles() []profile {
	return []profile{{name: "debian", image: "debian:bookworm-slim"}, {name: "alpine", image: "alpine:3.21"}}
}

func requireDocker(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testsupport.Scale(10*time.Second))
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput()
	if err != nil {
		if wire.RequireDocker() {
			t.Fatalf("Docker required: %v: %s", err, out)
		}
		t.Skipf("Docker unavailable: %v: %s; DAEMONPILOT_E2E_REQUIRE=1 makes this fatal", err, out)
	}
}

func dockerCLI(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testsupport.Scale(60*time.Second))
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker %v: %v\n%s\n%s", args, err, &stdout, &stderr)
	}
	return strings.TrimSpace(stdout.String())
}

func dockerCleanup(t *testing.T, args ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(15*time.Second))
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "No such container:") {
			t.Errorf("cleanup docker %v: %v: %s", args, err, out)
		}
	})
}

func startDocker(r *rig, p profile, id string) {
	t := r.t
	sum := sha256.Sum256([]byte(t.Name() + r.dir))
	name := fmt.Sprintf("daemonpilot-%d-%x", os.Getpid(), sum[:8])
	network := name + "-net"
	dockerCLI(t, "network", "create", "--internal", network)
	dockerCleanup(t, "network", "rm", network)
	// Bind volumes permit the same atomic control and evidence protocol natively and in Docker.
	// Their directories belong to t.TempDir and are removed after the containers.
	user := fmt.Sprintf("%d:%d", max(os.Getuid(), 0), max(os.Getgid(), 0))
	appUser := user
	if p.user != "" {
		appUser = p.user
	}
	server := name + "-releases"
	dockerCleanup(t, "rm", "--force", server)
	dockerCLI(t, "run", "--detach", "--name", server, "--user", user, "--network", network, "--network-alias", "releases",
		"--mount", "type=bind,src="+r.artifacts+",dst=/artifacts,readonly",
		"--mount", "type=bind,src="+r.releases+",dst=/releases",
		"--entrypoint", "/artifacts/relserver", p.image)
	r.config.Dir, r.config.ReleaseURL = "/state", "http://releases:8080"
	check(t, wire.WriteJSON(filepath.Join(r.dir, "config.json"), r.config))
	app := name + "-app"
	dockerCleanup(t, "rm", "--force", app)
	args := []string{
		"run", "--detach", "--name", app, "--user", appUser, "--network", network,
		"--mount", "type=bind,src=" + r.dir + ",dst=/state", "--env", wire.ConfigEnv + "=/state/config.json",
	}
	args = append(args, p.flags...)
	if p.imageEntrypoint {
		args = append(args, "--tty", "--mount", "type=bind,src="+r.artifacts+",dst=/artifacts,readonly")
	}
	switch {
	case p.imageEntrypoint:
		args = append(args, p.image)
	case p.entrypoint != "":
		args = append(args, "--entrypoint", p.entrypoint, p.image)
		args = append(args, p.args...)
	case id == "lifecycle-watchdog-sigkill":
		args = append(args, "--init", "--entrypoint", "/bin/sh", p.image, "-c", "/state/"+wire.Name+" run & wait; sleep 300")
	default:
		args = append(args, "--entrypoint", "/state/"+wire.Name, p.image, "run")
	}
	if p.imageEntrypoint {
		result := dockerResult(t, 60*time.Second, args...)
		if result.Exit != 0 {
			if reason := initFacilityGap(p.name, result.Output); reason != "" {
				t.Skip(reason)
			}
			t.Fatalf("launch init container: %+v", result)
		}
	} else {
		dockerCLI(t, args...)
	}
	r.containerName = app
	r.container = true
	r.noInit = p.entrypoint == ""
	r.exec = func(args ...string) string { return dockerCLI(t, append([]string{"exec", app}, args...)...) }
	r.stop = func() { dockerCLI(t, "stop", "--time", "6", app) }
	r.exit = func() (int, bool) {
		fields := strings.Fields(dockerCLI(t, "inspect", "--format", "{{.State.Running}} {{.State.ExitCode}}", app))
		if len(fields) != 2 {
			t.Fatalf("unexpected container state: %v", fields)
		}
		code, err := strconv.Atoi(fields[1])
		check(t, err)
		return code, fields[0] == "false"
	}
	r.kill = func(pid int) { dockerCLI(t, "exec", app, "/bin/sh", "-c", `kill -9 "$1"`, "kill", strconv.Itoa(pid)) }
	r.alive = func(pid int) bool {
		out := dockerCLI(t, "exec", app, "/bin/sh", "-c", `if test -r "/proc/$1/stat"; then cat "/proc/$1/stat"; fi`, "observe", strconv.Itoa(pid))
		return liveStat(t, out)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("application container:\n%s", dockerCommand(context.Background(), 10*time.Second, "logs", app).Output)
			t.Logf("release container:\n%s", dockerCommand(context.Background(), 10*time.Second, "logs", server).Output)
		}
	})
	if p.prepare != nil {
		p.prepare(r)
	}
}

func liveStat(t *testing.T, data string) bool {
	t.Helper()
	if data == "" {
		return false
	}
	pos := strings.LastIndex(data, ")")
	if pos < 0 {
		t.Fatalf("invalid proc stat: %q", data)
	}
	fields := strings.Fields(data[pos+1:])
	if len(fields) == 0 {
		t.Fatalf("missing proc state: %q", data)
	}
	return fields[0] != "Z" && fields[0] != "X"
}
