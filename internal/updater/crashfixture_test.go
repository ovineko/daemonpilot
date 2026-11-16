package updater

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

// buildCrashFixture selects a versioned prebuilt fixture or builds it locally.
func buildCrashFixture(t *testing.T, dir, version string) string {
	t.Helper()
	fixture := testsupport.Fixture(t, "crashfixture-"+version, "./testdata/crashfixture",
		"-ldflags", "-X main.version="+version)
	bin := filepath.Join(dir, filepath.Base(fixture))
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, data, 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

// The shared builder must yield a runnable binary on every host, Windows
// included: staged and installed names carry the .exe suffix there.
func TestBuildCrashFixtureRunsReportedVersion(t *testing.T) {
	dir := t.TempDir()
	bin := buildCrashFixture(t, dir, "vreg")
	if runtime.GOOS == "windows" && !strings.HasSuffix(bin, ".exe") {
		t.Fatalf("fixture path %q must carry the .exe suffix on windows", bin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(30*time.Second))
	defer cancel()
	out, err := exec.CommandContext(ctx, bin).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "crashfixture vreg") {
		t.Fatalf("built fixture must run and report its version: %v %s", err, out)
	}
}
