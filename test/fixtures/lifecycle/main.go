// Command lifecycle exercises service lifecycle behavior in integration tests.
package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	_ "github.com/ovineko/daemonpilot/pkg/daemonpilot/setup"
)

var fixtureVersion = "1.0.0"

func main() {
	installDir := os.Getenv("FIXTURE_INSTALL_DIR")
	if installDir == "" {
		installDir = filepath.Dir(os.Args[0])
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s, err := daemonpilot.New(daemonpilot.Config{
		Name:            "daemonpilot-e2e-fixture",
		Version:         fixtureVersion,
		WatchdogVersion: "1",
		InstallDir:      installDir,
		Logger:          logger,
		GracefulTimeout: 250 * time.Millisecond,
		StableThreshold: 150 * time.Millisecond,
		MaxRestartDelay: 100 * time.Millisecond,
		WorkerFunc:      runWorker,
		SelftestFunc:    runSelftest,
	})
	if err == nil {
		err = s.Run()
	}
	if err != nil {
		logger.Error("fixture command failed", "error", err)
		os.Exit(1)
	}
}

func runWorker(ctx context.Context) error {
	appendEvent("started:" + fixtureVersion)
	switch os.Getenv("FIXTURE_MODE") {
	case "crash_then_hang":
		if incrementAttempt() <= envInt("FIXTURE_CRASHES", 3) {
			os.Exit(1)
		}
		<-ctx.Done()
		return nil
	case "permanent":
		os.Exit(daemonpilot.ExitPermanent)
	case "graceful":
		<-ctx.Done()
		appendEvent("terminated")
		return nil
	case "ignore_term":
		select {}
	default:
		<-ctx.Done()
		return nil
	}
	return nil
}

func runSelftest(context.Context) daemonpilot.SelftestResult {
	return daemonpilot.SelftestResult{OK: os.Getenv("FIXTURE_SELFTEST_FAIL") != "1"}
}

func appendEvent(event string) {
	path := os.Getenv("FIXTURE_EVENTS")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // The integration harness supplies this path inside its temporary directory.
	if err != nil {
		return
	}
	_, _ = f.WriteString(event + "\n")
	_ = f.Close()
}

func incrementAttempt() int {
	path := os.Getenv("FIXTURE_ATTEMPTS")
	data, _ := os.ReadFile(path) //nolint:gosec // The integration harness supplies this path inside its temporary directory.
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	n++
	_ = os.WriteFile(path, []byte(strconv.Itoa(n)), 0o600) //nolint:gosec // The integration harness supplies this path inside its temporary directory.
	return n
}

func envInt(key string, fallback int) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil || n < 0 {
		return fallback
	}
	return n
}
