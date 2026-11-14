package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"testing"
)

type mockExecutor struct {
	calls               []mockCall
	results             map[string]mockResult
	fallbackErr         error
	fallbackOutput      []byte
	captureFileArg      bool
	capturedFileContent string
}

type mockCall struct {
	Name string
	Args []string
}

type mockResult struct {
	Output []byte
	Err    error
}

func newMockExecutor() *mockExecutor {
	return &mockExecutor{
		results: make(map[string]mockResult),
	}
}

func (m *mockExecutor) Run(name string, args ...string) ([]byte, error) {
	m.calls = append(m.calls, mockCall{Name: name, Args: args})
	if m.captureFileArg && name == "crontab" && len(args) == 1 && args[0] != "-l" {
		data, err := os.ReadFile(args[0])
		if err == nil {
			m.capturedFileContent = string(data)
		}
	}
	key := name + " " + strings.Join(args, " ")
	if r, ok := m.results[key]; ok {
		return r.Output, r.Err
	}
	if m.fallbackErr != nil {
		return m.fallbackOutput, m.fallbackErr
	}
	return nil, nil
}

func (m *mockExecutor) setResult(cmd string, output []byte, err error) {
	m.results[cmd] = mockResult{Output: output, Err: err}
}

func (m *mockExecutor) called(name string, args ...string) bool {
	for _, c := range m.calls {
		if c.Name == name && strings.Join(c.Args, " ") == strings.Join(args, " ") {
			return true
		}
	}
	return false
}

type mockFS struct {
	files    map[string][]byte
	removed  map[string]bool
	dirs     map[string]bool
	writeErr error
	// writeErrAfter fails every WriteFile past the nth, so a test can let an
	// install write its manifest and still break the rollback that restores it.
	// Zero disables it.
	writeErrAfter int
	writeCount    int
	readErr       error
	removeErr     error
	mkdirErr      error
	// mkdirErrPath fails MkdirAll for one directory only, so a test can break
	// the log directory while leaving the manifest directory writable.
	mkdirErrPath string
	chowned      map[string][2]int
	chownErr     error
	dirExistsErr error
}

func newMockFS() *mockFS {
	return &mockFS{
		files:   make(map[string][]byte),
		removed: make(map[string]bool),
		dirs:    make(map[string]bool),
		chowned: make(map[string][2]int),
	}
}

func (f *mockFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writeCount++
	if f.writeErrAfter > 0 && f.writeCount > f.writeErrAfter {
		return errors.New("read-only file system")
	}
	f.files[path] = data
	return nil
}

func (f *mockFS) ReadFile(path string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	data, ok := f.files[path]
	if !ok || f.removed[path] {
		return nil, fmt.Errorf("reading %s: %w", path, fs.ErrNotExist)
	}
	return data, nil
}

func (f *mockFS) Remove(path string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed[path] = true
	return nil
}

func (f *mockFS) RemoveAll(path string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed[path] = true
	return nil
}

func (f *mockFS) MkdirAll(path string, perm os.FileMode) error {
	if f.mkdirErrPath != "" && f.mkdirErrPath == path {
		return errors.New("permission denied")
	}
	if f.mkdirErr != nil {
		return f.mkdirErr
	}
	f.dirs[path] = true
	return nil
}

func (f *mockFS) DirExists(path string) (bool, error) {
	if f.dirExistsErr != nil {
		return false, f.dirExistsErr
	}
	return f.dirs[path] && !f.removed[path], nil
}

func (f *mockFS) Chown(path string, uid int, gid int) error {
	if f.chownErr != nil {
		return f.chownErr
	}
	if f.chowned == nil {
		f.chowned = make(map[string][2]int)
	}
	f.chowned[path] = [2]int{uid, gid}
	return nil
}

func TestGenerateUnitFile(t *testing.T) {
	config := ServiceConfig{
		Name:        "test-agent",
		Description: "Test Agent Service",
		ExecPath:    "/opt/test-agent/test-agent",
		InstallDir:  "/opt/test-agent",
		WorkingDir:  "/opt/test-agent",
		Arguments:   []string{"run"},
	}

	content, err := GenerateUnitFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedParts := []string{
		"Description=Test Agent Service",
		"After=network-online.target",
		"Wants=network-online.target",
		"Type=simple",
		"ExecStart=/opt/test-agent/test-agent run",
		"Restart=always",
		"RestartSec=5s",
		"WorkingDirectory=/opt/test-agent",
		"WantedBy=multi-user.target",
	}

	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("unit file missing %q\ngot:\n%s", part, content)
		}
	}
}

func TestGenerateUnitFile_Sections(t *testing.T) {
	config := ServiceConfig{
		Name:        "myapp",
		Description: "My App",
		ExecPath:    "/usr/local/bin/myapp",
		WorkingDir:  "/var/lib/myapp",
	}

	content, err := GenerateUnitFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sections := []string{"[Unit]", "[Service]", "[Install]"}
	for _, section := range sections {
		if !strings.Contains(content, section) {
			t.Errorf("unit file missing section %q", section)
		}
	}
}

func TestGenerateUnitFile_EscapesPercent(t *testing.T) {
	config := ServiceConfig{
		Name:        "myapp",
		Description: "App with %n specifier",
		ExecPath:    "/opt/my%app/myapp",
		WorkingDir:  "/opt/my%app",
		Arguments:   []string{"run"},
	}

	content, err := GenerateUnitFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(content, "App with %n") {
		t.Error("percent in Description was not escaped")
	}
	if !strings.Contains(content, "App with %%n specifier") {
		t.Errorf("expected escaped percent in Description, got:\n%s", content)
	}
	if strings.Contains(content, "/opt/my%app/myapp run") {
		t.Error("percent in ExecPath was not escaped")
	}
	if !strings.Contains(content, "/opt/my%%app/myapp run") {
		t.Errorf("expected escaped percent in ExecPath, got:\n%s", content)
	}
}

func TestGenerateUnitFile_EscapesDollarAndBackslash(t *testing.T) {
	config := ServiceConfig{
		Name:              "myapp",
		Description:       "App with $VAR and \\path",
		ExecPath:          "/opt/$myapp/myapp",
		WorkingDir:        "/opt/$myapp",
		Arguments:         []string{"run"},
		SystemdDirectives: map[string]string{"ReadWritePaths": "/srv/$data"},
	}

	content, err := GenerateUnitFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(content, "/opt/$myapp/myapp run") {
		t.Error("dollar in ExecPath was not escaped")
	}
	if !strings.Contains(content, "/opt/$$myapp/myapp run") {
		t.Errorf("expected escaped dollar in ExecPath, got:\n%s", content)
	}

	// Plain settings are not command lines: systemd neither expands variables nor
	// unescapes backslashes there, so escaping them would corrupt the value.
	if !strings.Contains(content, "Description=App with $VAR and \\path") {
		t.Errorf("expected verbatim dollar and backslash in Description, got:\n%s", content)
	}
	if !strings.Contains(content, "WorkingDirectory=/opt/$myapp\n") {
		t.Errorf("expected verbatim dollar in WorkingDirectory, got:\n%s", content)
	}
	if !strings.Contains(content, "ReadWritePaths=/srv/$data") {
		t.Errorf("expected verbatim dollar in a custom directive, got:\n%s", content)
	}
}

func TestGenerateUnitFile_EscapesCommandLineDirectives(t *testing.T) {
	config := ServiceConfig{
		Name:     "myapp",
		ExecPath: "/opt/myapp/myapp",
		SystemdDirectives: map[string]string{
			"ExecStartPre":   `/usr/bin/setup --root $HOME --sep \d`,
			"ExecStop":       `/usr/bin/stop /srv/data\`,
			"ExecSearchPath": "/opt/$bin",
		},
	}

	content, err := GenerateUnitFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Exec* directives are command lines: systemd expands `$VAR` and unescapes
	// backslashes there, so both must be escaped to reach the command literally.
	if !strings.Contains(content, `ExecStartPre=/usr/bin/setup --root $$HOME --sep \\d`+"\n") {
		t.Errorf("expected escaped dollar and backslash in ExecStartPre, got:\n%s", content)
	}
	// Escaping doubles the trailing backslash, but the config lexer tests for
	// line continuation before unescaping, so it still has to go.
	if !strings.Contains(content, "ExecStop=/usr/bin/stop /srv/data\n") {
		t.Errorf("expected trailing backslash dropped from ExecStop, got:\n%s", content)
	}
	// ExecSearchPath is a path list, not a command line.
	if !strings.Contains(content, "ExecSearchPath=/opt/$bin\n") {
		t.Errorf("expected verbatim dollar in ExecSearchPath, got:\n%s", content)
	}
}

func TestGenerateUnitFile_ClassifiesPaddedCommandDirectiveKeys(t *testing.T) {
	config := ServiceConfig{
		Name:     "myapp",
		ExecPath: "/opt/myapp/myapp",
		SystemdDirectives: map[string]string{
			" ExecStartPre\t": `/usr/bin/setup --root $HOME --sep \d`,
		},
	}

	content, err := GenerateUnitFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// systemd strips whitespace around a key, so a padded key still names a
	// command line and its value must get command-line escaping.
	if !strings.Contains(content, `ExecStartPre=/usr/bin/setup --root $$HOME --sep \\d`+"\n") {
		t.Errorf("expected padded key to be treated as a command line, got:\n%s", content)
	}
}

func TestGenerateUnitFile_RejectsTrailingBackslashInSettings(t *testing.T) {
	config := ServiceConfig{
		Name:              "myapp",
		Description:       `desc\`,
		ExecPath:          "/opt/myapp/myapp",
		WorkingDir:        `/opt/myapp\`,
		User:              `svc\`,
		Group:             `grp\`,
		SystemdDirectives: map[string]string{"ReadWritePaths": `/srv/data\`},
	}

	if _, err := GenerateUnitFile(config); err == nil {
		t.Fatal("unrepresentable setting must be rejected, not changed")
	}
}

func TestGenerateUnitFile_RejectsTrailingBackslashBeforeWhitespaceInSettings(t *testing.T) {
	config := ServiceConfig{
		Name:              "myapp",
		Description:       "desc\\\t",
		ExecPath:          "/opt/myapp/myapp",
		WorkingDir:        `/srv/app\ `,
		User:              "svc\\ \\ ",
		Group:             "grp\\\r",
		SystemdDirectives: map[string]string{"ReadWritePaths": `/srv/data\ `},
	}

	if _, err := GenerateUnitFile(config); err == nil {
		t.Fatal("unrepresentable setting must be rejected, not changed")
	}
}

func TestGenerateUnitFile_QuotesTrailingBackslashInExecStart(t *testing.T) {
	config := ServiceConfig{
		Name:      "myapp",
		ExecPath:  `/opt/myapp/myapp\`,
		Arguments: []string{`--data=/srv\`},
	}

	content, err := GenerateUnitFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for line := range strings.SplitSeq(content, "\n") {
		if strings.HasSuffix(line, `\`) {
			t.Errorf("line continues into the next directive: %q\nunit:\n%s", line, content)
		}
	}
	if !strings.Contains(content, `ExecStart="/opt/myapp/myapp\\" "--data=/srv\\"`) {
		t.Errorf("expected quoted ExecStart words, got:\n%s", content)
	}
}

func TestGenerateUnitFile_EscapesPercentInEnvironment(t *testing.T) {
	config := ServiceConfig{
		Name:        "myapp",
		ExecPath:    "/opt/myapp/myapp",
		Environment: map[string]string{"DB_PASS": "a%hb"},
	}

	content, err := GenerateUnitFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(content, `"DB_PASS=a%hb"`) {
		t.Errorf("percent in Environment value was not escaped, got:\n%s", content)
	}
	if !strings.Contains(content, `Environment="DB_PASS=a%%hb"`) {
		t.Errorf("expected escaped percent in Environment, got:\n%s", content)
	}
}

func TestGenerateUnitFile_RejectsUnrepresentableUserAndGroup(t *testing.T) {
	config := ServiceConfig{
		Name:     "myapp",
		ExecPath: "/opt/myapp/myapp",
		User:     "svc\nExecStartPost=/bin/sh -c evil",
		Group:    "grp\nRestart=always",
	}

	if _, err := GenerateUnitFile(config); err == nil {
		t.Fatal("unrepresentable setting must be rejected, not changed")
	}
}

func TestGenerateUnitFile_UserAndGroupEscaping(t *testing.T) {
	tests := []struct {
		name     string
		user     string
		expected string
	}{
		{name: "plain name is unchanged", user: "svc", expected: "User=svc"},
		{name: "machine account keeps its trailing dollar", user: "HOST$", expected: "User=HOST$"},
		{name: "backslash is not doubled", user: `DOMAIN\svc`, expected: `User=DOMAIN\svc`},
		{name: "percent is escaped as a literal specifier", user: "sv%c", expected: "User=sv%%c"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := GenerateUnitFile(ServiceConfig{
				Name:     "myapp",
				ExecPath: "/opt/myapp/myapp",
				User:     tt.user,
				Group:    tt.user,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(content, tt.expected) {
				t.Errorf("expected %q in output\ngot:\n%s", tt.expected, content)
			}
			if !strings.Contains(content, strings.Replace(tt.expected, "User=", "Group=", 1)) {
				t.Errorf("expected the same escaping for Group\ngot:\n%s", content)
			}
		})
	}
}

func TestSystemdInstall(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := &systemdManager{executor: exec, fs: fs}

	config := ServiceConfig{
		Name:        "test-svc",
		Description: "Test Service",
		ExecPath:    "/opt/test-svc/test-svc",
		InstallDir:  "/opt/test-svc",
		WorkingDir:  "/opt/test-svc",
	}

	err := mgr.Install(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	unitPath := "/etc/systemd/system/test-svc.service"
	if _, ok := fs.files[unitPath]; !ok {
		t.Error("unit file was not written")
	}

	if !exec.called("systemctl", "daemon-reload") {
		t.Error("daemon-reload was not called")
	}

	if mgr.serviceName != "test-svc" {
		t.Errorf("serviceName = %q, want %q", mgr.serviceName, "test-svc")
	}
}

func TestSystemdInstall_WriteFileError(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	fs.writeErr = errors.New("permission denied")
	mgr := &systemdManager{executor: exec, fs: fs}

	err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "writing unit file") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSystemdInstall_DaemonReloadError(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("systemctl daemon-reload", nil, errors.New("systemctl failed"))
	fs := newMockFS()
	mgr := &systemdManager{executor: exec, fs: fs}

	err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "daemon-reload") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSystemdUninstall(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := &systemdManager{serviceName: "test-svc", executor: exec, fs: fs}

	err := mgr.Uninstall()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !exec.called("systemctl", "disable", "test-svc") {
		t.Error("disable was not called")
	}
	if !exec.called("systemctl", "daemon-reload") {
		t.Error("daemon-reload was not called")
	}
	if !fs.removed["/etc/systemd/system/test-svc.service"] {
		t.Error("unit file was not removed")
	}
}

func TestSystemdUninstall_NoServiceName(t *testing.T) {
	mgr := &systemdManager{executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Uninstall()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSystemdUninstall_DisableError(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("systemctl disable test-svc", nil, errors.New("disable failed"))
	mgr := &systemdManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Uninstall()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "disabling service") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSystemdStart(t *testing.T) {
	exec := newMockExecutor()
	mgr := &systemdManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Start()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exec.called("systemctl", "start", "test-svc") {
		t.Error("start was not called")
	}
}

func TestSystemdStart_NoServiceName(t *testing.T) {
	mgr := &systemdManager{executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Start()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSystemdStop(t *testing.T) {
	exec := newMockExecutor()
	mgr := &systemdManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Stop()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exec.called("systemctl", "stop", "test-svc") {
		t.Error("stop was not called")
	}
}

func TestSystemdStop_NoServiceName(t *testing.T) {
	mgr := &systemdManager{executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Stop()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSystemdEnable(t *testing.T) {
	exec := newMockExecutor()
	mgr := &systemdManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Enable()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exec.called("systemctl", "enable", "test-svc") {
		t.Error("systemctl enable was not called")
	}
}

func TestSystemdEnable_NoServiceName(t *testing.T) {
	mgr := &systemdManager{executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Enable()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSystemdEnable_Error(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("systemctl enable test-svc", nil, errors.New("enable failed"))
	mgr := &systemdManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Enable()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "enabling service") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSystemdDisable(t *testing.T) {
	exec := newMockExecutor()
	mgr := &systemdManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Disable()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exec.called("systemctl", "disable", "test-svc") {
		t.Error("systemctl disable was not called")
	}
}

func TestSystemdDisable_NoServiceName(t *testing.T) {
	mgr := &systemdManager{executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Disable()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSystemdDisable_Error(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("systemctl disable test-svc", nil, errors.New("disable failed"))
	mgr := &systemdManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Disable()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "disabling service") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSystemdStatus(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		execErr    error
		wantStatus ServiceStatus
		wantErr    bool
	}{
		{"active", "active\n", nil, ServiceStatusRunning, false},
		{"inactive", "inactive\n", nil, ServiceStatusStopped, false},
		{"failed", "failed\n", nil, ServiceStatusFailed, false},
		{"unknown output", "activating\n", nil, ServiceStatusUnknown, false},
		{"error with inactive", "inactive\n", errors.New("exit 3"), ServiceStatusStopped, false},
		{"error with unknown", "something\n", errors.New("exit 4"), ServiceStatusUnknown, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			exec.setResult("systemctl is-active test-svc", []byte(tt.output), tt.execErr)
			mgr := &systemdManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

			status, err := mgr.Status()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != tt.wantStatus {
				t.Errorf("status = %v, want %v", status, tt.wantStatus)
			}
		})
	}
}

func TestSystemdStatus_NoServiceName(t *testing.T) {
	mgr := &systemdManager{executor: newMockExecutor(), fs: newMockFS()}
	_, err := mgr.Status()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSystemdUnitFilePath(t *testing.T) {
	mgr := &systemdManager{serviceName: "my-service"}
	got := mgr.unitFilePath()
	want := "/etc/systemd/system/my-service.service"
	if got != want {
		t.Errorf("unitFilePath() = %q, want %q", got, want)
	}
}

func TestSystemdGetManifestPath(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		want        string
	}{
		{"standard", "myapp", "/etc/systemd/system/myapp.service"},
		{"with-dash", "my-service", "/etc/systemd/system/my-service.service"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &systemdManager{serviceName: tt.serviceName}
			got := mgr.GetManifestPath()
			if got != tt.want {
				t.Errorf("GetManifestPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSystemdInstallUninstallRoundtrip(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := &systemdManager{executor: exec, fs: fs}

	config := ServiceConfig{
		Name:        "roundtrip-svc",
		Description: "Roundtrip Test",
		ExecPath:    "/opt/roundtrip-svc/roundtrip-svc",
		InstallDir:  "/opt/roundtrip-svc",
		WorkingDir:  "/opt/roundtrip-svc",
	}

	if err := mgr.Install(config); err != nil {
		t.Fatalf("install error: %v", err)
	}

	unitPath := "/etc/systemd/system/roundtrip-svc.service"
	if _, ok := fs.files[unitPath]; !ok {
		t.Fatal("unit file not written after install")
	}

	if err := mgr.Uninstall(); err != nil {
		t.Fatalf("uninstall error: %v", err)
	}

	if !fs.removed[unitPath] {
		t.Error("unit file not removed after uninstall")
	}
}

func TestGenerateUnitFile_AllOptionalFields(t *testing.T) {
	config := ServiceConfig{
		Name:        "test-agent",
		Description: "Test Agent Service",
		ExecPath:    "/opt/test-agent/test-agent",
		InstallDir:  "/opt/test-agent",
		WorkingDir:  "/opt/test-agent",
		User:        "testuser",
		Group:       "testgroup",
		Environment: map[string]string{
			"LOG_LEVEL": "debug",
			"API_KEY":   "secret123",
		},
		Arguments: []string{"run", "--port", "8080"},
		SystemdDirectives: map[string]string{
			"LimitNOFILE":   "65536",
			"ProtectSystem": "full",
		},
	}

	content, err := GenerateUnitFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedParts := []string{
		"Description=Test Agent Service",
		"ExecStart=/opt/test-agent/test-agent run --port 8080",
		"WorkingDirectory=/opt/test-agent",
		"User=testuser",
		"Group=testgroup",
		`Environment="API_KEY=secret123"`,
		`Environment="LOG_LEVEL=debug"`,
		"LimitNOFILE=65536",
		"ProtectSystem=full",
	}
	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("unit file missing %q\ngot:\n%s", part, content)
		}
	}

	sections := []string{"[Unit]", "[Service]", "[Install]"}
	for _, section := range sections {
		if !strings.Contains(content, section) {
			t.Errorf("unit file missing section %q", section)
		}
	}
}

func TestGenerateUnitFile_NoOptionalFields(t *testing.T) {
	config := ServiceConfig{
		Name:        "basic-svc",
		Description: "Basic Service",
		ExecPath:    "/usr/local/bin/basic-svc",
		WorkingDir:  "/var/lib/basic-svc",
		Arguments:   []string{"run"},
	}

	content, err := GenerateUnitFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedParts := []string{
		"Description=Basic Service",
		"ExecStart=/usr/local/bin/basic-svc run",
		"WorkingDirectory=/var/lib/basic-svc",
		"Restart=always",
		"RestartSec=5s",
		"WantedBy=multi-user.target",
	}
	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("unit file missing %q\ngot:\n%s", part, content)
		}
	}

	unexpectedParts := []string{"User=", "Group=", "Environment=", "LimitNOFILE="}
	for _, part := range unexpectedParts {
		if strings.Contains(content, part) {
			t.Errorf("unit file should not contain %q for basic config\ngot:\n%s", part, content)
		}
	}
}

func TestGenerateUnitFile_UserOnly(t *testing.T) {
	config := ServiceConfig{
		Name:        "svc",
		Description: "Service",
		ExecPath:    "/usr/bin/svc",
		WorkingDir:  "/var/lib/svc",
		User:        "appuser",
	}

	content, err := GenerateUnitFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(content, "User=appuser") {
		t.Errorf("expected User=appuser in output\ngot:\n%s", content)
	}
	if strings.Contains(content, "Group=") {
		t.Errorf("should not contain Group= when Group is empty\ngot:\n%s", content)
	}
}

func TestGenerateUnitFile_ArgumentsAppendedToExecStart(t *testing.T) {
	config := ServiceConfig{
		Name:       "svc",
		ExecPath:   "/usr/bin/svc",
		WorkingDir: "/var/lib/svc",
		Arguments:  []string{"run", "--config", "/etc/svc.yaml", "--verbose"},
	}

	content, err := GenerateUnitFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "ExecStart=/usr/bin/svc run --config /etc/svc.yaml --verbose"
	if !strings.Contains(content, expected) {
		t.Errorf("expected %q\ngot:\n%s", expected, content)
	}
}

func TestGenerateUnitFile_ExecStartQuoting(t *testing.T) {
	tests := []struct {
		name     string
		execPath string
		args     []string
		expected string
	}{
		{
			name:     "plain path unquoted",
			execPath: "/opt/test-agent/test-agent",
			expected: "ExecStart=/opt/test-agent/test-agent",
		},
		{
			name:     "path with space is quoted",
			execPath: "/home/first last/bin/svc",
			expected: `ExecStart="/home/first last/bin/svc"`,
		},
		{
			name:     "path with double quote is quoted and escaped",
			execPath: `/opt/we"ird/svc`,
			expected: `ExecStart="/opt/we\"ird/svc"`,
		},
		{
			name:     "path with space plus arguments",
			execPath: "/Users/a b/Library/Application Support/svc/svc",
			args:     []string{"run", "--port", "8080"},
			expected: `ExecStart="/Users/a b/Library/Application Support/svc/svc" run --port 8080`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := GenerateUnitFile(ServiceConfig{
				Name:       "svc",
				ExecPath:   tt.execPath,
				WorkingDir: "/var/lib/svc",
				Arguments:  tt.args,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(content, tt.expected+"\n") {
				t.Errorf("expected %q\ngot:\n%s", tt.expected, content)
			}
		})
	}
}

func TestGenerateUnitFile_EnvironmentSorted(t *testing.T) {
	config := ServiceConfig{
		Name:       "svc",
		ExecPath:   "/usr/bin/svc",
		WorkingDir: "/var/lib/svc",
		Environment: map[string]string{
			"ZZZ_VAR": "last",
			"AAA_VAR": "first",
		},
	}

	content, err := GenerateUnitFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	idxA := strings.Index(content, "AAA_VAR")
	idxZ := strings.Index(content, "ZZZ_VAR")
	if idxA < 0 || idxZ < 0 {
		t.Fatalf("missing environment variables in output\ngot:\n%s", content)
	}
	if idxA >= idxZ {
		t.Errorf("environment variables not sorted alphabetically\ngot:\n%s", content)
	}
}

func TestGenerateUnitFile_SystemdDirectivesSorted(t *testing.T) {
	config := ServiceConfig{
		Name:       "svc",
		ExecPath:   "/usr/bin/svc",
		WorkingDir: "/var/lib/svc",
		SystemdDirectives: map[string]string{
			"ProtectSystem": "full",
			"LimitNOFILE":   "65536",
		},
	}

	content, err := GenerateUnitFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	idxL := strings.Index(content, "LimitNOFILE=65536")
	idxP := strings.Index(content, "ProtectSystem=full")
	if idxL < 0 || idxP < 0 {
		t.Fatalf("missing directives in output\ngot:\n%s", content)
	}
	if idxL >= idxP {
		t.Errorf("systemd directives not sorted alphabetically\ngot:\n%s", content)
	}
}

func TestSystemdGenerateConfig(t *testing.T) {
	mgr := &systemdManager{executor: newMockExecutor(), fs: newMockFS()}
	config := ServiceConfig{
		Name:        "test-svc",
		Description: "Test Service",
		ExecPath:    "/opt/test-svc/test-svc",
		InstallDir:  "/opt/test-svc",
		WorkingDir:  "/opt/test-svc",
		Arguments:   []string{"run"},
	}

	content, err := mgr.GenerateConfig(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mgr.serviceName != "test-svc" {
		t.Errorf("serviceName = %q, want %q", mgr.serviceName, "test-svc")
	}

	expectedParts := []string{
		"Description=Test Service",
		"ExecStart=/opt/test-svc/test-svc run",
		"WorkingDirectory=/opt/test-svc",
	}
	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("generated config missing %q", part)
		}
	}
}

func TestSystemdWriteConfig(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		exec := newMockExecutor()
		fs := newMockFS()
		mgr := &systemdManager{serviceName: "test-svc", executor: exec, fs: fs}

		err := mgr.WriteConfig("[Unit]\nDescription=Test\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		unitPath := "/etc/systemd/system/test-svc.service"
		if _, ok := fs.files[unitPath]; !ok {
			t.Error("unit file was not written")
		}
		if !exec.called("systemctl", "daemon-reload") {
			t.Error("daemon-reload was not called")
		}
	})

	t.Run("no service name", func(t *testing.T) {
		mgr := &systemdManager{executor: newMockExecutor(), fs: newMockFS()}
		err := mgr.WriteConfig("content")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "service name not set") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("write error", func(t *testing.T) {
		fs := newMockFS()
		fs.writeErr = errors.New("permission denied")
		mgr := &systemdManager{serviceName: "test-svc", executor: newMockExecutor(), fs: fs}

		err := mgr.WriteConfig("content")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "writing unit file") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("daemon-reload error", func(t *testing.T) {
		exec := newMockExecutor()
		exec.setResult("systemctl daemon-reload", nil, errors.New("systemctl failed"))
		mgr := &systemdManager{serviceName: "test-svc", executor: exec, fs: newMockFS()}

		err := mgr.WriteConfig("content")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "daemon-reload") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestGenerateUnitFile_TypeNotify(t *testing.T) {
	tests := []struct {
		name        string
		notifyReady bool
		wantType    string
	}{
		{"default is Type=simple", false, "Type=simple"},
		{"NotifyReady sets Type=notify", true, "Type=notify"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := ServiceConfig{
				Name:        "test-svc",
				Description: "Test Service",
				ExecPath:    "/opt/test-svc/test-svc",
				WorkingDir:  "/opt/test-svc",
				NotifyReady: tt.notifyReady,
			}

			content, err := GenerateUnitFile(config)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !strings.Contains(content, tt.wantType) {
				t.Errorf("expected %q in unit file\ngot:\n%s", tt.wantType, content)
			}

			unwanted := "Type=simple"
			if !tt.notifyReady {
				unwanted = "Type=notify"
			}
			if strings.Contains(content, unwanted) {
				t.Errorf("should not contain %q\ngot:\n%s", unwanted, content)
			}
		})
	}
}

func TestGenerateUnitFile_TypeNotifyWithAllOptions(t *testing.T) {
	config := ServiceConfig{
		Name:        "test-svc",
		Description: "Test Service",
		ExecPath:    "/opt/test-svc/test-svc",
		WorkingDir:  "/opt/test-svc",
		NotifyReady: true,
		User:        "app",
		Group:       "app",
		Environment: map[string]string{"FOO": "bar"},
		SystemdDirectives: map[string]string{
			"WatchdogSec": "30",
		},
	}

	content, err := GenerateUnitFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedParts := []string{
		"Type=notify",
		"User=app",
		"Group=app",
		`Environment="FOO=bar"`,
		"WatchdogSec=30",
		"Restart=always",
	}
	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("unit file missing %q\ngot:\n%s", part, content)
		}
	}
}

func TestSystemdSupportsCapabilities(t *testing.T) {
	mgr := &systemdManager{}
	if !mgr.SupportsEnable() {
		t.Error("SupportsEnable() = false, want true")
	}
	if !mgr.SupportsStart() {
		t.Error("SupportsStart() = false, want true")
	}
	if !mgr.SupportsStatus() {
		t.Error("SupportsStatus() = false, want true")
	}
}

func newTestSystemdManager(opts ...func(*systemdManager)) *systemdManager {
	m := &systemdManager{
		executor: newMockExecutor(),
		fs:       newMockFS(),
		statFunc: func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func TestSystemdDetect(t *testing.T) {
	tests := []struct {
		name      string
		statPaths map[string]bool
		want      bool
	}{
		{
			name:      "systemd directory exists",
			statPaths: map[string]bool{"/run/systemd/system": true},
			want:      true,
		},
		{
			name:      "systemd directory absent",
			statPaths: map[string]bool{},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := newTestSystemdManager(func(m *systemdManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					if tt.statPaths[path] {
						return mockFileInfo{name: path, isDir: true}, nil
					}
					return nil, os.ErrNotExist
				}
			})
			got := mgr.detect()
			if got != tt.want {
				t.Errorf("detect() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSystemdDetect_LogsWhenDetected(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	mgr := newTestSystemdManager(func(m *systemdManager) {
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/run/systemd/system" {
				return mockFileInfo{name: "system", isDir: true}, nil
			}
			return nil, os.ErrNotExist
		}
	})

	result := mgr.detect()
	if !result {
		t.Fatal("expected detect() to return true")
	}

	output := buf.String()
	expectedMessages := []string{
		"init detection: systemd: checking systemd detection",
		"init detection: systemd: stat /run/systemd/system -> found",
		"init detection: systemd: detected=true reason=systemd directory found",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}
}

func TestSystemdDetect_LogsWhenNotDetected(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	mgr := newTestSystemdManager()

	result := mgr.detect()
	if result {
		t.Fatal("expected detect() to return false")
	}

	output := buf.String()
	expectedMessages := []string{
		"init detection: systemd: checking systemd detection",
		"init detection: systemd: stat /run/systemd/system ->",
		"init detection: systemd: detected=false reason=systemd directory not found",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}

	if strings.Contains(output, "detected=true") {
		t.Errorf("should not contain detected=true when systemd not detected")
	}
}

func TestSystemdUserManagerReachable(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		euid       int
		statPaths  map[string]bool
		want       bool
		wantProbed []string
	}{
		{
			name:      "reachable via XDG_RUNTIME_DIR",
			env:       map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"},
			euid:      1000,
			statPaths: map[string]bool{"/run/user/1000/systemd/private": true},
			want:      true,
		},
		{
			name:      "reachable via euid fallback when XDG_RUNTIME_DIR unset",
			env:       map[string]string{},
			euid:      1001,
			statPaths: map[string]bool{"/run/user/1001/systemd/private": true},
			want:      true,
		},
		{
			name:      "XDG_RUNTIME_DIR set but socket absent, euid fallback wins",
			env:       map[string]string{"XDG_RUNTIME_DIR": "/tmp/stale"},
			euid:      1002,
			statPaths: map[string]bool{"/run/user/1002/systemd/private": true},
			want:      true,
		},
		{
			name:      "no socket anywhere",
			env:       map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"},
			euid:      1000,
			statPaths: map[string]bool{},
			want:      false,
		},
		{
			name:      "root without a session",
			env:       map[string]string{},
			euid:      0,
			statPaths: map[string]bool{"/run/user/1000/systemd/private": true},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var probed []string
			mgr := newTestSystemdManager(func(m *systemdManager) {
				m.statFunc = func(path string) (os.FileInfo, error) {
					probed = append(probed, path)
					if tt.statPaths[path] {
						return mockFileInfo{name: path}, nil
					}
					return nil, os.ErrNotExist
				}
				m.getenvFunc = func(k string) string { return tt.env[k] }
				m.geteuidFunc = func() int { return tt.euid }
			})

			if got := mgr.userManagerReachable(); got != tt.want {
				t.Errorf("userManagerReachable() = %v, want %v (probed %v)", got, tt.want, probed)
			}
		})
	}
}

func TestSystemdUserManagerReachable_PrefersXDGRuntimeDir(t *testing.T) {
	var probed []string
	mgr := newTestSystemdManager(func(m *systemdManager) {
		m.statFunc = func(path string) (os.FileInfo, error) {
			probed = append(probed, path)
			return mockFileInfo{name: path}, nil
		}
		m.getenvFunc = func(k string) string {
			if k == "XDG_RUNTIME_DIR" {
				return "/run/user/1000"
			}
			return ""
		}
		m.geteuidFunc = func() int { return 1000 }
	})

	if !mgr.userManagerReachable() {
		t.Fatal("userManagerReachable() = false, want true")
	}
	if len(probed) != 1 || probed[0] != "/run/user/1000/systemd/private" {
		t.Errorf("probed = %v, want only /run/user/1000/systemd/private", probed)
	}
}

func TestSystemdUserManagerReachable_NilSeamsUseOSDefaults(t *testing.T) {
	// A manager built by struct literal (as several tests do) must not panic.
	mgr := &systemdManager{}
	_ = mgr.userManagerReachable()
}
