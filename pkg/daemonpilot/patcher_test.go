package daemonpilot

import (
	"errors"
	"testing"
)

func TestPatchInfoFieldAccess(t *testing.T) {
	info := PatchInfo{
		OS:          "linux",
		Arch:        "amd64",
		InitSystem:  "systemd",
		ServiceName: "my-service",
		FilePath:    "/etc/systemd/system/my-service.service",
	}

	if info.OS != "linux" {
		t.Errorf("OS: got %q, want %q", info.OS, "linux")
	}
	if info.Arch != "amd64" {
		t.Errorf("Arch: got %q, want %q", info.Arch, "amd64")
	}
	if info.InitSystem != "systemd" {
		t.Errorf("InitSystem: got %q, want %q", info.InitSystem, "systemd")
	}
	if info.ServiceName != "my-service" {
		t.Errorf("ServiceName: got %q, want %q", info.ServiceName, "my-service")
	}
	if info.FilePath != "/etc/systemd/system/my-service.service" {
		t.Errorf("FilePath: got %q, want %q", info.FilePath, "/etc/systemd/system/my-service.service")
	}
}

func TestConfigPatcherType(t *testing.T) {
	// Verify ConfigPatcher function type works correctly
	var patcher ConfigPatcher = func(path string, content string, info PatchInfo) (string, error) {
		return content + "\n# patched", nil
	}

	result, err := patcher("/etc/test.conf", "original", PatchInfo{ServiceName: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "original\n# patched"
	if result != want {
		t.Errorf("got %q, want %q", result, want)
	}
}

func TestConfigPatcherChain(t *testing.T) {
	patchers := []ConfigPatcher{
		func(path string, content string, info PatchInfo) (string, error) {
			return content + "-first", nil
		},
		func(path string, content string, info PatchInfo) (string, error) {
			return content + "-second", nil
		},
	}

	content := "base"
	for _, p := range patchers {
		var err error
		content, err = p("test.conf", content, PatchInfo{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	want := "base-first-second"
	if content != want {
		t.Errorf("got %q, want %q", content, want)
	}
}

func TestConfigPatcherError(t *testing.T) {
	patcher := ConfigPatcher(func(path string, content string, info PatchInfo) (string, error) {
		return "", errors.New("patcher failed")
	})

	_, err := patcher("test.conf", "content", PatchInfo{})
	if err == nil {
		t.Error("expected error from patcher")
	}
}
