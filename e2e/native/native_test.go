//go:build e2e_native && (windows || darwin)

package native

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/daemon"
	"github.com/ovineko/daemonpilot/pkg/release"
)

const (
	nativeDeadline         = 60 * time.Second
	nativeSignatureContext = "daemonpilot-native-e2e-v1\x00"
)

type workerState struct {
	PID         int    `json:"pid"`
	WatchdogPID int    `json:"watchdog_pid"`
	Version     string `json:"version"`
}

func TestNativeServiceLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("sc.exe"); err != nil {
			requireFacility(t, false, "sc.exe is unavailable")
		}
	} else if _, err := exec.LookPath("launchctl"); err != nil {
		requireFacility(t, false, "launchctl is unavailable")
	}
	if runtime.GOOS == "darwin" {
		account, err := user.Current()
		if err != nil {
			requireFacility(t, false, "cannot resolve current launchd account: "+err.Error())
		}
		available := false
		var detail string
		for _, domain := range []string{"gui/" + account.Uid, "user/" + account.Uid} {
			if output, err := exec.Command("launchctl", "print", domain).CombinedOutput(); err == nil {
				available = true
				break
			} else {
				detail = fmt.Sprintf("%s: %v: %s", domain, err, output)
			}
		}
		if !available {
			requireFacility(t, false, "no usable current-user launchd domain: "+detail)
		}
	}

	root := t.TempDir()
	installDir := filepath.Join(root, "install")
	if err := os.MkdirAll(installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("dpnative-%d", time.Now().UnixNano())
	binary := filepath.Join(root, "native-service"+exeSuffix())
	buildFixture(t, binary, "1.0.0", "1")
	fastBinary := filepath.Join(root, "fast-service"+exeSuffix())
	buildFixture(t, fastBinary, "2.0.0", "1")
	stagedBinary := filepath.Join(root, "staged-service"+exeSuffix())
	buildFixture(t, stagedBinary, "3.0.0", "2")
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var offered atomic.Pointer[release.Artifact]
	handler, err := release.NewHandler(release.HandlerConfig{
		Key: privateKey, SignatureContext: nativeSignatureContext,
		Resolve: func(string, string) (*release.Artifact, error) { return offered.Load(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	releases := httptest.NewServer(handler)
	t.Cleanup(releases.Close)
	env := []string{
		"DAEMONPILOT_NATIVE_DIR=" + installDir,
		"DAEMONPILOT_NATIVE_NAME=" + name,
		"DAEMONPILOT_NATIVE_RELEASE_URL=" + releases.URL,
		"DAEMONPILOT_NATIVE_PUBLIC_KEY=" + hex.EncodeToString(publicKey),
	}
	if runtime.GOOS == "darwin" {
		home := filepath.Join(root, "home")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		env = append(env, "HOME="+home)
		t.Setenv("HOME", home)
	}
	serviceCmd := func(path, verb string) error {
		ctx, cancel := context.WithTimeout(t.Context(), nativeDeadline)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, verb)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			return fmt.Errorf("%s timed out: %w: %s", verb, ctx.Err(), out)
		}
		if err != nil {
			return fmt.Errorf("%s: %w: %s", verb, err, out)
		}
		return nil
	}
	managerName, scope := "launchd", daemon.ScopeUser
	if runtime.GOOS == "windows" {
		managerName, scope = "windows", daemon.ScopeSystem
	}
	mgr, err := daemon.NewServiceManagerByName(managerName)
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.InitServiceManagerWithOptions(mgr, daemon.ManagerOptions{ServiceName: name, InstallDir: installDir, Scope: scope}); err != nil {
		if runtime.GOOS == "windows" {
			requireFacility(t, false, "SCM registration requires an elevated administrator token: "+err.Error())
		}
		t.Fatalf("bind isolated manager: %v", err)
	}
	t.Cleanup(func() {
		_ = mgr.Stop()
		_ = mgr.Uninstall()
	})
	if err := serviceCmd(binary, "install"); err != nil {
		message := strings.ToLower(err.Error())
		if runtime.GOOS == "windows" && (strings.Contains(message, "access is denied") || strings.Contains(message, "permission")) {
			requireFacility(t, false, "SCM registration requires an elevated administrator token: "+err.Error())
		}
		t.Fatalf("install: %v", err)
	}
	workerPath := filepath.Join(installDir, "worker.json")
	await(t, "worker identity after install", func() bool { return workerStateAt(workerPath, 0) })
	first := readWorker(t, workerPath)
	if first.PID <= 0 || first.WatchdogPID <= 0 {
		t.Fatalf("invalid worker identity: %+v", first)
	}
	if status, err := mgr.Status(); err != nil || status != daemon.ServiceStatusRunning {
		t.Fatalf("status after install = %s, %v", status, err)
	}
	if err := serviceCmd(binary, "stop"); err != nil {
		t.Fatal(err)
	}
	await(t, "stopped service", func() bool {
		status, err := mgr.Status()
		return err == nil && status == daemon.ServiceStatusStopped
	})
	if err := serviceCmd(binary, "start"); err != nil {
		t.Fatal(err)
	}
	await(t, "worker after start", func() bool { return workerStateAt(workerPath, first.PID) })
	second := readWorker(t, workerPath)
	if second.PID == first.PID {
		t.Fatalf("start did not replace worker identity: %d", second.PID)
	}
	if err := serviceCmd(binary, "restart"); err != nil {
		t.Fatal(err)
	}
	await(t, "worker after restart", func() bool { return workerStateAt(workerPath, second.PID) })
	third := readWorker(t, workerPath)
	watchdog, err := os.FindProcess(third.WatchdogPID)
	if err != nil {
		t.Fatalf("find watchdog process %d: %v", third.WatchdogPID, err)
	}
	if err := watchdog.Kill(); err != nil {
		t.Fatalf("crash watchdog process %d: %v", third.WatchdogPID, err)
	}
	await(t, "manager recovery after watchdog crash", func() bool {
		state := readWorkerIfPresent(workerPath)
		return state.PID > 0 && state.PID != third.PID && state.WatchdogPID != third.WatchdogPID
	})
	readWorker(t, workerPath)
	offered.Store(&release.Artifact{Path: fastBinary, Version: "2.0.0", WatchdogVersion: "1"})
	await(t, "signed fast update", func() bool { return readWorkerIfPresent(workerPath).Version == "2.0.0" })
	readWorker(t, workerPath)
	offered.Store(&release.Artifact{Path: stagedBinary, Version: "3.0.0", WatchdogVersion: "2"})
	await(t, "staged update applied", func() bool { return readWorkerIfPresent(workerPath).Version == "3.0.0" })
	stagedWorker := readWorkerIfPresent(workerPath)
	if stagedWorker.PID == 0 || stagedWorker.Version != "3.0.0" {
		t.Fatalf("staged worker identity was not observed: %+v", stagedWorker)
	}
	await(t, "staged update rollback", func() bool {
		state := readWorkerIfPresent(workerPath)
		return state.Version == "2.0.0" && state.PID != stagedWorker.PID
	})
	if workerProcessAlive(stagedWorker.PID) {
		t.Fatalf("unhealthy staged worker %d remained alive after rollback", stagedWorker.PID)
	}
	lastWorker := readWorker(t, workerPath)
	if err := serviceCmd(binary, "uninstall"); err != nil {
		t.Fatal(err)
	}
	await(t, "worker and watchdog process cleanup", func() bool {
		return !workerProcessAlive(lastWorker.PID) && !workerProcessAlive(lastWorker.WatchdogPID)
	})
	await(t, "registration removal", func() bool {
		_, err := os.Stat(mgr.GetManifestPath())
		return errors.Is(err, os.ErrNotExist)
	})
	installedBinaryName := name + exeSuffix()
	for _, path := range []string{
		filepath.Join(installDir, installedBinaryName),
		filepath.Join(installDir, ".daemonpilot-"+name+".json"),
		filepath.Join(installDir, name+".lock"),
		filepath.Join(installDir, installedBinaryName+".new"),
		filepath.Join(installDir, installedBinaryName+".restore"),
		filepath.Join(installDir, installedBinaryName+".rollback"),
		filepath.Join(installDir, installedBinaryName+".rollback.invalid"),
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("uninstall left artifact %s: %v", path, err)
		}
	}
}

func buildFixture(t *testing.T, output, version, watchdogVersion string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	ldflags := fmt.Sprintf("-X github.com/ovineko/daemonpilot/pkg/daemonpilot.Version=%s -X github.com/ovineko/daemonpilot/pkg/daemonpilot.WatchdogVersion=%s", version, watchdogVersion)
	cmd := exec.CommandContext(ctx, "go", "build", "-ldflags", ldflags, "-o", output, "./test/fixtures/service")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build service fixture: %v: %s", err, out)
	}
}

func await(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(nativeDeadline)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-t.Context().Done():
			t.Fatalf("waiting for %s: %v", what, t.Context().Err())
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func workerStateAt(path string, previousPID int) bool {
	state := readWorkerIfPresent(path)
	return state.PID > 0 && state.PID != previousPID
}

func readWorkerIfPresent(path string) workerState {
	data, err := os.ReadFile(path)
	if err != nil {
		return workerState{}
	}
	var state workerState
	if json.Unmarshal(data, &state) != nil {
		return workerState{}
	}
	return state
}

func readWorker(t *testing.T, path string) workerState {
	t.Helper()
	state := readWorkerIfPresent(path)
	if state.PID == 0 {
		t.Fatalf("missing worker identity in %s", path)
	}
	if !workerProcessAlive(state.PID) {
		t.Fatalf("worker process %d is not alive", state.PID)
	}
	return state
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func requireFacility(t *testing.T, available bool, reason string) {
	t.Helper()
	if available {
		return
	}
	if os.Getenv("DAEMONPILOT_E2E_REQUIRE") == "1" {
		t.Fatalf("required native E2E facility unavailable: %s", reason)
	}
	t.Skip(reason)
}
