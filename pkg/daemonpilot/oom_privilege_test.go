//go:build linux

package daemonpilot

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ovineko/procguard"
)

func TestSetOOMScoreAdjNegativeRequiresPrivilege(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test only meaningful for non-root users")
	}

	original, err := os.ReadFile("/proc/self/oom_score_adj")
	if err != nil {
		t.Fatalf("failed to read original oom_score_adj: %v", err)
	}
	originalScore, err := strconv.Atoi(strings.TrimSpace(string(original)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := procguard.ApplyLimitsToSelf(procguard.Limits{OOMScoreAdj: &originalScore}); err != nil {
			t.Errorf("restore OOM score: %v", err)
		}
	}()

	score := -1000
	err = procguard.ApplyLimitsToSelf(procguard.Limits{OOMScoreAdj: &score})
	if err == nil {
		t.Error("expected error setting negative oom_score_adj as non-root, got nil")
	}
}
