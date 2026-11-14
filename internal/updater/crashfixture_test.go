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

var fixtureBuildWait = testsupport.Scale(2 * time.Minute)

// buildCrashFixture compiles the versioned fixture so the installed file can
// be verified by ordinary execution: the printed version comes from the
// binary's own bytes, not from a sidecar.
func buildCrashFixture(t *testing.T, dir, version string) string {
	t.Helper()
	bin := filepath.Join(dir, "crashfixture-"+version)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), fixtureBuildWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-ldflags", "-X main.version="+version, "-o", bin, "./testdata/crashfixture")
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture %s: %v\n%s", version, err, out)
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
