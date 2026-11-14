package watchdog

import (
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	"github.com/ovineko/procguard"
)

func TestDefaultSpawnFuncExportsLockStatus(t *testing.T) {
	for _, status := range []daemonpilot.LockStatus{daemonpilot.LockStatusHeld, daemonpilot.LockStatusUnlocked} {
		t.Run(string(status), func(t *testing.T) {
			cfg := testConfig()
			cfg.LockStatus = status
			w := New(cfg)
			cmd, err := w.defaultSpec(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			helper := helperSpec(0, 0)
			cmd.Path, cmd.Args = helper.Path, helper.Args
			child, err := procguard.Start(t.Context(), cmd)
			if err != nil {
				t.Fatal(err)
			}
			if exit := child.Wait(); exit.Err != nil || exit.Code != 0 {
				t.Fatalf("worker: %+v", exit)
			}
			if w.ipcServer != nil {
				defer w.ipcServer.Close()
			}
			want := daemonpilot.EnvLockStatus + "=" + string(status)
			if slices.Contains(cmd.Env, want) {
				return
			}
			t.Fatalf("environment does not contain %q: %s", want, strings.Join(cmd.Env, " "))
		})
	}
}
