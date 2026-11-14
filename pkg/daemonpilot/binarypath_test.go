package daemonpilot

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstalledBinaryNameIsOneAnswer(t *testing.T) {
	cfg := &Config{Name: "sample-agent", InstallDir: "/var/lib/sample-agent"}

	dest := installedBinaryPath(cfg)
	svc := buildServiceConfig(cfg)
	if dest != svc.ExecPath {
		t.Fatalf("the copy destination and the service ExecPath disagree:\n copy %q\n svc  %q", dest, svc.ExecPath)
	}
	if filepath.Base(dest) != installedBinaryName(cfg.Name) {
		t.Errorf("path %q does not end in the installed file name %q", dest, installedBinaryName(cfg.Name))
	}
}

func TestInstalledBinaryNameOffWindowsIsUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this is the POSIX invariant")
	}
	if got := installedBinaryName("sample-agent"); got != "sample-agent" {
		t.Errorf("installedBinaryName = %q, want the bare name off Windows", got)
	}
	cfg := &Config{Name: "sample-agent", InstallDir: "/var/lib/sample-agent"}
	if got := installedBinaryPath(cfg); got != "/var/lib/sample-agent/sample-agent" {
		t.Errorf("installedBinaryPath = %q, want the path every enrolled agent already uses", got)
	}
}

func TestInstalledBinaryNameOnWindows(t *testing.T) {
	if got := installedBinaryNameFor("sample-agent", "windows"); got != "sample-agent.exe" {
		t.Errorf("installedBinaryNameFor(windows) = %q, want sample-agent.exe", got)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if got := installedBinaryNameFor("sample-agent", goos); got != "sample-agent" {
			t.Errorf("installedBinaryNameFor(%s) = %q, want the bare name", goos, got)
		}
	}
}

func TestInstalledBinaryNameDoesNotDoubleAnExtension(t *testing.T) {
	for _, name := range []string{"svc.exe", "svc.EXE"} {
		got := installedBinaryNameFor(name, "windows")
		if strings.Count(strings.ToLower(got), ".exe") > 1 {
			t.Errorf("installedBinaryName(%q) = %q, which doubles the extension", name, got)
		}
	}
}

func TestStagedNewPathStaysExecutableOnWindows(t *testing.T) {
	got := stagedNewPathFor(`C:\ProgramData\sample-agent\sample-agent.exe`, "windows")
	want := `C:\ProgramData\sample-agent\sample-agent.new.exe`
	if got != want {
		t.Errorf("windows staged path = %q, want %q", got, want)
	}
	if !strings.HasSuffix(strings.ToLower(got), ".exe") {
		t.Errorf("staged path %q does not end in .exe; os/exec will refuse to start it", got)
	}

	// POSIX keeps the plain suffix every fielded install already uses.
	if got := stagedNewPathFor("/var/lib/sample-agent/sample-agent", "linux"); got != "/var/lib/sample-agent/sample-agent.new" {
		t.Errorf("posix staged path = %q, want the plain .new suffix", got)
	}
}

func TestStagedNewPathDiffersFromTheLegacyOne(t *testing.T) {
	const win = `C:\a\agent.exe`
	if stagedNewPathFor(win, "windows") == win+".new" {
		t.Error("the windows staged path is the legacy spelling; the probe will fail")
	}
	if stagedNewPathFor("/a/agent", "linux") != LegacyStagedNewPath("/a/agent") {
		t.Error("off windows the staged path must be exactly the legacy one, or fielded nodes lose their download")
	}
}
