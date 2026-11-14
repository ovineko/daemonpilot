//go:build !windows

package daemonpilot

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestStartStackDumpHandler(t *testing.T) {
	for _, tc := range []struct {
		name    string
		missing bool
	}{{name: "success"}, {name: "file failure", missing: true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.missing {
				dir = filepath.Join(dir, "absent")
			}
			stderr, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			original := os.Stderr
			os.Stderr = stderr
			defer func() { os.Stderr = original; _ = stderr.Close() }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				path string
				err  error
			}
			done := make(chan result, 1)
			if !StartStackDumpHandler(ctx, dir, func(path string, err error) { done <- result{path, err} }) {
				t.Fatal("SIGUSR1 unsupported")
			}
			if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				cancel()
				if (got.err != nil) != tc.missing {
					t.Fatalf("dump: %v", got.err)
				}
				if !regexp.MustCompile(`^goroutine-dump-[0-9]+\.txt$`).MatchString(filepath.Base(got.path)) {
					t.Fatal(got.path)
				}
				if !tc.missing {
					data, err := os.ReadFile(got.path)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(data), "TestStartStackDumpHandler") || !strings.Contains(string(data), "goroutine ") {
						t.Fatalf("missing stacks: %s", data)
					}
					info, err := os.Stat(got.path)
					if err != nil {
						t.Fatal(err)
					}
					if info.Mode().Perm() != 0o600 {
						t.Fatal(info.Mode())
					}
				}
				data, err := os.ReadFile(stderr.Name())
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(data), "TestStartStackDumpHandler") {
					t.Fatalf("missing stderr dump: %s", data)
				}
			case <-time.After(testsupport.Scale(15 * time.Second)):
				t.Fatal("timed out waiting for SIGUSR1 dump")
			}
		})
	}
}
