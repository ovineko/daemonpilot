//go:build e2e_native && !windows && !darwin

package native

import (
	"os"
	"testing"
)

func TestNativePlatformUnavailable(t *testing.T) {
	if os.Getenv("DAEMONPILOT_E2E_REQUIRE") == "1" {
		t.Fatal("native service-manager E2E requires Windows SCM or macOS launchd")
	}
	t.Skip("native service-manager E2E requires Windows SCM or macOS launchd")
}
