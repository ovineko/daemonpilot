package daemonpilot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ovineko/daemonpilot/internal/daemon"
)

type rejectedConfigManager struct {
	*mockServiceManager
}

func (m rejectedConfigManager) WriteConfig(content string) error {
	if err := os.WriteFile(m.manifestPath, []byte(content), 0o600); err != nil {
		return err
	}
	if content == m.generateContent {
		return errors.New("init rejected the new definition")
	}
	return nil
}

func TestUpdateConfigWriteFailureRemovesBackup(t *testing.T) {
	saved := saveCommandVars()
	defer restoreCommandVars2(saved)
	previousType, previousWrite, previousRemove := initSystemTypeFn, writeFileFn, removeFileFn
	defer func() { initSystemTypeFn, writeFileFn, removeFileFn = previousType, previousWrite, previousRemove }()
	dir := t.TempDir()
	manifest := filepath.Join(dir, "service")
	const oldContent = "old environment"
	if err := os.WriteFile(manifest, []byte(oldContent), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := rejectedConfigManager{mockServiceManager: &mockServiceManager{manifestPath: manifest, readInstalledContent: oldContent, generateContent: "new environment"}}
	initSystemTypeFn = func(daemon.ServiceManager) string { return "systemd" }
	detectManager = func() (daemon.ServiceManager, error) { return manager, nil }
	initManager = func(daemon.ServiceManager, daemon.ManagerOptions) error { return nil }
	loadStateFn = func(_, _ string) (*StateData, error) {
		return &StateData{ServiceName: "service", InitSystem: "systemd", ManifestPath: manifest}, nil
	}
	writeFileFn, removeFileFn = os.WriteFile, os.Remove
	cfg := &Config{Name: "service", InstallDir: dir, Logger: NewDefaultLogger(LogLevelInfo)}
	if err := handleUpdateConfig(cfg); err == nil {
		t.Fatal("rejected write must report failure")
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != oldContent {
		t.Fatalf("manifest was not restored: %q", data)
	}
	if _, err := os.Stat(manifest + ".backup"); !os.IsNotExist(err) {
		t.Fatalf("successful rollback must consume backup: %v", err)
	}
	if !manager.startCalled {
		t.Fatal("restored service was not started")
	}
}
