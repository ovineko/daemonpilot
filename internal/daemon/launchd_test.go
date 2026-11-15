//go:build !windows

package daemon

import (
	"errors"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestGeneratePlistFile(t *testing.T) {
	config := ServiceConfig{
		Name:        "test-agent",
		Description: "Test Agent Service",
		ExecPath:    "/usr/local/bin/test-agent",
		InstallDir:  "/usr/local/bin",
		WorkingDir:  "/usr/local/bin",
		Arguments:   []string{"run"},
	}

	content, err := GeneratePlistFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedParts := []string{
		"<key>Label</key>",
		"<string>com.test-agent</string>",
		"<key>ProgramArguments</key>",
		"<string>/usr/local/bin/test-agent</string>",
		"<string>run</string>",
		"<key>WorkingDirectory</key>",
		"<key>KeepAlive</key>",
		"<true/>",
		"<key>RunAtLoad</key>",
		"<key>StandardOutPath</key>",
		"<key>StandardErrorPath</key>",
	}

	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("plist file missing %q\ngot:\n%s", part, content)
		}
	}
}

func TestGeneratePlistFile_ValidXML(t *testing.T) {
	config := ServiceConfig{
		Name:       "myapp",
		ExecPath:   "/usr/local/bin/myapp",
		WorkingDir: "/var/lib/myapp",
	}

	content, err := GeneratePlistFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := ValidatePlistXML(content); err != nil {
		t.Errorf("generated plist is not valid XML: %v\ncontent:\n%s", err, content)
	}
}

func TestGeneratePlistFile_XMLHeader(t *testing.T) {
	config := ServiceConfig{
		Name:       "myapp",
		ExecPath:   "/usr/local/bin/myapp",
		WorkingDir: "/tmp",
	}

	content, err := GeneratePlistFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasPrefix(content, "<?xml version=\"1.0\"") {
		t.Error("plist missing XML header")
	}
	if !strings.Contains(content, "<!DOCTYPE plist") {
		t.Error("plist missing DOCTYPE")
	}
}

func TestLaunchdLabel(t *testing.T) {
	got := launchdLabel("my-service")
	want := "com.my-service"
	if got != want {
		t.Errorf("launchdLabel() = %q, want %q", got, want)
	}
}

func TestLaunchdInstall(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := &launchdManager{executor: exec, fs: fs}

	config := ServiceConfig{
		Name:        "test-svc",
		Description: "Test Service",
		ExecPath:    "/usr/local/bin/test-svc",
		InstallDir:  "/usr/local/bin",
		WorkingDir:  "/usr/local/bin",
	}

	err := mgr.Install(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var plistWritten bool
	for path, data := range fs.files {
		if strings.HasSuffix(path, "com.test-svc.plist") {
			plistWritten = true
			if !strings.Contains(string(data), "com.test-svc") {
				t.Error("plist content missing label")
			}
		}
	}
	if !plistWritten {
		t.Error("plist file was not written")
	}

	var bootstrapCalled bool
	for _, c := range exec.calls {
		if c.Name == "launchctl" && len(c.Args) >= 1 && c.Args[0] == "bootstrap" {
			bootstrapCalled = true
		}
	}
	if !bootstrapCalled {
		t.Error("launchctl bootstrap was not called")
	}

	if mgr.label != "com.test-svc" {
		t.Errorf("label = %q, want %q", mgr.label, "com.test-svc")
	}
}

func TestLaunchdInstall_WriteFileError(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	fs.writeErr = errors.New("permission denied")
	mgr := &launchdManager{executor: exec, fs: fs}

	err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "writing plist file") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLaunchdInstall_LoadError(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()

	// First do a successful install to discover the plist path
	probe := &launchdManager{executor: newMockExecutor(), fs: newMockFS()}
	_ = probe.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
	})
	plistPath := probe.plistPath()

	exec.setResult("launchctl bootstrap system "+plistPath, nil, errors.New("bootstrap failed"))
	mgr := &launchdManager{executor: exec, fs: fs}

	err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bootstrapping service") {
		t.Errorf("unexpected error: %v", err)
	}

	// launchd auto-loads every plist it finds at the next login, and a failed
	// install writes no state file, so the plist must not survive the failure.
	if !fs.removed[plistPath] {
		t.Errorf("plist %s left behind after a failed bootstrap", plistPath)
	}
}

// A re-install that fails to bootstrap must put back the plist it overwrote:
// the state file still describes the previous service, and the documented
// asynchronous-bootout race would otherwise turn a transient failure into the
// destruction of a working install.
func TestLaunchdInstall_LoadErrorRestoresPreviousPlist(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()

	probe := &launchdManager{executor: newMockExecutor(), fs: newMockFS()}
	_ = probe.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
	})
	plistPath := probe.plistPath()

	const previous = "<plist>previous install</plist>"
	fs.files[plistPath] = []byte(previous)

	exec.setResult("launchctl bootstrap system "+plistPath, nil, errors.New("Input/output error"))
	mgr := &launchdManager{executor: exec, fs: fs}

	err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if fs.removed[plistPath] {
		t.Errorf("plist %s was deleted instead of restored", plistPath)
	}
	if got := string(fs.files[plistPath]); got != previous {
		t.Errorf("previous plist not restored:\ngot  %q\nwant %q", got, previous)
	}
}

// A read failure that is not "file missing" must abort before anything is
// mutated: treating it as absence would delete the plist of a service the
// state file still describes.
func TestLaunchdInstall_PlistReadErrorAborts(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	fs.readErr = errors.New("permission denied")
	mgr := &launchdManager{executor: exec, fs: fs}

	err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "reading plist file") {
		t.Errorf("unexpected error: %v", err)
	}
	if exec.called("launchctl", "bootstrap", launchdSystemDomain, mgr.plistPath()) {
		t.Error("service was bootstrapped despite the plist read failing")
	}
}

func TestLaunchdUninstall(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := &launchdManager{label: "com.test-svc", executor: exec, fs: fs}

	err := mgr.Uninstall()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var bootoutCalled bool
	for _, c := range exec.calls {
		if c.Name == "launchctl" && len(c.Args) >= 1 && c.Args[0] == "bootout" {
			bootoutCalled = true
		}
	}
	if !bootoutCalled {
		t.Error("launchctl bootout was not called")
	}

	var plistRemoved bool
	for path := range fs.removed {
		if strings.HasSuffix(path, "com.test-svc.plist") {
			plistRemoved = true
		}
	}
	if !plistRemoved {
		t.Error("plist file was not removed")
	}
}

func TestLaunchdUninstall_NoLabel(t *testing.T) {
	mgr := &launchdManager{executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Uninstall()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLaunchdUninstall_RemoveError(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	fs.removeErr = errors.New("permission denied")
	mgr := &launchdManager{label: "com.test-svc", executor: exec, fs: fs}

	err := mgr.Uninstall()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "removing plist file") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLaunchdStart(t *testing.T) {
	exec := newMockExecutor()
	mgr := &launchdManager{label: "com.test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Start()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exec.called("launchctl", "kickstart", "-k", "system/com.test-svc") {
		t.Error("launchctl kickstart was not called")
	}
}

func TestLaunchdStart_NoLabel(t *testing.T) {
	mgr := &launchdManager{executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Start()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLaunchdStart_Error(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("launchctl kickstart -k system/com.test-svc", nil, errors.New("start failed"))
	mgr := &launchdManager{label: "com.test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Start()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "starting service") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLaunchdStop(t *testing.T) {
	exec := newMockExecutor()
	mgr := &launchdManager{label: "com.test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Stop()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exec.called("launchctl", "kill", "SIGTERM", "system/com.test-svc") {
		t.Error("launchctl kill was not called")
	}
}

func TestLaunchdStop_NoLabel(t *testing.T) {
	mgr := &launchdManager{executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Stop()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLaunchdStop_Error(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("launchctl kill SIGTERM system/com.test-svc", nil, errors.New("stop failed"))
	mgr := &launchdManager{label: "com.test-svc", executor: exec, fs: newMockFS()}

	err := mgr.Stop()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "stopping service") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLaunchdEnableReturnsError(t *testing.T) {
	mgr := &launchdManager{label: "com.test-svc", executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Enable()
	if err == nil {
		t.Fatal("expected error from Enable()")
	}
	if !errors.Is(err, ErrEnableNotSupported) {
		t.Errorf("Enable() error = %v, want ErrEnableNotSupported", err)
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error message should contain 'not supported', got: %v", err)
	}
}

func TestLaunchdDisableReturnsError(t *testing.T) {
	mgr := &launchdManager{label: "com.test-svc", executor: newMockExecutor(), fs: newMockFS()}
	err := mgr.Disable()
	if err == nil {
		t.Fatal("expected error from Disable()")
	}
	if !errors.Is(err, ErrEnableNotSupported) {
		t.Errorf("Disable() error = %v, want ErrEnableNotSupported", err)
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error message should contain 'not supported', got: %v", err)
	}
}

func TestLaunchdStatus(t *testing.T) {
	const runningOutput = `com.test-svc = {
	active count = 1
	state = running
	pid = 1234
	last exit code = 0
}`
	const stoppedOutput = `com.test-svc = {
	active count = 0
	state = not running
	last exit code = 0
}`
	const failedOutput = `com.test-svc = {
	state = not running
	last exit code = 2
}`
	// Trimmed from real `launchctl print` output. The nested coalition
	// dictionaries carry a state key of their own and come *after* the job's,
	// so a parser that takes the last match reports a healthy service as failed;
	// "(never exited)" is what launchd prints for a job that has yet to run.
	const nestedRunningOutput = "system/com.test-svc = {\n\tactive count = 7\n\tpath = /Library/LaunchDaemons/com.test-svc.plist\n\tstate = running\n\tpid = 399\n\tlast exit code = (never exited)\n\n\tspawn type = daemon (3)\n\tresource coalition = {\n\t\tID = 301\n\t\ttype = resource\n\t\tstate = active\n\t\tcount = 1\n\t}\n\n\tjetsam coalition = {\n\t\tID = 302\n\t\ttype = jetsam\n\t\tstate = active\n\t\tcount = 1\n\t}"
	const nestedStoppedOutput = `system/com.test-svc = {
	active count = 0
	state = not running
	last exit code = (never exited)

	resource coalition = {
		state = active
	}
}`
	const signalledOutput = `com.test-svc = {
	state = not running
	last exit code = (signal: 15)
}`
	const pidOnlyOutput = `com.test-svc = {
	pid = 4242
}`
	// The output formats the pid fallback exists for omit state entirely. A job
	// that is not running and last exited non-zero is failed there too, so the
	// exit code is read whether or not state was present.
	const exitCodeWithoutStateOutput = `com.test-svc = {
	last exit code = 2
}`

	tests := []struct {
		name       string
		output     string
		execErr    error
		wantStatus ServiceStatus
		wantErr    string
	}{
		{name: "running", output: runningOutput, wantStatus: ServiceStatusRunning},
		{name: "loaded but stopped", output: stoppedOutput, wantStatus: ServiceStatusStopped},
		{name: "loaded and failed", output: failedOutput, wantStatus: ServiceStatusFailed},
		{
			name:       "running despite nested coalition state keys",
			output:     nestedRunningOutput,
			wantStatus: ServiceStatusRunning,
		},
		{
			name:       "never exited is stopped, not failed",
			output:     nestedStoppedOutput,
			wantStatus: ServiceStatusStopped,
		},
		{
			name:       "stopped by signal is not a failure",
			output:     signalledOutput,
			wantStatus: ServiceStatusStopped,
		},
		{name: "pid without state falls back to running", output: pidOnlyOutput, wantStatus: ServiceStatusRunning},
		{
			name:       "non-zero exit code without state is failed",
			output:     exitCodeWithoutStateOutput,
			wantStatus: ServiceStatusFailed,
		},
		{name: "empty output is stopped", output: "", wantStatus: ServiceStatusStopped},
		{
			name:       "not loaded",
			output:     "Could not find service \"com.test-svc\" in domain for system",
			execErr:    errors.New("exit status 113"),
			wantStatus: ServiceStatusStopped,
		},
		{
			name:       "no such process",
			output:     "launchctl print: No such process",
			execErr:    errors.New("exit status 3"),
			wantStatus: ServiceStatusStopped,
		},
		{
			name:       "permission denied",
			output:     "Could not print domain: 1: Operation not permitted",
			execErr:    errors.New("exit status 1"),
			wantStatus: ServiceStatusUnknown,
			wantErr:    "Operation not permitted",
		},
		{
			name:       "unreachable domain",
			output:     "Could not find domain for system",
			execErr:    errors.New("exit status 113"),
			wantStatus: ServiceStatusUnknown,
			wantErr:    "querying service status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			exec.setResult("launchctl print system/com.test-svc", []byte(tt.output), tt.execErr)
			mgr := &launchdManager{label: "com.test-svc", executor: exec, fs: newMockFS()}

			status, err := mgr.Status()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error, got status %v", status)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %v does not mention %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != tt.wantStatus {
				t.Errorf("status = %v, want %v", status, tt.wantStatus)
			}
		})
	}
}

// Status must name the domain the agent was bootstrapped into, not the caller's
// implicit one.
func TestLaunchdStatus_UsesResolvedDomain(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("launchctl print gui/501/com.test-svc", []byte("state = running\npid = 42"), nil)
	mgr := newUserLaunchdManager(exec, newMockFS())
	mgr.label = "com.test-svc"

	status, err := mgr.Status()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != ServiceStatusRunning {
		t.Errorf("status = %v, want running", status)
	}
	assertCalls(t, exec.calls, []mockCall{
		{Name: "launchctl", Args: []string{"print", "gui/501"}},
		{Name: "launchctl", Args: []string{"print", "gui/501/com.test-svc"}},
	})
}

func TestLaunchdStatus_UnresolvableDomain(t *testing.T) {
	exec := newMockExecutor()
	exec.fallbackErr = errors.New("exit status 113")
	mgr := newUserLaunchdManager(exec, newMockFS())
	mgr.label = "com.test-svc"

	status, err := mgr.Status()
	if err == nil {
		t.Fatal("expected error when no domain is reachable")
	}
	if status != ServiceStatusUnknown {
		t.Errorf("status = %v, want unknown", status)
	}
}

func TestGeneratePlistFile_UserAndGroupKeys(t *testing.T) {
	tests := []struct {
		name      string
		scope     Scope
		user      string
		group     string
		wantParts []string
		denyParts []string
	}{
		{
			name:      "neither set",
			scope:     ScopeSystem,
			denyParts: []string{"<key>UserName</key>", "<key>GroupName</key>"},
		},
		{
			name:      "user only",
			scope:     ScopeSystem,
			user:      "daemonpilot",
			wantParts: []string{"\t<key>UserName</key>\n\t<string>daemonpilot</string>"},
			denyParts: []string{"<key>GroupName</key>"},
		},
		{
			name:      "group only",
			scope:     ScopeSystem,
			group:     "staff",
			wantParts: []string{"\t<key>GroupName</key>\n\t<string>staff</string>"},
			denyParts: []string{"<key>UserName</key>"},
		},
		{
			name:  "both set",
			scope: ScopeSystem,
			user:  "daemonpilot",
			group: "staff",
			wantParts: []string{
				"\t<key>UserName</key>\n\t<string>daemonpilot</string>",
				"\t<key>GroupName</key>\n\t<string>staff</string>",
			},
		},
		{
			name:      "dropped in user scope",
			scope:     ScopeUser,
			user:      "daemonpilot",
			group:     "staff",
			denyParts: []string{"<key>UserName</key>", "<key>GroupName</key>"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := newUserLaunchdManager(newMockExecutor(), newMockFS())
			mgr.scope = tt.scope

			content, err := mgr.GenerateConfig(ServiceConfig{
				Name:       "test-svc",
				Scope:      tt.scope,
				User:       tt.user,
				Group:      tt.group,
				ExecPath:   "/usr/local/bin/test-svc",
				WorkingDir: "/usr/local/bin",
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			for _, part := range tt.wantParts {
				if !strings.Contains(content, part) {
					t.Errorf("plist missing %q\ngot:\n%s", part, content)
				}
			}
			for _, part := range tt.denyParts {
				if strings.Contains(content, part) {
					t.Errorf("plist unexpectedly contains %q\ngot:\n%s", part, content)
				}
			}
			if err := ValidatePlistXML(content); err != nil {
				t.Errorf("generated plist is not valid XML: %v", err)
			}
		})
	}
}

func TestLaunchdStatus_NoLabel(t *testing.T) {
	mgr := &launchdManager{executor: newMockExecutor(), fs: newMockFS()}
	_, err := mgr.Status()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLaunchdInstallUninstallRoundtrip(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := &launchdManager{executor: exec, fs: fs}

	config := ServiceConfig{
		Name:        "roundtrip-svc",
		Description: "Roundtrip Test",
		ExecPath:    "/usr/local/bin/roundtrip-svc",
		InstallDir:  "/usr/local/bin",
		WorkingDir:  "/usr/local/bin",
	}

	if err := mgr.Install(config); err != nil {
		t.Fatalf("install error: %v", err)
	}

	var plistWritten bool
	for path := range fs.files {
		if strings.HasSuffix(path, "com.roundtrip-svc.plist") {
			plistWritten = true
		}
	}
	if !plistWritten {
		t.Fatal("plist file not written after install")
	}

	if err := mgr.Uninstall(); err != nil {
		t.Fatalf("uninstall error: %v", err)
	}

	var plistRemoved bool
	for path := range fs.removed {
		if strings.HasSuffix(path, "com.roundtrip-svc.plist") {
			plistRemoved = true
		}
	}
	if !plistRemoved {
		t.Error("plist file not removed after uninstall")
	}
}

func TestLaunchdGetManifestPath(t *testing.T) {
	mgr := &launchdManager{label: "com.myapp", executor: newMockExecutor(), fs: newMockFS()}
	got := mgr.GetManifestPath()
	if !strings.HasSuffix(got, "Library/LaunchDaemons/com.myapp.plist") {
		t.Errorf("GetManifestPath() = %q, want suffix Library/LaunchDaemons/com.myapp.plist", got)
	}
}

func TestLaunchdGetManifestPath_EmptyLabel(t *testing.T) {
	mgr := &launchdManager{label: "", executor: newMockExecutor(), fs: newMockFS()}
	got := mgr.GetManifestPath()
	if !strings.HasSuffix(got, ".plist") {
		t.Errorf("GetManifestPath() = %q, want .plist suffix", got)
	}
}

func TestGeneratePlistFile_WithEnvironment(t *testing.T) {
	config := ServiceConfig{
		Name:       "env-app",
		ExecPath:   "/usr/local/bin/env-app",
		WorkingDir: "/tmp",
		Environment: map[string]string{
			"APP_ENV":  "production",
			"APP_PORT": "8080",
		},
	}

	content, err := GeneratePlistFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedParts := []string{
		"<key>EnvironmentVariables</key>",
		"<dict>",
		"<key>APP_ENV</key>",
		"<string>production</string>",
		"<key>APP_PORT</key>",
		"<string>8080</string>",
	}
	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("plist missing %q\ngot:\n%s", part, content)
		}
	}

	if err := ValidatePlistXML(content); err != nil {
		t.Errorf("generated plist is not valid XML: %v", err)
	}
}

func TestGeneratePlistFile_WithArguments(t *testing.T) {
	config := ServiceConfig{
		Name:       "arg-app",
		ExecPath:   "/usr/local/bin/arg-app",
		WorkingDir: "/tmp",
		Arguments:  []string{"run", "--port", "9090", "--verbose"},
	}

	content, err := GeneratePlistFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedParts := []string{
		"<string>run</string>",
		"<string>--port</string>",
		"<string>9090</string>",
		"<string>--verbose</string>",
	}
	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("plist missing %q\ngot:\n%s", part, content)
		}
	}

	if err := ValidatePlistXML(content); err != nil {
		t.Errorf("generated plist is not valid XML: %v", err)
	}
}

func TestGeneratePlistFile_WithLaunchdKeys(t *testing.T) {
	config := ServiceConfig{
		Name:       "custom-app",
		ExecPath:   "/usr/local/bin/custom-app",
		WorkingDir: "/tmp",
		LaunchdKeys: map[string]any{
			"ThrottleInterval": 10,
			"Nice":             -5,
			"LowPriorityIO":    true,
		},
	}

	content, err := GeneratePlistFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedParts := []string{
		"<key>LowPriorityIO</key>",
		"<true/>",
		"<key>Nice</key>",
		"<integer>-5</integer>",
		"<key>ThrottleInterval</key>",
		"<integer>10</integer>",
	}
	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("plist missing %q\ngot:\n%s", part, content)
		}
	}

	if err := ValidatePlistXML(content); err != nil {
		t.Errorf("generated plist is not valid XML: %v", err)
	}
}

func TestGeneratePlistFile_WithAllOptionalFields(t *testing.T) {
	config := ServiceConfig{
		Name:       "full-app",
		ExecPath:   "/usr/local/bin/full-app",
		WorkingDir: "/tmp",
		Arguments:  []string{"run", "--config", "/etc/full-app.conf"},
		Environment: map[string]string{
			"LOG_LEVEL": "debug",
		},
		LaunchdKeys: map[string]any{
			"ProcessType": "Background",
		},
	}

	content, err := GeneratePlistFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedParts := []string{
		"<string>--config</string>",
		"<string>/etc/full-app.conf</string>",
		"<key>EnvironmentVariables</key>",
		"<key>LOG_LEVEL</key>",
		"<string>debug</string>",
		"<key>ProcessType</key>",
		"<string>Background</string>",
	}
	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("plist missing %q\ngot:\n%s", part, content)
		}
	}

	if err := ValidatePlistXML(content); err != nil {
		t.Errorf("generated plist is not valid XML: %v\ncontent:\n%s", err, content)
	}
}

func TestGeneratePlistFile_NoOptionalFields(t *testing.T) {
	config := ServiceConfig{
		Name:       "basic-app",
		ExecPath:   "/usr/local/bin/basic-app",
		WorkingDir: "/tmp",
		Arguments:  []string{"run"},
	}

	content, err := GeneratePlistFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(content, "EnvironmentVariables") {
		t.Error("plist should not contain EnvironmentVariables when none set")
	}

	argCount := strings.Count(content, "<string>run</string>")
	if argCount != 1 {
		t.Errorf("expected exactly 1 'run' argument, got %d", argCount)
	}

	if err := ValidatePlistXML(content); err != nil {
		t.Errorf("generated plist is not valid XML: %v", err)
	}
}

func TestGeneratePlistFile_XMLEscapingInEnvironment(t *testing.T) {
	config := ServiceConfig{
		Name:       "escape-app",
		ExecPath:   "/usr/local/bin/escape-app",
		WorkingDir: "/tmp",
		Environment: map[string]string{
			"QUERY": "a<b&c",
		},
	}

	content, err := GeneratePlistFile(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(content, "a<b&c") {
		t.Error("environment value was not XML-escaped")
	}
	if !strings.Contains(content, "a&lt;b&amp;c") {
		t.Errorf("expected escaped value 'a&lt;b&amp;c' in plist\ngot:\n%s", content)
	}

	if err := ValidatePlistXML(content); err != nil {
		t.Errorf("generated plist is not valid XML: %v", err)
	}
}

func TestRenderPlistValue(t *testing.T) {
	tests := []struct {
		name string
		val  any
		want string
	}{
		{"string", "hello", "\t<string>hello</string>"},
		{"bool true", true, "\t<true/>"},
		{"bool false", false, "\t<false/>"},
		{"int", 42, "\t<integer>42</integer>"},
		{"int64", int64(100), "\t<integer>100</integer>"},
		{"float64", 3.14, "\t<real>3.14</real>"},
		{"other", []int{1}, "\t<string>[1]</string>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderPlistValue(tt.val)
			if got != tt.want {
				t.Errorf("renderPlistValue(%v) = %q, want %q", tt.val, got, tt.want)
			}
		})
	}
}

func TestLaunchdGenerateConfig(t *testing.T) {
	mgr := &launchdManager{executor: newMockExecutor(), fs: newMockFS()}
	config := ServiceConfig{
		Name:        "test-agent",
		Description: "Test Agent",
		ExecPath:    "/usr/local/bin/test-agent",
		InstallDir:  "/usr/local/bin",
		WorkingDir:  "/usr/local/bin",
	}

	content, err := mgr.GenerateConfig(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mgr.label != "com.test-agent" {
		t.Errorf("label = %q, want %q", mgr.label, "com.test-agent")
	}

	expectedParts := []string{
		"<key>Label</key>",
		"<string>com.test-agent</string>",
		"<key>ProgramArguments</key>",
	}
	for _, part := range expectedParts {
		if !strings.Contains(content, part) {
			t.Errorf("generated config missing %q", part)
		}
	}
}

func TestLaunchdSupportsCapabilities(t *testing.T) {
	mgr := &launchdManager{}
	if mgr.SupportsEnable() {
		t.Error("SupportsEnable() = true, want false")
	}
	if !mgr.SupportsStart() {
		t.Error("SupportsStart() = false, want true")
	}
	if !mgr.SupportsStatus() {
		t.Error("SupportsStatus() = false, want true")
	}
}

// The tests below pin the exact system-scope behaviour (log paths, plist
// location, launchctl argv) so a later change introducing the user scope cannot
// silently alter it.

func TestGeneratePlistFile_SystemScopeLogPaths(t *testing.T) {
	tests := []struct {
		name     string
		svcName  string
		wantOut  string
		wantErrP string
	}{
		{
			name:     "plain name",
			svcName:  "test-agent",
			wantOut:  "\t<key>StandardOutPath</key>\n\t<string>/var/log/test-agent/test-agent.out.log</string>",
			wantErrP: "\t<key>StandardErrorPath</key>\n\t<string>/var/log/test-agent/test-agent.err.log</string>",
		},
		{
			name:     "dotted name",
			svcName:  "com.example.agent",
			wantOut:  "\t<key>StandardOutPath</key>\n\t<string>/var/log/com.example.agent/com.example.agent.out.log</string>",
			wantErrP: "\t<key>StandardErrorPath</key>\n\t<string>/var/log/com.example.agent/com.example.agent.err.log</string>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := GeneratePlistFile(ServiceConfig{
				Name:       tt.svcName,
				ExecPath:   "/usr/local/bin/" + tt.svcName,
				WorkingDir: "/usr/local/bin",
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !strings.Contains(content, tt.wantOut) {
				t.Errorf("plist missing exact StandardOutPath block %q\ngot:\n%s", tt.wantOut, content)
			}
			if !strings.Contains(content, tt.wantErrP) {
				t.Errorf("plist missing exact StandardErrorPath block %q\ngot:\n%s", tt.wantErrP, content)
			}
		})
	}
}

func TestLaunchdLogPaths_PerScope(t *testing.T) {
	tests := []struct {
		name    string
		scope   Scope
		logDir  string
		wantDir string
	}{
		{name: "system scope default", scope: ScopeSystem, wantDir: "/var/log/test-svc"},
		{name: "user scope default", scope: ScopeUser, wantDir: "/Users/alice/Library/Logs/test-svc"},
		{name: "explicit override in system scope", scope: ScopeSystem, logDir: "/srv/logs/test-svc", wantDir: "/srv/logs/test-svc"},
		{name: "explicit override in user scope", scope: ScopeUser, logDir: "/srv/logs/test-svc", wantDir: "/srv/logs/test-svc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := newUserLaunchdManager(newMockExecutor(), newMockFS())
			mgr.scope = tt.scope

			content, err := mgr.GenerateConfig(ServiceConfig{
				Name:       "test-svc",
				Scope:      tt.scope,
				LogDir:     tt.logDir,
				ExecPath:   "/usr/local/bin/test-svc",
				WorkingDir: "/usr/local/bin",
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			wantOut := "\t<key>StandardOutPath</key>\n\t<string>" + tt.wantDir + "/test-svc.out.log</string>"
			wantErr := "\t<key>StandardErrorPath</key>\n\t<string>" + tt.wantDir + "/test-svc.err.log</string>"
			if !strings.Contains(content, wantOut) {
				t.Errorf("plist missing %q\ngot:\n%s", wantOut, content)
			}
			if !strings.Contains(content, wantErr) {
				t.Errorf("plist missing %q\ngot:\n%s", wantErr, content)
			}
			if strings.Contains(content, "<string>/tmp/") {
				t.Errorf("plist still points at /tmp:\n%s", content)
			}
		})
	}
}

func TestLaunchdInstall_CreatesLogDir(t *testing.T) {
	tests := []struct {
		name    string
		scope   Scope
		logDir  string
		wantDir string
	}{
		{name: "system scope", scope: ScopeSystem, wantDir: "/var/log/test-svc"},
		{name: "user scope", scope: ScopeUser, wantDir: "/Users/alice/Library/Logs/test-svc"},
		{name: "explicit override", scope: ScopeUser, logDir: "/srv/logs/test-svc", wantDir: "/srv/logs/test-svc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newMockFS()
			mgr := newUserLaunchdManager(newMockExecutor(), fs)
			mgr.scope = tt.scope

			if err := mgr.Install(ServiceConfig{
				Name:       "test-svc",
				Scope:      tt.scope,
				LogDir:     tt.logDir,
				ExecPath:   "/usr/local/bin/test-svc",
				WorkingDir: "/usr/local/bin",
			}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !fs.dirs[tt.wantDir] {
				t.Errorf("expected log directory %q to be created, got %v", tt.wantDir, mapKeys(fs.dirs))
			}
		})
	}
}

// launchd opens StandardOutPath after dropping privileges to UserName, and the
// log directory under /var/log is created here by root. A system-scope job
// carrying UserName therefore needs the directory handed to that account, or it
// never spawns — the former /tmp target was world-writable and hid this.
func TestLaunchdInstall_ChownsLogDirToServiceUser(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skipf("cannot resolve the current user: %v", err)
	}
	wantUID, err := strconv.Atoi(me.Uid)
	if err != nil {
		t.Skipf("non-numeric uid %q", me.Uid)
	}

	tests := []struct {
		name     string
		scope    Scope
		user     string
		wantUID  int
		wantCall bool
	}{
		{name: "system scope with a service user", scope: ScopeSystem, user: me.Username, wantUID: wantUID, wantCall: true},
		{name: "system scope without a service user", scope: ScopeSystem},
		{name: "user scope never drops privileges", scope: ScopeUser, user: me.Username},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newMockFS()
			mgr := newUserLaunchdManager(newMockExecutor(), fs)
			mgr.scope = tt.scope

			config := ServiceConfig{
				Name:       "test-svc",
				Scope:      tt.scope,
				User:       tt.user,
				LogDir:     "/var/log/test-svc",
				ExecPath:   "/usr/local/bin/test-svc",
				WorkingDir: "/usr/local/bin",
			}
			if err := mgr.Install(config); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			owner, chowned := fs.chowned["/var/log/test-svc"]
			if !tt.wantCall {
				if chowned {
					t.Errorf("log directory was chowned to %v, want no chown", owner)
				}
				return
			}
			if !chowned {
				t.Fatal("log directory was not chowned to the service user")
			}
			if owner[0] != tt.wantUID {
				t.Errorf("chown uid = %d, want %d", owner[0], tt.wantUID)
			}
			if owner[1] != -1 {
				t.Errorf("chown gid = %d, want -1 for an unset group", owner[1])
			}
		})
	}
}

// MkdirAll succeeds silently on an existing directory, so an install pointed at
// a shared path must not hand that path to the service account: root would be
// giving away a system directory it did not create. The per-service default is
// the exception — it belongs to the service by construction.
func TestLaunchdInstall_DoesNotChownAPreexistingSharedLogDir(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skipf("cannot resolve the current user: %v", err)
	}

	tests := []struct {
		name      string
		logDir    string
		wantChown bool
	}{
		{name: "shared directory that already exists", logDir: "/var/log"},
		{name: "per-service default that already exists", logDir: "/var/log/test-svc", wantChown: true},
		// The default is recognised by path, so it has to survive the spellings
		// a configured LogDir can arrive in: an unchowned log directory makes
		// launchd refuse to spawn the job with no useful diagnostic.
		{name: "per-service default with a trailing slash", logDir: "/var/log/test-svc/", wantChown: true},
		{name: "per-service default with a dot segment", logDir: "/var/log/./test-svc", wantChown: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newMockFS()
			fs.dirs[tt.logDir] = true
			mgr := newUserLaunchdManager(newMockExecutor(), fs)
			mgr.scope = ScopeSystem

			if err := mgr.Install(ServiceConfig{
				Name:       "test-svc",
				Scope:      ScopeSystem,
				User:       me.Username,
				LogDir:     tt.logDir,
				ExecPath:   "/usr/local/bin/test-svc",
				WorkingDir: "/usr/local/bin",
			}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			owner, chowned := fs.chowned[tt.logDir]
			if chowned != tt.wantChown {
				t.Errorf("chown of %s = %v (owner %v), want %v", tt.logDir, chowned, owner, tt.wantChown)
			}
		})
	}
}

// A re-install resolves its own domain, but the agent may still be loaded in the
// domain the previous install recorded. Booting out only the new target would
// leave two supervisors against one binary, with the state file — now naming the
// new domain — unable to remove the old one.
func TestLaunchdInstall_BootsOutThePreviousDomain(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := newUserLaunchdManager(exec, fs)
	mgr.scope = ScopeUser
	mgr.domain = "user/501"
	mgr.previousDomain = "gui/501"

	if err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		Scope:      ScopeUser,
		ExecPath:   "/Users/alice/bin/test-svc",
		WorkingDir: "/Users/alice/bin",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !exec.called("launchctl", "bootout", "gui/501/com.test-svc") {
		t.Error("the previously recorded domain was not booted out")
	}
	if !exec.called("launchctl", "bootout", "user/501/com.test-svc") {
		t.Error("the resolved domain was not booted out")
	}
}

// The previously recorded domain is not always available: install reads it
// through readableState, which reports an unreadable or corrupt state file as
// "no previous install", and an install that failed before writing state never
// recorded one. Both per-user domains are therefore booted out regardless.
func TestLaunchdBootsOutBothUserDomainsWithoutARecordedOne(t *testing.T) {
	tests := []struct {
		name  string
		write func(m *launchdManager) error
	}{
		{
			name: "Install",
			write: func(m *launchdManager) error {
				return m.Install(ServiceConfig{
					Name:       "test-svc",
					Scope:      ScopeUser,
					ExecPath:   "/Users/alice/bin/test-svc",
					WorkingDir: "/Users/alice/bin",
				})
			},
		},
		{
			name:  "WriteConfig",
			write: func(m *launchdManager) error { return m.WriteConfig("<plist>new</plist>") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			mgr := newUserLaunchdManager(exec, newMockFS())
			mgr.label = "com.test-svc"
			mgr.scope = ScopeUser
			mgr.domain = "user/501"

			if err := tt.write(mgr); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			for _, domain := range []string{"gui/501", "user/501"} {
				if !exec.called("launchctl", "bootout", domain+"/com.test-svc") {
					t.Errorf("%s was not booted out, calls = %v", domain, exec.calls)
				}
			}
		})
	}
}

// The idempotency bootout unloads the running job before the new plist is
// bootstrapped. When that bootstrap fails and the previous plist is restored,
// the job has to be put back under supervision too — otherwise a transient
// failure leaves a working service unloaded with a state file still claiming it
// is installed.
func TestLaunchdInstall_ReloadsThePreviousPlistAfterRollback(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()

	probe := &launchdManager{executor: newMockExecutor(), fs: newMockFS()}
	_ = probe.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
	})
	plistPath := probe.plistPath()

	fs.files[plistPath] = []byte("<plist>previous install</plist>")
	exec.setResult("launchctl bootstrap system "+plistPath, nil, errors.New("Input/output error"))

	mgr := &launchdManager{executor: exec, fs: fs}
	if err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
	}); err == nil {
		t.Fatal("expected the install to report the bootstrap failure")
	}

	bootstraps := 0
	for _, c := range exec.calls {
		if c.Name == "launchctl" && len(c.Args) > 0 && c.Args[0] == "bootstrap" {
			bootstraps++
		}
	}
	if bootstraps != 2 {
		t.Errorf("bootstrap called %d times, want 2 (the failed install and the reload)", bootstraps)
	}
}

// A restore that cannot be written leaves the rejected plist on disk. Reloading
// then bootstraps the very configuration launchd just refused, so the reload
// has to be gated on the restore having succeeded, not on there having been a
// previous plist.
func TestLaunchdInstall_NoReloadWhenTheRestoreWriteFails(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()

	probe := &launchdManager{executor: newMockExecutor(), fs: newMockFS()}
	_ = probe.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
	})
	plistPath := probe.plistPath()

	fs.files[plistPath] = []byte("<plist>previous install</plist>")
	// The install's own plist write succeeds; the rollback's restore does not.
	fs.writeErrAfter = 1
	exec.setResult("launchctl bootstrap system "+plistPath, nil, errors.New("Input/output error"))

	mgr := &launchdManager{executor: exec, fs: fs}
	if err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
	}); err == nil {
		t.Fatal("expected the install to report the bootstrap failure")
	}

	bootstraps := 0
	for _, c := range exec.calls {
		if c.Name == "launchctl" && len(c.Args) > 0 && c.Args[0] == "bootstrap" {
			bootstraps++
		}
	}
	if bootstraps != 1 {
		t.Errorf("bootstrap called %d times, want 1 (the failed install only, with no reload)", bootstraps)
	}
}

// Nothing was loaded before a first install, so a failed bootstrap must not
// bootstrap the plist it just deleted.
func TestLaunchdInstall_NoReloadWhenThereWasNoPreviousPlist(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()

	probe := &launchdManager{executor: newMockExecutor(), fs: newMockFS()}
	_ = probe.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
	})
	plistPath := probe.plistPath()

	exec.setResult("launchctl bootstrap system "+plistPath, nil, errors.New("Input/output error"))
	mgr := &launchdManager{executor: exec, fs: fs}
	if err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
	}); err == nil {
		t.Fatal("expected the install to report the bootstrap failure")
	}

	bootstraps := 0
	for _, c := range exec.calls {
		if c.Name == "launchctl" && len(c.Args) > 0 && c.Args[0] == "bootstrap" {
			bootstraps++
		}
	}
	if bootstraps != 1 {
		t.Errorf("bootstrap called %d times, want 1", bootstraps)
	}
}

// An unknown service user must fail the install rather than leave a log
// directory the job cannot write into.
func TestLaunchdInstall_UnknownServiceUserFails(t *testing.T) {
	fs := newMockFS()
	mgr := newUserLaunchdManager(newMockExecutor(), fs)
	mgr.scope = ScopeSystem

	err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		Scope:      ScopeSystem,
		User:       "no-such-user-for-daemonpilot",
		LogDir:     "/var/log/test-svc",
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
	})
	if err == nil {
		t.Fatal("Install accepted an unknown service user")
	}
	if !strings.Contains(err.Error(), "no-such-user-for-daemonpilot") {
		t.Errorf("error %v does not name the unknown user", err)
	}
	if len(fs.files) != 0 {
		t.Errorf("a failed install wrote %v", mapKeys(fs.files))
	}
}

// A user-scope install with no resolvable home must fail rather than fall back
// to a machine-wide log location.
func TestLaunchdLogDir_UserScopeWithoutHome(t *testing.T) {
	mgr := newUserLaunchdManager(newMockExecutor(), newMockFS())
	mgr.homeFunc = func() (string, error) { return "", nil }

	if _, err := mgr.GenerateConfig(ServiceConfig{
		Name:       "test-svc",
		Scope:      ScopeUser,
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
	}); !errors.Is(err, ErrNoHomeDir) {
		t.Fatalf("GenerateConfig() error = %v, want %v", err, ErrNoHomeDir)
	}
}

// launchd auto-loads every plist under ~/Library/LaunchAgents at the next login,
// so an install that fails on the log directory must not leave one behind: it
// would come back on its own and fail to spawn, with no state file recording it.
func TestLaunchd_LogDirFailureLeavesNoPlist(t *testing.T) {
	const logDir = "/srv/logs/test-svc"
	config := ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
		LogDir:     logDir,
	}

	t.Run("Install", func(t *testing.T) {
		exec := newMockExecutor()
		fs := newMockFS()
		fs.mkdirErrPath = logDir
		mgr := &launchdManager{executor: exec, fs: fs}

		if err := mgr.Install(config); err == nil || !strings.Contains(err.Error(), "creating log directory") {
			t.Fatalf("Install() error = %v, want a log directory error", err)
		}
		if len(fs.files) != 0 {
			t.Errorf("plist written despite the log directory failing: %v", mapKeys(fs.files))
		}
		if exec.called("launchctl", "bootstrap", launchdSystemDomain, "/Library/LaunchDaemons/com.test-svc.plist") {
			t.Error("service was bootstrapped despite the log directory failing")
		}
	})

	// WriteConfig rewrites an existing install, so failing before the write is
	// what keeps a working plist from being replaced by one launchd cannot spawn.
	t.Run("WriteConfig", func(t *testing.T) {
		exec := newMockExecutor()
		fs := newMockFS()
		fs.mkdirErrPath = logDir
		mgr := &launchdManager{executor: exec, fs: fs, label: "com.test-svc", logDir: logDir}

		if err := mgr.WriteConfig("<plist/>"); err == nil || !strings.Contains(err.Error(), "creating log directory") {
			t.Fatalf("WriteConfig() error = %v, want a log directory error", err)
		}
		if len(fs.files) != 0 {
			t.Errorf("plist rewritten despite the log directory failing: %v", mapKeys(fs.files))
		}
	})
}

func TestLaunchdInstall_SystemScopePlistPath(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := &launchdManager{executor: exec, fs: fs}

	if err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/usr/local/bin/test-svc",
		WorkingDir: "/usr/local/bin",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const wantPath = "/Library/LaunchDaemons/com.test-svc.plist"
	if _, ok := fs.files[wantPath]; !ok {
		t.Errorf("plist not written to %q; written paths: %v", wantPath, mapKeys(fs.files))
	}
	if len(fs.files) != 1 {
		t.Errorf("expected exactly 1 file written, got %v", mapKeys(fs.files))
	}
	if !fs.dirs["/Library/LaunchDaemons"] {
		t.Errorf("expected /Library/LaunchDaemons to be created, got %v", mapKeys(fs.dirs))
	}
	if got := mgr.GetManifestPath(); got != wantPath {
		t.Errorf("GetManifestPath() = %q, want %q", got, wantPath)
	}
}

func TestLaunchdSystemScopeLaunchctlArgv(t *testing.T) {
	tests := []struct {
		name string
		run  func(m *launchdManager) error
		want []mockCall
	}{
		{
			name: "install",
			run: func(m *launchdManager) error {
				return m.Install(ServiceConfig{
					Name:       "test-svc",
					ExecPath:   "/usr/local/bin/test-svc",
					WorkingDir: "/usr/local/bin",
				})
			},
			want: []mockCall{
				{Name: "launchctl", Args: []string{"bootout", "system/com.test-svc"}},
				{Name: "launchctl", Args: []string{"bootstrap", "system", "/Library/LaunchDaemons/com.test-svc.plist"}},
			},
		},
		{
			name: "uninstall",
			run:  func(m *launchdManager) error { m.label = "com.test-svc"; return m.Uninstall() },
			want: []mockCall{
				{Name: "launchctl", Args: []string{"bootout", "system/com.test-svc"}},
			},
		},
		{
			name: "start",
			run:  func(m *launchdManager) error { m.label = "com.test-svc"; return m.Start() },
			want: []mockCall{
				{Name: "launchctl", Args: []string{"kickstart", "-k", "system/com.test-svc"}},
			},
		},
		{
			name: "stop",
			run:  func(m *launchdManager) error { m.label = "com.test-svc"; return m.Stop() },
			want: []mockCall{
				{Name: "launchctl", Args: []string{"kill", "SIGTERM", "system/com.test-svc"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			mgr := &launchdManager{executor: exec, fs: newMockFS()}

			if err := tt.run(mgr); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(exec.calls) != len(tt.want) {
				t.Fatalf("got %d calls %v, want %d %v", len(exec.calls), exec.calls, len(tt.want), tt.want)
			}
			for i, want := range tt.want {
				got := exec.calls[i]
				if got.Name != want.Name || strings.Join(got.Args, "\x00") != strings.Join(want.Args, "\x00") {
					t.Errorf("call %d = %s %v, want %s %v", i, got.Name, got.Args, want.Name, want.Args)
				}
			}
		})
	}
}

// newUserLaunchdManager builds a user-scope manager whose home and uid are
// fixed, so the plist path and the domain target are host-independent.
func newUserLaunchdManager(exec *mockExecutor, fs *mockFS) *launchdManager {
	return &launchdManager{
		scope:       ScopeUser,
		executor:    exec,
		fs:          fs,
		homeFunc:    func() (string, error) { return "/Users/alice", nil },
		geteuidFunc: func() int { return 501 },
	}
}

func TestLaunchdPlistPath_PerScope(t *testing.T) {
	tests := []struct {
		name     string
		scope    Scope
		home     string
		homeErr  error
		want     string
		wantErrI error
	}{
		{name: "system scope", scope: ScopeSystem, home: "/Users/alice", want: "/Library/LaunchDaemons/com.test-svc.plist"},
		{name: "user scope", scope: ScopeUser, home: "/Users/alice", want: "/Users/alice/Library/LaunchAgents/com.test-svc.plist"},
		{name: "user scope with empty home", scope: ScopeUser, home: "", wantErrI: ErrNoHomeDir},
		{name: "user scope with home error", scope: ScopeUser, homeErr: errors.New("no such user"), wantErrI: ErrNoHomeDir},
		// The system-scope path must not consult $HOME at all.
		{name: "system scope with home error", scope: ScopeSystem, homeErr: errors.New("no such user"), want: "/Library/LaunchDaemons/com.test-svc.plist"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &launchdManager{
				label:    "com.test-svc",
				scope:    tt.scope,
				executor: newMockExecutor(),
				fs:       newMockFS(),
				homeFunc: func() (string, error) { return tt.home, tt.homeErr },
			}

			got, err := mgr.plistPathErr()
			if tt.wantErrI != nil {
				if !errors.Is(err, tt.wantErrI) {
					t.Fatalf("plistPathErr() error = %v, want %v", err, tt.wantErrI)
				}
				if path := mgr.plistPath(); path != "" {
					t.Errorf("plistPath() = %q, want empty on error", path)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("plistPathErr() = %q, want %q", got, tt.want)
			}
			if path := mgr.plistPath(); path != tt.want {
				t.Errorf("plistPath() = %q, want %q", path, tt.want)
			}
			if path := mgr.GetManifestPath(); path != tt.want {
				t.Errorf("GetManifestPath() = %q, want %q", path, tt.want)
			}
		})
	}
}

func TestLaunchdUserScopeLaunchctlArgv(t *testing.T) {
	const plistPath = "/Users/alice/Library/LaunchAgents/com.test-svc.plist"

	tests := []struct {
		name string
		// guiErr makes `launchctl print gui/501` fail, forcing the user/501 fallback.
		guiErr bool
		run    func(m *launchdManager) error
		want   []mockCall
	}{
		{
			name: "install under gui",
			run: func(m *launchdManager) error {
				return m.Install(ServiceConfig{
					Name:       "test-svc",
					Scope:      ScopeUser,
					ExecPath:   "/Users/alice/.local/share/test-svc/test-svc",
					WorkingDir: "/Users/alice/.local/share/test-svc",
				})
			},
			want: []mockCall{
				{Name: "launchctl", Args: []string{"print", "gui/501"}},
				{Name: "launchctl", Args: []string{"bootout", "user/501/com.test-svc"}},
				{Name: "launchctl", Args: []string{"bootout", "gui/501/com.test-svc"}},
				{Name: "launchctl", Args: []string{"bootstrap", "gui/501", plistPath}},
			},
		},
		{
			name:   "install falls back to user domain",
			guiErr: true,
			run: func(m *launchdManager) error {
				return m.Install(ServiceConfig{
					Name:       "test-svc",
					Scope:      ScopeUser,
					ExecPath:   "/Users/alice/.local/share/test-svc/test-svc",
					WorkingDir: "/Users/alice/.local/share/test-svc",
				})
			},
			want: []mockCall{
				{Name: "launchctl", Args: []string{"print", "gui/501"}},
				{Name: "launchctl", Args: []string{"print", "user/501"}},
				{Name: "launchctl", Args: []string{"bootout", "gui/501/com.test-svc"}},
				{Name: "launchctl", Args: []string{"bootout", "user/501/com.test-svc"}},
				{Name: "launchctl", Args: []string{"bootstrap", "user/501", plistPath}},
			},
		},
		{
			name: "uninstall",
			run:  func(m *launchdManager) error { m.label = "com.test-svc"; return m.Uninstall() },
			want: []mockCall{
				{Name: "launchctl", Args: []string{"print", "gui/501"}},
				{Name: "launchctl", Args: []string{"bootout", "gui/501/com.test-svc"}},
				{Name: "launchctl", Args: []string{"bootout", "user/501/com.test-svc"}},
			},
		},
		{
			name:   "uninstall under user fallback",
			guiErr: true,
			run:    func(m *launchdManager) error { m.label = "com.test-svc"; return m.Uninstall() },
			want: []mockCall{
				{Name: "launchctl", Args: []string{"print", "gui/501"}},
				{Name: "launchctl", Args: []string{"print", "user/501"}},
				{Name: "launchctl", Args: []string{"bootout", "user/501/com.test-svc"}},
				{Name: "launchctl", Args: []string{"bootout", "gui/501/com.test-svc"}},
			},
		},
		{
			name: "start",
			run:  func(m *launchdManager) error { m.label = "com.test-svc"; return m.Start() },
			want: []mockCall{
				{Name: "launchctl", Args: []string{"print", "gui/501"}},
				{Name: "launchctl", Args: []string{"kickstart", "-k", "gui/501/com.test-svc"}},
			},
		},
		{
			name:   "start under user fallback",
			guiErr: true,
			run:    func(m *launchdManager) error { m.label = "com.test-svc"; return m.Start() },
			want: []mockCall{
				{Name: "launchctl", Args: []string{"print", "gui/501"}},
				{Name: "launchctl", Args: []string{"print", "user/501"}},
				{Name: "launchctl", Args: []string{"kickstart", "-k", "user/501/com.test-svc"}},
			},
		},
		{
			name: "stop",
			run:  func(m *launchdManager) error { m.label = "com.test-svc"; return m.Stop() },
			want: []mockCall{
				{Name: "launchctl", Args: []string{"print", "gui/501"}},
				{Name: "launchctl", Args: []string{"kill", "SIGTERM", "gui/501/com.test-svc"}},
			},
		},
		{
			name:   "stop under user fallback",
			guiErr: true,
			run:    func(m *launchdManager) error { m.label = "com.test-svc"; return m.Stop() },
			want: []mockCall{
				{Name: "launchctl", Args: []string{"print", "gui/501"}},
				{Name: "launchctl", Args: []string{"print", "user/501"}},
				{Name: "launchctl", Args: []string{"kill", "SIGTERM", "user/501/com.test-svc"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			if tt.guiErr {
				exec.setResult("launchctl print gui/501", []byte("Could not find domain for"), errors.New("exit status 113"))
			}
			mgr := newUserLaunchdManager(exec, newMockFS())

			if err := tt.run(mgr); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			assertCalls(t, exec.calls, tt.want)
		})
	}
}

func TestLaunchdInstall_UserScopeWritesToLaunchAgents(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := newUserLaunchdManager(exec, fs)

	if err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		Scope:      ScopeUser,
		ExecPath:   "/Users/alice/.local/share/test-svc/test-svc",
		WorkingDir: "/Users/alice/.local/share/test-svc",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const wantPath = "/Users/alice/Library/LaunchAgents/com.test-svc.plist"
	if _, ok := fs.files[wantPath]; !ok {
		t.Errorf("plist not written to %q; written paths: %v", wantPath, mapKeys(fs.files))
	}
	if !fs.dirs["/Users/alice/Library/LaunchAgents"] {
		t.Errorf("expected LaunchAgents directory to be created, got %v", mapKeys(fs.dirs))
	}
	if got := mgr.GetManifestPath(); got != wantPath {
		t.Errorf("GetManifestPath() = %q, want %q", got, wantPath)
	}
	if got := mgr.ResolvedDomain(); got != "gui/501" {
		t.Errorf("ResolvedDomain() = %q, want gui/501", got)
	}
}

func TestLaunchdResolveDomain_BothDomainsFail(t *testing.T) {
	exec := newMockExecutor()
	exec.fallbackErr = errors.New("exit status 113")
	exec.fallbackOutput = []byte("Could not find domain for")
	mgr := newUserLaunchdManager(exec, newMockFS())
	mgr.label = "com.test-svc"

	_, err := mgr.serviceTarget()
	if err == nil {
		t.Fatal("expected error when neither domain is reachable")
	}
	for _, want := range []string{"gui/501", "user/501", "Could not find domain for"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention %q", err, want)
		}
	}
	if got := mgr.ResolvedDomain(); got != "" {
		t.Errorf("ResolvedDomain() = %q, want empty after a failed resolution", got)
	}
}

// A domain pinned from the state file must be used verbatim: re-resolving on a
// host whose session state has changed would target a domain the agent was
// never bootstrapped into.
func TestLaunchdPinnedDomainIsNotReResolved(t *testing.T) {
	exec := newMockExecutor()
	// Make any resolution attempt fail, so a pinned domain is the only way
	// through.
	exec.fallbackErr = errors.New("exit status 113")

	mgr := newUserLaunchdManager(exec, newMockFS())
	if err := InitServiceManagerWithOptions(mgr, ManagerOptions{
		ServiceName:   "test-svc",
		Scope:         ScopeUser,
		LaunchdDomain: "user/501",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	target, err := mgr.serviceTarget()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target != "user/501/com.test-svc" {
		t.Errorf("serviceTarget() = %q, want user/501/com.test-svc", target)
	}
	for _, c := range exec.calls {
		if len(c.Args) > 0 && c.Args[0] == "print" {
			t.Errorf("domain was re-resolved despite being pinned: %v", exec.calls)
		}
	}
	if got := mgr.ResolvedDomain(); got != "user/501" {
		t.Errorf("ResolvedDomain() = %q, want user/501", got)
	}
}

func TestLaunchdInitServiceManagerWithOptions_BindsScope(t *testing.T) {
	mgr := newLaunchdManager()
	mgr.homeFunc = func() (string, error) { return "/Users/alice", nil }

	if err := InitServiceManagerWithOptions(mgr, ManagerOptions{ServiceName: "test-svc", Scope: ScopeUser}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const want = "/Users/alice/Library/LaunchAgents/com.test-svc.plist"
	if got := mgr.GetManifestPath(); got != want {
		t.Errorf("GetManifestPath() = %q, want %q", got, want)
	}
}

func assertCalls(t *testing.T, got, want []mockCall) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d calls %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i].Name != want[i].Name || strings.Join(got[i].Args, "\x00") != strings.Join(want[i].Args, "\x00") {
			t.Errorf("call %d = %s %v, want %s %v", i, got[i].Name, got[i].Args, want[i].Name, want[i].Args)
		}
	}
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// uninstall is the only way to clean up, so a session that can no longer reach
// the agent's domain must still get the plist off disk rather than be told the
// domain is unreachable and left with an unremovable LaunchAgent.
func TestLaunchdUninstall_UnresolvableDomainStillRemovesPlist(t *testing.T) {
	exec := newMockExecutor()
	exec.fallbackErr = errors.New("exit status 113")
	exec.fallbackOutput = []byte("Could not find domain")
	fs := newMockFS()
	mgr := &launchdManager{
		label:       "com.test-svc",
		scope:       ScopeUser,
		executor:    exec,
		fs:          fs,
		homeFunc:    func() (string, error) { return "/Users/alice", nil },
		geteuidFunc: func() int { return 501 },
	}

	if err := mgr.Uninstall(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const path = "/Users/alice/Library/LaunchAgents/com.test-svc.plist"
	if !fs.removed[path] {
		t.Errorf("plist %q was not removed, removed = %v", path, fs.removed)
	}
	// No domain resolved, so there is no single target to unload: both per-user
	// domains are tried, since the job may be loaded in either one.
	if !exec.called("launchctl", "bootout", "gui/501/com.test-svc") {
		t.Errorf("gui/501 was not booted out, calls = %v", exec.calls)
	}
	if !exec.called("launchctl", "bootout", "user/501/com.test-svc") {
		t.Errorf("user/501 was not booted out, calls = %v", exec.calls)
	}
}

// The domain uninstall resolves is not necessarily the one the agent was
// bootstrapped into: a lost state file drops the pin and resolveDomain probes
// fresh. Removing the plist while the job is still loaded in the other per-user
// domain would leave nothing on disk able to unload it.
func TestLaunchdUninstall_BootsOutBothUserDomains(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := &launchdManager{
		label:       "com.test-svc",
		scope:       ScopeUser,
		executor:    exec,
		fs:          fs,
		homeFunc:    func() (string, error) { return "/Users/alice", nil },
		geteuidFunc: func() int { return 501 },
	}

	if err := mgr.Uninstall(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// gui/501 resolves first and is the direct target; user/501 is the stale one.
	if !exec.called("launchctl", "bootout", "gui/501/com.test-svc") {
		t.Errorf("resolved domain was not booted out, calls = %v", exec.calls)
	}
	if !exec.called("launchctl", "bootout", "user/501/com.test-svc") {
		t.Errorf("stale domain user/501 was not booted out, calls = %v", exec.calls)
	}
}

// A system-scope uninstall has only one domain and must not probe for per-user
// ones.
func TestLaunchdUninstall_SystemScopeBootsOutOnlySystem(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := &launchdManager{
		label:    "com.test-svc",
		scope:    ScopeSystem,
		executor: exec,
		fs:       fs,
	}

	if err := mgr.Uninstall(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exec.called("launchctl", "bootout", "system/com.test-svc") {
		t.Errorf("system domain was not booted out, calls = %v", exec.calls)
	}
	for _, c := range exec.calls {
		if len(c.Args) >= 2 && c.Args[0] == "bootout" && c.Args[1] != "system/com.test-svc" {
			t.Errorf("unexpected extra bootout %v", c.Args)
		}
	}
}

func TestLaunchdReadInstalledConfig(t *testing.T) {
	t.Run("user scope reads from LaunchAgents", func(t *testing.T) {
		home := t.TempDir()
		agents := filepath.Join(home, "Library", "LaunchAgents")
		const content = "<plist>agent</plist>"
		fs := newMockFS()
		fs.files[filepath.Join(agents, "com.test-svc.plist")] = []byte(content)
		mgr := &launchdManager{
			label:    "com.test-svc",
			scope:    ScopeUser,
			executor: newMockExecutor(),
			fs:       fs,
			homeFunc: func() (string, error) { return home, nil },
		}

		got, err := mgr.ReadInstalledConfig()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != content {
			t.Errorf("content = %q, want %q", got, content)
		}
	})

	t.Run("user scope without a home reports ErrNoHomeDir", func(t *testing.T) {
		mgr := &launchdManager{
			label:    "com.test-svc",
			scope:    ScopeUser,
			executor: newMockExecutor(),
			fs:       newMockFS(),
			homeFunc: func() (string, error) { return "", nil },
		}

		if _, err := mgr.ReadInstalledConfig(); !errors.Is(err, ErrNoHomeDir) {
			t.Errorf("error = %v, want ErrNoHomeDir", err)
		}
	})

	t.Run("missing plist is an error", func(t *testing.T) {
		mgr := &launchdManager{
			label:    "com.absent-svc",
			scope:    ScopeUser,
			executor: newMockExecutor(),
			fs:       newMockFS(),
			homeFunc: func() (string, error) { return t.TempDir(), nil },
		}

		if _, err := mgr.ReadInstalledConfig(); err == nil {
			t.Fatal("expected an error for a plist that does not exist")
		}
	})
}

func TestLaunchdWriteConfig(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		fs := newMockFS()
		mgr := &launchdManager{label: "com.test-agent", executor: newMockExecutor(), fs: fs}

		err := mgr.WriteConfig("<plist>test</plist>")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		found := false
		for path := range fs.files {
			if strings.HasSuffix(path, "com.test-agent.plist") {
				found = true
				break
			}
		}
		if !found {
			t.Error("plist file was not written")
		}
	})

	// The config-patcher install path and update-config never call Install, so
	// without this the plist would name a StandardOutPath whose parent does not
	// exist and launchd would refuse to spawn the job.
	t.Run("creates the log directory in system scope", func(t *testing.T) {
		fs := newMockFS()
		mgr := &launchdManager{label: "com.test-agent", executor: newMockExecutor(), fs: fs}

		if err := mgr.WriteConfig("<plist>test</plist>"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !fs.dirs["/var/log/test-agent"] {
			t.Errorf("log directory was not created, dirs = %v", fs.dirs)
		}
	})

	t.Run("creates the log directory in user scope", func(t *testing.T) {
		fs := newMockFS()
		mgr := &launchdManager{
			label:       "com.test-agent",
			scope:       ScopeUser,
			domain:      "gui/501",
			executor:    newMockExecutor(),
			fs:          fs,
			homeFunc:    func() (string, error) { return "/Users/alice", nil },
			geteuidFunc: func() int { return 501 },
		}

		if err := mgr.WriteConfig("<plist>test</plist>"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "/Users/alice/Library/Logs/test-agent"
		if !fs.dirs[want] {
			t.Errorf("log directory %q was not created, dirs = %v", want, fs.dirs)
		}
		if _, ok := fs.files["/Users/alice/Library/LaunchAgents/com.test-agent.plist"]; !ok {
			t.Errorf("plist was not written under LaunchAgents, files = %v", fs.files)
		}
	})

	// An explicit LogDir, as ManagerOptions carries it into a manager rebuilt
	// from state, must win over the scope default.
	t.Run("honours an explicit log directory", func(t *testing.T) {
		fs := newMockFS()
		mgr := &launchdManager{
			label: "com.test-agent", logDir: "/srv/logs/test-agent",
			executor: newMockExecutor(), fs: fs,
		}

		if err := mgr.WriteConfig("<plist>test</plist>"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !fs.dirs["/srv/logs/test-agent"] {
			t.Errorf("explicit log directory was not created, dirs = %v", fs.dirs)
		}
	})

	t.Run("no label", func(t *testing.T) {
		mgr := &launchdManager{executor: newMockExecutor(), fs: newMockFS()}
		err := mgr.WriteConfig("content")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "service label not set") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("write error", func(t *testing.T) {
		fs := newMockFS()
		fs.writeErr = errors.New("permission denied")
		mgr := &launchdManager{label: "com.test-agent", executor: newMockExecutor(), fs: fs}

		err := mgr.WriteConfig("content")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "writing plist file") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// WriteConfig is the install path whenever ConfigPatchers is set, so its
// first-install case has no state file either: a plist left behind by a failed
// bootstrap comes back at the next login with nothing recording it, and
// uninstall reports the service as not installed.
func TestLaunchdWriteConfig_RemovesTheNewPlistAfterAFailedBootstrap(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := &launchdManager{label: "com.test-agent", executor: exec, fs: fs}
	path := mgr.plistPath()
	exec.setResult("launchctl bootstrap system "+path, nil, errors.New("Input/output error"))

	if err := mgr.WriteConfig("<plist>new</plist>"); err == nil {
		t.Fatal("expected a bootstrap error")
	}

	if !fs.removed[path] {
		t.Errorf("plist %q was not removed after the failed bootstrap", path)
	}
}

// A rewrite of an existing install is the other way round: the state file still
// describes the service, so what was overwritten is restored and reloaded rather
// than deleted.
func TestLaunchdWriteConfig_RestoresThePreviousPlistAfterAFailedBootstrap(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := &launchdManager{label: "com.test-agent", executor: exec, fs: fs}
	path := mgr.plistPath()
	fs.files[path] = []byte("<plist>previous</plist>")
	exec.setResult("launchctl bootstrap system "+path, nil, errors.New("Input/output error"))

	if err := mgr.WriteConfig("<plist>new</plist>"); err == nil {
		t.Fatal("expected a bootstrap error")
	}

	if got := string(fs.files[path]); got != "<plist>previous</plist>" {
		t.Errorf("plist content = %q, want the previous plist restored", got)
	}
	if fs.removed[path] {
		t.Error("an existing install's plist was removed instead of restored")
	}
}

// The caller is itself rolling back, so what is on disk is the plist being
// rolled back away from. Restoring and reloading it would put the rejected
// configuration back under supervision while the caller reports the rollback as
// failed.
func TestLaunchdWriteConfigForRollback_KeepsTheRestoredPlistAfterAFailedBootstrap(t *testing.T) {
	exec := newMockExecutor()
	fs := newMockFS()
	mgr := &launchdManager{label: "com.test-agent", executor: exec, fs: fs}
	path := mgr.plistPath()
	fs.files[path] = []byte("<plist>rejected</plist>")
	exec.setResult("launchctl bootstrap system "+path, nil, errors.New("Input/output error"))

	if err := mgr.WriteConfigForRollback("<plist>previous</plist>"); err == nil {
		t.Fatal("expected a bootstrap error")
	}

	if got := string(fs.files[path]); got != "<plist>previous</plist>" {
		t.Errorf("plist content = %q, want the rolled-back plist kept", got)
	}
	bootstraps := 0
	for _, c := range exec.calls {
		if len(c.Args) >= 1 && c.Args[0] == "bootstrap" {
			bootstraps++
		}
	}
	if bootstraps != 1 {
		t.Errorf("bootstrap calls = %d, want 1 (the rejected plist must not be reloaded): %v", bootstraps, exec.calls)
	}
}

// Same reason as Install: booting out only the newly resolved target leaves the
// job the previous install recorded loaded against the same binary, with the
// state file now naming a domain it was never bootstrapped into.
func TestLaunchdWriteConfig_BootsOutThePreviousDomain(t *testing.T) {
	exec := newMockExecutor()
	mgr := &launchdManager{
		label:          "com.test-agent",
		scope:          ScopeUser,
		domain:         "user/501",
		previousDomain: "gui/501",
		executor:       exec,
		fs:             newMockFS(),
		homeFunc:       func() (string, error) { return "/Users/alice", nil },
		geteuidFunc:    func() int { return 501 },
	}

	if err := mgr.WriteConfig("<plist>new</plist>"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !exec.called("launchctl", "bootout", "gui/501/com.test-agent") {
		t.Error("the previously recorded domain was not booted out")
	}
	if !exec.called("launchctl", "bootout", "user/501/com.test-agent") {
		t.Error("the resolved domain was not booted out")
	}
}

// The restored plist belongs to the domain the untouched state file still
// records, not the one this session resolved. Reloading it into the newly
// resolved domain would leave the job in user/<uid> while state says gui/<uid>,
// so every later start/stop/status would address an empty domain.
func TestLaunchdRollback_ReloadsIntoThePreviouslyRecordedDomain(t *testing.T) {
	newMgr := func(exec *mockExecutor, fs *mockFS) *launchdManager {
		return &launchdManager{
			label:          "com.test-agent",
			scope:          ScopeUser,
			domain:         "user/501",
			previousDomain: "gui/501",
			executor:       exec,
			fs:             fs,
			homeFunc:       func() (string, error) { return "/Users/alice", nil },
			geteuidFunc:    func() int { return 501 },
		}
	}

	tests := []struct {
		name  string
		write func(m *launchdManager, path string) error
	}{
		{
			name: "Install",
			write: func(m *launchdManager, _ string) error {
				return m.Install(ServiceConfig{Name: "test-agent", Scope: ScopeUser})
			},
		},
		{
			name:  "WriteConfig",
			write: func(m *launchdManager, _ string) error { return m.WriteConfig("<plist>new</plist>") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			fs := newMockFS()
			mgr := newMgr(exec, fs)
			path := mgr.plistPath()
			fs.files[path] = []byte("<plist>previous</plist>")
			exec.setResult("launchctl bootstrap user/501 "+path, nil, errors.New("Input/output error"))

			if err := tt.write(mgr, path); err == nil {
				t.Fatal("expected a bootstrap error")
			}

			if !exec.called("launchctl", "bootstrap", "gui/501", path) {
				t.Errorf("the restored plist was not reloaded into gui/501, calls = %v", exec.calls)
			}
		})
	}
}
