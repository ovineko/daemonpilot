//go:build !windows

package updater

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

// The crash-injection suite proves the repair guarantee with a real dead
// process, not a returned error: a helper process runs ApplyStagedUpdate and
// hard-kills itself (no defers, no undo) at a named stage, then an
// independent execution of the installed path must still start the service.

const (
	crashStageEnv   = "DAEMONPILOT_UPDATER_CRASH_STAGE"
	crashInstallEnv = "DAEMONPILOT_UPDATER_CRASH_INSTALL_DIR"
	crashNewPathEnv = "DAEMONPILOT_UPDATER_CRASH_NEW_PATH"
	umaskBackupEnv  = "DAEMONPILOT_UPDATER_UMASK_BACKUP" // src|dst|result[|chmodfail]
	fixtureTimeout  = 30 * time.Second
)

func TestMain(m *testing.M) {
	if stage := os.Getenv(crashStageEnv); stage != "" {
		runCrashHelper(stage)
		// The helper is expected to die inside ApplyStagedUpdate; reaching
		// this line means the barrier never fired.
		os.Exit(42)
	}
	if spec := os.Getenv(umaskBackupEnv); spec != "" {
		runUmaskBackupHelper(spec)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runUmaskBackupHelper performs the copy-fallback backup under a restrictive
// umask in an isolated process: umask is process-wide, so a parallel test
// binary must not observe it.
func runUmaskBackupHelper(spec string) {
	parts := strings.SplitN(spec, "|", 4)
	src, dst, result := parts[0], parts[1], parts[2]
	syscall.Umask(0o077)
	origLink := linkFile
	linkFile = func(string, string) error { return errors.New("hard links unsupported") }
	defer func() { linkFile = origLink }()
	if len(parts) > 3 && parts[3] == "chmodfail" {
		origChmod := chmodFile
		chmodFile = func(*os.File, os.FileMode) error { return errors.New("chmod refused") }
		defer func() { chmodFile = origChmod }()
	}
	err := createRollbackBackup(src, dst)
	mode := "absent"
	if info, statErr := os.Stat(dst); statErr == nil {
		mode = strconv.FormatUint(uint64(info.Mode().Perm()), 8)
	}
	report := fmt.Sprintf("%s\n%v\n", mode, err)
	if werr := os.WriteFile(result, []byte(report), 0o600); werr != nil {
		fmt.Fprintln(os.Stderr, "write result:", werr)
	}
}

func runCrashHelper(stage string) {
	config := &daemonpilot.UpdateConfig{
		StagedUpdateFunc: func(context.Context, *daemonpilot.UpdateInfo, daemonpilot.SelftestResult) (bool, error) {
			return true, nil
		},
	}
	u := New(config, newTestLogger(), os.Getenv(crashInstallEnv), "myapp")
	crashBarrier = func(reached string) {
		if reached == stage {
			// No defers run, no error returns, no undo: this is the
			// process-death class, not a returned failure.
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
	}
	info := &daemonpilot.UpdateInfo{Version: "2.0.0", WatchdogVersion: "2"}
	_ = u.ApplyStagedUpdate(context.Background(), os.Getenv(crashNewPathEnv), info, &daemonpilot.SelftestResult{OK: true})
}

func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

// runInstalled starts the installation the way an init system would and
// returns its output; os.Stat alone proves nothing here.
func runInstalled(t *testing.T, path string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path).CombinedOutput()
	if err != nil {
		t.Fatalf("installed binary must start: %v\n%s", err, out)
	}
	return string(out)
}

// A process killed at any stage boundary of a staged update must leave the
// installation startable: the whole old version before publication, the
// whole new version after it.
func TestApplyStagedUpdate_CrashKeepsInstallationStartable(t *testing.T) {
	for _, stage := range []string{"backup", "digest", "publish"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			v1 := buildCrashFixture(t, dir, "v1")
			v2 := buildCrashFixture(t, dir, "v2")
			installed := filepath.Join(dir, "myapp")
			copyExecutable(t, v1, installed)
			copyExecutable(t, v2, installed+".new")

			helper := exec.Command(testsupport.Executable(), "-test.run=^$")
			helper.Env = append(os.Environ(),
				crashStageEnv+"="+stage,
				crashInstallEnv+"="+dir,
				crashNewPathEnv+"="+filepath.Join(dir, "myapp.new"),
			)
			if err := helper.Run(); err == nil {
				t.Fatal("helper must die inside ApplyStagedUpdate")
			}
			ws, ok := helper.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
				t.Fatalf("helper must die by SIGKILL, got %+v", helper.ProcessState)
			}

			want := "v1"
			if stage == "publish" {
				want = "v2"
			}
			if got := runInstalled(t, installed); !strings.Contains(got, "crashfixture "+want) {
				t.Fatalf("installed binary after %s-stage crash = %q, want version %s", stage, got, want)
			}
		})
	}
}

// Negative control: the pre-fix operation order (rename the installed file
// away, record the digest, die before publishing) must fail this very
// scenario — the stage tests above can detect the historical defect.
func TestCrashNegativeControl_LegacyOrderIsUnstartable(t *testing.T) {
	dir := t.TempDir()
	v1 := buildCrashFixture(t, dir, "v1")
	installed := filepath.Join(dir, "myapp")
	copyExecutable(t, v1, installed)

	// The legacy sequence up to the crash point: installed file moved away,
	// digest recorded, publication never happens.
	if err := os.Rename(installed, installed+".rollback"); err != nil {
		t.Fatal(err)
	}
	if err := daemonpilot.RecordRollbackDigest(dir, "myapp", installed+".rollback"); err != nil {
		t.Fatal(err)
	}

	if _, err := exec.Command(installed).CombinedOutput(); err == nil {
		t.Fatal("negative control: legacy order must leave the installation unstartable")
	}
	// The only runnable copy of v1 is the rollback file; restoring it is the
	// manual intervention the fix removes the need for.
	if _, err := exec.Command(installed + ".rollback").CombinedOutput(); err != nil {
		t.Fatalf("rollback copy itself must still run: %v", err)
	}
}

// Cleanup removes stale artifacts; the installed executable is never one of
// them, so the only runnable copy must survive any cleanup invocation.
func TestCleanupStaleFiles_KeepsInstalledBinaryRunnable(t *testing.T) {
	dir := t.TempDir()
	v1 := buildCrashFixture(t, dir, "v1")
	v2 := buildCrashFixture(t, dir, "v2")
	installed := filepath.Join(dir, "myapp")
	copyExecutable(t, v1, installed)
	copyExecutable(t, v2, installed+".new")
	copyExecutable(t, v1, installed+".superseded-123")

	u := New(&daemonpilot.UpdateConfig{Source: &mockUpdateSource{}}, newTestLogger(), dir, "myapp")
	if err := u.CleanupStaleFiles(); err != nil {
		t.Fatalf("CleanupStaleFiles() error = %v", err)
	}
	if _, err := os.Stat(installed + ".new"); !os.IsNotExist(err) {
		t.Errorf("stale .new must be removed, got err = %v", err)
	}
	if _, err := os.Stat(installed + ".superseded-123"); !os.IsNotExist(err) {
		t.Errorf("superseded copy must be removed, got err = %v", err)
	}
	if got := runInstalled(t, installed); !strings.Contains(got, "crashfixture v1") {
		t.Fatalf("installed binary after cleanup = %q, want v1", got)
	}
}

// The copy fallback preserves the source's exact permission bits under a
// restrictive umask: OpenFile perms alone would have masked 0755 down to
// 0700. umask is process-wide, so the backup runs in an isolated helper.
func TestCreateRollbackBackupUmaskKeepsExactMode(t *testing.T) {
	dir := t.TempDir()
	v1 := buildCrashFixture(t, dir, "v1")
	// Pin the source to 0755: go build output depends on the building
	// process's umask.
	if err := os.Chmod(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "backup")
	result := filepath.Join(dir, "result")

	helper := exec.Command(testsupport.Executable(), "-test.run=^$")
	helper.Env = append(os.Environ(), umaskBackupEnv+"="+v1+"|"+dst+"|"+result)
	if out, err := helper.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("malformed helper report: %q", raw)
	}
	if lines[0] != "755" {
		t.Fatalf("backup mode = %s, want 755", lines[0])
	}
	if lines[1] != "<nil>" {
		t.Fatalf("backup error = %s", lines[1])
	}
	want, err := os.ReadFile(v1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("backup content mismatch: %v", err)
	}
}

// A refused mode change cleans the partial copy up and leaves the installed
// binary exactly as it was — runnable.
func TestCreateRollbackBackupModeFailureCleansUp(t *testing.T) {
	dir := t.TempDir()
	v1 := buildCrashFixture(t, dir, "v1")
	dst := filepath.Join(dir, "backup")
	result := filepath.Join(dir, "result")

	helper := exec.Command(testsupport.Executable(), "-test.run=^$")
	helper.Env = append(os.Environ(),
		umaskBackupEnv+"="+v1+"|"+dst+"|"+result+"|chmodfail")
	if out, err := helper.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(raw), "\n", 2)
	if lines[0] != "absent" {
		t.Fatalf("partial copy must be removed, mode = %s", lines[0])
	}
	if !strings.Contains(lines[1], "set rollback copy mode") {
		t.Fatalf("error must name the refused mode change, got %s", lines[1])
	}
	// The installed path (the copy source in this scenario) still runs.
	out, err := exec.Command(v1).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "crashfixture v1") {
		t.Fatalf("installed binary must stay runnable: %v %s", err, out)
	}
}
