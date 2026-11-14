package daemonpilot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
)

var validCommands = map[string]bool{
	"install":          true,
	"uninstall":        true,
	"run":              true,
	"worker":           true,
	"selftest":         true,
	"watchdog-version": true,
	"version":          true,
	"status":           true,
	"start":            true,
	"stop":             true,
	"restart":          true,
	"enable":           true,
	"disable":          true,
	"validate":         true,
	"update-config":    true,
}

func parseCommand(args []string) (string, error) {
	if len(args) < 2 {
		return "", errors.New("no command specified")
	}

	cmd := args[1]
	if !validCommands[cmd] {
		return "", fmt.Errorf("unknown command: %s", cmd)
	}

	return cmd, nil
}

var lookupEnv = os.LookupEnv

func isExecThrough(args []string) bool {
	if v, ok := lookupEnv("DAEMONPILOT_EXEC_THROUGH"); ok && v == "1" {
		return true
	}
	return slices.Contains(args[2:], "--exec-through")
}

func printUsage(w io.Writer, programName string) {
	_, _ = fmt.Fprintf(w, "Usage: %s <command>\n\n", programName)
	_, _ = fmt.Fprintln(w, "Commands:")
	_, _ = fmt.Fprintln(w, "  install          Register service in the OS init system")
	_, _ = fmt.Fprintln(w, "  uninstall        Unregister service from the OS init system")
	_, _ = fmt.Fprintln(w, "  run              Start in watchdog mode (parent process)")
	_, _ = fmt.Fprintln(w, "  worker           Start in worker mode (child process)")
	_, _ = fmt.Fprintln(w, "  selftest         Run self-test checks")
	_, _ = fmt.Fprintln(w, "  watchdog-version Print watchdog version")
	_, _ = fmt.Fprintln(w, "  version          Print full version information")
	_, _ = fmt.Fprintln(w, "  status           Show service status")
	_, _ = fmt.Fprintln(w, "  start            Start the service")
	_, _ = fmt.Fprintln(w, "  stop             Stop the service")
	_, _ = fmt.Fprintln(w, "  restart          Restart the service")
	_, _ = fmt.Fprintln(w, "  enable           Enable service auto-start on boot")
	_, _ = fmt.Fprintln(w, "  disable          Disable service auto-start on boot")
	_, _ = fmt.Fprintln(w, "  validate         Verify installed config matches expected state")
	_, _ = fmt.Fprintln(w, "  update-config    Update service config in-place without reinstalling")
}
