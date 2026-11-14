package daemonpilot

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func FuzzLoadState(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip("bounded state file")
		}
		dir := t.TempDir()
		if err := os.WriteFile(StateFilePath(dir, "fuzz"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := LoadState(dir, "fuzz")
		var want *StateData
		valid := json.Unmarshal(data, &want) == nil && want != nil
		if (err == nil) != valid {
			t.Fatalf("state acceptance: %q, %v", data, err)
		}
		if err == nil && !reflect.DeepEqual(got, want) {
			t.Fatal("state values changed")
		}
	})
}

func FuzzConfigValidate(f *testing.F) {
	f.Fuzz(func(t *testing.T, name, scope, logDir, prefix string, interval int64) {
		cfg := Config{Name: name, Scope: InstallScope(scope), LogDir: logDir, CommandPrefix: []string{prefix}, HealthInterval: time.Duration(interval), WorkerFunc: func(context.Context) error { return nil }}
		err := cfg.Validate()
		if err == nil && (!isValidServiceName(name) || interval < 0 || strings.ContainsRune(prefix, 0)) {
			t.Fatal("invalid config accepted")
		}
		again := cfg.Validate()
		if (err == nil) != (again == nil) {
			t.Fatal("validation is not deterministic")
		}
	})
}

func FuzzDaemonpilotArgs(f *testing.F) {
	f.Fuzz(func(t *testing.T, prefix, verb, version string) {
		before := []string{prefix, version}
		snapshot := slices.Clone(before)
		got := DaemonpilotArgs(before, verb)
		want := []string{prefix, version, verb}
		if !slices.Equal(got, want) || !slices.Equal(before, snapshot) {
			t.Fatalf("argument corruption: %q", got)
		}
		got[0] = "changed"
		if !slices.Equal(before, snapshot) {
			t.Fatal("arguments alias prefix")
		}
		// Versions are opaque data; argument construction must preserve even bytes
		// that exec would later refuse, without interpreting them as flags.
	})
}
