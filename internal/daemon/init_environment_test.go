//go:build !windows

package daemon

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestInitEnvironmentFilesAndReload(t *testing.T) {
	for _, name := range []string{"dinit", "finit"} {
		t.Run(name, func(t *testing.T) {
			fs := newMockFS()
			executor := newMockExecutor()
			var mgr ServiceManager
			var envPath, command string
			var reloadArgs []string
			if name == "dinit" {
				m := newTestDinitManager()
				m.fs, m.executor = fs, executor
				m.lookPathFunc = func(string) (string, error) { return "/bin/dinitctl", nil }
				mgr, envPath, command, reloadArgs = m, "/etc/dinit.d/myapp.env", "dinitctl", []string{"reload", "myapp"}
			} else {
				m := newTestFinitManager()
				m.fs, m.executor = fs, executor
				m.lookPathFunc = func(string) (string, error) { return "/bin/initctl", nil }
				mgr, envPath, command, reloadArgs = m, "/etc/finit.d/available/myapp.env", "initctl", []string{"reload"}
			}
			cfg := ServiceConfig{Name: "myapp", ExecPath: "/opt/myapp", Environment: map[string]string{"CONFIG": "/state/config.json", "VALUE": "before"}}
			if err := mgr.Install(cfg); err != nil {
				t.Fatal(err)
			}
			want := "CONFIG=/state/config.json\nVALUE=before\n"
			if command == "initctl" {
				want = "export CONFIG='/state/config.json'\nexport VALUE='before'\n"
			}
			if got := string(fs.files[envPath]); got != want {
				t.Fatalf("environment file = %q, want %q", got, want)
			}
			env := mgr.(EnvironmentFileReadWriter)
			old := append([]byte(nil), fs.files[envPath]...)
			content, err := mgr.GenerateConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := mgr.WriteConfig(content); err != nil {
				t.Fatal(err)
			}
			if err := env.WriteEnvironmentFile(map[string]string{"VALUE": "after"}); err != nil {
				t.Fatal(err)
			}
			executor.calls = nil
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			if len(executor.calls) != 2 || executor.calls[0].Name != command || strings.Join(executor.calls[0].Args, " ") != strings.Join(reloadArgs, " ") || executor.calls[1].Args[0] != "start" {
				t.Fatalf("must reload latest manifest/environment before start: %+v", executor.calls)
			}
			if err := env.RestoreEnvironmentFile(old); err != nil {
				t.Fatal(err)
			}
			executor.calls = nil
			executor.setResult(command+" "+strings.Join(reloadArgs, " "), []byte("invalid configuration"), errors.New("exit status 1"))
			if err := mgr.Start(); err == nil || !strings.Contains(err.Error(), "invalid configuration") {
				t.Fatalf("reload failure lost: %v", err)
			}
			if len(executor.calls) != 1 {
				t.Fatalf("started after rejected reload: %+v", executor.calls)
			}
			if string(fs.files[envPath]) != string(old) {
				t.Fatal("rollback did not restore environment bytes")
			}
			for _, value := range []string{"secret\ninjected=yes", "secret\rvalue", "secret\x00value"} {
				if err := env.WriteEnvironmentFile(map[string]string{"TOKEN": value}); err == nil || strings.Contains(err.Error(), "secret") {
					t.Fatalf("must reject invalid value without exposing it: %v", err)
				}
			}
			if string(fs.files[envPath]) != string(old) {
				t.Fatal("invalid environment changed file")
			}
		})
	}
}

func TestDinitUninstallRemovesRegistration(t *testing.T) {
	fs, executor := newMockFS(), newMockExecutor()
	m := newTestDinitManager(func(m *dinitManager) { m.fs, m.executor, m.serviceName = fs, executor, "myapp" })
	if err := m.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if !executor.called("dinitctl", "disable", "myapp") || !executor.called("dinitctl", "unload", "myapp") || !fs.removed["/etc/dinit.d/myapp.env"] {
		t.Fatalf("registration/environment retained: %+v %+v", executor.calls, fs.removed)
	}
	executor.setResult("dinitctl disable myapp", []byte("permission denied"), os.ErrPermission)
	if err := m.Uninstall(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("lost disable error: %v", err)
	}
}
