package daemonpilot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestLivenessMark(t *testing.T) {
	dir := t.TempDir()
	l := NewLiveness(filepath.Join(dir, "alive"))
	if l.Path() != filepath.Join(dir, "alive") {
		t.Fatal(l.Path())
	}
	for _, scale := range []string{"1", "8"} {
		t.Setenv("DAEMONPILOT_TEST_TIME_SCALE", scale)
		before := time.Now().Add(time.Minute).UnixNano()
		if err := l.Mark(time.Minute); err != nil {
			t.Fatal(err)
		}
		after := time.Now().Add(time.Minute).UnixNano()
		data, err := os.ReadFile(l.Path())
		if err != nil {
			t.Fatal(err)
		}
		deadline, err := strconv.ParseInt(string(data), 10, 64)
		if err != nil || string(data) != strconv.FormatInt(deadline, 10) || deadline < before || deadline > after {
			t.Fatalf("invalid wire format/deadline: %q (%v)", data, err)
		}
		info, err := os.Stat(l.Path())
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("permissions: %v", info.Mode())
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

func TestLivenessCheck(t *testing.T) {
	for _, tc := range []struct {
		name, content              string
		missing, directory, lapsed bool
	}{
		{name: "missing", missing: true},
		{name: "unreadable directory", directory: true},
		{name: "garbage", content: "garbage"},
		{name: "overflow", content: "999999999999999999999999"},
		{name: "future", content: strconv.FormatInt(time.Now().Add(testsupport.Scale(time.Hour)).UnixNano(), 10)},
		{name: "past", content: strconv.FormatInt(time.Now().Add(-testsupport.Scale(time.Hour)).UnixNano(), 10), lapsed: true},
		{name: "whitespace", content: " \n" + strconv.FormatInt(time.Now().Add(-testsupport.Scale(time.Hour)).UnixNano(), 10) + "\t", lapsed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLiveness(filepath.Join(t.TempDir(), "alive"))
			if tc.directory {
				if err := os.Mkdir(l.Path(), 0o700); err != nil {
					t.Fatal(err)
				}
			} else if !tc.missing {
				if err := os.WriteFile(l.Path(), []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, check := range []func() error{l.Check, func() error { return l.HealthFunc()(context.Background()) }} {
				err := check()
				if errors.Is(err, ErrLivenessLapsed) != tc.lapsed {
					t.Fatalf("Check/HealthFunc: %v", err)
				}
				if tc.lapsed && !strings.Contains(err.Error(), " ago") {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestLivenessMarkCleansUpFailedRename(t *testing.T) {
	dir := t.TempDir()
	l := NewLiveness(filepath.Join(dir, "destination"))
	if err := os.Mkdir(l.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := l.Mark(time.Minute); err == nil {
		t.Fatal("expected rename failure")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "destination" {
		t.Fatalf("leaked temp file: %v", entries)
	}
}
