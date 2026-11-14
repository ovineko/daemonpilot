//go:build linux

package daemonpilot

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestApplyOOMScoreAdj(t *testing.T) {
	original, err := os.ReadFile("/proc/self/oom_score_adj")
	if err != nil {
		t.Fatalf("failed to read original oom_score_adj: %v", err)
	}
	defer func() {
		_ = os.WriteFile("/proc/self/oom_score_adj", original, 0o644)
	}()

	tests := []struct {
		name      string
		envVal    string
		wantApply bool
		wantValue int
	}{
		{
			name:      "no env var set",
			envVal:    "",
			wantApply: false,
		},
		{
			name:      "valid value 100",
			envVal:    "100",
			wantApply: true,
			wantValue: 100,
		},
		{
			name:      "valid value zero",
			envVal:    "0",
			wantApply: true,
			wantValue: 0,
		},
		{
			name:      "invalid non-numeric",
			envVal:    "abc",
			wantApply: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.WriteFile("/proc/self/oom_score_adj", original, 0o644)

			if tt.envVal != "" {
				t.Setenv(EnvOOMScoreAdj, tt.envVal)
			}

			logger := NewDefaultLogger(LogLevelInfo)
			applyOOMScoreAdj(logger)

			data, err := os.ReadFile("/proc/self/oom_score_adj")
			if err != nil {
				t.Fatalf("failed to read oom_score_adj: %v", err)
			}

			got := strings.TrimSpace(string(data))
			if tt.wantApply {
				want := strconv.Itoa(tt.wantValue)
				if got != want {
					t.Errorf("oom_score_adj = %q, want %q", got, want)
				}
			} else {
				orig := strings.TrimSpace(string(original))
				if got != orig {
					t.Errorf("expected oom_score_adj unchanged (%q), got %q", orig, got)
				}
			}
		})
	}
}

func TestApplyOOMScoreAdjWithNilLogger(t *testing.T) {
	original, err := os.ReadFile("/proc/self/oom_score_adj")
	if err != nil {
		t.Fatalf("failed to read original oom_score_adj: %v", err)
	}
	defer func() {
		_ = os.WriteFile("/proc/self/oom_score_adj", original, 0o644)
	}()

	t.Setenv(EnvOOMScoreAdj, "200")
	applyOOMScoreAdj(nil)

	data, err := os.ReadFile("/proc/self/oom_score_adj")
	if err != nil {
		t.Fatalf("failed to read oom_score_adj: %v", err)
	}

	got := strings.TrimSpace(string(data))
	if got != "200" {
		t.Errorf("expected oom_score_adj=200, got %q", got)
	}
}
