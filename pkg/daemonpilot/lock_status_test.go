package daemonpilot

import (
	"errors"
	"strings"
	"testing"
)

func TestTakeInstanceLock_ContinuesWhenMechanismIsUnavailable(t *testing.T) {
	savedAcquire := acquireLockFn
	t.Cleanup(func() { acquireLockFn = savedAcquire })
	originalErr := errors.New("flock failed")
	probeErr := errors.New("probe failed")
	acquireLockFn = func(string, lockOwner) (func(), error) {
		return func() {}, &lockUnavailableError{cause: originalErr, probe: probeErr}
	}
	logger := &recordingLogger{}
	cfg := &Config{Name: "test", InstallDir: t.TempDir()}

	release, err := takeInstanceLock(cfg, logger)
	defer release()
	if err != nil {
		t.Fatalf("takeInstanceLock returned %v", err)
	}
	if cfg.LockStatus != LockStatusUnlocked {
		t.Fatalf("LockStatus = %q, want %q", cfg.LockStatus, LockStatusUnlocked)
	}
	if len(logger.calls) != 1 || logger.calls[0].level != "error" {
		t.Fatalf("log calls = %#v, want one error", logger.calls)
	}
	joined := logger.calls[0].msg
	var joinedSb32 strings.Builder
	for _, arg := range logger.calls[0].args {
		if value, ok := arg.(string); ok {
			joinedSb32.WriteString(" " + value)
		}
	}
	joined += joinedSb32.String()
	for _, want := range []string{instanceLockPath(cfg), originalErr.Error(), probeErr.Error()} {
		if !strings.Contains(joined, want) {
			t.Errorf("error log %q does not contain %q", joined, want)
		}
	}
}

func TestTakeInstanceLock_RecordsHeldStatus(t *testing.T) {
	savedAcquire := acquireLockFn
	t.Cleanup(func() { acquireLockFn = savedAcquire })
	acquireLockFn = func(string, lockOwner) (func(), error) { return func() {}, nil }
	cfg := &Config{Name: "test", InstallDir: t.TempDir()}

	release, err := takeInstanceLock(cfg, noopLogger{})
	defer release()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LockStatus != platformLockStatus() {
		t.Fatalf("LockStatus = %q, want %q", cfg.LockStatus, platformLockStatus())
	}
}

func TestReadStaticEnvReadsLockStatus(t *testing.T) {
	savedLookup := envLookup
	t.Cleanup(func() { envLookup = savedLookup })
	for _, status := range []LockStatus{LockStatusHeld, LockStatusUnlocked} {
		envLookup = func(key string) string {
			if key == EnvLockStatus {
				return string(status)
			}
			return ""
		}
		if got := ReadStaticEnv().LockStatus; got != status {
			t.Errorf("LockStatus = %q, want %q", got, status)
		}
	}
}
