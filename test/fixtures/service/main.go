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
	"strings"
	"time"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	_ "github.com/ovineko/daemonpilot/pkg/daemonpilot/setup"
	"github.com/ovineko/daemonpilot/pkg/httpsource"
)

func nativeConfigValue(flag, env string) string {
	prefix := flag + "="
	for _, arg := range os.Args[2:] {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
	}
	return os.Getenv(env)
}

func main() {
	dir := nativeConfigValue("--native-dir", "GO_DAEMONPILOT_NATIVE_DIR")
	name := nativeConfigValue("--native-name", "GO_DAEMONPILOT_NATIVE_NAME")
	releaseURL := nativeConfigValue("--native-release-url", "GO_DAEMONPILOT_NATIVE_RELEASE_URL")
	publicKey := nativeConfigValue("--native-public-key", "GO_DAEMONPILOT_NATIVE_PUBLIC_KEY")
	if dir == "" || name == "" {
		fmt.Fprintln(os.Stderr, "native fixture directory and service name are required")
		os.Exit(2)
	}
	for _, entry := range [][2]string{
		{"GO_DAEMONPILOT_NATIVE_DIR", dir},
		{"GO_DAEMONPILOT_NATIVE_NAME", name},
		{"GO_DAEMONPILOT_NATIVE_RELEASE_URL", releaseURL},
		{"GO_DAEMONPILOT_NATIVE_PUBLIC_KEY", publicKey},
	} {
		if err := os.Setenv(entry[0], entry[1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	scope := daemonpilot.ScopeSystem
	if runtime.GOOS == "darwin" {
		scope = daemonpilot.ScopeUser
	}
	config := daemonpilot.Config{
		Name:           name,
		InstallDir:     dir,
		Scope:          scope,
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
	}
	if runtime.GOOS == "windows" {
		config.ServiceArguments = []string{
			"--native-dir=" + dir,
			"--native-name=" + name,
			"--native-release-url=" + releaseURL,
			"--native-public-key=" + publicKey,
		}
	} else {
		config.ServiceEnvironment = map[string]string{
			"GO_DAEMONPILOT_NATIVE_DIR":         dir,
			"GO_DAEMONPILOT_NATIVE_NAME":        name,
			"GO_DAEMONPILOT_NATIVE_RELEASE_URL": releaseURL,
			"GO_DAEMONPILOT_NATIVE_PUBLIC_KEY":  publicKey,
		}
	}
	config.WorkerFunc = func(ctx context.Context) error {
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
