package updater

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

type mockUpdateSource struct {
	checkUpdateFn    func(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error)
	downloadUpdateFn func(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error)
}

func (m *mockUpdateSource) CheckUpdate(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error) {
	return m.checkUpdateFn(ctx, currentVersion)
}

func (m *mockUpdateSource) DownloadUpdate(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
	if m.downloadUpdateFn != nil {
		return m.downloadUpdateFn(ctx, info)
	}
	return nil, nil, nil
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func newTestLoggerWithBuffer(buf *bytes.Buffer) *slog.Logger {
	// Debug level so the buffer captures the routine "checking for update" /
	// "no update available" lines (demoted to Debug to keep the journal quiet).
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestNew(t *testing.T) {
	source := &mockUpdateSource{}
	config := &daemonpilot.UpdateConfig{Source: source}
	logger := newTestLogger()

	u := New(config, logger, "/opt/myapp", "myapp")

	if u.config != config {
		t.Error("config not set")
	}
	if u.logger != logger {
		t.Error("logger not set")
	}
	if u.installDir != "/opt/myapp" {
		t.Errorf("installDir = %q, want /opt/myapp", u.installDir)
	}
	if u.binaryName != daemonpilot.InstalledBinaryName("myapp") {
		t.Errorf("binaryName = %q, want %q", u.binaryName, daemonpilot.InstalledBinaryName("myapp"))
	}
}

func TestCheckForUpdate_UpdateAvailable(t *testing.T) {
	expected := &daemonpilot.UpdateInfo{
		Version:         "2.0.0",
		WatchdogVersion: "1",
		Arch:            "linux/amd64",
		Size:            1024,
		SHA256:          "abc123",
	}

	source := &mockUpdateSource{
		checkUpdateFn: func(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error) {
			if currentVersion != "1.0.0" {
				t.Errorf("currentVersion = %q, want 1.0.0", currentVersion)
			}
			return expected, nil
		},
	}

	var buf bytes.Buffer
	u := New(&daemonpilot.UpdateConfig{Source: source}, newTestLoggerWithBuffer(&buf), "/opt/myapp", "myapp")

	info, err := u.CheckForUpdate(context.Background(), "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info != expected {
		t.Errorf("info = %v, want %v", info, expected)
	}
	if !bytes.Contains(buf.Bytes(), []byte("update available")) {
		t.Error("expected 'update available' log message")
	}
}

func TestCheckForUpdate_NoUpdate(t *testing.T) {
	source := &mockUpdateSource{
		checkUpdateFn: func(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error) {
			return nil, nil
		},
	}

	var buf bytes.Buffer
	u := New(&daemonpilot.UpdateConfig{Source: source}, newTestLoggerWithBuffer(&buf), "/opt/myapp", "myapp")

	info, err := u.CheckForUpdate(context.Background(), "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info != nil {
		t.Errorf("info = %v, want nil", info)
	}
	if !bytes.Contains(buf.Bytes(), []byte("no update available")) {
		t.Error("expected 'no update available' log message")
	}
}

func TestCheckForUpdate_Error(t *testing.T) {
	expectedErr := errors.New("network error")

	source := &mockUpdateSource{
		checkUpdateFn: func(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error) {
			return nil, expectedErr
		},
	}

	var buf bytes.Buffer
	u := New(&daemonpilot.UpdateConfig{Source: source}, newTestLoggerWithBuffer(&buf), "/opt/myapp", "myapp")

	info, err := u.CheckForUpdate(context.Background(), "1.0.0")
	if !errors.Is(err, expectedErr) {
		t.Errorf("err = %v, want %v", err, expectedErr)
	}
	if info != nil {
		t.Errorf("info = %v, want nil", info)
	}
	if !bytes.Contains(buf.Bytes(), []byte("update check failed")) {
		t.Error("expected 'update check failed' log message")
	}
}

func TestCheckForUpdate_ContextCancelled(t *testing.T) {
	source := &mockUpdateSource{
		checkUpdateFn: func(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error) {
			return nil, ctx.Err()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	u := New(&daemonpilot.UpdateConfig{Source: source}, newTestLogger(), "/opt/myapp", "myapp")

	info, err := u.CheckForUpdate(ctx, "1.0.0")
	if err == nil {
		t.Error("expected error for cancelled context")
	}
	if info != nil {
		t.Errorf("info = %v, want nil", info)
	}
}

func makeSignedBinary(t *testing.T) (content []byte, hash string, pubKey ed25519.PublicKey, sig []byte) {
	t.Helper()
	content = []byte("#!/bin/sh\necho hello\n")
	h := sha256.Sum256(content)
	hash = hex.EncodeToString(h[:])
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sig = ed25519.Sign(priv, h[:])
	return content, hash, pub, sig
}

func TestDownloadAndVerify_Success(t *testing.T) {
	content, hash, pubKey, sig := makeSignedBinary(t)

	tmpDir := t.TempDir()
	source := &mockUpdateSource{
		checkUpdateFn: func(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error) {
			return nil, nil
		},
		downloadUpdateFn: func(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			return io.NopCloser(bytes.NewReader(content)), sig, nil
		},
	}

	config := &daemonpilot.UpdateConfig{Source: source, PublicKey: pubKey}
	u := New(config, newTestLogger(), tmpDir, "myapp")

	info := &daemonpilot.UpdateInfo{Version: "2.0.0", SHA256: hash}
	path, err := u.DownloadAndVerify(context.Background(), info)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedPath := daemonpilot.StagedNewPath(filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp")))
	if path != expectedPath {
		t.Errorf("path = %q, want %q", path, expectedPath)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !bytes.Equal(data, content) {
		t.Error("file content mismatch")
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS == "windows" {
		if filepath.Ext(path) != ".exe" {
			t.Errorf("staged executable extension = %q, want .exe", filepath.Ext(path))
		}
	} else if fi.Mode().Perm()&0o111 == 0 {
		t.Error("file is not executable")
	}
}

func TestDownloadAndVerify_SignatureContext(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("context-signed artifact")
	digest := sha256.Sum256(content)
	for _, tc := range []struct {
		name, signedContext, verifyContext string
		wantError                          bool
	}{
		{"matching", "myapp-binary-v1\x00", "myapp-binary-v1\x00", false},
		{"missing", "myapp-binary-v1\x00", "", true},
		{"different", "myapp-binary-v1\x00", "myapp-binary-v2\x00", true},
		{"legacy", "", "", false},
		{"legacy with context", "", "myapp-binary-v1\x00", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sig := ed25519.Sign(priv, append([]byte(tc.signedContext), digest[:]...))
			source := &mockUpdateSource{
				downloadUpdateFn: func(context.Context, *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
					return io.NopCloser(bytes.NewReader(content)), sig, nil
				},
			}
			dir := t.TempDir()
			u := New(&daemonpilot.UpdateConfig{Source: source, PublicKey: pub, SignatureContext: tc.verifyContext}, newTestLogger(), dir, "myapp")
			path, err := u.DownloadAndVerify(context.Background(), &daemonpilot.UpdateInfo{Version: "2", SHA256: hex.EncodeToString(digest[:])})
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "signature verification failed") {
					t.Fatalf("expected signature failure, got %v", err)
				}
				if _, err := os.Stat(daemonpilot.StagedNewPath(filepath.Join(dir, daemonpilot.InstalledBinaryName("myapp")))); !os.IsNotExist(err) {
					t.Fatalf("failed verification left staging file: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("staged artifact = %q, error = %v", got, err)
			}
		})
	}
}

func TestDownloadAndVerify_SHA256Mismatch(t *testing.T) {
	content, _, pubKey, sig := makeSignedBinary(t)

	tmpDir := t.TempDir()
	source := &mockUpdateSource{
		checkUpdateFn: func(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error) {
			return nil, nil
		},
		downloadUpdateFn: func(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			return io.NopCloser(bytes.NewReader(content)), sig, nil
		},
	}

	config := &daemonpilot.UpdateConfig{Source: source, PublicKey: pubKey}
	u := New(config, newTestLogger(), tmpDir, "myapp")

	// Valid hex of a digest computed over different bytes: the
	// representation checks pass and the comparison itself must fail.
	wrong := sha256.Sum256([]byte("bytes-other-than-the-staged-content"))
	info := &daemonpilot.UpdateInfo{Version: "2.0.0", SHA256: hex.EncodeToString(wrong[:])}
	_, err := u.DownloadAndVerify(context.Background(), info)
	if err == nil {
		t.Fatal("expected error for SHA256 mismatch")
	}
	if !contains(err.Error(), "digest mismatch") {
		t.Errorf("error = %q, want digest mismatch", err)
	}

	newPath := daemonpilot.StagedNewPath(filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp")))
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Error(".new file should have been deleted after SHA256 mismatch")
	}
}

func TestDownloadAndVerify_InvalidSignature(t *testing.T) {
	content, hash, _, _ := makeSignedBinary(t)

	otherPub, _, _ := ed25519.GenerateKey(nil)

	tmpDir := t.TempDir()
	source := &mockUpdateSource{
		checkUpdateFn: func(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error) {
			return nil, nil
		},
		downloadUpdateFn: func(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			wrongSig := make([]byte, ed25519.SignatureSize)
			return io.NopCloser(bytes.NewReader(content)), wrongSig, nil
		},
	}

	config := &daemonpilot.UpdateConfig{Source: source, PublicKey: otherPub}
	u := New(config, newTestLogger(), tmpDir, "myapp")

	info := &daemonpilot.UpdateInfo{Version: "2.0.0", SHA256: hash}
	_, err := u.DownloadAndVerify(context.Background(), info)
	if err == nil {
		t.Fatal("expected error for invalid signature")
	}
	if !contains(err.Error(), "signature verification failed") {
		t.Errorf("error = %q, want signature verification failed", err)
	}

	newPath := daemonpilot.StagedNewPath(filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp")))
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Error(".new file should have been deleted after signature failure")
	}
}

func TestDownloadAndVerify_DownloadError(t *testing.T) {
	tmpDir := t.TempDir()
	source := &mockUpdateSource{
		checkUpdateFn: func(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error) {
			return nil, nil
		},
		downloadUpdateFn: func(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			return nil, nil, errors.New("download failed")
		},
	}

	config := &daemonpilot.UpdateConfig{Source: source}
	u := New(config, newTestLogger(), tmpDir, "myapp")

	info := &daemonpilot.UpdateInfo{Version: "2.0.0", SHA256: "abc"}
	_, err := u.DownloadAndVerify(context.Background(), info)
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(err.Error(), "download update") {
		t.Errorf("error = %q, want download update error", err)
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func buildProbe(t *testing.T) string {
	t.Helper()
	return testsupport.Fixture(t, "probe", "./testdata/probe")
}

func executableExtension() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func setProbeMode(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("DAEMONPILOT_TEST_PROBE", mode)
}

func TestRunSelftest_Success(t *testing.T) {
	probe := buildProbe(t)
	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), t.TempDir(), "myapp")
	result, err := u.RunSelftest(context.Background(), probe, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.OK || len(result.Checks) != 1 || result.Checks["db"].Message != "connected" {
		t.Fatalf("unexpected selftest result: %+v", result)
	}
}

func TestRunSelftest_Timeout(t *testing.T) {
	setProbeMode(t, "delay")
	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), t.TempDir(), "myapp")
	_, err := u.RunSelftest(context.Background(), buildProbe(t), 100*time.Millisecond)
	if err == nil || !contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want timeout error", err)
	}
}

func TestRunSelftest_InvalidJSON(t *testing.T) {
	setProbeMode(t, "invalid-json")
	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), t.TempDir(), "myapp")
	_, err := u.RunSelftest(context.Background(), buildProbe(t), 5*time.Second)
	if err == nil || !contains(err.Error(), "parse selftest output") {
		t.Fatalf("error = %v, want parse error", err)
	}
}

func TestRunSelftest_FailureResult(t *testing.T) {
	setProbeMode(t, "failure-result")
	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), t.TempDir(), "myapp")
	result, err := u.RunSelftest(context.Background(), buildProbe(t), 5*time.Second)
	if err != nil || result.OK || result.Checks["api"].Message != "unreachable" {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
}

func TestRunSelftest_ProcessError(t *testing.T) {
	setProbeMode(t, "nonzero")
	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), t.TempDir(), "myapp")
	_, err := u.RunSelftest(context.Background(), buildProbe(t), 5*time.Second)
	if err == nil || !contains(err.Error(), "selftest process exited with error") {
		t.Fatalf("error = %v, want exit error", err)
	}
}

func TestGetBinaryWatchdogVersion_Success(t *testing.T) {
	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), t.TempDir(), "myapp")
	version, err := u.GetBinaryWatchdogVersion(context.Background(), buildProbe(t))
	if err != nil || version != "2" {
		t.Fatalf("version = %q, error = %v", version, err)
	}
}

func TestGetBinaryWatchdogVersion_TrimWhitespace(t *testing.T) {
	setProbeMode(t, "whitespace")
	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), t.TempDir(), "myapp")
	version, err := u.GetBinaryWatchdogVersion(context.Background(), buildProbe(t))
	if err != nil || version != "3" {
		t.Fatalf("version = %q, error = %v", version, err)
	}
}

func TestGetBinaryWatchdogVersion_Timeout(t *testing.T) {
	setProbeMode(t, "delay")
	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), t.TempDir(), "myapp")
	_, err := u.GetBinaryWatchdogVersion(context.Background(), buildProbe(t))
	if err == nil || !contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want timeout error", err)
	}
}

func TestGetBinaryWatchdogVersion_EmptyOutput(t *testing.T) {
	setProbeMode(t, "empty")
	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), t.TempDir(), "myapp")
	_, err := u.GetBinaryWatchdogVersion(context.Background(), buildProbe(t))
	if err == nil || !contains(err.Error(), "empty output") {
		t.Fatalf("error = %v, want empty output error", err)
	}
}

func TestGetBinaryWatchdogVersion_ProcessError(t *testing.T) {
	setProbeMode(t, "nonzero")
	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), t.TempDir(), "myapp")
	_, err := u.GetBinaryWatchdogVersion(context.Background(), buildProbe(t))
	if err == nil || !contains(err.Error(), "watchdog-version process exited with error") {
		t.Fatalf("error = %v, want exit error", err)
	}
}

func TestGetBinaryWatchdogVersion_BinaryNotFound(t *testing.T) {
	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), "/tmp", "myapp")
	_, err := u.GetBinaryWatchdogVersion(context.Background(), "/nonexistent/binary")
	if err == nil || !contains(err.Error(), "start watchdog-version process") {
		t.Fatalf("error = %v, want start error", err)
	}
}

func TestRunSelftest_BinaryNotFound(t *testing.T) {
	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), "/tmp", "myapp")
	_, err := u.RunSelftest(context.Background(), "/nonexistent/binary", 5*time.Second)
	if err == nil || !contains(err.Error(), "start selftest process") {
		t.Fatalf("error = %v, want start error", err)
	}
}

func TestApplyFastUpdate_Success(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	newPath := daemonpilot.StagedNewPath(currentPath)
	if err := os.WriteFile(newPath, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	u := New(&daemonpilot.UpdateConfig{}, newTestLoggerWithBuffer(&buf), tmpDir, "myapp")

	err := u.ApplyFastUpdate(context.Background(), newPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current binary: %v", err)
	}
	if string(data) != "new-binary" {
		t.Errorf("current binary content = %q, want %q", string(data), "new-binary")
	}

	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Error(".new file should not exist after rename")
	}

	logOutput := buf.String()
	if !contains(logOutput, "applying fast-path update") {
		t.Error("expected 'applying fast-path update' log")
	}
	if !contains(logOutput, "fast-path update applied") {
		t.Error("expected 'fast-path update applied' log")
	}
}

func TestApplyFastUpdate_NilWorkerCmd(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	newPath := daemonpilot.StagedNewPath(currentPath)
	if err := os.WriteFile(newPath, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), tmpDir, "myapp")

	err := u.ApplyFastUpdate(context.Background(), newPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current binary: %v", err)
	}
	if string(data) != "new-binary" {
		t.Errorf("current binary content = %q, want %q", string(data), "new-binary")
	}
}

func TestApplyFastUpdate_RenameFailure(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp")), []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), tmpDir, "myapp")

	err := u.ApplyFastUpdate(context.Background(), daemonpilot.StagedNewPath(filepath.Join("/nonexistent/path", daemonpilot.InstalledBinaryName("myapp"))))
	if err == nil {
		t.Fatal("expected error for rename failure")
	}
	if !contains(err.Error(), "rename binary") {
		t.Errorf("error = %q, want rename error", err)
	}
}

func TestCleanupStaleFilesRemovesLegacyWindowsRollbackNames(t *testing.T) {
	dir := t.TempDir()
	u := newForPlatform(&daemonpilot.UpdateConfig{}, newTestLogger(), dir, "agent", "windows")
	legacy := filepath.Join(dir, "agent.rollback")
	legacyInvalid := legacy + ".invalid"
	for _, path := range []string{legacy, legacyInvalid} {
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := u.CleanupStaleFiles(); err != nil {
		t.Fatalf("CleanupStaleFiles(): %v", err)
	}
	for _, path := range []string{legacy, legacyInvalid} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("legacy rollback artifact %q survived: %v", path, err)
		}
	}
}

func TestApplyStagedUpdate_Approved(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	newPath := daemonpilot.StagedNewPath(currentPath)
	if err := os.WriteFile(newPath, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	callbackCalled := false
	config := &daemonpilot.UpdateConfig{
		StagedUpdateFunc: func(ctx context.Context, info *daemonpilot.UpdateInfo, report daemonpilot.SelftestResult) (bool, error) {
			callbackCalled = true
			if info.Version != "2.0.0" {
				t.Errorf("info.Version = %q, want 2.0.0", info.Version)
			}
			if !report.OK {
				t.Error("expected selftest to be OK")
			}
			return true, nil
		},
	}

	var buf bytes.Buffer
	u := New(config, newTestLoggerWithBuffer(&buf), tmpDir, "myapp")

	info := &daemonpilot.UpdateInfo{Version: "2.0.0", WatchdogVersion: "2"}
	selftestResult := &daemonpilot.SelftestResult{OK: true}

	err := u.ApplyStagedUpdate(context.Background(), newPath, info, selftestResult)
	if !errors.Is(err, ErrWatchdogRestart) {
		t.Fatalf("err = %v, want ErrWatchdogRestart", err)
	}
	if !callbackCalled {
		t.Error("StagedUpdateFunc was not called")
	}

	data, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current binary: %v", err)
	}
	if string(data) != "new-binary" {
		t.Errorf("current binary content = %q, want %q", string(data), "new-binary")
	}

	rollbackPath := currentPath + ".rollback"
	data, err = os.ReadFile(rollbackPath)
	if err != nil {
		t.Fatalf("read rollback binary: %v", err)
	}
	if string(data) != "old-binary" {
		t.Errorf("rollback binary content = %q, want %q", string(data), "old-binary")
	}

	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Error(".new file should not exist after staged update")
	}

	logOutput := buf.String()
	if !contains(logOutput, "staged update approved") {
		t.Error("expected 'staged update approved' log")
	}
	if !contains(logOutput, "staged-path update applied") {
		t.Error("expected 'staged-path update applied' log")
	}
}

func TestApplyStagedUpdate_Rejected(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	newPath := daemonpilot.StagedNewPath(currentPath)
	if err := os.WriteFile(newPath, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	config := &daemonpilot.UpdateConfig{
		StagedUpdateFunc: func(ctx context.Context, info *daemonpilot.UpdateInfo, report daemonpilot.SelftestResult) (bool, error) {
			return false, nil
		},
	}

	u := New(config, newTestLogger(), tmpDir, "myapp")

	info := &daemonpilot.UpdateInfo{Version: "2.0.0", WatchdogVersion: "2"}
	selftestResult := &daemonpilot.SelftestResult{OK: true}

	err := u.ApplyStagedUpdate(context.Background(), newPath, info, selftestResult)
	if err == nil {
		t.Fatal("expected error for rejected update")
	}
	if !contains(err.Error(), "staged update rejected") {
		t.Errorf("error = %q, want staged update rejected", err)
	}

	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Error(".new file should have been deleted after rejection")
	}

	data, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current binary: %v", err)
	}
	if string(data) != "old-binary" {
		t.Errorf("current binary should be unchanged, got %q", string(data))
	}
}

func TestApplyStagedUpdate_NoStagedUpdateFunc(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	newPath := daemonpilot.StagedNewPath(currentPath)
	if err := os.WriteFile(newPath, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	config := &daemonpilot.UpdateConfig{}
	u := New(config, newTestLogger(), tmpDir, "myapp")

	info := &daemonpilot.UpdateInfo{Version: "2.0.0", WatchdogVersion: "2"}
	selftestResult := &daemonpilot.SelftestResult{OK: true}

	err := u.ApplyStagedUpdate(context.Background(), newPath, info, selftestResult)
	if err == nil {
		t.Fatal("expected error when no StagedUpdateFunc configured")
	}
	if !contains(err.Error(), "no StagedUpdateFunc configured") {
		t.Errorf("error = %q, want no StagedUpdateFunc configured", err)
	}

	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Error(".new file should have been deleted")
	}
}

func TestApplyStagedUpdate_CallbackError(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	newPath := daemonpilot.StagedNewPath(currentPath)
	if err := os.WriteFile(newPath, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	config := &daemonpilot.UpdateConfig{
		StagedUpdateFunc: func(ctx context.Context, info *daemonpilot.UpdateInfo, report daemonpilot.SelftestResult) (bool, error) {
			return false, errors.New("approval service unavailable")
		},
	}

	u := New(config, newTestLogger(), tmpDir, "myapp")

	info := &daemonpilot.UpdateInfo{Version: "2.0.0", WatchdogVersion: "2"}
	selftestResult := &daemonpilot.SelftestResult{OK: true}

	err := u.ApplyStagedUpdate(context.Background(), newPath, info, selftestResult)
	if err == nil {
		t.Fatal("expected error for callback error")
	}
	if !contains(err.Error(), "staged update callback") {
		t.Errorf("error = %q, want staged update callback error", err)
	}
	if !contains(err.Error(), "approval service unavailable") {
		t.Errorf("error = %q, want approval service unavailable", err)
	}

	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Error(".new file should have been deleted after callback error")
	}
}

func TestApplyStagedUpdate_RollbackCreation(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(currentPath, []byte("current-v1"), 0o755); err != nil {
		t.Fatal(err)
	}

	newPath := daemonpilot.StagedNewPath(currentPath)
	if err := os.WriteFile(newPath, []byte("updated-v2"), 0o755); err != nil {
		t.Fatal(err)
	}

	config := &daemonpilot.UpdateConfig{
		StagedUpdateFunc: func(ctx context.Context, info *daemonpilot.UpdateInfo, report daemonpilot.SelftestResult) (bool, error) {
			return true, nil
		},
	}

	u := New(config, newTestLogger(), tmpDir, "myapp")

	info := &daemonpilot.UpdateInfo{Version: "2.0.0", WatchdogVersion: "2"}
	selftestResult := &daemonpilot.SelftestResult{OK: true}

	err := u.ApplyStagedUpdate(context.Background(), newPath, info, selftestResult)
	if !errors.Is(err, ErrWatchdogRestart) {
		t.Fatalf("err = %v, want ErrWatchdogRestart", err)
	}

	rollbackPath := currentPath + ".rollback"
	data, err := os.ReadFile(rollbackPath)
	if err != nil {
		t.Fatalf("rollback file should exist: %v", err)
	}
	if string(data) != "current-v1" {
		t.Errorf("rollback content = %q, want %q", string(data), "current-v1")
	}

	data, err = os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("current binary should exist: %v", err)
	}
	if string(data) != "updated-v2" {
		t.Errorf("current binary content = %q, want %q", string(data), "updated-v2")
	}
}

func TestApplyStagedUpdate_WithRunningWorker(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	newPath := daemonpilot.StagedNewPath(currentPath)
	if err := os.WriteFile(newPath, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	probe := buildProbe(t)
	cmd := exec.Command(probe, "worker")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	config := &daemonpilot.UpdateConfig{
		StagedUpdateFunc: func(ctx context.Context, info *daemonpilot.UpdateInfo, report daemonpilot.SelftestResult) (bool, error) {
			return true, nil
		},
	}

	u := New(config, newTestLogger(), tmpDir, "myapp")

	info := &daemonpilot.UpdateInfo{Version: "2.0.0", WatchdogVersion: "2"}
	selftestResult := &daemonpilot.SelftestResult{OK: true}

	err := u.ApplyStagedUpdate(context.Background(), newPath, info, selftestResult)
	if !errors.Is(err, ErrWatchdogRestart) {
		t.Fatalf("err = %v, want ErrWatchdogRestart", err)
	}

	data, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current binary: %v", err)
	}
	if string(data) != "new-binary" {
		t.Errorf("current binary content = %q, want %q", string(data), "new-binary")
	}
}

// recordTestRollbackDigest binds a hand-written .rollback file to the state
// file the way ApplyStagedUpdate does, so the restore paths accept it.
func recordTestRollbackDigest(t *testing.T, installDir, binaryName, rollbackPath string) {
	t.Helper()
	if err := daemonpilot.RecordRollbackDigest(installDir, binaryName, rollbackPath); err != nil {
		t.Fatalf("record rollback digest: %v", err)
	}
}

func TestCheckRollbackFile_Exists(t *testing.T) {
	tmpDir := t.TempDir()

	binaryPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	rollbackPath := binaryPath + ".rollback"
	if err := os.WriteFile(rollbackPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	recordTestRollbackDigest(t, tmpDir, "myapp", rollbackPath)

	var buf bytes.Buffer
	u := New(&daemonpilot.UpdateConfig{}, newTestLoggerWithBuffer(&buf), tmpDir, "myapp")

	exists, err := u.CheckRollbackFile(context.Background(), binaryPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Error("expected rollback file to exist")
	}
	if !contains(buf.String(), "rollback file detected") {
		t.Error("expected 'rollback file detected' log")
	}
}

func TestCheckRollbackFile_NotExists(t *testing.T) {
	tmpDir := t.TempDir()

	binaryPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))

	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), tmpDir, "myapp")

	exists, err := u.CheckRollbackFile(context.Background(), binaryPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Error("expected rollback file to not exist")
	}
}

func TestHandleRollback_NoConfirmFunc_DeletesRollback(t *testing.T) {
	tmpDir := t.TempDir()

	rollbackPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp")) + ".rollback"
	if err := os.WriteFile(rollbackPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	config := &daemonpilot.UpdateConfig{}
	var buf bytes.Buffer
	u := New(config, newTestLoggerWithBuffer(&buf), tmpDir, "myapp")

	err := u.HandleRollback(context.Background(), rollbackPath, 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(rollbackPath); !os.IsNotExist(err) {
		t.Error("rollback file should have been deleted")
	}
	if !contains(buf.String(), "no RollbackConfirmFunc configured") {
		t.Error("expected 'no RollbackConfirmFunc configured' log")
	}
}

func TestHandleRollback_ConfirmationSuccess(t *testing.T) {
	tmpDir := t.TempDir()

	rollbackPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp")) + ".rollback"
	if err := os.WriteFile(rollbackPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	config := &daemonpilot.UpdateConfig{
		RollbackConfirmFunc: func(ctx context.Context) (bool, error) {
			return true, nil
		},
	}
	var buf bytes.Buffer
	u := New(config, newTestLoggerWithBuffer(&buf), tmpDir, "myapp")

	err := u.HandleRollback(context.Background(), rollbackPath, 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(rollbackPath); !os.IsNotExist(err) {
		t.Error("rollback file should have been deleted after confirmation")
	}
	if !contains(buf.String(), "rollback confirmation received") {
		t.Error("expected 'rollback confirmation received' log")
	}
}

func TestHandleRollback_ConfirmationDenied_RestoresOldBinary(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(currentPath, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	rollbackPath := currentPath + ".rollback"
	if err := os.WriteFile(rollbackPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	recordTestRollbackDigest(t, tmpDir, "myapp", rollbackPath)

	config := &daemonpilot.UpdateConfig{
		RollbackConfirmFunc: func(ctx context.Context) (bool, error) {
			return false, nil
		},
	}
	u := New(config, newTestLogger(), tmpDir, "myapp")

	err := u.HandleRollback(context.Background(), rollbackPath, 5*time.Minute)
	if !errors.Is(err, daemonpilot.ErrRollbackRestored) {
		t.Fatalf("err = %v, want daemonpilot.ErrRollbackRestored", err)
	}

	data, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current binary: %v", err)
	}
	if string(data) != "old-binary" {
		t.Errorf("current binary content = %q, want %q", string(data), "old-binary")
	}

	if _, err := os.Stat(rollbackPath); !os.IsNotExist(err) {
		t.Error("rollback file should have been renamed to current")
	}
}

func TestHandleRollback_Timeout_RestoresOldBinary(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(currentPath, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	rollbackPath := currentPath + ".rollback"
	if err := os.WriteFile(rollbackPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	recordTestRollbackDigest(t, tmpDir, "myapp", rollbackPath)

	config := &daemonpilot.UpdateConfig{
		RollbackConfirmFunc: func(ctx context.Context) (bool, error) {
			<-ctx.Done()
			return false, ctx.Err()
		},
	}
	u := New(config, newTestLogger(), tmpDir, "myapp")

	err := u.HandleRollback(context.Background(), rollbackPath, 100*time.Millisecond)
	if !errors.Is(err, daemonpilot.ErrRollbackRestored) {
		t.Fatalf("err = %v, want daemonpilot.ErrRollbackRestored", err)
	}

	data, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current binary: %v", err)
	}
	if string(data) != "old-binary" {
		t.Errorf("current binary content = %q, want %q", string(data), "old-binary")
	}
}

func TestHandleRollback_CallbackError_RestoresOldBinary(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(currentPath, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	rollbackPath := currentPath + ".rollback"
	if err := os.WriteFile(rollbackPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	recordTestRollbackDigest(t, tmpDir, "myapp", rollbackPath)

	config := &daemonpilot.UpdateConfig{
		RollbackConfirmFunc: func(ctx context.Context) (bool, error) {
			return false, errors.New("health check failed")
		},
	}
	u := New(config, newTestLogger(), tmpDir, "myapp")

	err := u.HandleRollback(context.Background(), rollbackPath, 5*time.Minute)
	if !errors.Is(err, daemonpilot.ErrRollbackRestored) {
		t.Fatalf("err = %v, want daemonpilot.ErrRollbackRestored", err)
	}

	data, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read current binary: %v", err)
	}
	if string(data) != "old-binary" {
		t.Errorf("current binary content = %q, want %q", string(data), "old-binary")
	}
}

func TestHandleRollback_NoConfirmFunc_RollbackFileNotExist(t *testing.T) {
	tmpDir := t.TempDir()

	rollbackPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp")) + ".rollback"

	config := &daemonpilot.UpdateConfig{}
	u := New(config, newTestLogger(), tmpDir, "myapp")

	err := u.HandleRollback(context.Background(), rollbackPath, 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v (should not error if file doesn't exist)", err)
	}
}

func TestApplyStagedUpdate_RenameToCurrentFails(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	config := &daemonpilot.UpdateConfig{
		StagedUpdateFunc: func(ctx context.Context, info *daemonpilot.UpdateInfo, report daemonpilot.SelftestResult) (bool, error) {
			return true, nil
		},
	}

	u := New(config, newTestLogger(), tmpDir, "myapp")

	info := &daemonpilot.UpdateInfo{Version: "2.0.0", WatchdogVersion: "2"}
	selftestResult := &daemonpilot.SelftestResult{OK: true}

	// Use a nonexistent staged binary path so rename to current fails.
	err := u.ApplyStagedUpdate(context.Background(), daemonpilot.StagedNewPath(filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))), info, selftestResult)
	if err == nil {
		t.Fatal("expected error")
	}
	// The backup and digest succeed, then nonexistent.new -> current fails.
	// The current binary was never moved: the executable path must still hold
	// the old content, and the spent rollback copy must be gone.
	data, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("current binary must stay in place: %v", err)
	}
	if string(data) != "old-binary" {
		t.Errorf("current binary content = %q, want %q", string(data), "old-binary")
	}
	if _, err := os.Stat(currentPath + ".rollback"); !os.IsNotExist(err) {
		t.Errorf("spent rollback copy should be removed, got err = %v", err)
	}
}

// A digest-recording failure must undo the backup without ever leaving the
// installed path absent — the crash window this guards against is what made
// a killed watchdog unrecoverable.
func TestApplyStagedUpdate_DigestFailureKeepsCurrentExecutable(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	newPath := daemonpilot.StagedNewPath(currentPath)
	if err := os.WriteFile(newPath, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory at the state-file path makes RecordRollbackDigest fail.
	if err := os.Mkdir(filepath.Join(tmpDir, ".daemonpilot-myapp.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	config := &daemonpilot.UpdateConfig{
		StagedUpdateFunc: func(ctx context.Context, info *daemonpilot.UpdateInfo, report daemonpilot.SelftestResult) (bool, error) {
			return true, nil
		},
	}
	u := New(config, newTestLogger(), tmpDir, "myapp")

	info := &daemonpilot.UpdateInfo{Version: "2.0.0", WatchdogVersion: "2"}
	err := u.ApplyStagedUpdate(context.Background(), newPath, info, &daemonpilot.SelftestResult{OK: true})
	if err == nil {
		t.Fatal("expected error")
	}

	data, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("current binary must stay in place: %v", err)
	}
	if string(data) != "old-binary" {
		t.Errorf("current binary content = %q, want %q", string(data), "old-binary")
	}
	if _, err := os.Stat(currentPath + ".rollback"); !os.IsNotExist(err) {
		t.Errorf("undone backup must be removed, got err = %v", err)
	}
}

// The rollback copy is byte-identical. Whether the installed path also
// stays in place is platform policy: hard link on Unix, move-aside on
// Windows (rollback_backup_*_test.go).
func TestCreateRollbackBackup_PreservesContent(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := createRollbackBackup(currentPath, currentPath+".rollback"); err != nil {
		t.Fatalf("createRollbackBackup() error = %v", err)
	}
	data, err := os.ReadFile(currentPath + ".rollback")
	if err != nil {
		t.Fatalf("read rollback copy: %v", err)
	}
	if string(data) != "old-binary" {
		t.Errorf("rollback content = %q, want %q", string(data), "old-binary")
	}
}

// A stale rollback copy from an earlier update is replaced, matching the
// rename semantics the backup supersedes.
func TestCreateRollbackBackup_ReplacesExistingRollback(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(currentPath+".rollback", []byte("stale-rollback"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := createRollbackBackup(currentPath, currentPath+".rollback"); err != nil {
		t.Fatalf("createRollbackBackup() error = %v", err)
	}
	data, err := os.ReadFile(currentPath + ".rollback")
	if err != nil {
		t.Fatalf("read rollback copy: %v", err)
	}
	if string(data) != "old-binary" {
		t.Errorf("rollback content = %q, want %q", string(data), "old-binary")
	}
}

func TestCleanupStaleFiles_RemovesNewFile(t *testing.T) {
	tmpDir := t.TempDir()
	binaryName := "myapp"
	newPath := daemonpilot.StagedNewPath(filepath.Join(tmpDir, daemonpilot.InstalledBinaryName(binaryName)))

	if err := os.WriteFile(newPath, []byte("stale-binary"), 0o755); err != nil {
		t.Fatalf("create stale file: %v", err)
	}

	u := New(&daemonpilot.UpdateConfig{Source: &mockUpdateSource{}}, newTestLogger(), tmpDir, binaryName)
	if err := u.CleanupStaleFiles(); err != nil {
		t.Fatalf("CleanupStaleFiles() error = %v", err)
	}

	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Errorf("stale .new file should be deleted, got err = %v", err)
	}
}

func TestCleanupStaleFiles_NoStaleFiles(t *testing.T) {
	tmpDir := t.TempDir()
	binaryName := "myapp"

	u := New(&daemonpilot.UpdateConfig{Source: &mockUpdateSource{}}, newTestLogger(), tmpDir, binaryName)
	if err := u.CleanupStaleFiles(); err != nil {
		t.Fatalf("CleanupStaleFiles() should not error when no stale files exist, got %v", err)
	}
}

func TestCleanupStaleFiles_LogsCleanup(t *testing.T) {
	tmpDir := t.TempDir()
	binaryName := "myapp"
	newPath := daemonpilot.StagedNewPath(filepath.Join(tmpDir, daemonpilot.InstalledBinaryName(binaryName)))

	if err := os.WriteFile(newPath, []byte("stale"), 0o755); err != nil {
		t.Fatalf("create stale file: %v", err)
	}

	var buf bytes.Buffer
	logger := newTestLoggerWithBuffer(&buf)
	u := New(&daemonpilot.UpdateConfig{Source: &mockUpdateSource{}}, logger, tmpDir, binaryName)

	if err := u.CleanupStaleFiles(); err != nil {
		t.Fatalf("CleanupStaleFiles() error = %v", err)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "stale .new file") {
		t.Errorf("expected log about stale file cleanup, got: %s", logOutput)
	}
}

func TestDownloadAndVerify_CustomVerifyFunc(t *testing.T) {
	content, hash, _, _ := makeSignedBinary(t)
	customSig := []byte("opaque-app-signature")

	tmpDir := t.TempDir()
	source := &mockUpdateSource{
		downloadUpdateFn: func(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			return io.NopCloser(bytes.NewReader(content)), customSig, nil
		},
	}

	// PublicKey deliberately unrelated to the signature: with VerifyFunc set,
	// the built-in digest check must be bypassed entirely.
	otherPub, _, _ := ed25519.GenerateKey(nil)

	var gotPath string
	var gotSig []byte
	config := &daemonpilot.UpdateConfig{
		Source:           source,
		PublicKey:        otherPub,
		SignatureContext: "ignored-by-custom-verifier\x00",
		VerifyFunc: func(binaryPath string, signature []byte) error {
			gotPath = binaryPath
			gotSig = signature
			return nil
		},
	}
	u := New(config, newTestLogger(), tmpDir, "myapp")

	info := &daemonpilot.UpdateInfo{Version: "2.0.0", SHA256: hash}
	path, err := u.DownloadAndVerify(context.Background(), info)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotPath != path {
		t.Errorf("VerifyFunc binaryPath = %q, want %q", gotPath, path)
	}
	if !bytes.Equal(gotSig, customSig) {
		t.Error("VerifyFunc did not receive the download signature")
	}
}

func TestDownloadAndVerify_CustomVerifyFuncRejects(t *testing.T) {
	content, hash, pubKey, _ := makeSignedBinary(t)

	tmpDir := t.TempDir()
	source := &mockUpdateSource{
		downloadUpdateFn: func(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			return io.NopCloser(bytes.NewReader(content)), []byte("bad"), nil
		},
	}

	config := &daemonpilot.UpdateConfig{
		Source:    source,
		PublicKey: pubKey,
		VerifyFunc: func(binaryPath string, signature []byte) error {
			return errors.New("nope")
		},
	}
	u := New(config, newTestLogger(), tmpDir, "myapp")

	info := &daemonpilot.UpdateInfo{Version: "2.0.0", SHA256: hash}
	if _, err := u.DownloadAndVerify(context.Background(), info); err == nil {
		t.Fatal("expected verification error")
	}

	if _, err := os.Stat(daemonpilot.StagedNewPath(filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp")))); !os.IsNotExist(err) {
		t.Error("staged .new file should be removed after failed verification")
	}
}

func TestDownloadAndVerify_EmptySHA256Skipped(t *testing.T) {
	content, _, pubKey, sig := makeSignedBinary(t)

	tmpDir := t.TempDir()
	source := &mockUpdateSource{
		downloadUpdateFn: func(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			return io.NopCloser(bytes.NewReader(content)), sig, nil
		},
	}

	var buf bytes.Buffer
	config := &daemonpilot.UpdateConfig{Source: source, PublicKey: pubKey}
	u := New(config, newTestLoggerWithBuffer(&buf), tmpDir, "myapp")

	info := &daemonpilot.UpdateInfo{Version: "2.0.0"} // no SHA256
	if _, err := u.DownloadAndVerify(context.Background(), info); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("skipping hash check")) {
		t.Error("expected 'skipping hash check' log message")
	}
}

func TestDownloadAndVerify_SHA256CaseInsensitive(t *testing.T) {
	content, hash, pubKey, sig := makeSignedBinary(t)

	tmpDir := t.TempDir()
	source := &mockUpdateSource{
		downloadUpdateFn: func(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			return io.NopCloser(bytes.NewReader(content)), sig, nil
		},
	}

	config := &daemonpilot.UpdateConfig{Source: source, PublicKey: pubKey}
	u := New(config, newTestLogger(), tmpDir, "myapp")

	info := &daemonpilot.UpdateInfo{Version: "2.0.0", SHA256: strings.ToUpper(hash)}
	if _, err := u.DownloadAndVerify(context.Background(), info); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestHandleRollback_Integrity pins the rule that a .rollback file is only ever
// restored when it still hashes to the digest recorded when the staged update
// displaced it: the install directory is writable by the service's own uid in
// user scope, so anyone sharing that uid could otherwise plant a binary there
// and have it executed with no signature check at all.
func TestHandleRollback_Integrity(t *testing.T) {
	const previous = "previous-binary"

	tests := []struct {
		name        string
		setup       func(t *testing.T, dir, rollbackPath string)
		wantRestore bool
	}{
		{
			name: "recorded digest matches",
			setup: func(t *testing.T, dir, rollbackPath string) {
				t.Helper()
				recordTestRollbackDigest(t, dir, "myapp", rollbackPath)
			},
			wantRestore: true,
		},
		{
			name: "rollback file replaced after recording",
			setup: func(t *testing.T, dir, rollbackPath string) {
				t.Helper()
				recordTestRollbackDigest(t, dir, "myapp", rollbackPath)
				if err := os.WriteFile(rollbackPath, []byte("planted-binary"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:  "no digest ever recorded",
			setup: func(t *testing.T, dir, rollbackPath string) { t.Helper() },
		},
		{
			name: "rollback path swapped for a symlink",
			setup: func(t *testing.T, dir, rollbackPath string) {
				t.Helper()
				recordTestRollbackDigest(t, dir, "myapp", rollbackPath)
				if err := os.Remove(rollbackPath); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(dir, "planted")
				if err := os.WriteFile(target, []byte(previous), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, rollbackPath); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
			rollbackPath := currentPath + ".rollback"
			if err := os.WriteFile(currentPath, []byte("new-binary"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(rollbackPath, []byte(previous), 0o755); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, tmpDir, rollbackPath)

			config := &daemonpilot.UpdateConfig{
				RollbackConfirmFunc: func(ctx context.Context) (bool, error) { return false, nil },
			}
			u := New(config, newTestLogger(), tmpDir, "myapp")

			err := u.HandleRollback(context.Background(), rollbackPath, 5*time.Minute)

			current, readErr := os.ReadFile(currentPath)
			if readErr != nil {
				t.Fatalf("read current binary: %v", readErr)
			}

			if tt.wantRestore {
				if !errors.Is(err, daemonpilot.ErrRollbackRestored) {
					t.Fatalf("err = %v, want daemonpilot.ErrRollbackRestored", err)
				}
				if string(current) != previous {
					t.Errorf("current binary = %q, want %q", current, previous)
				}
				return
			}

			if !errors.Is(err, daemonpilot.ErrRollbackUnverified) {
				t.Fatalf("err = %v, want daemonpilot.ErrRollbackUnverified", err)
			}
			if string(current) != "new-binary" {
				t.Errorf("current binary = %q, want it left untouched", current)
			}
			// The refused copy is moved aside rather than left in place: its mere
			// presence re-arms staged probation at every start, burning a full
			// RollbackTimeout window over a file that can never be restored.
			if _, err := os.Lstat(rollbackPath); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("refused rollback file should have been quarantined, Lstat err = %v", err)
			}
			if _, err := os.Lstat(rollbackPath + ".invalid"); err != nil {
				t.Errorf("refused rollback file should have been moved aside for inspection: %v", err)
			}
		})
	}
}

// TestCheckRollbackFile_UnverifiedReportsAbsent pins that an unverifiable
// rollback file is never reported as a pending rollback: handleRun logs the
// error and starts normally rather than acting on it.
func TestCheckRollbackFile_UnverifiedReportsAbsent(t *testing.T) {
	tmpDir := t.TempDir()
	binaryPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	if err := os.WriteFile(binaryPath+".rollback", []byte("planted"), 0o755); err != nil {
		t.Fatal(err)
	}

	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), tmpDir, "myapp")

	exists, err := u.CheckRollbackFile(context.Background(), binaryPath)
	if exists {
		t.Error("an unverifiable rollback file must not be reported as pending")
	}
	if !errors.Is(err, daemonpilot.ErrRollbackUnverified) {
		t.Fatalf("err = %v, want daemonpilot.ErrRollbackUnverified", err)
	}

	// It is also moved aside: left in place it would be re-examined — and, with
	// StagedRollbackOnUnstable, re-arm a full probation window — at every start.
	if _, err := os.Lstat(binaryPath + ".rollback"); !os.IsNotExist(err) {
		t.Errorf("lstat(.rollback) = %v, want the unverifiable file to be quarantined", err)
	}
	got, err := os.ReadFile(binaryPath + ".rollback.invalid")
	if err != nil {
		t.Fatalf("reading the quarantined file: %v", err)
	}
	if string(got) != "planted" {
		t.Errorf("quarantined file = %q, want %q kept for inspection", got, "planted")
	}
}

// TestCheckRollbackFile_QuarantineClearsDigest pins that quarantining also drops
// the recorded digest, so a later file dropped at the .rollback path cannot
// inherit the digest of the one that was moved aside.
func TestCheckRollbackFile_QuarantineClearsDigest(t *testing.T) {
	tmpDir := t.TempDir()
	binaryPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	rollbackPath := binaryPath + ".rollback"
	if err := os.WriteFile(rollbackPath, []byte("recorded"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := daemonpilot.RecordRollbackDigest(tmpDir, "myapp", rollbackPath); err != nil {
		t.Fatal(err)
	}
	// Tamper after recording: the digest no longer matches the file.
	if err := os.WriteFile(rollbackPath, []byte("planted"), 0o755); err != nil {
		t.Fatal(err)
	}

	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), tmpDir, "myapp")
	if _, err := u.CheckRollbackFile(context.Background(), binaryPath); !errors.Is(err, daemonpilot.ErrRollbackUnverified) {
		t.Fatalf("err = %v, want daemonpilot.ErrRollbackUnverified", err)
	}

	state, err := daemonpilot.LoadState(tmpDir, "myapp")
	if err != nil {
		t.Fatal(err)
	}
	if state.RollbackDigest != "" {
		t.Errorf("RollbackDigest = %q, want it cleared after quarantine", state.RollbackDigest)
	}
}

// TestApplyStagedUpdate_RecordsRollbackDigest pins the other half: the update
// path itself records the digest, so a rollback it created is accepted.
func TestApplyStagedUpdate_RecordsRollbackDigest(t *testing.T) {
	tmpDir := t.TempDir()
	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	newPath := daemonpilot.StagedNewPath(currentPath)
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	config := &daemonpilot.UpdateConfig{
		StagedUpdateFunc: func(ctx context.Context, info *daemonpilot.UpdateInfo, result daemonpilot.SelftestResult) (bool, error) {
			return true, nil
		},
	}
	u := New(config, newTestLogger(), tmpDir, "myapp")

	err := u.ApplyStagedUpdate(context.Background(), newPath,
		&daemonpilot.UpdateInfo{Version: "2.0.0"}, &daemonpilot.SelftestResult{OK: true})
	if !errors.Is(err, ErrWatchdogRestart) {
		t.Fatalf("err = %v, want ErrWatchdogRestart", err)
	}

	rollbackPath := currentPath + ".rollback"
	if err := daemonpilot.VerifyRollbackBinary(tmpDir, "myapp", rollbackPath); err != nil {
		t.Fatalf("the rollback the update created failed verification: %v", err)
	}

	// And the digest is dropped again once the rollback is resolved.
	u.clearRollbackDigest()
	if err := daemonpilot.VerifyRollbackBinary(tmpDir, "myapp", rollbackPath); !errors.Is(err, daemonpilot.ErrRollbackUnverified) {
		t.Fatalf("err = %v, want ErrRollbackUnverified after the digest is cleared", err)
	}
}

// recordSyncSeam swaps the durability seam for one test and returns a pointer
// to the ordered list of sync events it observed.
func recordSyncSeam(t *testing.T) *[]string {
	t.Helper()

	events := []string{}
	origFile, origDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = origFile, origDir })

	syncFile = func(f *os.File) error {
		events = append(events, "file:"+f.Name())
		return origFile(f)
	}
	syncDir = func(dir string) error {
		events = append(events, "dir:"+dir)
		return origDir(dir)
	}
	return &events
}

// TestUpdaterUsesCanonicalInstalledBinaryName is the negative control for the
// watchdog call that once passed Config.Name directly to updater.New: changing
// newForPlatform back to the raw name makes the Windows assertion fail.
func TestUpdaterUsesCanonicalInstalledBinaryName(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want string
	}{
		{goos: "windows", want: "sample-agent.exe"},
		{goos: "linux", want: "sample-agent"},
		{goos: "darwin", want: "sample-agent"},
	} {
		u := newForPlatform(&daemonpilot.UpdateConfig{}, newTestLogger(), t.TempDir(), "sample-agent", tc.goos)
		if u.binaryName != tc.want {
			t.Errorf("binaryName on %s = %q, want %q", tc.goos, u.binaryName, tc.want)
		}
		wantPath := filepath.Join(u.installDir, daemonpilot.InstalledBinaryNameFor("sample-agent", tc.goos))
		if got := filepath.Join(u.installDir, u.binaryName); got != wantPath {
			t.Errorf("replacement target on %s = %q, want %q", tc.goos, got, wantPath)
		}
	}
}

func TestWindowsRollbackPathsShareCanonicalBinaryName(t *testing.T) {
	dir := t.TempDir()
	u := newForPlatform(&daemonpilot.UpdateConfig{
		StagedUpdateFunc: func(context.Context, *daemonpilot.UpdateInfo, daemonpilot.SelftestResult) (bool, error) {
			return true, nil
		},
	}, newTestLogger(), dir, "agent", "windows")
	currentPath := filepath.Join(dir, daemonpilot.InstalledBinaryNameFor("agent", "windows"))
	rollbackPath := currentPath + ".rollback"
	newPath := daemonpilot.StagedNewPath(currentPath)
	if err := os.WriteFile(currentPath, []byte("old"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("new"), 0o750); err != nil {
		t.Fatal(err)
	}

	err := u.ApplyStagedUpdate(context.Background(), newPath, &daemonpilot.UpdateInfo{Version: "2"}, &daemonpilot.SelftestResult{OK: true})
	if !errors.Is(err, ErrWatchdogRestart) {
		t.Fatalf("ApplyStagedUpdate() error = %v, want ErrWatchdogRestart", err)
	}
	if _, err := os.Stat(rollbackPath); err != nil {
		t.Fatalf("rollback producer did not use %q: %v", rollbackPath, err)
	}
	hasRollback, err := u.CheckRollbackFile(context.Background(), currentPath)
	if err != nil || !hasRollback {
		t.Fatalf("rollback consumer disagreed with producer: has=%v err=%v", hasRollback, err)
	}
	if err := u.performRollback(rollbackPath); !errors.Is(err, daemonpilot.ErrRollbackRestored) {
		t.Fatalf("performRollback() error = %v, want ErrRollbackRestored", err)
	}
	got, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("restored content = %q, want old", got)
	}
}

func TestSyncInstallDirSkipsWindows(t *testing.T) {
	original := syncDir
	t.Cleanup(func() { syncDir = original })
	calls := 0
	syncDir = func(string) error {
		calls++
		return nil
	}
	u := newForPlatform(&daemonpilot.UpdateConfig{}, newTestLogger(), t.TempDir(), "agent", "linux")

	u.syncInstallDirFor("windows")
	if calls != 0 {
		t.Fatalf("sync hook called %d times on Windows, want 0", calls)
	}
	u.syncInstallDirFor("linux")
	if calls != 1 {
		t.Fatalf("sync hook called %d times after Linux sync, want 1", calls)
	}
}

func newDownloadUpdater(t *testing.T, installDir string, content []byte, sig []byte, pubKey ed25519.PublicKey) *Updater {
	t.Helper()

	source := &mockUpdateSource{
		downloadUpdateFn: func(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			return io.NopCloser(bytes.NewReader(content)), sig, nil
		},
	}
	return New(&daemonpilot.UpdateConfig{Source: source, PublicKey: pubKey}, newTestLogger(), installDir, "myapp")
}

func TestDownloadAndVerify_SymlinkAtStagingPathNotFollowed(t *testing.T) {
	content, hash, pubKey, sig := makeSignedBinary(t)

	tmpDir := t.TempDir()
	victimPath := filepath.Join(tmpDir, "victim")
	if err := os.WriteFile(victimPath, []byte("do-not-overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}

	newPath := daemonpilot.StagedNewPath(filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp")))
	if err := os.Symlink(victimPath, newPath); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	u := newDownloadUpdater(t, tmpDir, content, sig, pubKey)

	path, err := u.DownloadAndVerify(context.Background(), &daemonpilot.UpdateInfo{Version: "2.0.0", SHA256: hash})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != newPath {
		t.Errorf("path = %q, want %q", path, newPath)
	}

	victim, err := os.ReadFile(victimPath)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(victim) != "do-not-overwrite" {
		t.Errorf("symlink was followed: victim content = %q", string(victim))
	}

	fi, err := os.Lstat(newPath)
	if err != nil {
		t.Fatalf("lstat staging path: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("staging path is still a symlink")
	}
	staged, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read staged binary: %v", err)
	}
	if !bytes.Equal(staged, content) {
		t.Error("staged binary content mismatch")
	}
}

func TestDownloadAndVerify_StaleStagingFileReplaced(t *testing.T) {
	content, hash, pubKey, sig := makeSignedBinary(t)

	tmpDir := t.TempDir()
	newPath := daemonpilot.StagedNewPath(filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp")))
	if err := os.WriteFile(newPath, []byte("stale-leftover-from-an-interrupted-update"), 0o755); err != nil {
		t.Fatal(err)
	}

	u := newDownloadUpdater(t, tmpDir, content, sig, pubKey)

	if _, err := u.DownloadAndVerify(context.Background(), &daemonpilot.UpdateInfo{Version: "2.0.0", SHA256: hash}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	staged, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read staged binary: %v", err)
	}
	if !bytes.Equal(staged, content) {
		t.Error("stale staging file was not replaced")
	}
}

func TestDownloadAndVerify_CreatesMissingInstallDir(t *testing.T) {
	content, hash, pubKey, sig := makeSignedBinary(t)

	installDir := filepath.Join(t.TempDir(), "missing", "install")
	u := newDownloadUpdater(t, installDir, content, sig, pubKey)

	path, err := u.DownloadAndVerify(context.Background(), &daemonpilot.UpdateInfo{Version: "2.0.0", SHA256: hash})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != daemonpilot.StagedNewPath(filepath.Join(installDir, daemonpilot.InstalledBinaryName("myapp"))) {
		t.Errorf("path = %q, want it inside %q", path, installDir)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("staged binary missing: %v", err)
	}
}

func TestDownloadAndVerify_SyncsStagedBinary(t *testing.T) {
	content, hash, pubKey, sig := makeSignedBinary(t)

	tmpDir := t.TempDir()
	events := recordSyncSeam(t)
	u := newDownloadUpdater(t, tmpDir, content, sig, pubKey)

	path, err := u.DownloadAndVerify(context.Background(), &daemonpilot.UpdateInfo{Version: "2.0.0", SHA256: hash})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"file:" + path}
	if len(*events) != len(want) || (*events)[0] != want[0] {
		t.Errorf("sync events = %v, want %v", *events, want)
	}
}

func TestDownloadAndVerify_SyncFailureRemovesStagedBinary(t *testing.T) {
	content, hash, pubKey, sig := makeSignedBinary(t)

	tmpDir := t.TempDir()
	orig := syncFile
	t.Cleanup(func() { syncFile = orig })
	syncFile = func(f *os.File) error { return errors.New("disk on fire") }

	u := newDownloadUpdater(t, tmpDir, content, sig, pubKey)

	_, err := u.DownloadAndVerify(context.Background(), &daemonpilot.UpdateInfo{Version: "2.0.0", SHA256: hash})
	if err == nil {
		t.Fatal("expected an error when the staged binary cannot be synced")
	}
	if !contains(err.Error(), "sync staged binary") {
		t.Errorf("error = %q, want it to mention the failed sync", err)
	}
	if _, err := os.Stat(daemonpilot.StagedNewPath(filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp")))); !os.IsNotExist(err) {
		t.Error("staged file should be removed after a failed sync")
	}
}

func TestApplyFastUpdate_SyncsInstallDirAfterRename(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	newPath := daemonpilot.StagedNewPath(currentPath)
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	events := recordSyncSeam(t)
	origDir := syncDir
	syncDir = func(dir string) error {
		data, err := os.ReadFile(currentPath)
		if err != nil {
			t.Errorf("install directory synced before the rename landed: %v", err)
		} else if string(data) != "new-binary" {
			t.Errorf("install directory synced before the rename landed: current = %q", string(data))
		}
		return origDir(dir)
	}

	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), tmpDir, "myapp")
	if err := u.ApplyFastUpdate(context.Background(), newPath); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(*events) != 1 || (*events)[0] != "dir:"+tmpDir {
		t.Errorf("sync events = %v, want [dir:%s]", *events, tmpDir)
	}
}

func TestApplyFastUpdate_DirSyncFailureIsNotFatal(t *testing.T) {
	tmpDir := t.TempDir()

	currentPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	newPath := daemonpilot.StagedNewPath(currentPath)
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	orig := syncDir
	t.Cleanup(func() { syncDir = orig })
	syncDir = func(dir string) error { return errors.New("fsync unsupported") }

	var buf bytes.Buffer
	u := New(&daemonpilot.UpdateConfig{}, newTestLoggerWithBuffer(&buf), tmpDir, "myapp")
	if err := u.ApplyFastUpdate(context.Background(), newPath); err != nil {
		t.Fatalf("a failed directory sync must not fail an applied update: %v", err)
	}
	if !contains(buf.String(), "failed to sync the install directory") {
		t.Error("expected a warning about the failed directory sync")
	}
}

func TestCleanupStaleFiles_RemovesDanglingSymlink(t *testing.T) {
	tmpDir := t.TempDir()
	newPath := daemonpilot.StagedNewPath(filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp")))
	if err := os.Symlink(filepath.Join(tmpDir, "does-not-exist"), newPath); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), tmpDir, "myapp")
	if err := u.CleanupStaleFiles(); err != nil {
		t.Fatalf("CleanupStaleFiles() error = %v", err)
	}
	if _, err := os.Lstat(newPath); !os.IsNotExist(err) {
		t.Error("dangling staging symlink should be removed")
	}
}

// TestCheckRollbackFile_UnreadableDigestLeavesTheCopyAlone pins that a state
// file that could not be read is not treated as an integrity verdict. The state
// file is rewritten in place, so a start racing an install or update-config can
// observe it truncated — quarantining on that would destroy a valid rollback
// copy over an unrelated command. The check still refuses; it just stays
// retryable.
func TestCheckRollbackFile_UnreadableDigestLeavesTheCopyAlone(t *testing.T) {
	tmpDir := t.TempDir()
	binaryPath := filepath.Join(tmpDir, daemonpilot.InstalledBinaryName("myapp"))
	rollbackPath := binaryPath + ".rollback"
	if err := os.WriteFile(rollbackPath, []byte("previous"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := daemonpilot.RecordRollbackDigest(tmpDir, "myapp", rollbackPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemonpilot.StateFilePath(tmpDir, "myapp"), []byte(`{"rollback_dig`), 0o600); err != nil {
		t.Fatal(err)
	}

	u := New(&daemonpilot.UpdateConfig{}, newTestLogger(), tmpDir, "myapp")

	exists, err := u.CheckRollbackFile(context.Background(), binaryPath)
	if exists {
		t.Error("a rollback file whose digest could not be read must not be reported as pending")
	}
	if !errors.Is(err, daemonpilot.ErrRollbackDigestUnreadable) {
		t.Fatalf("err = %v, want daemonpilot.ErrRollbackDigestUnreadable", err)
	}
	if errors.Is(err, daemonpilot.ErrRollbackUnverified) {
		t.Error("an unreadable digest must not read as an integrity failure")
	}
	if got, err := os.ReadFile(rollbackPath); err != nil || string(got) != "previous" {
		t.Errorf("rollback copy = %q, %v; want it left in place", got, err)
	}
	if _, err := os.Lstat(rollbackPath + ".invalid"); !os.IsNotExist(err) {
		t.Error("the rollback copy was quarantined over a read failure")
	}
}
