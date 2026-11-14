//go:build e2e_docker

package docker

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

func (r *initRig) await(step string, timeout time.Duration, condition func() bool) {
	r.t.Helper()
	r.step = step
	await(r.t, step, timeout, condition)
}

func (r *initRig) failureEvidence() {
	r.t.Helper()
	snapshot, snapshotErr := os.ReadFile(filepath.Join(r.dir, "worker.json"))
	r.evidence("first-failure", map[string]any{"step": r.step, "last_command": r.lastCommand, "snapshot": string(snapshot), "snapshot_error": snapshotErr})
	for _, query := range []struct {
		step string
		args []string
	}{
		{"container-state", []string{"inspect", "--format", "{{json .State}}", r.containerName}},
		{"container-logs", []string{"logs", "--timestamps", r.containerName}},
		{"failure-observation", []string{"exec", r.containerName, "/artifacts/v1", "inspect-init"}},
		{"init-status", []string{"exec", r.containerName, "/bin/sh", "-c", initStatusQuery(r.profile.name)}},
		{"process-table", []string{"exec", r.containerName, "/bin/sh", "-c", "ps -ef 2>&1 || ps"}},
		{"service-logs", []string{"exec", r.containerName, "/bin/sh", "-c", `for path in /var/log/daemonpilot-e2e.log /var/log/daemonpilot-e2e/*; do [ ! -f "$path" ] || { printf '\n%s\n' "$path"; cat "$path"; }; done`}},
	} {
		r.evidence(query.step, dockerCommand(context.Background(), 10*time.Second, query.args...))
	}
}

func initStatusQuery(name string) string {
	switch name {
	case "systemd":
		return "systemctl status --no-pager -l daemonpilot-e2e.service; journalctl --no-pager -b -u daemonpilot-e2e.service"
	case "openrc":
		return "rc-status -a; rc-service daemonpilot-e2e status"
	case "sysvinit":
		return "runlevel; service daemonpilot-e2e status"
	case "runit":
		return "sv status /var/service/daemonpilot-e2e"
	case "s6":
		return "s6-svstat /var/service/daemonpilot-e2e"
	case "dinit":
		return "dinitctl list; dinitctl status daemonpilot-e2e; cat /etc/dinit.d/daemonpilot-e2e.env"
	case "finit":
		return "initctl runlevel; initctl status; initctl status daemonpilot-e2e; cat /etc/finit.d/available/daemonpilot-e2e.env"
	case "procd":
		return "ubus call service list; service daemonpilot-e2e info; logread"
	case "busybox":
		return "cat /etc/inittab; ls -l /etc/init.d; cat /var/run/daemonpilot-e2e.pid /var/run/daemonpilot-e2e.stop"
	case "crontab":
		return "crontab -l; cat /var/run/crond.pid"
	default:
		return "exit 99"
	}
}
