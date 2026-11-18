//go:build !windows

package daemon

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdLingeringEnabled(t *testing.T) {
	tests := []struct {
		name      string
		userName  string
		statPaths map[string]bool
		want      bool
		wantPath  string
	}{
		{
			name:      "lingering enabled for the invoking user",
			userName:  "alice",
			statPaths: map[string]bool{"/var/lib/systemd/linger/alice": true},
			want:      true,
			wantPath:  "/var/lib/systemd/linger/alice",
		},
		{
			name:      "lingering disabled",
			userName:  "alice",
			statPaths: map[string]bool{},
			want:      false,
			wantPath:  "/var/lib/systemd/linger/alice",
		},
		{
			name:      "another user lingering does not count",
			userName:  "alice",
			statPaths: map[string]bool{"/var/lib/systemd/linger/bob": true},
			want:      false,
			wantPath:  "/var/lib/systemd/linger/alice",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var probed []string
			mgr := newTestSystemdManager(func(m *systemdManager) {
				m.scope = ScopeUser
				m.userNameFunc = func() (string, error) { return tt.userName, nil }
				m.statFunc = func(path string) (os.FileInfo, error) {
					probed = append(probed, path)
					if tt.statPaths[path] {
						return mockFileInfo{name: path}, nil
					}
					return nil, os.ErrNotExist
				}
			})

			got, name, err := mgr.LingeringEnabled()
			if err != nil {
				t.Fatalf("LingeringEnabled() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("LingeringEnabled() = %v, want %v (probed %v)", got, tt.want, probed)
			}
			if name != tt.userName {
				t.Errorf("user name = %q, want %q", name, tt.userName)
			}
			if len(probed) != 1 || probed[0] != tt.wantPath {
				t.Errorf("probed = %v, want [%s]", probed, tt.wantPath)
			}
		})
	}
}

// The user name must come from user.Current, not $USER: under `sudo -u` the
// environment variable is often still root, which would stat the wrong file and
// print a remedy naming the wrong account.
func TestSystemdLingeringEnabled_UsesCurrentUserNotEnv(t *testing.T) {
	origCurrentUser := currentUserFunc
	t.Cleanup(func() { currentUserFunc = origCurrentUser })
	currentUserFunc = func() (*user.User, error) { return &user.User{Username: "alice"}, nil }

	t.Setenv("USER", "root")

	var probed []string
	mgr := &systemdManager{
		scope: ScopeUser,
		statFunc: func(path string) (os.FileInfo, error) {
			probed = append(probed, path)
			return nil, os.ErrNotExist
		},
	}

	_, name, err := mgr.LingeringEnabled()
	if err != nil {
		t.Fatalf("LingeringEnabled() error = %v", err)
	}
	if name != "alice" {
		t.Errorf("user name = %q, want alice", name)
	}
	if len(probed) != 1 || probed[0] != "/var/lib/systemd/linger/alice" {
		t.Errorf("probed = %v, want [/var/lib/systemd/linger/alice]", probed)
	}
}

// An unresolvable account name must surface as an error rather than a
// placeholder: stat'ing /var/lib/systemd/linger/<user> would always miss and
// report lingering as disabled for every NSS/LDAP account on a CGO-less build.
func TestSystemdLingeringEnabled_UnknownUser(t *testing.T) {
	origCurrentUser := currentUserFunc
	t.Cleanup(func() { currentUserFunc = origCurrentUser })
	currentUserFunc = func() (*user.User, error) {
		return nil, errors.New("user: lookup userid 1000: no such file or directory")
	}

	var probed []string
	mgr := &systemdManager{
		scope: ScopeUser,
		statFunc: func(path string) (os.FileInfo, error) {
			probed = append(probed, path)
			return nil, os.ErrNotExist
		},
	}

	enabled, name, err := mgr.LingeringEnabled()
	if !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("LingeringEnabled() error = %v, want ErrUnknownUser", err)
	}
	if enabled {
		t.Error("LingeringEnabled() reported enabled on an unresolvable account")
	}
	if name != "" {
		t.Errorf("user name = %q, want empty", name)
	}
	if len(probed) != 0 {
		t.Errorf("probed = %v, want no linger path probed", probed)
	}
}

func TestSystemdVerifyRegistered(t *testing.T) {
	const unitPath = "/home/alice/.config/systemd/user/test-svc.service"

	tests := []struct {
		name     string
		output   string
		runErr   error
		wantErr  bool
		wantMsgs []string
	}{
		{
			name:   "reported path matches where the unit was written",
			output: "FragmentPath=" + unitPath + "\n",
		},
		{
			name:   "bare path without the property prefix",
			output: unitPath,
		},
		{
			name:     "systemd loads the unit from somewhere else",
			output:   "FragmentPath=/home/alice/cfg/systemd/user/test-svc.service",
			wantErr:  true,
			wantMsgs: []string{"/home/alice/cfg/systemd/user/test-svc.service", unitPath},
		},
		{
			name:     "systemd does not know the unit at all",
			output:   "FragmentPath=",
			wantErr:  true,
			wantMsgs: []string{"test-svc.service", unitPath},
		},
		{
			name:     "systemctl fails",
			output:   "Failed to connect to bus",
			runErr:   errors.New("exit status 1"),
			wantErr:  true,
			wantMsgs: []string{"verifying unit registration", "Failed to connect to bus"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex := newMockExecutor()
			ex.setResult("systemctl --user show -p FragmentPath test-svc", []byte(tt.output), tt.runErr)
			mgr := userScopeManager("/home/alice", ex, newMockFS())
			mgr.serviceName = "test-svc"

			err := mgr.VerifyRegistered()
			if !ex.called("systemctl", "--user", "show", "-p", "FragmentPath", "test-svc") {
				t.Errorf("FragmentPath was not queried; calls: %v", ex.calls)
			}
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("VerifyRegistered() = nil, want error")
			}
			for _, want := range tt.wantMsgs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// A home reached through a symlink — /home -> /var/home on rpm-ostree hosts,
// bind-mounted homes — makes systemd's FragmentPath and the path the install
// wrote differ as strings while naming the same file.
func TestSystemdVerifyRegistered_SymlinkedHome(t *testing.T) {
	base := t.TempDir()
	realHome := filepath.Join(base, "real")
	unitDir := filepath.Join(realHome, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	unitPath := filepath.Join(unitDir, "test-svc.service")
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	linkHome := filepath.Join(base, "link")
	if err := os.Symlink(realHome, linkHome); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	ex := newMockExecutor()
	ex.setResult("systemctl --user show -p FragmentPath test-svc", []byte("FragmentPath="+unitPath+"\n"), nil)
	mgr := userScopeManager(linkHome, ex, newMockFS())
	mgr.serviceName = "test-svc"

	if err := mgr.VerifyRegistered(); err != nil {
		t.Fatalf("unexpected error for a symlinked home: %v", err)
	}
}

func TestSystemdVerifyRegistered_SystemScopeIsANoOp(t *testing.T) {
	ex := newMockExecutor()
	mgr := &systemdManager{serviceName: "test-svc", executor: ex, fs: newMockFS()}

	if err := mgr.VerifyRegistered(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ex.calls) != 0 {
		t.Errorf("system scope ran %v, want no systemctl call", ex.calls)
	}
}

func TestSystemdVerifyRegistered_NoHome(t *testing.T) {
	mgr := userScopeManager("", newMockExecutor(), newMockFS())
	mgr.serviceName = "test-svc"

	if err := mgr.VerifyRegistered(); !errors.Is(err, ErrNoHomeDir) {
		t.Fatalf("VerifyRegistered() error = %v, want ErrNoHomeDir", err)
	}
}
