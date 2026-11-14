package daemon

import (
	"strings"
	"time"
)

const (
	supervisionStartTimeout  = 10 * time.Second
	supervisionStartInterval = 100 * time.Millisecond
)

// A newly linked service is not controllable until its directory scanner has
// started the supervisor. Retry only that registration race.
func startSupervisedService(executor commandExecutor, timeout, interval time.Duration, command string, args ...string) error {
	deadline := time.Now().Add(timeout)
	for {
		out, err := executor.Run(command, args...)
		if err == nil {
			return nil
		}
		message := strings.ToLower(string(out))
		pending := command == "sv" && strings.Contains(message, "unable to open supervise/ok: file does not exist") ||
			command == "s6-svc" && strings.Contains(message, "unable to control") &&
				(strings.Contains(message, "supervisor not listening") || strings.Contains(message, "no such file or directory"))
		if !pending || !time.Now().Before(deadline) {
			return runErr("starting service", out, err)
		}
		time.Sleep(interval)
	}
}
