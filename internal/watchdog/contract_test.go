package watchdog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/procguard"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestWorkerArgumentContract(t *testing.T) {
	binary := testsupport.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(time.Minute))
	defer cancel()
	original := os.Args
	defer func() { os.Args = original }()
	for _, mode := range []string{"run", "container"} {
		t.Run(mode, func(t *testing.T) {
			result := filepath.Join(t.TempDir(), "args")
			t.Setenv(testsupport.WorkerArgumentOutputKey, result)
			os.Args = []string{binary, mode, "application-argument"}
			cfg := testConfig()
			cfg.InstallDir = t.TempDir()
			w := New(cfg)
			spec, err := w.defaultSpec(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if w.ipcServer != nil {
					w.ipcServer.Close()
				}
			}()
			child, err := procguard.Start(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			if exit := child.Wait(); exit.Err != nil || exit.Code != 0 {
				t.Fatalf("worker: %+v", exit)
			}

			data, err := os.ReadFile(result)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "worker" {
				t.Fatalf("worker arguments = %q, want exactly worker", data)
			}
		})
	}
}

func TestMain(m *testing.M) {
	if output := testsupport.WorkerArgumentOutput(); output != "" {
		if err := os.WriteFile(output, []byte(strings.Join(os.Args[1:], "\n")), 0o600); err != nil {
			panic(err)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
