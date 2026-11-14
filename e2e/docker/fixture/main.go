//go:build e2e_docker

// Command fixture is a controllable daemonpilot application for process contracts.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ovineko/daemonpilot/e2e/docker/internal/releasehost"
	"github.com/ovineko/daemonpilot/e2e/docker/internal/wire"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	_ "github.com/ovineko/daemonpilot/pkg/daemonpilot/setup"
	"github.com/ovineko/daemonpilot/pkg/httpsource"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	c, err := wire.Load()
	if err != nil {
		return fmt.Errorf("load fixture: %w", err)
	}
	if len(os.Args) > 1 && os.Args[1] == "inspect-init" {
		return inspectInit(c)
	}
	if len(os.Args) > 1 && os.Args[1] == "grandchild" {
		return grandchild(c)
	}
	if c.DisableParentMonitor {
		if err := os.Setenv(daemonpilot.EnvDisableParentMonitor, "1"); err != nil {
			return fmt.Errorf("disable cooperative monitor: %w", err)
		}
	}
	logFile, err := os.OpenFile(filepath.Join(c.Dir, "logs.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // Isolated test directory.
	if err != nil {
		return fmt.Errorf("open logs: %w", err)
	}
	defer func() { _ = logFile.Close() }()
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stderr, logFile), &slog.HandlerOptions{Level: slog.LevelDebug}))
	config := daemonpilot.Config{
		ServiceEnvironment: c.ServiceEnvironment,
		ServiceArguments:   c.ServiceArguments,
		Name:               wire.Name, InstallDir: c.Dir, Logger: logger, VerboseLogging: true,
		GracefulTimeout: wire.Grace, StableThreshold: c.Stable, MaxRestartDelay: 4 * time.Second,
		HealthInterval: 300 * time.Millisecond, HealthFailures: wire.HealthFailures,
		DumpWorkerOnHealthFailure: true,
		WorkerFunc:                func(ctx context.Context) error { return worker(ctx, c) },
		HealthFunc: func(ctx context.Context) error {
			if _, err := os.Stat(filepath.Join(c.Dir, "health-hang")); err == nil {
				<-ctx.Done()
				return fmt.Errorf("controlled health timeout: %w", ctx.Err())
			}
			return nil
		},
		SelftestFunc: func(context.Context) daemonpilot.SelftestResult {
			ok := true
			for _, v := range wire.Variants() {
				if v.Version == daemonpilot.Version {
					ok = !v.Broken
				}
			}
			return daemonpilot.SelftestResult{OK: ok}
		},
	}
	if c.FailConfigApply {
		config.ConfigPatchers = []daemonpilot.ConfigPatcher{failConfigApply}
	}
	if c.ReleaseURL != "" {
		config.Update, err = updateConfig(c)
		if err != nil {
			return err
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "run" {
		emit(c, "watchdog-start", "")
	}
	s, err := daemonpilot.New(config)
	if err != nil {
		return fmt.Errorf("configure daemonpilot: %w", err)
	}
	if err := s.Run(); err != nil {
		return fmt.Errorf("run daemonpilot: %w", err)
	}
	return nil
}

func updateConfig(c wire.Config) (*daemonpilot.UpdateConfig, error) {
	var client *http.Client
	if c.NativeReleaseDir != "" {
		native, err := nativeClient(c)
		if err != nil {
			return nil, err
		}
		client = native
	}
	source, err := httpsource.New(httpsource.Config{Client: client, ManifestURL: c.ReleaseURL + "/manifest/{os}/{arch}", DownloadURL: c.ReleaseURL + "/download/{version}/{os}/{arch}"})
	if err != nil {
		return nil, fmt.Errorf("create update source: %w", err)
	}
	key, err := hex.DecodeString(c.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("decode public key: %w", err)
	}
	return &daemonpilot.UpdateConfig{
		Source: source, PublicKey: key, CheckInterval: wire.UpdateInterval,
		RetryMaxAttempts: wire.RetryAttempts, RetryBaseBackoff: 300 * time.Millisecond, RetryMaxBackoff: 600 * time.Millisecond,
		SelftestTimeout: 3 * time.Second, RollbackTimeout: wire.RollbackWindow, StagedRollbackOnUnstable: true,
		StagedUpdateFunc: func(context.Context, *daemonpilot.UpdateInfo, daemonpilot.SelftestResult) (bool, error) {
			return true, nil
		},
		AcceptVersion: func(_, offered string) error {
			if offered == c.RejectVersion {
				return errors.New("fixture version policy")
			}
			return nil
		},
	}, nil
}

func worker(ctx context.Context, c wire.Config) error {
	emit(c, "worker-start", "")
	for _, v := range wire.Variants() {
		if v.Version == daemonpilot.Version && v.Unstable {
			os.Exit(3)
		}
	}
	daemonpilot.StartStackDumpHandler(ctx, c.Dir, func(path string, err error) {
		if err != nil {
			panic(err)
		}
		emit(c, "dump", filepath.Base(path))
	})
	readyCount := 0
	if data, err := os.ReadFile(filepath.Join(c.Dir, "readiness-count")); err == nil {
		readyCount, _ = strconv.Atoi(string(data))
	}
	ticker := time.NewTicker(wire.Poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			emit(c, "worker-stop", "")
			return nil
		case <-ticker.C:
			info, err := daemonpilot.GetRuntimeInfo()
			if err != nil {
				return fmt.Errorf("read runtime: %w", err)
			}
			must(wire.WriteJSON(filepath.Join(c.Dir, "worker.json"), wire.Snapshot{Environment: wire.EnvironmentValue(), Version: daemonpilot.Version, PID: os.Getpid(), At: time.Now(), Runtime: *info}))
			for name, value := range map[string]string{"version": daemonpilot.Version, "watchdog-version": info.WatchdogVersion, "worker-pid": strconv.Itoa(os.Getpid()), "readiness-count": strconv.Itoa(readyCount)} {
				must(wire.Write(filepath.Join(c.Dir, name), []byte(value)))
			}
			var command wire.Command
			data, err := os.ReadFile(filepath.Join(c.Dir, "command.json"))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read command: %w", err)
			}
			if err := json.Unmarshal(data, &command); err != nil {
				return fmt.Errorf("decode command: %w", err)
			}
			if command.PID != os.Getpid() {
				continue
			}
			must(os.Remove(filepath.Join(c.Dir, "command.json")))
			switch command.Action {
			case "exit":
				emit(c, "exit-request", strconv.Itoa(command.Code))
				os.Exit(command.Code) //nolint:gocritic // Deliberate abrupt worker exit, including reserved request codes.
			case "ready":
				must(daemonpilot.NotifyReady()) //nolint:contextcheck // The public readiness API has no context parameter.
				readyCount++
				emit(c, "ready", "")
			case "grandchild":
				// The child outlives worker cancellation so supervisor tree cleanup is exercised.
				cmd := exec.CommandContext(context.WithoutCancel(ctx), os.Args[0], "grandchild") //nolint:gosec // The fixture starts itself.
				cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
				must(cmd.Start())
				go func() { _ = cmd.Wait() }()
			default:
				return fmt.Errorf("unknown command %q", command.Action)
			}
		}
	}
}

func emit(c wire.Config, event, value string) {
	r := wire.Record{Event: event, PID: os.Getpid(), Parent: os.Getppid(), At: time.Now(), Version: daemonpilot.Version, WatchdogVersion: daemonpilot.WatchdogVersion, Value: value}
	if event == "watchdog-start" {
		r.Arguments = os.Args[1:]
		directory, err := os.Getwd()
		must(err)
		r.Directory = directory
	}
	data, err := json.Marshal(r)
	must(err)
	data = append(data, '\n')
	f, err := os.OpenFile(filepath.Join(c.Dir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // Fixture-owned journal.
	must(err)
	_, err = f.Write(data)
	must(err)
	must(f.Close())
	_, err = os.Stdout.Write(data)
	must(err)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func nativeClient(c wire.Config) (*http.Client, error) {
	keys := make([]ed25519.PrivateKey, 2)
	for i, name := range []string{"key", "wrong-key"} {
		seed, err := os.ReadFile(filepath.Join(c.NativeReleaseDir, name)) //nolint:gosec // Native test-owned signing seeds.
		if err != nil {
			return nil, fmt.Errorf("read native signing seed: %w", err)
		}
		if len(seed) != ed25519.SeedSize {
			return nil, errors.New("invalid native seed length")
		}
		keys[i] = ed25519.NewKeyFromSeed(seed)
	}
	h, err := releasehost.New(c.NativeReleaseDir, c.NativeArtifacts, keys[0], keys[1])
	if err != nil {
		return nil, fmt.Errorf("native release handler: %w", err)
	}
	return &http.Client{Transport: releasehost.Transport{Handler: h}}, nil
}
