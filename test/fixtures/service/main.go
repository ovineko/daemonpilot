package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	_ "github.com/ovineko/daemonpilot/pkg/daemonpilot/setup"
	"github.com/ovineko/daemonpilot/pkg/httpsource"
)

func main() {
	dir := os.Getenv("GO_DAEMONPILOT_NATIVE_DIR")
	name := os.Getenv("GO_DAEMONPILOT_NATIVE_NAME")
	if dir == "" || name == "" {
		fmt.Fprintln(os.Stderr, "native fixture directory and service name are required")
		os.Exit(2)
	}
	scope := daemonpilot.ScopeSystem
	if runtime.GOOS == "darwin" {
		scope = daemonpilot.ScopeUser
	}
	config := daemonpilot.Config{
		Name:       name,
		InstallDir: dir,
		Scope:      scope,
		ServiceEnvironment: map[string]string{
			"GO_DAEMONPILOT_NATIVE_DIR":         dir,
			"GO_DAEMONPILOT_NATIVE_NAME":        name,
			"GO_DAEMONPILOT_NATIVE_RELEASE_URL": os.Getenv("GO_DAEMONPILOT_NATIVE_RELEASE_URL"),
			"GO_DAEMONPILOT_NATIVE_PUBLIC_KEY":  os.Getenv("GO_DAEMONPILOT_NATIVE_PUBLIC_KEY"),
		},
		HealthInterval: 200 * time.Millisecond,
		HealthFailures: 3,
		HealthFunc: func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				return nil
			}
		},
		WorkerFunc: func(ctx context.Context) error {
			if daemonpilot.Version == "3.0.0" {
				data, err := json.Marshal(map[string]any{"pid": os.Getpid(), "watchdog_pid": os.Getppid(), "version": daemonpilot.Version, "at": time.Now()})
				if err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(dir, "worker.json"), data, 0o600); err != nil {
					return err
				}
				return fmt.Errorf("fixture staged worker is unhealthy")
			}
			for {
				data, err := json.Marshal(map[string]any{"pid": os.Getpid(), "watchdog_pid": os.Getppid(), "version": daemonpilot.Version, "at": time.Now()})
				if err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(dir, "worker.json"), data, 0o600); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(100 * time.Millisecond):
				}
			}
		},
	}
	if releaseURL := os.Getenv("GO_DAEMONPILOT_NATIVE_RELEASE_URL"); releaseURL != "" {
		publicKey, err := hex.DecodeString(os.Getenv("GO_DAEMONPILOT_NATIVE_PUBLIC_KEY"))
		if err != nil || len(publicKey) != ed25519.PublicKeySize {
			fmt.Fprintln(os.Stderr, "invalid native update public key")
			os.Exit(2)
		}
		source, err := httpsource.New(httpsource.Config{
			ManifestURL: releaseURL + "/manifest/{os}/{arch}",
			DownloadURL: releaseURL + "/download/{version}/{os}/{arch}",
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		config.StableThreshold = time.Second
		config.Update = &daemonpilot.UpdateConfig{
			Source:                   source,
			PublicKey:                ed25519.PublicKey(publicKey),
			SignatureContext:         "daemonpilot-native-e2e-v1\x00",
			CheckInterval:            300 * time.Millisecond,
			SelftestTimeout:          5 * time.Second,
			RetryMaxAttempts:         2,
			RetryBaseBackoff:         250 * time.Millisecond,
			RetryMaxBackoff:          time.Second,
			RollbackTimeout:          5 * time.Second,
			StagedRollbackOnUnstable: true,
			StagedUpdateFunc: func(context.Context, *daemonpilot.UpdateInfo, daemonpilot.SelftestResult) (bool, error) {
				return true, nil
			},
		}
	}
	service, err := daemonpilot.New(config)
	if err == nil {
		err = service.Run()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
