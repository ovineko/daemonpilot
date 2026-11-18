//go:build windows

package daemonpilot

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ovineko/daemonpilot/internal/daemon"
	"github.com/ovineko/daemonpilot/internal/process"
	"golang.org/x/sys/windows"
)

type windowsServiceBinaryPathUpdater interface {
	UpdateBinaryPath(path string) error
}

var updateWindowsServiceBinaryPathFn = updateWindowsServiceBinaryPath

var rollbackRestoreCrashBarrier func(stage string)

func updateWindowsServiceBinaryPath(installDir, serviceName, imagePath string) (bool, error) {
	state, err := LoadState(installDir, serviceName)
	if err != nil {
		return false, fmt.Errorf("loading service state before changing the SCM image path: %w", err)
	}
	if state == nil || state.InitSystem != "windows-service" {
		return false, nil
	}
	manager, err := daemon.NewServiceManagerByName(state.InitSystem)
	if err != nil {
		return false, fmt.Errorf("creating Windows service manager: %w", err)
	}
	if err := daemon.InitServiceManagerWithOptions(manager, daemon.ManagerOptions{
		ServiceName: serviceName,
		InstallDir:  installDir,
		Scope:       daemon.ScopeSystem,
	}); err != nil {
		return false, fmt.Errorf("binding Windows service manager: %w", err)
	}
	manifestPath := manager.GetManifestPath()
	if state.ManifestPath != "" && !strings.EqualFold(filepath.Clean(state.ManifestPath), filepath.Clean(manifestPath)) {
		return false, fmt.Errorf("Windows service manifest path changed from %s to %s", state.ManifestPath, manifestPath)
	}
	updater, ok := manager.(windowsServiceBinaryPathUpdater)
	if !ok {
		return false, errors.New("Windows service manager cannot update its image path")
	}
	if err := updater.UpdateBinaryPath(imagePath); err != nil {
		return false, err
	}
	return true, nil
}

func windowsRecoveryImagePath(targetPath, rollbackPath string) string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	currentInfo, err := os.Stat(executable)
	if err != nil {
		return ""
	}
	for _, path := range []string{rollbackPath, StagedNewPath(targetPath)} {
		candidate, err := os.Stat(path)
		if err == nil && os.SameFile(currentInfo, candidate) {
			return path
		}
	}
	return ""
}

func isRollbackRecoveryExecutable(cfg *Config) bool {
	targetPath := installedBinaryPath(cfg)
	rollbackPath := targetPath + ".rollback"
	if _, err := os.Lstat(rollbackPath); err != nil {
		return false
	}
	if windowsRecoveryImagePath(targetPath, rollbackPath) != "" {
		return true
	}
	return VerifyRollbackBinary(cfg.InstallDir, cfg.Name, StagedNewPath(targetPath)) == nil
}

func replaceRestoredBinary(tempPath, targetPath, installDir, serviceName, rollbackPath string, mode os.FileMode, recorded string) error {
	recoveryPath := windowsRecoveryImagePath(targetPath, rollbackPath)
	directErr := moveRollbackRestoreFile(tempPath, targetPath)
	if directErr == nil {
		if recoveryPath != "" {
			managed, err := updateWindowsServiceBinaryPathFn(installDir, serviceName, targetPath)
			if err != nil {
				return fmt.Errorf("restored the rollback binary but could not switch SCM back to it: %w", err)
			}
			if !managed && process.IsWindowsService() {
				return errors.New("restored the rollback binary but the running SCM service has no recorded image-path manager")
			}
			hitRollbackRestoreCrashBarrier("service-path-canonical")
		}
		hitRollbackRestoreCrashBarrier("target-published")
		return nil
	}
	info, err := os.Lstat(targetPath)
	if err != nil {
		return fmt.Errorf("replacing the installed binary: %w", directErr)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to move non-regular installed binary %s aside", targetPath)
	}
	if err := VerifyRollbackBinary(installDir, serviceName, rollbackPath); err != nil {
		return fmt.Errorf("verifying the SCM recovery image: %w", err)
	}
	createdRecovery := false
	cleanupRecovery := false
	if recoveryPath == "" {
		recoveryPath = StagedNewPath(targetPath)
		cleanupRecovery = true
		if _, err := os.Lstat(recoveryPath); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("checking SCM recovery image %s: %w", recoveryPath, err)
			}
			if err := moveRollbackRestoreFileNoReplace(tempPath, recoveryPath); err != nil {
				return fmt.Errorf("publishing the verified SCM recovery image: %w", err)
			}
			createdRecovery = true
			hitRollbackRestoreCrashBarrier("recovery-image-published")
		}
	}
	if err := VerifyRollbackBinary(installDir, serviceName, recoveryPath); err != nil {
		return fmt.Errorf("verifying the running SCM recovery image: %w", err)
	}
	managed, err := updateWindowsServiceBinaryPathFn(installDir, serviceName, recoveryPath)
	if err != nil {
		return fmt.Errorf("switching SCM to the verified recovery image: %w", err)
	}
	if !managed {
		if createdRecovery {
			_ = os.Remove(recoveryPath)
		}
		return fmt.Errorf("cannot safely replace the running Windows image without an installed SCM recovery path: %w", directErr)
	}
	hitRollbackRestoreCrashBarrier("service-path-fallback")

	asidePath := targetPath + SupersededBinarySuffix + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := os.Lstat(asidePath); err == nil {
		return fmt.Errorf("moving the running binary aside: %s already exists", asidePath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checking aside path %s: %w", asidePath, err)
	}
	if err := moveRollbackRestoreFileNoReplace(targetPath, asidePath); err != nil {
		return fmt.Errorf("moving the running binary aside: %w", err)
	}
	hitRollbackRestoreCrashBarrier("target-aside")
	if createdRecovery {
		if err := copyVerifiedRecoveryImage(recoveryPath, tempPath, mode, recorded); err != nil {
			if restoreErr := moveRollbackRestoreFileNoReplace(asidePath, targetPath); restoreErr != nil {
				return fmt.Errorf("preparing rollback publication: %w (restoring the current binary failed: %w)", err, restoreErr)
			}
			reset, switchErr := updateWindowsServiceBinaryPathFn(installDir, serviceName, targetPath)
			if switchErr == nil && !reset {
				switchErr = errors.New("SCM image path manager disappeared")
			}
			if switchErr != nil {
				return fmt.Errorf("preparing rollback publication: %w (resetting SCM image path failed: %w)", err, switchErr)
			}
			if cleanupRecovery {
				_ = os.Remove(recoveryPath)
			}
			return fmt.Errorf("preparing rollback publication: %w", err)
		}
	}
	if err := moveRollbackRestoreFileNoReplace(tempPath, targetPath); err != nil {
		if restoreErr := moveRollbackRestoreFileNoReplace(asidePath, targetPath); restoreErr != nil {
			return fmt.Errorf("restoring %s: %w (putting the current binary back from %s failed: %w)", targetPath, err, asidePath, restoreErr)
		}
		reset, switchErr := updateWindowsServiceBinaryPathFn(installDir, serviceName, targetPath)
		if switchErr == nil && !reset {
			switchErr = errors.New("SCM image path manager disappeared")
		}
		if switchErr != nil {
			return fmt.Errorf("restoring %s: %w (resetting SCM image path failed: %w)", targetPath, err, switchErr)
		}
		if cleanupRecovery {
			_ = os.Remove(recoveryPath)
		}
		return fmt.Errorf("restoring %s: %w", targetPath, err)
	}
	hitRollbackRestoreCrashBarrier("target-published")
	if managed, err := updateWindowsServiceBinaryPathFn(installDir, serviceName, targetPath); err != nil {
		return fmt.Errorf("restored the rollback binary but could not reset SCM to %s; recovery remains at %s: %w", targetPath, recoveryPath, err)
	} else if !managed {
		return errors.New("restored the rollback binary but the SCM image path manager disappeared")
	}
	hitRollbackRestoreCrashBarrier("service-path-canonical")
	if cleanupRecovery {
		_ = os.Remove(recoveryPath)
	}
	_ = os.Remove(asidePath)
	return nil
}

func copyVerifiedRecoveryImage(sourcePath, recoveryPath string, mode os.FileMode, recorded string) error {
	if _, err := os.Lstat(recoveryPath); err == nil {
		return fmt.Errorf("recovery path %s already exists", recoveryPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checking recovery path %s: %w", recoveryPath, err)
	}
	src, err := openRegularNoFollow(sourcePath)
	if err != nil {
		return fmt.Errorf("opening verified restore image %s: %w", sourcePath, err)
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(recoveryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("creating recovery image %s: %w", recoveryPath, err)
	}
	cleanup := func() {
		_ = dst.Close()
		_ = os.Remove(recoveryPath)
	}
	if err := dst.Chmod(mode); err != nil {
		cleanup()
		return fmt.Errorf("setting recovery image mode: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(dst, h), src); err != nil {
		cleanup()
		return fmt.Errorf("copying recovery image: %w", err)
	}
	if digest := hex.EncodeToString(h.Sum(nil)); digest != recorded {
		cleanup()
		return fmt.Errorf("recovery image hashes to %s, recorded %s", digest, recorded)
	}
	if err := dst.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("syncing recovery image: %w", err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(recoveryPath)
		return fmt.Errorf("closing recovery image: %w", err)
	}
	return nil
}

func hitRollbackRestoreCrashBarrier(stage string) {
	if rollbackRestoreCrashBarrier != nil {
		rollbackRestoreCrashBarrier(stage)
	}
}

func moveRollbackRestoreFile(oldPath, newPath string) error {
	return moveWindowsFile(oldPath, newPath, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func moveRollbackRestoreFileNoReplace(oldPath, newPath string) error {
	return moveWindowsFile(oldPath, newPath, windows.MOVEFILE_WRITE_THROUGH)
}

func moveWindowsFile(oldPath, newPath string, flags uint32) error {
	oldName, err := windows.UTF16PtrFromString(oldPath)
	if err != nil {
		return err
	}
	newName, err := windows.UTF16PtrFromString(newPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(oldName, newName, flags)
}
