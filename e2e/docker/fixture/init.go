//go:build e2e_docker

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ovineko/daemonpilot/e2e/docker/internal/wire"
	"github.com/ovineko/daemonpilot/internal/daemon"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

func inspectInit(c wire.Config) error {
	det, err := daemon.DetectForScope(daemon.ScopeSystem)
	if err != nil {
		return fmt.Errorf("detect init: %w", err)
	}
	mgr := det.Manager
	if err := daemon.InitServiceManagerWithOptions(mgr, daemon.ManagerOptions{ServiceName: wire.Name, InstallDir: c.Dir}); err != nil {
		return fmt.Errorf("bind init: %w", err)
	}
	o := wire.InitObservation{UID: os.Geteuid(), Name: mgr.Name(), Manifest: mgr.GetManifestPath(), Files: map[string]bool{}, Capabilities: wire.Capabilities{Enable: mgr.SupportsEnable(), Start: mgr.SupportsStart(), Status: mgr.SupportsStatus(), RestartWatchdog: mgr.CanRestartWatchdog()}}
	o.PID1, err = os.Readlink("/proc/1/exe")
	if err != nil {
		return fmt.Errorf("observe PID 1: %w", err)
	}
	command, err := os.ReadFile("/proc/1/cmdline")
	if err != nil {
		return fmt.Errorf("observe PID 1 arguments: %w", err)
	}
	o.PID1Command = strings.ReplaceAll(string(command), "\x00", " ")
	o.Content, _ = mgr.ReadInstalledConfig()
	manifest, _ := os.ReadFile(o.Manifest)
	o.ManifestContent = string(manifest)
	status, statusErr := mgr.Status()
	o.Status = status.String()
	if statusErr != nil {
		o.StatusError = statusErr.Error()
	}
	o.State, err = daemonpilot.LoadState(c.Dir, wire.Name)
	if err != nil {
		o.StateError = err.Error()
	}
	binary := filepath.Join(c.Dir, wire.Name)
	registration := registrationArtifacts(mgr.Name())
	paths := make([]string, 0, 10+len(registration))
	paths = append(paths, o.Manifest, o.Manifest+".backup", binary, binary+".lock", binary+".new", binary+".rollback", binary+".rollback.invalid", binary+".restore", daemonpilot.StagedNewPath(binary), daemonpilot.StateFilePath(c.Dir, wire.Name))
	paths = append(paths, registration...)
	for _, path := range paths {
		_, statErr := os.Lstat(path)
		if statErr != nil && !os.IsNotExist(statErr) {
			return fmt.Errorf("observe artifact %s: %w", path, statErr)
		}
		o.Files[path] = statErr == nil
	}
	data, err := os.ReadFile(binary) //nolint:gosec // Test-owned installed binary.
	if err == nil {
		sum := sha256.Sum256(data)
		o.BinaryDigest = hex.EncodeToString(sum[:])
	}
	o.RollbackVerified = daemonpilot.VerifyRollbackBinary(c.Dir, wire.Name, binary+".rollback") == nil
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return fmt.Errorf("list processes: %w", err)
	}
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || pid == os.Getpid() {
			continue
		}
		target, linkErr := os.Readlink(filepath.Join("/proc", entry.Name(), "exe"))
		if linkErr == nil && strings.TrimSuffix(target, " (deleted)") == binary {
			o.Processes = append(o.Processes, pid)
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(o); err != nil {
		return fmt.Errorf("encode init evidence: %w", err)
	}
	return nil
}

func failConfigApply(_ string, content string, info daemonpilot.PatchInfo) (string, error) {
	switch info.InitSystem {
	case "systemd":
		return strings.Replace(content, "ExecStart=", "ExecStart=/bin/true\nExecStart=", 1), nil
	case "openrc", "sysvinit", "busybox":
		first, rest, _ := strings.Cut(content, "\n")
		return first + "\nexit 71\n" + rest, nil
	case "dinit":
		return strings.Replace(content, "type = process", "type = invalid", 1), nil
	case "crontab":
		return strings.Replace(content, "@reboot", "61 * * * *", 1), nil
	default:
		return "", fmt.Errorf("no synchronous apply rejection for %s", info.InitSystem)
	}
}

func registrationArtifacts(name string) []string {
	switch name {
	case "busybox":
		return []string{"/etc/init.d/S95" + wire.Name, "/etc/init.d/K95" + wire.Name, "/etc/init.d/" + wire.Name, "/etc/init.d/" + wire.Name + ".disabled"}
	case "runit":
		return []string{"/var/service/" + wire.Name, "/etc/sv/" + wire.Name}
	case "s6":
		return []string{"/var/service/" + wire.Name, "/etc/s6/sv/" + wire.Name}
	case "dinit":
		return []string{"/etc/dinit.d/" + wire.Name + ".env", "/etc/dinit.d/boot.d/" + wire.Name}
	case "finit":
		return []string{"/etc/finit.d/enabled/" + wire.Name + ".conf", "/etc/finit.d/available/" + wire.Name + ".env"}
	case "sysvinit", "procd":
		matches, _ := filepath.Glob("/etc/rc*.d/*" + wire.Name)
		return matches
	default:
		return nil
	}
}
