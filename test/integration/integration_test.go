package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/updater"
	"github.com/ovineko/daemonpilot/internal/watchdog"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	"github.com/ovineko/procguard"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func makeTestBinary(t *testing.T) (content []byte, hash string, pubKey ed25519.PublicKey, sig []byte) {
	t.Helper()
	content = []byte("#!/bin/sh\necho 'new binary'\n")
	h := sha256.Sum256(content)
	hash = hex.EncodeToString(h[:])
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sig = ed25519.Sign(priv, h[:])
	return content, hash, pub, sig
}

type testUpdateSource struct {
	checkFn    func(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error)
	downloadFn func(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error)
}

func (s *testUpdateSource) CheckUpdate(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error) {
	return s.checkFn(ctx, currentVersion)
}

func (s *testUpdateSource) DownloadUpdate(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
	return s.downloadFn(ctx, info)
}

func TestIntegration_WorkerCrashRestartWithBackoff(t *testing.T) {
	cfg := &daemonpilot.Config{
		Name:            "testapp",
		Version:         "1.0.0",
		WatchdogVersion: "1",
		MaxRestartDelay: 5 * time.Second,
		StableThreshold: 5 * time.Minute,
		WorkerFunc:      func(ctx context.Context) error { <-ctx.Done(); return nil },
		Logger:          discardLogger(),
	}

	w := watchdog.New(cfg)

	var spawnCount atomic.Int32
	var spawnTimes []time.Time
	timeMu := make(chan struct{}, 1)

	binary := buildProcessFixture(t)
	w.SetSpecFunc(func(_ context.Context) (procguard.Spec, error) {
		count := spawnCount.Add(1)

		timeMu <- struct{}{}
		spawnTimes = append(spawnTimes, time.Now())
		<-timeMu

		if count <= 3 {
			cmd := integrationWorkerSpec(binary, 0, 1)
			return cmd, nil
		}
		cmd := integrationWorkerSpec(binary, 30*time.Second, 0)
		return cmd, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	deadline := time.After(testsupport.Scale(30 * time.Second))
	for spawnCount.Load() < 4 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("timed out waiting for 4 spawns")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watchdog Run returned error: %v", err)
		}
	case <-time.After(testsupport.Scale(10 * time.Second)):
		t.Fatal("watchdog did not stop in time")
	}

	if spawnCount.Load() < 4 {
		t.Errorf("expected at least 4 spawns (3 crashes + 1 stable), got %d", spawnCount.Load())
	}

	timeMu <- struct{}{}
	times := make([]time.Time, len(spawnTimes))
	copy(times, spawnTimes)
	<-timeMu

	if len(times) >= 3 {
		delay1 := times[1].Sub(times[0])
		delay2 := times[2].Sub(times[1])
		if delay2 < delay1 {
			t.Errorf("expected increasing backoff delays, got delay1=%v delay2=%v", delay1, delay2)
		}
	}
}

func TestIntegration_FastPathUpdate(t *testing.T) {
	tmpDir := t.TempDir()

	currentBinary := []byte("#!/bin/sh\necho 'old binary'\n")
	currentPath := integrationBinaryPath(tmpDir)
	if err := os.WriteFile(currentPath, currentBinary, 0o755); err != nil {
		t.Fatal(err)
	}

	newContent, newHash, pubKey, newSig := makeTestBinary(t)

	source := &testUpdateSource{
		checkFn: func(_ context.Context, _ string) (*daemonpilot.UpdateInfo, error) {
			return &daemonpilot.UpdateInfo{
				Version:         "2.0.0",
				WatchdogVersion: "1",
				SHA256:          newHash,
			}, nil
		},
		downloadFn: func(_ context.Context, _ *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			return io.NopCloser(bytes.NewReader(newContent)), newSig, nil
		},
	}

	updateCfg := &daemonpilot.UpdateConfig{
		Source:    source,
		PublicKey: pubKey,
	}

	u := updater.New(updateCfg, discardLogger(), tmpDir, "testapp")

	info, err := u.CheckForUpdate(context.Background(), "1.0.0")
	if err != nil {
		t.Fatalf("check for update: %v", err)
	}
	if info == nil {
		t.Fatal("expected update info")
	}
	if info.Version != "2.0.0" {
		t.Errorf("expected version 2.0.0, got %s", info.Version)
	}

	newPath, err := u.DownloadAndVerify(context.Background(), info)
	if err != nil {
		t.Fatalf("download and verify: %v", err)
	}

	err = u.ApplyFastUpdate(context.Background(), newPath)
	if err != nil {
		t.Fatalf("apply fast update: %v", err)
	}

	data, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current binary: %v", err)
	}
	if !bytes.Equal(data, newContent) {
		t.Error("current binary should contain new content after fast-path update")
	}

	newFilePath := currentPath + ".new"
	if _, err := os.Stat(newFilePath); !os.IsNotExist(err) {
		t.Error(".new file should not exist after fast-path update")
	}
}

func TestIntegration_StagedUpdateApprovalRollbackConfirm(t *testing.T) {
	tmpDir := t.TempDir()

	oldContent := []byte("#!/bin/sh\necho 'old binary'\n")
	currentPath := integrationBinaryPath(tmpDir)
	if err := os.WriteFile(currentPath, oldContent, 0o755); err != nil {
		t.Fatal(err)
	}

	newContent, newHash, pubKey, newSig := makeTestBinary(t)

	var approvalCalled bool
	source := &testUpdateSource{
		checkFn: func(_ context.Context, _ string) (*daemonpilot.UpdateInfo, error) {
			return &daemonpilot.UpdateInfo{
				Version:         "2.0.0",
				WatchdogVersion: "2",
				SHA256:          newHash,
			}, nil
		},
		downloadFn: func(_ context.Context, _ *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			return io.NopCloser(bytes.NewReader(newContent)), newSig, nil
		},
	}

	updateCfg := &daemonpilot.UpdateConfig{
		Source:    source,
		PublicKey: pubKey,
		StagedUpdateFunc: func(_ context.Context, _ *daemonpilot.UpdateInfo, _ daemonpilot.SelftestResult) (bool, error) {
			approvalCalled = true
			return true, nil
		},
		RollbackConfirmFunc: func(_ context.Context) (bool, error) {
			return true, nil
		},
		RollbackTimeout: 5 * time.Second,
	}

	u := updater.New(updateCfg, discardLogger(), tmpDir, "testapp")

	info, err := u.CheckForUpdate(context.Background(), "1.0.0")
	if err != nil {
		t.Fatalf("check for update: %v", err)
	}

	newPath, err := u.DownloadAndVerify(context.Background(), info)
	if err != nil {
		t.Fatalf("download and verify: %v", err)
	}

	selftestResult := &daemonpilot.SelftestResult{OK: true, Checks: map[string]daemonpilot.CheckResult{
		"basic": {OK: true, Message: "ok"},
	}}

	err = u.ApplyStagedUpdate(context.Background(), newPath, info, selftestResult)
	if !errors.Is(err, updater.ErrWatchdogRestart) {
		t.Fatalf("expected ErrWatchdogRestart, got: %v", err)
	}
	if !approvalCalled {
		t.Error("expected StagedUpdateFunc to be called")
	}

	data, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current binary: %v", err)
	}
	if !bytes.Equal(data, newContent) {
		t.Error("current binary should contain new content after staged update")
	}

	rollbackPath := currentPath + ".rollback"
	rollbackData, err := os.ReadFile(rollbackPath)
	if err != nil {
		t.Fatalf("read rollback binary: %v", err)
	}
	if !bytes.Equal(rollbackData, oldContent) {
		t.Error("rollback binary should contain old content")
	}

	hasRollback, err := u.CheckRollbackFile(context.Background(), currentPath)
	if err != nil {
		t.Fatalf("check rollback: %v", err)
	}
	if !hasRollback {
		t.Fatal("expected rollback file to exist")
	}

	err = u.HandleRollback(context.Background(), rollbackPath, 5*time.Second)
	if err != nil {
		t.Fatalf("handle rollback: %v", err)
	}

	if _, err := os.Stat(rollbackPath); !os.IsNotExist(err) {
		t.Error("rollback file should be deleted after successful confirmation")
	}

	data, err = os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current binary after confirmation: %v", err)
	}
	if !bytes.Equal(data, newContent) {
		t.Error("current binary should still be new content after rollback confirmation")
	}
}

func TestIntegration_StagedUpdateRollbackTimeout(t *testing.T) {
	tmpDir := t.TempDir()

	oldContent := []byte("#!/bin/sh\necho 'old binary'\n")
	currentPath := integrationBinaryPath(tmpDir)
	if err := os.WriteFile(currentPath, oldContent, 0o755); err != nil {
		t.Fatal(err)
	}

	newContent, newHash, pubKey, newSig := makeTestBinary(t)

	source := &testUpdateSource{
		checkFn: func(_ context.Context, _ string) (*daemonpilot.UpdateInfo, error) {
			return &daemonpilot.UpdateInfo{
				Version:         "2.0.0",
				WatchdogVersion: "2",
				SHA256:          newHash,
			}, nil
		},
		downloadFn: func(_ context.Context, _ *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			return io.NopCloser(bytes.NewReader(newContent)), newSig, nil
		},
	}

	updateCfg := &daemonpilot.UpdateConfig{
		Source:    source,
		PublicKey: pubKey,
		StagedUpdateFunc: func(_ context.Context, _ *daemonpilot.UpdateInfo, _ daemonpilot.SelftestResult) (bool, error) {
			return true, nil
		},
		RollbackConfirmFunc: func(ctx context.Context) (bool, error) {
			<-ctx.Done()
			return false, ctx.Err()
		},
		RollbackTimeout: 100 * time.Millisecond,
	}

	u := updater.New(updateCfg, discardLogger(), tmpDir, "testapp")

	info, err := u.CheckForUpdate(context.Background(), "1.0.0")
	if err != nil {
		t.Fatalf("check for update: %v", err)
	}

	newPath, err := u.DownloadAndVerify(context.Background(), info)
	if err != nil {
		t.Fatalf("download and verify: %v", err)
	}

	selftestResult := &daemonpilot.SelftestResult{OK: true}
	err = u.ApplyStagedUpdate(context.Background(), newPath, info, selftestResult)
	if !errors.Is(err, updater.ErrWatchdogRestart) {
		t.Fatalf("expected ErrWatchdogRestart, got: %v", err)
	}

	rollbackPath := currentPath + ".rollback"
	err = u.HandleRollback(context.Background(), rollbackPath, 100*time.Millisecond)
	if !errors.Is(err, daemonpilot.ErrRollbackRestored) {
		t.Fatalf("expected ErrRollbackRestored, got: %v", err)
	}

	data, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current binary after rollback: %v", err)
	}
	if !bytes.Equal(data, oldContent) {
		t.Error("current binary should be restored to old content after rollback timeout")
	}

	if _, err := os.Stat(rollbackPath); !os.IsNotExist(err) {
		t.Error("rollback file should not exist after restore (renamed to current)")
	}
}

func TestIntegration_GracefulShutdown(t *testing.T) {
	cfg := &daemonpilot.Config{
		Name:            "testapp",
		Version:         "1.0.0",
		WatchdogVersion: "1",
		GracefulTimeout: 5 * time.Second,
		MaxRestartDelay: 1 * time.Second,
		StableThreshold: 5 * time.Minute,
		WorkerFunc:      func(ctx context.Context) error { <-ctx.Done(); return nil },
		Logger:          discardLogger(),
	}

	w := watchdog.New(cfg)

	var workerStarted atomic.Bool
	binary := buildProcessFixture(t)
	w.SetSpecFunc(func(_ context.Context) (procguard.Spec, error) {
		workerStarted.Store(true)
		cmd := integrationWorkerSpec(binary, 60*time.Second, 0)
		return cmd, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	for !workerStarted.Load() {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watchdog Run returned error on shutdown: %v", err)
		}
	case <-time.After(testsupport.Scale(10 * time.Second)):
		t.Fatal("watchdog did not shut down within timeout")
	}

	if !w.IsShuttingDown() {
		t.Error("watchdog should be in shutting down state")
	}
}

func TestIntegration_SelftestFailureRejectsUpdate(t *testing.T) {
	tmpDir := t.TempDir()

	currentContent := []byte("#!/bin/sh\necho 'current'\n")
	currentPath := integrationBinaryPath(tmpDir)
	if err := os.WriteFile(currentPath, currentContent, 0o755); err != nil {
		t.Fatal(err)
	}

	newContent, newHash, pubKey, newSig := makeTestBinary(t)

	source := &testUpdateSource{
		checkFn: func(_ context.Context, _ string) (*daemonpilot.UpdateInfo, error) {
			return &daemonpilot.UpdateInfo{
				Version:         "2.0.0",
				WatchdogVersion: "1",
				SHA256:          newHash,
			}, nil
		},
		downloadFn: func(_ context.Context, _ *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			return io.NopCloser(bytes.NewReader(newContent)), newSig, nil
		},
	}

	updateCfg := &daemonpilot.UpdateConfig{
		Source:    source,
		PublicKey: pubKey,
	}

	u := updater.New(updateCfg, discardLogger(), tmpDir, "testapp")

	info, err := u.CheckForUpdate(context.Background(), "1.0.0")
	if err != nil {
		t.Fatalf("check for update: %v", err)
	}

	newPath, err := u.DownloadAndVerify(context.Background(), info)
	if err != nil {
		t.Fatalf("download and verify: %v", err)
	}

	failScript := testsupport.Fixture(t, "update", "../../internal/watchdog/testdata/updatefixture")
	failMarker := filepath.Join(filepath.Dir(failScript), "fail-selftest")
	if err := os.WriteFile(failMarker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := u.RunSelftest(context.Background(), failScript, 5*time.Second)
	if err != nil {
		t.Fatalf("run selftest: %v", err)
	}
	if result.OK {
		t.Fatal("expected selftest to fail")
	}

	if !result.OK {
		if err := os.Remove(newPath); err != nil {
			t.Fatalf("remove new binary: %v", err)
		}
	}

	data, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current binary: %v", err)
	}
	if !bytes.Equal(data, currentContent) {
		t.Error("current binary should remain unchanged when selftest fails")
	}

	newFilePath := currentPath + ".new"
	if _, err := os.Stat(newFilePath); !os.IsNotExist(err) {
		t.Error(".new file should be cleaned up after selftest failure")
	}
}

func TestIntegration_ExecThroughChainCollapse(t *testing.T) {
	tmpDir := t.TempDir()
	binaryName := daemonpilot.InstalledBinaryName("testapp")
	binaryPath := filepath.Join(tmpDir, binaryName)
	nextPath := filepath.Join(tmpDir, "testapp.next"+filepath.Ext(binaryName))
	versionFile := filepath.Join(tmpDir, "version")
	releaseFile := filepath.Join(tmpDir, "release-v2")
	buildExecThroughFixture(t, binaryPath, "v2")
	buildExecThroughFixture(t, nextPath, "v3")
	t.Setenv("FIXTURE_VERSION_FILE", versionFile)
	t.Setenv("FIXTURE_RELEASE_FILE", releaseFile)

	cfg := &daemonpilot.Config{
		Name:            "testapp",
		Version:         "1.0.0",
		WatchdogVersion: "1",
		InstallDir:      tmpDir,
		MaxRestartDelay: 200 * time.Millisecond,
		StableThreshold: 5 * time.Minute,
		GracefulTimeout: 5 * time.Second,
		WorkerFunc:      func(ctx context.Context) error { <-ctx.Done(); return nil },
		Logger:          discardLogger(),
	}
	w := watchdog.New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(15*time.Second))
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.RunExecThroughChildExported(ctx)
		close(done)
	}()

	waitForFile := func(path, expected string, timeout time.Duration) bool {
		deadline := time.After(testsupport.Scale(timeout))
		for {
			select {
			case <-deadline:
				return false
			default:
				data, err := os.ReadFile(path)
				if err == nil && strings.TrimSpace(string(data)) == expected {
					return true
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testsupport.Scale(10 * time.Second)):
			t.Fatal("proxy mode did not shut down")
		}
	}

	if !waitForFile(versionFile, "v2", 5*time.Second) {
		stop()
		t.Fatal("v2 child never started")
	}
	if err := os.WriteFile(releaseFile, []byte("release"), 0o600); err != nil {
		stop()
		t.Fatalf("release v2 child: %v", err)
	}
	if !waitForFile(versionFile+".done", "done", 5*time.Second) {
		stop()
		t.Fatal("v2 child did not exit")
	}

	replaced := false
	deadline := time.Now().Add(testsupport.Scale(5 * time.Second))
	for time.Now().Before(deadline) {
		if err := os.Rename(nextPath, binaryPath); err == nil {
			replaced = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !replaced {
		stop()
		t.Fatal("could not replace the v2 executable with v3")
	}
	if !waitForFile(versionFile, "v3", 5*time.Second) {
		stop()
		t.Fatal("v3 child never started after chain collapse")
	}
	if !w.InProxyMode() {
		t.Error("root watchdog should remain in proxy mode throughout chain collapse")
	}
	stop()
}

func buildExecThroughFixture(t *testing.T, outputPath, version string) {
	t.Helper()
	fixturePath := testsupport.Fixture(
		t,
		"execfixture-"+version,
		"./testdata/execfixture",
		"-ldflags",
		"-X main.version="+version,
	)
	binary, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read %s exec-through fixture: %v", version, err)
	}
	if err := os.WriteFile(outputPath, binary, 0o700); err != nil {
		t.Fatalf("install %s exec-through fixture: %v", version, err)
	}
}

func buildProcessFixture(t *testing.T) string {
	t.Helper()
	return testsupport.Fixture(t, "process", "./testdata/processfixture")
}

func integrationWorkerSpec(binary string, delay time.Duration, code int) procguard.Spec {
	return procguard.Spec{Path: binary, Env: append(os.Environ(), "FIXTURE_DELAY="+delay.String(), fmt.Sprintf("FIXTURE_CODE=%d", code)), GracePeriod: 100 * time.Millisecond}
}

func integrationBinaryPath(dir string) string {
	return filepath.Join(dir, daemonpilot.InstalledBinaryName("testapp"))
}
