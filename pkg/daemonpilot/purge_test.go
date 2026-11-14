package daemonpilot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPurgeInstallDir(t *testing.T) {
	for _, path := range []string{"", ".", "..", filepath.VolumeName(t.TempDir()) + string(filepath.Separator), filepath.Join("child", "..")} {
		t.Run("unsafe "+path, func(t *testing.T) {
			if err := PurgeInstallDir(path); err == nil {
				t.Fatalf("accepted %q", path)
			}
		})
	}
	dir := filepath.Join(t.TempDir(), "app")
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "state"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := PurgeInstallDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("directory remains: %v", err)
	}
}

func TestOnlyPendingDeleteLeftovers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		files     []string
		directory bool
		want      bool
	}{
		{name: "empty"},
		{name: "running image", files: []string{"agent.exe.pending-delete-dakl123"}, want: true},
		{name: "multiple", files: []string{"agent.exe.pending-delete-dakl123", "app.exe.pending-delete-123"}, want: true},
		{name: "other data", files: []string{"agent.exe.pending-delete-dakl123", "data"}},
		{name: "missing timestamp", files: []string{"agent.exe.pending-delete-"}},
		{name: "invalid timestamp", files: []string{"agent.exe.pending-delete-1!"}},
		{name: "overflow", files: []string{"agent.exe.pending-delete-zzzzzzzzzzzzzzzz"}},
		{name: "no image", files: []string{".exe.pending-delete-123"}},
		{name: "wrong suffix", files: []string{"agent.pending-delete-123"}},
		{name: "directory", files: []string{"agent.exe.pending-delete-123"}, directory: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tc.files {
				var err error
				if tc.directory {
					err = os.Mkdir(filepath.Join(dir, name), 0o700)
				} else {
					err = os.WriteFile(filepath.Join(dir, name), nil, 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := onlyPendingDeleteLeftovers(entries); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
