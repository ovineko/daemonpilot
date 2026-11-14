//go:build !windows && !openbsd

package daemonpilot

import (
	"syscall"
	"testing"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

// requireSettableRLimitAS skips the test on kernels that expose RLIMIT_AS but
// reject Setrlimit on it (darwin returns EINVAL), where applyMemoryLimit
// correctly logs a warning and continues instead of changing the limit.
func requireSettableRLimitAS(t *testing.T, original syscall.Rlimit) {
	t.Helper()
	testsupport.SkipAddressSpace(t)
	probe := original
	probe.Cur = 1 << 30
	err := syscall.Setrlimit(syscall.RLIMIT_AS, &probe)
	_ = syscall.Setrlimit(syscall.RLIMIT_AS, &original)
	if err != nil {
		t.Skipf("RLIMIT_AS is not settable on this platform: %v", err)
	}
}

func TestApplyMemoryLimit(t *testing.T) {
	if raceEnabled {
		t.Skip("skipping under race detector: RLIMIT_AS conflicts with ThreadSanitizer memory needs")
	}
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_AS, &original); err != nil {
		t.Fatalf("failed to get original RLIMIT_AS: %v", err)
	}
	defer func() {
		_ = syscall.Setrlimit(syscall.RLIMIT_AS, &original)
	}()
	requireSettableRLimitAS(t, original)

	tests := []struct {
		name      string
		envVal    string
		wantApply bool
		wantLimit int64
	}{
		{
			name:      "no env var set",
			envVal:    "",
			wantApply: false,
		},
		{
			name:      "valid limit 256MB",
			envVal:    "268435456",
			wantApply: true,
			wantLimit: 268435456,
		},
		{
			name:      "invalid non-numeric",
			envVal:    "abc",
			wantApply: false,
		},
		{
			name:      "zero value",
			envVal:    "0",
			wantApply: false,
		},
		{
			name:      "negative value",
			envVal:    "-100",
			wantApply: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = syscall.Setrlimit(syscall.RLIMIT_AS, &original)

			if tt.envVal != "" {
				t.Setenv(EnvMaxMemoryBytes, tt.envVal)
			}

			logger := NewDefaultLogger(LogLevelInfo)
			applyMemoryLimit(logger)

			var rlim syscall.Rlimit
			if err := syscall.Getrlimit(syscall.RLIMIT_AS, &rlim); err != nil {
				t.Fatalf("failed to get RLIMIT_AS: %v", err)
			}

			if tt.wantApply {
				if int64(rlim.Cur) != tt.wantLimit {
					t.Errorf("expected RLIMIT_AS Cur=%d, got %d", tt.wantLimit, rlim.Cur)
				}
			} else {
				if rlim.Cur != original.Cur {
					t.Errorf("expected RLIMIT_AS unchanged, Cur was %d now %d", original.Cur, rlim.Cur)
				}
			}
		})
	}
}

func TestApplyMemoryLimitWithNilLogger(t *testing.T) {
	if raceEnabled {
		t.Skip("skipping under race detector: RLIMIT_AS conflicts with ThreadSanitizer memory needs")
	}
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_AS, &original); err != nil {
		t.Fatalf("failed to get original RLIMIT_AS: %v", err)
	}
	defer func() {
		_ = syscall.Setrlimit(syscall.RLIMIT_AS, &original)
	}()
	requireSettableRLimitAS(t, original)

	t.Setenv(EnvMaxMemoryBytes, "134217728")
	applyMemoryLimit(nil)

	var rlim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_AS, &rlim); err != nil {
		t.Fatalf("failed to get RLIMIT_AS: %v", err)
	}

	if rlim.Cur != 134217728 {
		t.Errorf("expected RLIMIT_AS Cur=134217728, got %d", rlim.Cur)
	}
}
