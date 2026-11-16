//go:build windows

package daemonpilot

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	rollbackCrashModeEnv = "GO_DAEMONPILOT_TEST_ROLLBACK_CRASH_MODE"
	rollbackCrashDirEnv  = "GO_DAEMONPILOT_TEST_ROLLBACK_CRASH_DIR"
	rollbackCrashNameEnv = "GO_DAEMONPILOT_TEST_ROLLBACK_CRASH_NAME"
	rollbackCrashSCMEnv  = "GO_DAEMONPILOT_TEST_ROLLBACK_SCM_PATH"
	rollbackCrashAtEnv   = "GO_DAEMONPILOT_TEST_ROLLBACK_CRASH_AT"
)

func TestWindowsRollbackRestoreCrashHelper(t *testing.T) {
	mode := os.Getenv(rollbackCrashModeEnv)
	if mode == "" {
		return
	}
	installDir := os.Getenv(rollbackCrashDirEnv)
	serviceName := os.Getenv(rollbackCrashNameEnv)
	targetPath := filepath.Join(installDir, InstalledBinaryName(serviceName))
	rollbackPath := targetPath + ".rollback"
	scmPath := os.Getenv(rollbackCrashSCMEnv)
	updateWindowsServiceBinaryPathFn = func(_, _, imagePath string) (bool, error) {
		if err := os.WriteFile(scmPath, []byte(imagePath), 0o600); err != nil {
			return false, err
		}
		return true, nil
	}
	if mode == "crash" {
		crashAt := os.Getenv(rollbackCrashAtEnv)
		rollbackRestoreCrashBarrier = func(stage string) {
			if stage == crashAt {
				os.Exit(97)
			}
		}
		_, err := RestoreRollbackBinary(installDir, serviceName, rollbackPath, targetPath)
		t.Fatalf("RestoreRollbackBinary returned before the injected crash: %v", err)
	}
	if mode != "recover" {
		t.Fatalf("unknown helper mode %q", mode)
	}
	cfg := &Config{Name: serviceName, InstallDir: installDir, Logger: NewDefaultLogger(LogLevelError)}
	recovered, err := recoverInterruptedWindowsRollback(cfg, cfg.Logger)
	if !recovered || !errors.Is(err, ErrExecRestart) {
		t.Fatalf("recoverInterruptedWindowsRollback() = %v, %v; want recovered with ErrExecRestart", recovered, err)
	}
}

func TestWindowsRollbackRestoreRecoversAcrossCrashWindows(t *testing.T) {
	for _, crashAt := range []string{"recovery-image-published", "target-aside"} {
		t.Run(crashAt, func(t *testing.T) {
			testWindowsRollbackRestoreAfterCrash(t, crashAt)
		})
	}
}

func testWindowsRollbackRestoreAfterCrash(t *testing.T, crashAt string) {
	installDir := t.TempDir()
	const serviceName = "rollback-recovery"
	targetPath := filepath.Join(installDir, InstalledBinaryName(serviceName))
	rollbackPath := targetPath + ".rollback"
	recoveryPath := StagedNewPath(targetPath)
	scmPath := filepath.Join(installDir, "scm-image-path")

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G304 -- Copying this package's own test executable into its private fixture directory.
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{targetPath, rollbackPath} {
		// #nosec G306 -- Executable fixture owned by this test.
		if err := os.WriteFile(path, binary, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordRollbackDigest(installDir, serviceName, rollbackPath); err != nil {
		t.Fatal(err)
	}

	runHelper := func(binaryPath, mode, crashAt string) ([]byte, error) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), binaryPath, "-test.run=^TestWindowsRollbackRestoreCrashHelper$")
		cmd.Env = append(os.Environ(),
			rollbackCrashModeEnv+"="+mode,
			rollbackCrashDirEnv+"="+installDir,
			rollbackCrashNameEnv+"="+serviceName,
			rollbackCrashSCMEnv+"="+scmPath,
			rollbackCrashAtEnv+"="+crashAt,
		)
		return cmd.CombinedOutput()
	}

	output, err := runHelper(targetPath, "crash", crashAt)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 97 {
		t.Fatalf("crash helper = %v, output %s; want injected exit 97", err, output)
	}
	if crashAt == "target-aside" {
		if _, err := os.Lstat(targetPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("installed path after injected crash = %v; want the move/publish window", err)
		}
		fallback, err := os.ReadFile(scmPath)
		if err != nil || string(fallback) != recoveryPath {
			t.Fatalf("SCM fallback path = %q, %v; want verified recovery path %q", fallback, err, recoveryPath)
		}
	} else if _, err := os.Lstat(targetPath); err != nil {
		t.Fatalf("canonical binary after fallback image publication = %v; want the old image", err)
	}
	if err := VerifyRollbackBinary(installDir, serviceName, rollbackPath); err != nil {
		t.Fatalf("rollback source is not verified: %v", err)
	}
	if err := VerifyRollbackBinary(installDir, serviceName, recoveryPath); err != nil {
		t.Fatalf("SCM fallback is not verified: %v", err)
	}

	recoveryExecutable := targetPath
	if crashAt == "target-aside" {
		recoveryExecutable = recoveryPath
	}
	output, err = runHelper(recoveryExecutable, "recover", "")
	if err != nil {
		t.Fatalf("recovery helper: %v, output %s", err, output)
	}
	fallback, err := os.ReadFile(scmPath)
	if err != nil || string(fallback) != targetPath {
		t.Fatalf("SCM path after recovery = %q, %v; want canonical path %q", fallback, err, targetPath)
	}
	if _, err := os.Lstat(rollbackPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback source survived recovery: %v", err)
	}
	state, err := LoadState(installDir, serviceName)
	if err != nil || state.RollbackDigest != "" {
		t.Fatalf("rollback digest after recovery = %+v, %v; want cleared", state, err)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("recovered image mismatch: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, targetPath, "-test.run=^$")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("canonical image did not start after recovery: %v, output %s", err, output)
	}
}
