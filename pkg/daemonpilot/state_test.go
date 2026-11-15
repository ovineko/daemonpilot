package daemonpilot

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStateFilePath(t *testing.T) {
	tests := []struct {
		name        string
		installDir  string
		serviceName string
		want        string
	}{
		{
			"standard path",
			"/opt/myservice",
			"myservice",
			filepath.Join("/opt/myservice", ".daemonpilot-myservice.json"),
		},
		{
			"different service name",
			"/usr/local/bin",
			"test-app",
			filepath.Join("/usr/local/bin", ".daemonpilot-test-app.json"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StateFilePath(tt.installDir, tt.serviceName)
			if got != tt.want {
				t.Errorf("StateFilePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSaveState(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 3, 20, 15, 30, 0, 0, time.UTC)
	state := &StateData{
		ServiceName:  "myservice",
		InitSystem:   "systemd",
		ManifestPath: "/etc/systemd/system/myservice.service",
		BinaryPath:   "/opt/myservice/myservice",
		InstalledAt:  ts,
		Version:      "1.5.0",
	}

	if err := SaveState(dir, state); err != nil {
		t.Fatalf("SaveState() error: %v", err)
	}

	path := filepath.Join(dir, ".daemonpilot-myservice.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading state file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("state file is empty")
	}

	// Verify round-trip by loading back
	loaded, err := LoadState(dir, "myservice")
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	if loaded.ServiceName != state.ServiceName {
		t.Errorf("ServiceName: got %q, want %q", loaded.ServiceName, state.ServiceName)
	}
	if loaded.InitSystem != state.InitSystem {
		t.Errorf("InitSystem: got %q, want %q", loaded.InitSystem, state.InitSystem)
	}
	if loaded.ManifestPath != state.ManifestPath {
		t.Errorf("ManifestPath: got %q, want %q", loaded.ManifestPath, state.ManifestPath)
	}
	if loaded.BinaryPath != state.BinaryPath {
		t.Errorf("BinaryPath: got %q, want %q", loaded.BinaryPath, state.BinaryPath)
	}
	if !loaded.InstalledAt.Equal(state.InstalledAt) {
		t.Errorf("InstalledAt: got %v, want %v", loaded.InstalledAt, state.InstalledAt)
	}
	if loaded.Version != state.Version {
		t.Errorf("Version: got %q, want %q", loaded.Version, state.Version)
	}
}

func TestLoadState_FileExists(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	state := &StateData{
		ServiceName:  "test-svc",
		InitSystem:   "launchd",
		ManifestPath: "/Library/LaunchDaemons/com.test-svc.plist",
		BinaryPath:   "/usr/local/bin/test-svc",
		InstalledAt:  ts,
		Version:      "2.0.0",
	}

	if err := SaveState(dir, state); err != nil {
		t.Fatalf("SaveState() error: %v", err)
	}

	loaded, err := LoadState(dir, "test-svc")
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadState() returned nil")
	}
	if loaded.ServiceName != "test-svc" {
		t.Errorf("ServiceName: got %q, want %q", loaded.ServiceName, "test-svc")
	}
	if loaded.InitSystem != "launchd" {
		t.Errorf("InitSystem: got %q, want %q", loaded.InitSystem, "launchd")
	}
}

func TestLoadState_FileMissing(t *testing.T) {
	dir := t.TempDir()
	loaded, err := LoadState(dir, "nonexistent")
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	if loaded != nil {
		t.Errorf("LoadState() = %v, want nil for missing file", loaded)
	}
}

func TestLoadState_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".daemonpilot-corrupt.json")
	if err := os.WriteFile(path, []byte("{invalid json"), 0o644); err != nil {
		t.Fatalf("writing corrupt file: %v", err)
	}

	loaded, err := LoadState(dir, "corrupt")
	if err == nil {
		t.Fatal("LoadState() expected error for corrupt JSON")
	}
	if loaded != nil {
		t.Errorf("LoadState() = %v, want nil for corrupt JSON", loaded)
	}
}

func TestDeleteState(t *testing.T) {
	dir := t.TempDir()
	state := &StateData{
		ServiceName: "delete-me",
		InitSystem:  "systemd",
	}
	if err := SaveState(dir, state); err != nil {
		t.Fatalf("SaveState() error: %v", err)
	}

	if err := DeleteState(dir, "delete-me"); err != nil {
		t.Fatalf("DeleteState() error: %v", err)
	}

	// Verify file is gone
	path := filepath.Join(dir, ".daemonpilot-delete-me.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("state file still exists after DeleteState()")
	}
}

func TestDeleteState_FileMissing(t *testing.T) {
	dir := t.TempDir()
	if err := DeleteState(dir, "nonexistent"); err != nil {
		t.Fatalf("DeleteState() error for missing file: %v", err)
	}
}

// TestSaveState_SystemScopeBytesUnchanged pins the on-disk form of a
// system-scope state file: the scope fields must stay absent so a file written
// by the new code is byte-identical to one written before scopes existed.
func TestSaveState_SystemScopeBytesUnchanged(t *testing.T) {
	dir := t.TempDir()
	state := &StateData{
		ServiceName:  "myservice",
		InitSystem:   "systemd",
		ManifestPath: "/etc/systemd/system/myservice.service",
		BinaryPath:   "/opt/myservice/myservice",
		InstalledAt:  time.Date(2026, 3, 20, 15, 30, 0, 0, time.UTC),
		Version:      "1.5.0",
	}

	if err := SaveState(dir, state); err != nil {
		t.Fatalf("SaveState() error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".daemonpilot-myservice.json"))
	if err != nil {
		t.Fatalf("reading state file: %v", err)
	}

	want := `{
  "service_name": "myservice",
  "init_system": "systemd",
  "manifest_path": "/etc/systemd/system/myservice.service",
  "binary_path": "/opt/myservice/myservice",
  "installed_at": "2026-03-20T15:30:00Z",
  "version": "1.5.0"
}`
	if string(data) != want {
		t.Errorf("state file contents:\ngot:\n%s\nwant:\n%s", data, want)
	}
}

func TestSaveState_UserScopeFields(t *testing.T) {
	dir := t.TempDir()
	state := &StateData{
		ServiceName:   "myservice",
		InitSystem:    "systemd",
		InstalledAt:   time.Date(2026, 3, 20, 15, 30, 0, 0, time.UTC),
		Scope:         ScopeUser,
		InstallDir:    "/home/alice/.local/share/myservice",
		Lingering:     true,
		LaunchdDomain: "gui/501",
	}

	if err := SaveState(dir, state); err != nil {
		t.Fatalf("SaveState() error: %v", err)
	}

	// The json tag is asserted directly: every lifecycle command re-resolves the
	// domain when the field comes back empty, so a tag typo would silently
	// restore the orphaned-agent bug the field exists to prevent.
	raw, err := os.ReadFile(filepath.Join(dir, ".daemonpilot-myservice.json"))
	if err != nil {
		t.Fatalf("reading state file: %v", err)
	}
	if !strings.Contains(string(raw), `"launchd_domain": "gui/501"`) {
		t.Errorf("state file is missing the launchd_domain key:\n%s", raw)
	}

	loaded, err := LoadState(dir, "myservice")
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	if loaded.Scope != ScopeUser {
		t.Errorf("Scope: got %q, want %q", loaded.Scope, ScopeUser)
	}
	if loaded.InstallDir != state.InstallDir {
		t.Errorf("InstallDir: got %q, want %q", loaded.InstallDir, state.InstallDir)
	}
	if !loaded.Lingering {
		t.Error("Lingering: got false, want true")
	}
	if loaded.LaunchdDomain != state.LaunchdDomain {
		t.Errorf("LaunchdDomain: got %q, want %q", loaded.LaunchdDomain, state.LaunchdDomain)
	}
	if loaded.EffectiveScope() != ScopeUser {
		t.Errorf("EffectiveScope: got %q, want %q", loaded.EffectiveScope(), ScopeUser)
	}
}

// TestLoadState_LegacyFileReadsAsSystemScope covers a state file written before
// the scope fields existed.
func TestLoadState_LegacyFileReadsAsSystemScope(t *testing.T) {
	dir := t.TempDir()
	legacy := `{
  "service_name": "legacy",
  "init_system": "systemd",
  "manifest_path": "/etc/systemd/system/legacy.service",
  "binary_path": "/opt/legacy/legacy",
  "installed_at": "2026-03-20T15:30:00Z",
  "version": "1.0.0"
}`
	if err := os.WriteFile(filepath.Join(dir, ".daemonpilot-legacy.json"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("writing legacy state file: %v", err)
	}

	loaded, err := LoadState(dir, "legacy")
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	if loaded.Scope != "" {
		t.Errorf("Scope: got %q, want empty", loaded.Scope)
	}
	if loaded.EffectiveScope() != ScopeSystem {
		t.Errorf("EffectiveScope: got %q, want %q", loaded.EffectiveScope(), ScopeSystem)
	}
	if loaded.InstallDir != "" {
		t.Errorf("InstallDir: got %q, want empty", loaded.InstallDir)
	}
}

func TestFileDigestRefusesNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := fileDigest(link); err == nil {
		t.Fatal("fileDigest followed a symlink, want a refusal")
	}

	digest, err := fileDigest(target)
	if err != nil {
		t.Fatalf("fileDigest(regular file): %v", err)
	}
	sum := sha256.Sum256([]byte("payload"))
	if want := hex.EncodeToString(sum[:]); digest != want {
		t.Errorf("digest = %q, want %q", digest, want)
	}
}

func TestVerifyRollbackBinary(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir, rollbackPath string)
		want  bool // want ErrRollbackUnverified
	}{
		{
			name: "recorded digest matches",
			setup: func(t *testing.T, dir, rollbackPath string) {
				t.Helper()
				if err := RecordRollbackDigest(dir, "svc", rollbackPath); err != nil {
					t.Fatalf("RecordRollbackDigest: %v", err)
				}
			},
		},
		{
			name: "contents changed after recording",
			setup: func(t *testing.T, dir, rollbackPath string) {
				t.Helper()
				if err := RecordRollbackDigest(dir, "svc", rollbackPath); err != nil {
					t.Fatalf("RecordRollbackDigest: %v", err)
				}
				if err := os.WriteFile(rollbackPath, []byte("planted"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: true,
		},
		{
			name:  "no state file at all",
			setup: func(t *testing.T, dir, rollbackPath string) { t.Helper() },
			want:  true,
		},
		{
			name: "state file without a recorded digest",
			setup: func(t *testing.T, dir, rollbackPath string) {
				t.Helper()
				if err := SaveState(dir, &StateData{ServiceName: "svc", InitSystem: "systemd"}); err != nil {
					t.Fatalf("SaveState: %v", err)
				}
			},
			want: true,
		},
		{
			name: "rollback path is a symlink onto matching content",
			setup: func(t *testing.T, dir, rollbackPath string) {
				t.Helper()
				if err := RecordRollbackDigest(dir, "svc", rollbackPath); err != nil {
					t.Fatalf("RecordRollbackDigest: %v", err)
				}
				if err := os.Remove(rollbackPath); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(dir, "elsewhere")
				if err := os.WriteFile(target, []byte("previous binary"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, rollbackPath); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			rollbackPath := filepath.Join(dir, "svc.rollback")
			if err := os.WriteFile(rollbackPath, []byte("previous binary"), 0o755); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, dir, rollbackPath)

			err := VerifyRollbackBinary(dir, "svc", rollbackPath)
			if got := errors.Is(err, ErrRollbackUnverified); got != tt.want {
				t.Fatalf("VerifyRollbackBinary() error = %v, want ErrRollbackUnverified = %v", err, tt.want)
			}
		})
	}
}

// A rollback copy that cannot be *read* is refused, but must not be reported as
// ErrRollbackUnverified: that daemonpilot is what makes every caller quarantine the
// file, and a failed open says nothing about its provenance. Quarantining on it
// would destroy the valid safety net over a transient condition.
func TestVerifyRollbackBinaryUnreadableCopyIsNotAVerdict(t *testing.T) {
	// Root bypasses the mode bits, and Windows does not deny reads on them, so
	// the hosts that cannot produce the refusal skip rather than assert it.
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("host cannot make a file unreadable through its mode bits")
	}

	dir := t.TempDir()
	rollbackPath := filepath.Join(dir, "svc.rollback")
	if err := os.WriteFile(rollbackPath, []byte("previous binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RecordRollbackDigest(dir, "svc", rollbackPath); err != nil {
		t.Fatalf("RecordRollbackDigest: %v", err)
	}
	if err := os.Chmod(rollbackPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(rollbackPath, 0o755) })

	err := VerifyRollbackBinary(dir, "svc", rollbackPath)
	if !errors.Is(err, ErrRollbackDigestUnreadable) {
		t.Fatalf("VerifyRollbackBinary() error = %v, want ErrRollbackDigestUnreadable", err)
	}
	if errors.Is(err, ErrRollbackUnverified) {
		t.Fatalf("VerifyRollbackBinary() error = %v, must not wrap ErrRollbackUnverified", err)
	}

	restored, err := RestoreRollbackBinary(dir, "svc", rollbackPath, filepath.Join(dir, "svc"))
	if restored {
		t.Fatal("RestoreRollbackBinary() restored an unreadable copy")
	}
	if !errors.Is(err, ErrRollbackDigestUnreadable) || errors.Is(err, ErrRollbackUnverified) {
		t.Fatalf("RestoreRollbackBinary() error = %v, want ErrRollbackDigestUnreadable and not ErrRollbackUnverified", err)
	}
}

func TestRecordRollbackDigestKeepsExistingState(t *testing.T) {
	dir := t.TempDir()
	rollbackPath := filepath.Join(dir, "svc.rollback")
	if err := os.WriteFile(rollbackPath, []byte("previous binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(dir, &StateData{ServiceName: "svc", InitSystem: "launchd", LaunchdDomain: "gui/501"}); err != nil {
		t.Fatal(err)
	}

	if err := RecordRollbackDigest(dir, "svc", rollbackPath); err != nil {
		t.Fatalf("RecordRollbackDigest: %v", err)
	}

	state, err := LoadState(dir, "svc")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.InitSystem != "launchd" || state.LaunchdDomain != "gui/501" {
		t.Errorf("recording a digest lost state: %+v", state)
	}
	if state.RollbackDigest == "" {
		t.Error("digest was not recorded")
	}

	if err := ClearRollbackDigest(dir, "svc"); err != nil {
		t.Fatalf("ClearRollbackDigest: %v", err)
	}
	state, err = LoadState(dir, "svc")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.RollbackDigest != "" {
		t.Errorf("digest = %q, want it cleared", state.RollbackDigest)
	}
	if state.InitSystem != "launchd" {
		t.Errorf("clearing a digest lost state: %+v", state)
	}
}

func TestClearRollbackDigestWithoutStateFile(t *testing.T) {
	dir := t.TempDir()
	if err := ClearRollbackDigest(dir, "svc"); err != nil {
		t.Fatalf("ClearRollbackDigest with no state file: %v", err)
	}
	if _, err := os.Stat(StateFilePath(dir, "svc")); !os.IsNotExist(err) {
		t.Error("ClearRollbackDigest created a state file")
	}
}

// SaveState derives the file path from state.ServiceName, and the state file is
// writable by the unprivileged service account by design. Record/Clear must pin
// the name back to the one they were asked for, so a file naming something else
// cannot redirect the write away from the file it was just read from.
func TestRollbackDigestPinsTheServiceName(t *testing.T) {
	dir := t.TempDir()
	const name = "testapp"

	if err := os.WriteFile(StateFilePath(dir, name),
		[]byte(`{"service_name":"elsewhere"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rollbackPath := filepath.Join(dir, name+".rollback")
	if err := os.WriteFile(rollbackPath, []byte("previous binary"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RecordRollbackDigest(dir, name, rollbackPath); err != nil {
		t.Fatalf("RecordRollbackDigest: %v", err)
	}
	if _, err := os.Stat(StateFilePath(dir, "elsewhere")); !os.IsNotExist(err) {
		t.Errorf("the write followed the file's own service_name: %v", err)
	}
	if err := VerifyRollbackBinary(dir, name, rollbackPath); err != nil {
		t.Fatalf("the digest was not recorded under %q: %v", name, err)
	}

	if err := ClearRollbackDigest(dir, name); err != nil {
		t.Fatalf("ClearRollbackDigest: %v", err)
	}
	if _, err := os.Stat(StateFilePath(dir, "elsewhere")); !os.IsNotExist(err) {
		t.Errorf("the clear followed the file's own service_name: %v", err)
	}
	if err := VerifyRollbackBinary(dir, name, rollbackPath); !errors.Is(err, ErrRollbackUnverified) {
		t.Errorf("after the clear, VerifyRollbackBinary = %v, want ErrRollbackUnverified", err)
	}
}

func TestRestoreRollbackBinary(t *testing.T) {
	dir := t.TempDir()
	const name = "testapp"
	rollbackPath := filepath.Join(dir, name+".rollback")
	targetPath := filepath.Join(dir, name)

	if err := os.WriteFile(targetPath, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollbackPath, []byte("previous binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RecordRollbackDigest(dir, name, rollbackPath); err != nil {
		t.Fatalf("RecordRollbackDigest: %v", err)
	}

	restored, err := RestoreRollbackBinary(dir, name, rollbackPath, targetPath)
	if err != nil || !restored {
		t.Fatalf("RestoreRollbackBinary() = %v, %v; want true, nil", restored, err)
	}

	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "previous binary" {
		t.Errorf("target holds %q, want the rollback copy's contents", got)
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("restored binary is not executable (mode %s)", info.Mode())
	}
	if _, err := os.Lstat(rollbackPath); !os.IsNotExist(err) {
		t.Errorf("the spent rollback copy survived the restore: %v", err)
	}
	if _, err := os.Lstat(targetPath + ".restore"); !os.IsNotExist(err) {
		t.Errorf("the staging file survived the restore: %v", err)
	}
	state, err := LoadState(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	if state.RollbackDigest != "" {
		t.Errorf("the recorded digest survived the restore: %q", state.RollbackDigest)
	}
}

func TestRestoreRollbackBinaryRefusesUnverifiedCopies(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir, rollbackPath string)
	}{
		{
			name: "no digest recorded",
			setup: func(t *testing.T, dir, rollbackPath string) {
				t.Helper()
				if err := os.WriteFile(rollbackPath, []byte("planted"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "contents replaced after the digest was recorded",
			setup: func(t *testing.T, dir, rollbackPath string) {
				t.Helper()
				if err := os.WriteFile(rollbackPath, []byte("previous binary"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := RecordRollbackDigest(dir, "testapp", rollbackPath); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(rollbackPath, []byte("planted"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink to content matching the recorded digest",
			setup: func(t *testing.T, dir, rollbackPath string) {
				t.Helper()
				if err := os.WriteFile(rollbackPath, []byte("previous binary"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := RecordRollbackDigest(dir, "testapp", rollbackPath); err != nil {
					t.Fatal(err)
				}
				// Swap the path for a symlink whose target hashes to the recorded
				// digest: following it would restore a file the digest never covered.
				payload := filepath.Join(dir, "payload")
				if err := os.WriteFile(payload, []byte("previous binary"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(rollbackPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(payload, rollbackPath); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			const name = "testapp"
			rollbackPath := filepath.Join(dir, name+".rollback")
			targetPath := filepath.Join(dir, name)
			if err := os.WriteFile(targetPath, []byte("new binary"), 0o755); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, dir, rollbackPath)

			restored, err := RestoreRollbackBinary(dir, name, rollbackPath, targetPath)
			if restored {
				t.Fatal("RestoreRollbackBinary restored an unverified copy")
			}
			if !errors.Is(err, ErrRollbackUnverified) {
				t.Fatalf("error = %v, want ErrRollbackUnverified", err)
			}
			got, err := os.ReadFile(targetPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "new binary" {
				t.Errorf("target was overwritten with %q", got)
			}
			if _, err := os.Lstat(targetPath + ".restore"); !os.IsNotExist(err) {
				t.Errorf("the staging file survived a refused restore: %v", err)
			}
		})
	}
}

// The state file lives in an install directory the lock hardening already
// treats as group-writable, so an unprivileged account can plant a symlink or a
// hard link at its path before a root install writes it.
// Windows has no O_NOFOLLOW and its install directory is not group-writable by
// construction, so writeFileNoFollow is a plain os.WriteFile there.
func TestStateFileRefusesEscapingServiceName(t *testing.T) {
	names := []struct {
		name        string
		serviceName string
	}{
		{"parent traversal", "/../../victim"},
		{"absolute path", "/etc/victim"},
		{"nested path", "sub/victim"},
		{"empty", ""},
		{"uppercase", "Svc"},
	}
	for _, tc := range names {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			outside := filepath.Dir(dir)

			if err := SaveState(dir, &StateData{ServiceName: tc.serviceName, InitSystem: "systemd"}); err == nil {
				t.Errorf("SaveState accepted service name %q", tc.serviceName)
			}
			if _, err := LoadState(dir, tc.serviceName); err == nil {
				t.Errorf("LoadState accepted service name %q", tc.serviceName)
			}
			if err := DeleteState(dir, tc.serviceName); err == nil {
				t.Errorf("DeleteState accepted service name %q", tc.serviceName)
			}

			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".daemonpilot-") || strings.Contains(e.Name(), "victim") {
					t.Errorf("a state file escaped the install directory: %s", e.Name())
				}
			}
		})
	}
}

func TestSaveStateRefusesPlantedPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no no-follow write on Windows")
	}
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		outside := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, StateFilePath(dir, "svc")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		err := SaveState(dir, &StateData{ServiceName: "svc", InitSystem: "systemd"})
		if err == nil {
			t.Fatal("SaveState wrote through a symlink, want a refusal")
		}
		got, err := os.ReadFile(outside)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "untouched" {
			t.Errorf("the symlink target was rewritten with %q", got)
		}
	})

	t.Run("hard link", func(t *testing.T) {
		dir := t.TempDir()
		victim := filepath.Join(dir, "victim")
		if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(victim, StateFilePath(dir, "svc")); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}

		err := SaveState(dir, &StateData{ServiceName: "svc", InitSystem: "systemd"})
		if err == nil {
			t.Fatal("SaveState wrote into a hard-linked file, want a refusal")
		}
		got, err := os.ReadFile(victim)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "untouched" {
			t.Errorf("the hard-link alias was rewritten with %q", got)
		}
	})
}

// A state file that cannot be read is not a verdict about the rollback copy.
// The state file is rewritten in place, so a reader can catch it truncated while
// an install or update-config rewrites it — and every caller quarantines the
// copy on ErrRollbackUnverified, destroying a valid safety net over an unrelated
// command. The restore is still refused, it is just refused recoverably.
func TestRollbackDigestReadFailureIsNotAnIntegrityFailure(t *testing.T) {
	dir := t.TempDir()
	const name = "testapp"
	rollbackPath := filepath.Join(dir, name+".rollback")
	targetPath := filepath.Join(dir, name)
	if err := os.WriteFile(targetPath, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollbackPath, []byte("previous binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RecordRollbackDigest(dir, name, rollbackPath); err != nil {
		t.Fatal(err)
	}
	// A torn read of the state file: valid JSON is what a complete write leaves.
	if err := os.WriteFile(StateFilePath(dir, name), []byte(`{"rollback_dig`), 0o600); err != nil {
		t.Fatal(err)
	}

	verifyErr := VerifyRollbackBinary(dir, name, rollbackPath)
	restored, restoreErr := RestoreRollbackBinary(dir, name, rollbackPath, targetPath)
	if restored {
		t.Error("RestoreRollbackBinary restored a copy it could not verify")
	}
	for _, tc := range []struct {
		label string
		err   error
	}{{"VerifyRollbackBinary", verifyErr}, {"RestoreRollbackBinary", restoreErr}} {
		if !errors.Is(tc.err, ErrRollbackDigestUnreadable) {
			t.Errorf("%s error = %v, want ErrRollbackDigestUnreadable", tc.label, tc.err)
		}
		if errors.Is(tc.err, ErrRollbackUnverified) {
			t.Errorf("%s error = %v must not read as an integrity failure: callers quarantine on it", tc.label, tc.err)
		}
	}
	if got, err := os.ReadFile(rollbackPath); err != nil || string(got) != "previous binary" {
		t.Errorf("rollback copy = %q, %v; want it left intact", got, err)
	}
}
