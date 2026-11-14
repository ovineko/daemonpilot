package daemonpilot

// DaemonpilotArgs builds the argument list for re-executing this binary with one
// of daemonpilot's own verbs. An application whose CLI already owns names like
// "run" or "worker" puts them behind Config.CommandPrefix, and every path that
// starts the binary again — the service manifest, the worker spawn, the update
// probes — goes through here so none of them can forget the prefix.
func DaemonpilotArgs(prefix []string, verb ...string) []string { //nolint:revive // Preserve the published DaemonpilotArgs API name.
	args := make([]string, 0, len(prefix)+len(verb))
	args = append(args, prefix...)

	return append(args, verb...)
}
