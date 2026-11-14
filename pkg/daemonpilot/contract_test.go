package daemonpilot

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestResidentWatchdogStale(t *testing.T) {
	for _, tc := range []struct {
		name, resident, own string
		unset, want         bool
	}{
		{name: "unset", own: "2", unset: true},
		{name: "empty", own: "2"},
		{name: "same", resident: "2", own: "2"},
		{name: "different", resident: "1", own: "2", want: true},
		{name: "empty own", resident: "1", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvWatchdogVersion, tc.resident)
			if tc.unset {
				if err := os.Unsetenv(EnvWatchdogVersion); err != nil {
					t.Fatal(err)
				}
			}
			if got := ResidentWatchdogStale(tc.own); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestContractProcessExit(t *testing.T) {
	for _, tc := range []struct {
		name, resident string
		code           int
	}{
		{"respawn", "", ExitRespawnWatchdog},
		{"stop", "", ExitPermanent},
		{"stale", "1", ExitRespawnWatchdog},
		{"matching", "2", 0},
		{"empty", "", 0},
		{"disabled", "1", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(15*time.Second))
			defer cancel()

			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestContractExitChild$", "--", tc.name)
			cmd.Env = append(os.Environ(), EnvWatchdogVersion+"="+tc.resident)
			output, err := cmd.CombinedOutput()
			if tc.code == 0 {
				if err != nil {
					t.Fatalf("%v: %s", err, output)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.code {
					t.Fatalf("exit = %v; output: %s", err, output)
				}
			}
			if tc.name == "stale" && (!strings.Contains(string(output), "watchdog_version=2") || !strings.Contains(string(output), "resident_watchdog_version=1")) {
				t.Fatalf("missing versions: %s", output)
			}
		})
	}
}

func TestContractExitChild(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "respawn":
		RespawnWatchdog()
	case "stop":
		StopPermanently()
	}
	cfg := Config{Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)), Name: "contract-test", WatchdogVersion: "2", RespawnStaleWatchdog: mode != "disabled", WorkerFunc: func(context.Context) error {
		if mode == "stale" {
			t.Fatal("stale worker started")
		}
		return nil
	}}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatal(err)
	}
	if err := handleWorker(&cfg); err != nil {
		t.Fatal(err)
	}
}
