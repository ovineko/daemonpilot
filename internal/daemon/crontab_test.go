//go:build !windows

package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "'simple'"},
		{"/opt/my-app", "'/opt/my-app'"},
		{"/path with spaces", "'/path with spaces'"},
		{"it's", "'it'\\''s'"},
		{"", "''"},
	}
	for _, tt := range tests {
		got := shellQuote(tt.input)
		if got != tt.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatCrontabEntry(t *testing.T) {
	config := ServiceConfig{
		Name:       "test-agent",
		ExecPath:   "/opt/test-agent/test-agent",
		WorkingDir: "/opt/test-agent",
		Arguments:  []string{"run"},
	}

	entry := FormatCrontabEntry(config)

	if !strings.Contains(entry, "# daemonpilot: test-agent") {
		t.Errorf("entry missing marker comment, got:\n%s", entry)
	}
	if !strings.Contains(entry, "@reboot cd '/opt/test-agent' && '/opt/test-agent/test-agent' 'run' >> /var/log/test-agent.log 2>&1") {
		t.Errorf("entry missing @reboot line, got:\n%s", entry)
	}
}

func TestFormatCrontabEntry_PathsWithSpaces(t *testing.T) {
	config := ServiceConfig{
		Name:       "test-agent",
		ExecPath:   "/opt/my app/test-agent",
		WorkingDir: "/opt/my app",
		Arguments:  []string{"run"},
	}

	entry := FormatCrontabEntry(config)

	if !strings.Contains(entry, "cd '/opt/my app'") {
		t.Errorf("working dir not quoted correctly, got:\n%s", entry)
	}
	if !strings.Contains(entry, "'/opt/my app/test-agent' 'run'") {
		t.Errorf("exec path not quoted correctly, got:\n%s", entry)
	}
}

func TestParseCrontabEntries_Found(t *testing.T) {
	content := "# other job\n0 * * * * /usr/bin/something\n# daemonpilot: my-svc\n@reboot cd '/opt/my-svc' && '/opt/my-svc/my-svc' run >> /var/log/my-svc.log 2>&1\n# another job\n"
	kept, found := ParseCrontabEntries(content, "my-svc")
	if !found {
		t.Fatal("expected entry to be found")
	}
	if strings.Contains(kept, "daemonpilot: my-svc") {
		t.Error("kept content should not contain removed entry marker")
	}
	if strings.Contains(kept, "@reboot") {
		t.Error("kept content should not contain @reboot line")
	}
	if !strings.Contains(kept, "# other job") {
		t.Error("kept content should preserve other entries")
	}
	if !strings.Contains(kept, "# another job") {
		t.Error("kept content should preserve other entries")
	}
}

func TestParseCrontabEntries_NotFound(t *testing.T) {
	content := "0 * * * * /usr/bin/something\n"
	kept, found := ParseCrontabEntries(content, "nonexistent")
	if found {
		t.Fatal("expected entry to not be found")
	}
	if kept != content {
		t.Errorf("content should be unchanged, got:\n%s", kept)
	}
}

func TestParseCrontabEntries_Empty(t *testing.T) {
	kept, found := ParseCrontabEntries("", "my-svc")
	if found {
		t.Fatal("expected entry to not be found")
	}
	if kept != "" {
		t.Errorf("expected empty string, got: %q", kept)
	}
}

func TestCrontabInstall(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(""), nil)
	mgr := &crontabManager{executor: exec}

	config := ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
	}

	err := mgr.Install(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mgr.serviceName != "test-svc" {
		t.Errorf("serviceName = %q, want %q", mgr.serviceName, "test-svc")
	}

	if !exec.called("crontab", "-l") {
		t.Error("crontab -l was not called")
	}

	// Verify crontab was called with a temp file (second call)
	var crontabInstallCalled bool
	for _, c := range exec.calls {
		if c.Name == "crontab" && len(c.Args) == 1 && c.Args[0] != "-l" {
			crontabInstallCalled = true
		}
	}
	if !crontabInstallCalled {
		t.Error("crontab was not called with temp file")
	}
}

func TestCrontabInstall_ExistingEntries(t *testing.T) {
	existing := "0 * * * * /usr/bin/backup\n"
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(existing), nil)
	exec.captureFileArg = true
	mgr := &crontabManager{executor: exec}

	config := ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
		Arguments:  []string{"run"},
	}

	err := mgr.Install(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	written := exec.capturedFileContent
	if !strings.Contains(written, "0 * * * * /usr/bin/backup") {
		t.Error("written crontab should preserve existing entries")
	}
	if !strings.Contains(written, "# daemonpilot: test-svc") {
		t.Error("written crontab should contain daemonpilot marker")
	}
	if !strings.Contains(written, "@reboot cd '/opt/test-svc' && '/opt/test-svc/test-svc' 'run'") {
		t.Error("written crontab should contain @reboot entry")
	}
}

func TestCrontabInstall_ReplacesExisting(t *testing.T) {
	existing := "# daemonpilot: test-svc\n@reboot cd '/old' && '/old/test-svc' run >> /var/log/test-svc.log 2>&1\n"
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(existing), nil)
	exec.captureFileArg = true
	mgr := &crontabManager{executor: exec}

	config := ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/new/test-svc",
		WorkingDir: "/new",
		Arguments:  []string{"run"},
	}

	err := mgr.Install(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	written := exec.capturedFileContent
	if strings.Contains(written, "/old") {
		t.Error("written crontab should not contain old entry paths")
	}
	if !strings.Contains(written, "# daemonpilot: test-svc") {
		t.Error("written crontab should contain daemonpilot marker for new entry")
	}
	if !strings.Contains(written, "@reboot cd '/new' && '/new/test-svc' 'run'") {
		t.Errorf("written crontab should contain new @reboot entry, got:\n%s", written)
	}
}

func TestCrontabInstall_NoCrontab(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte("no crontab for user"), errors.New("exit 1"))
	mgr := &crontabManager{executor: exec}

	config := ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
	}

	err := mgr.Install(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCrontabInstall_ReadError(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte("some other error"), errors.New("exit 1"))
	mgr := &crontabManager{executor: exec}

	err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "reading crontab") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCrontabInstall_CrontabCommandError(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(""), nil)
	exec.fallbackErr = errors.New("crontab failed")
	mgr := &crontabManager{executor: exec}

	config := ServiceConfig{
		Name:       "fail-svc",
		ExecPath:   "/opt/fail-svc/fail-svc",
		WorkingDir: "/opt/fail-svc",
	}

	err := mgr.Install(config)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "installing crontab") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCrontabUninstall(t *testing.T) {
	existing := "# daemonpilot: test-svc\n@reboot cd '/opt/test-svc' && '/opt/test-svc/test-svc' run >> /var/log/test-svc.log 2>&1\n# other\n"
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(existing), nil)
	mgr := &crontabManager{serviceName: "test-svc", executor: exec}

	err := mgr.Uninstall()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCrontabUninstall_NoServiceName(t *testing.T) {
	mgr := &crontabManager{executor: newMockExecutor()}
	err := mgr.Uninstall()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCrontabUninstall_EntryNotFound(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte("0 * * * * /usr/bin/backup\n"), nil)
	mgr := &crontabManager{serviceName: "test-svc", executor: exec}

	err := mgr.Uninstall()
	if !errors.Is(err, ErrCrontabEntryNotFound) {
		t.Fatalf("Uninstall() error = %v, want ErrCrontabEntryNotFound", err)
	}
	if !strings.Contains(err.Error(), "test-svc") {
		t.Errorf("error should name the service, got: %v", err)
	}
	for _, c := range exec.calls {
		if c.Name == "crontab" && len(c.Args) == 1 && c.Args[0] != "-l" {
			t.Error("crontab must not be rewritten when there is no entry to remove")
		}
	}
}

func TestCrontabEnable(t *testing.T) {
	disabled := "# daemonpilot: test-svc\n#@reboot cd '/opt/test-svc' && '/opt/test-svc/test-svc' run >> /var/log/test-svc.log 2>&1\n"
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(disabled), nil)
	exec.captureFileArg = true
	mgr := &crontabManager{serviceName: "test-svc", executor: exec}

	err := mgr.Enable()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	written := exec.capturedFileContent
	if strings.Contains(written, "#@reboot") {
		t.Error("@reboot line should be uncommented after Enable")
	}
	if !strings.Contains(written, "@reboot cd") {
		t.Error("@reboot line should be present after Enable")
	}
}

func TestCrontabEnable_AlreadyEnabled(t *testing.T) {
	enabled := "# daemonpilot: test-svc\n@reboot cd '/opt/test-svc' && '/opt/test-svc/test-svc' run >> /var/log/test-svc.log 2>&1\n"
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(enabled), nil)
	mgr := &crontabManager{serviceName: "test-svc", executor: exec}

	err := mgr.Enable()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var crontabWriteCalled bool
	for _, c := range exec.calls {
		if c.Name == "crontab" && len(c.Args) == 1 && c.Args[0] != "-l" {
			crontabWriteCalled = true
		}
	}
	if crontabWriteCalled {
		t.Error("crontab should not be written when already enabled")
	}
}

func TestCrontabEnable_NoServiceName(t *testing.T) {
	mgr := &crontabManager{executor: newMockExecutor()}
	err := mgr.Enable()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCrontabEnable_ReadError(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte("some error"), errors.New("exit 1"))
	mgr := &crontabManager{serviceName: "test-svc", executor: exec}

	err := mgr.Enable()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCrontabDisable(t *testing.T) {
	enabled := "# daemonpilot: test-svc\n@reboot cd '/opt/test-svc' && '/opt/test-svc/test-svc' run >> /var/log/test-svc.log 2>&1\n"
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(enabled), nil)
	exec.captureFileArg = true
	mgr := &crontabManager{serviceName: "test-svc", executor: exec}

	err := mgr.Disable()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	written := exec.capturedFileContent
	if !strings.Contains(written, "#@reboot cd") {
		t.Errorf("@reboot line should be commented out after Disable, got:\n%s", written)
	}
}

func TestCrontabDisable_AlreadyDisabled(t *testing.T) {
	disabled := "# daemonpilot: test-svc\n#@reboot cd '/opt/test-svc' && '/opt/test-svc/test-svc' run >> /var/log/test-svc.log 2>&1\n"
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(disabled), nil)
	mgr := &crontabManager{serviceName: "test-svc", executor: exec}

	err := mgr.Disable()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var crontabWriteCalled bool
	for _, c := range exec.calls {
		if c.Name == "crontab" && len(c.Args) == 1 && c.Args[0] != "-l" {
			crontabWriteCalled = true
		}
	}
	if crontabWriteCalled {
		t.Error("crontab should not be written when already disabled")
	}
}

func TestCrontabDisable_NoServiceName(t *testing.T) {
	mgr := &crontabManager{executor: newMockExecutor()}
	err := mgr.Disable()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCrontabDisable_ReadError(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte("some error"), errors.New("exit 1"))
	mgr := &crontabManager{serviceName: "test-svc", executor: exec}

	err := mgr.Disable()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCrontabStart(t *testing.T) {
	tests := []struct {
		name    string
		crontab string
		svcName string
		execErr error
		wantErr bool
	}{
		{
			name:    "starts service from crontab entry",
			svcName: "test-svc",
			crontab: "# daemonpilot: test-svc\n@reboot cd '/opt/test-svc' && '/opt/test-svc/test-svc' run >> /var/log/test-svc.log 2>&1\n",
			wantErr: false,
		},
		{
			name:    "starts service with environment variables",
			svcName: "env-svc",
			crontab: "# daemonpilot: env-svc\n@reboot APP_ENV='production' cd '/opt/env-svc' && '/opt/env-svc/env-svc' run >> /var/log/env-svc.log 2>&1\n",
			wantErr: false,
		},
		{
			name:    "no service name",
			svcName: "",
			wantErr: true,
		},
		{
			name:    "service not found in crontab",
			svcName: "missing-svc",
			crontab: "# some other entry\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			exec.setResult("crontab -l", []byte(tt.crontab), nil)
			exec.setResult("sh -c cd '/opt/test-svc' && '/opt/test-svc/test-svc' run &", nil, tt.execErr)
			exec.setResult("pgrep -f /opt/env-svc/env-svc run", nil, errors.New("not running"))
			mgr := &crontabManager{serviceName: tt.svcName, executor: exec}
			err := mgr.Start()
			if (err != nil) != tt.wantErr {
				t.Errorf("Start() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCrontabStart_IncludesEnvVars(t *testing.T) {
	crontab := "# daemonpilot: env-svc\n@reboot APP_ENV='production' DB='test' cd '/opt/env-svc' && '/opt/env-svc/env-svc' run >> /var/log/env-svc.log 2>&1\n"
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(crontab), nil)
	exec.setResult("pgrep -f /opt/env-svc/env-svc run", nil, errors.New("not running"))
	mgr := &crontabManager{serviceName: "env-svc", executor: exec}

	err := mgr.Start()
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	found := false
	for _, c := range exec.calls {
		if c.Name == "sh" && len(c.Args) >= 2 && c.Args[0] == "-c" {
			cmd := c.Args[1]
			found = true
			if !strings.Contains(cmd, "APP_ENV='production'") {
				t.Errorf("expected APP_ENV in command, got: %s", cmd)
			}
			if !strings.Contains(cmd, "DB='test'") {
				t.Errorf("expected DB in command, got: %s", cmd)
			}
			if !strings.Contains(cmd, "cd '/opt/env-svc'") {
				t.Errorf("expected cd in command, got: %s", cmd)
			}
		}
	}
	if !found {
		t.Error("Start() did not call sh -c")
	}
}

func TestExtractCrontabCommand(t *testing.T) {
	tests := []struct {
		name    string
		entry   string
		want    string
		wantErr bool
	}{
		{
			name:  "special schedule @reboot",
			entry: "@reboot cd '/opt/svc' && '/opt/svc/svc' run >> /var/log/svc.log 2>&1",
			want:  "cd '/opt/svc' && '/opt/svc/svc' run >> /var/log/svc.log 2>&1",
		},
		{
			name:  "special schedule with env vars",
			entry: "@reboot APP_ENV='prod' cd '/opt/svc' && '/opt/svc/svc' run >> /var/log/svc.log 2>&1",
			want:  "APP_ENV='prod' cd '/opt/svc' && '/opt/svc/svc' run >> /var/log/svc.log 2>&1",
		},
		{
			name:  "5-field cron schedule",
			entry: "*/5 * * * * cd '/opt/svc' && '/opt/svc/svc' run >> /var/log/svc.log 2>&1",
			want:  "cd '/opt/svc' && '/opt/svc/svc' run >> /var/log/svc.log 2>&1",
		},
		{
			name:    "empty entry",
			entry:   "",
			wantErr: true,
		},
		{
			name:    "schedule only",
			entry:   "@reboot",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractCrontabCommand(tt.entry)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractCrontabCommand() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("extractCrontabCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCrontabStop(t *testing.T) {
	crontab := "# daemonpilot: test-svc\n@reboot cd '/opt/test-svc' && '/opt/test-svc/test-svc' run >> /var/log/test-svc.log 2>&1\n"
	pattern := "/opt/test-svc/test-svc run"

	tests := []struct {
		name     string
		svcName  string
		crontab  string
		pgrepOut string
		pgrepErr error
		pkillErr error
		wantErr  bool
	}{
		{
			name:     "stops running process",
			svcName:  "test-svc",
			crontab:  crontab,
			pgrepOut: "12345\n",
			wantErr:  false,
		},
		{
			name:     "process not running",
			svcName:  "test-svc",
			crontab:  crontab,
			pgrepErr: errors.New("exit 1"),
			wantErr:  false,
		},
		{
			name:    "no service name",
			svcName: "",
			wantErr: true,
		},
		{
			name:     "pkill fails",
			svcName:  "test-svc",
			crontab:  crontab,
			pgrepOut: "12345\n",
			pkillErr: errors.New("pkill failed"),
			wantErr:  true,
		},
		{
			name:    "service not in crontab",
			svcName: "test-svc",
			crontab: "# some other entry\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			if tt.crontab != "" {
				exec.setResult("crontab -l", []byte(tt.crontab), nil)
			}
			exec.setResult("pgrep -f "+pattern, []byte(tt.pgrepOut), tt.pgrepErr)
			exec.setResult("pkill -f "+pattern, nil, tt.pkillErr)
			mgr := &crontabManager{serviceName: tt.svcName, executor: exec}
			err := mgr.Stop()
			if (err != nil) != tt.wantErr {
				t.Errorf("Stop() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCrontabStatus(t *testing.T) {
	crontab := "# daemonpilot: test-svc\n@reboot cd '/opt/test-svc' && '/opt/test-svc/test-svc' run >> /var/log/test-svc.log 2>&1\n"
	pattern := "/opt/test-svc/test-svc run"

	tests := []struct {
		name       string
		output     string
		execErr    error
		wantStatus ServiceStatus
	}{
		{"running", "12345\n", nil, ServiceStatusRunning},
		{"not running", "", errors.New("exit 1"), ServiceStatusStopped},
		{"empty output no error", "", nil, ServiceStatusStopped},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			exec.setResult("crontab -l", []byte(crontab), nil)
			exec.setResult("pgrep -f "+pattern, []byte(tt.output), tt.execErr)
			mgr := &crontabManager{serviceName: "test-svc", executor: exec}

			status, err := mgr.Status()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != tt.wantStatus {
				t.Errorf("status = %v, want %v", status, tt.wantStatus)
			}
		})
	}
}

func TestCrontabStatus_NoServiceName(t *testing.T) {
	mgr := &crontabManager{executor: newMockExecutor()}
	_, err := mgr.Status()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCrontabStatus_ServiceNotInCrontab(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte("# some other entry\n"), nil)
	mgr := &crontabManager{serviceName: "test-svc", executor: exec}

	status, err := mgr.Status()
	if err == nil {
		t.Fatal("expected error when service not in crontab")
	}
	if status != ServiceStatusUnknown {
		t.Errorf("status = %v, want %v", status, ServiceStatusUnknown)
	}
}

func TestProcessPattern(t *testing.T) {
	tests := []struct {
		name        string
		svcName     string
		crontab     string
		crontabErr  error
		wantPattern string
		wantErr     bool
	}{
		{
			name:        "extracts full path from crontab",
			svcName:     "test-svc",
			crontab:     "# daemonpilot: test-svc\n@reboot cd '/opt/test-svc' && '/opt/test-svc/test-svc' run >> /var/log/test-svc.log 2>&1\n",
			wantPattern: "/opt/test-svc/test-svc run",
		},
		{
			name:        "extracts path from disabled (commented) entry",
			svcName:     "test-svc",
			crontab:     "# daemonpilot: test-svc\n#@reboot cd '/opt/test-svc' && '/opt/test-svc/test-svc' run >> /var/log/test-svc.log 2>&1\n",
			wantPattern: "/opt/test-svc/test-svc run",
		},
		{
			name:        "handles path with spaces",
			svcName:     "test-svc",
			crontab:     "# daemonpilot: test-svc\n@reboot cd '/opt/my app' && '/opt/my app/test-svc' run >> /var/log/test-svc.log 2>&1\n",
			wantPattern: "/opt/my app/test-svc run",
		},
		{
			name:        "handles path containing run substring",
			svcName:     "runner-svc",
			crontab:     "# daemonpilot: runner-svc\n@reboot cd '/opt/runner' && '/opt/runner/runner-svc' run >> /var/log/runner-svc.log 2>&1\n",
			wantPattern: "/opt/runner/runner-svc run",
		},
		{
			name:        "handles path with space-run segment uses LastIndex correctly",
			svcName:     "test-svc",
			crontab:     "# daemonpilot: test-svc\n@reboot cd '/opt/to run' && '/opt/to run/test-svc' run >> /var/log/test-svc.log 2>&1\n",
			wantPattern: "/opt/to run/test-svc run",
		},
		{
			name:        "handles path with single quote via shell escaping",
			svcName:     "test-svc",
			crontab:     "# daemonpilot: test-svc\n@reboot cd '/opt/it'\\''s here' && '/opt/it'\\''s here/test-svc' run >> /var/log/test-svc.log 2>&1\n",
			wantPattern: "/opt/it's here/test-svc run",
		},
		{
			name:        "handles working dir containing ampersands",
			svcName:     "test-svc",
			crontab:     "# daemonpilot: test-svc\n@reboot cd '/opt/a && b' && '/opt/app/test-svc' run >> /var/log/test-svc.log 2>&1\n",
			wantPattern: "/opt/app/test-svc run",
		},
		{
			name:        "handles path with single quote and ampersand delimiter collision",
			svcName:     "test-svc",
			crontab:     "# daemonpilot: test-svc\n@reboot cd '/opt/it'\\''s && here' && '/opt/it'\\''s && here/test-svc' run >> /var/log/test-svc.log 2>&1\n",
			wantPattern: "/opt/it's && here/test-svc run",
		},
		{
			name:       "error when crontab read fails",
			svcName:    "test-svc",
			crontabErr: errors.New("permission denied"),
			wantErr:    true,
		},
		{
			name:    "error when service not in crontab",
			svcName: "test-svc",
			crontab: "# some other entry\n0 * * * * /usr/bin/backup\n",
			wantErr: true,
		},
		{
			name:    "error on empty crontab",
			svcName: "test-svc",
			crontab: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newMockExecutor()
			if tt.crontabErr != nil {
				exec.setResult("crontab -l", []byte("error"), tt.crontabErr)
			} else {
				exec.setResult("crontab -l", []byte(tt.crontab), nil)
			}
			mgr := &crontabManager{serviceName: tt.svcName, executor: exec}

			pattern, err := mgr.processPattern()
			if (err != nil) != tt.wantErr {
				t.Errorf("processPattern() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && pattern != tt.wantPattern {
				t.Errorf("processPattern() = %q, want %q", pattern, tt.wantPattern)
			}
		})
	}
}

func TestCrontabInstallUninstallRoundtrip(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(""), nil)
	mgr := &crontabManager{executor: exec}

	config := ServiceConfig{
		Name:       "roundtrip-svc",
		ExecPath:   "/opt/roundtrip-svc/roundtrip-svc",
		WorkingDir: "/opt/roundtrip-svc",
	}

	if err := mgr.Install(config); err != nil {
		t.Fatalf("install error: %v", err)
	}

	// For uninstall, simulate that the crontab now contains our entry
	entry := FormatCrontabEntry(config)
	exec.setResult("crontab -l", []byte(entry+"\n"), nil)

	if err := mgr.Uninstall(); err != nil {
		t.Fatalf("uninstall error: %v", err)
	}
}

func TestCrontabGetManifestPath(t *testing.T) {
	mgr := &crontabManager{serviceName: "myapp", installDir: "/opt/myapp"}
	got := mgr.GetManifestPath()
	want := "/opt/myapp/.daemonpilot-crontab-myapp.entry"
	if got != want {
		t.Errorf("GetManifestPath() = %q, want %q", got, want)
	}
}

func TestCrontabGetManifestPathNoInstallDir(t *testing.T) {
	mgr := &crontabManager{serviceName: "myapp"}
	got := mgr.GetManifestPath()
	want := ".daemonpilot-crontab-myapp.entry"
	if got != want {
		t.Errorf("GetManifestPath() = %q, want %q", got, want)
	}
}

func TestCrontabGenerateConfig(t *testing.T) {
	mgr := &crontabManager{executor: newMockExecutor()}
	config := ServiceConfig{
		Name:       "test-agent",
		ExecPath:   "/opt/test-agent/test-agent",
		WorkingDir: "/opt/test-agent",
	}

	content, err := mgr.GenerateConfig(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mgr.serviceName != "test-agent" {
		t.Errorf("serviceName = %q, want %q", mgr.serviceName, "test-agent")
	}

	if !strings.Contains(content, "# daemonpilot: test-agent") {
		t.Errorf("generated config missing marker, got: %s", content)
	}
	if !strings.Contains(content, "@reboot") {
		t.Errorf("generated config missing @reboot, got: %s", content)
	}
}

func TestFormatCrontabEntry_CustomSchedule(t *testing.T) {
	config := ServiceConfig{
		Name:            "test-agent",
		ExecPath:        "/opt/test-agent/test-agent",
		WorkingDir:      "/opt/test-agent",
		CrontabSchedule: "*/5 * * * *",
	}

	entry := FormatCrontabEntry(config)

	if strings.Contains(entry, "@reboot") {
		t.Errorf("entry should not contain @reboot when custom schedule set, got:\n%s", entry)
	}
	if !strings.Contains(entry, "*/5 * * * * cd") {
		t.Errorf("entry should use custom schedule, got:\n%s", entry)
	}
}

func TestFormatCrontabEntry_WithEnvironment(t *testing.T) {
	config := ServiceConfig{
		Name:       "test-agent",
		ExecPath:   "/opt/test-agent/test-agent",
		WorkingDir: "/opt/test-agent",
		Environment: map[string]string{
			"APP_ENV": "production",
			"DEBUG":   "false",
		},
	}

	entry := FormatCrontabEntry(config)

	if !strings.Contains(entry, "APP_ENV='production'") {
		t.Errorf("entry should contain APP_ENV, got:\n%s", entry)
	}
	if !strings.Contains(entry, "DEBUG='false'") {
		t.Errorf("entry should contain DEBUG, got:\n%s", entry)
	}
	// Environment should come before cd
	lines := strings.Split(entry, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	cmdLine := lines[1]
	envIdx := strings.Index(cmdLine, "APP_ENV=")
	cdIdx := strings.Index(cmdLine, "cd ")
	if envIdx == -1 || cdIdx == -1 || envIdx > cdIdx {
		t.Errorf("environment should come before cd command, got:\n%s", cmdLine)
	}
}

func TestFormatCrontabEntry_WithArguments(t *testing.T) {
	config := ServiceConfig{
		Name:       "test-agent",
		ExecPath:   "/opt/test-agent/test-agent",
		WorkingDir: "/opt/test-agent",
		Arguments:  []string{"run", "--port", "8080", "--verbose"},
	}

	entry := FormatCrontabEntry(config)

	if !strings.Contains(entry, "'run' '--port' '8080' '--verbose'") {
		t.Errorf("entry should contain arguments after run, got:\n%s", entry)
	}
}

func TestFormatCrontabEntry_AllOptionalFields(t *testing.T) {
	config := ServiceConfig{
		Name:            "test-agent",
		ExecPath:        "/opt/test-agent/test-agent",
		WorkingDir:      "/opt/test-agent",
		CrontabSchedule: "0 * * * *",
		Arguments:       []string{"run", "--config", "/etc/app.conf"},
		Environment: map[string]string{
			"LOG_LEVEL": "info",
		},
	}

	entry := FormatCrontabEntry(config)

	if !strings.Contains(entry, "# daemonpilot: test-agent") {
		t.Errorf("entry missing marker, got:\n%s", entry)
	}
	if !strings.Contains(entry, "0 * * * *") {
		t.Errorf("entry missing custom schedule, got:\n%s", entry)
	}
	if !strings.Contains(entry, "LOG_LEVEL='info'") {
		t.Errorf("entry missing environment, got:\n%s", entry)
	}
	if !strings.Contains(entry, "'run' '--config' '/etc/app.conf'") {
		t.Errorf("entry missing arguments, got:\n%s", entry)
	}
}

func TestCrontabGenerateConfig_WithOptionalFields(t *testing.T) {
	mgr := &crontabManager{executor: newMockExecutor()}
	config := ServiceConfig{
		Name:            "test-agent",
		ExecPath:        "/opt/test-agent/test-agent",
		WorkingDir:      "/opt/test-agent",
		CrontabSchedule: "*/10 * * * *",
		Arguments:       []string{"--daemon"},
		Environment:     map[string]string{"KEY": "val"},
	}

	content, err := mgr.GenerateConfig(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(content, "*/10 * * * *") {
		t.Errorf("generated config missing custom schedule, got: %s", content)
	}
	if !strings.Contains(content, "KEY='val'") {
		t.Errorf("generated config missing environment, got: %s", content)
	}
	if !strings.Contains(content, "'--daemon'") {
		t.Errorf("generated config missing arguments, got: %s", content)
	}
}

func TestCrontabWriteConfig(t *testing.T) {
	t.Run("success with empty crontab", func(t *testing.T) {
		exec := newMockExecutor()
		exec.captureFileArg = true
		exec.setResult("crontab -l", []byte("no crontab for user"), errors.New("exit 1"))
		mgr := &crontabManager{serviceName: "test-agent", executor: exec}

		entry := FormatCrontabEntry(ServiceConfig{
			Name:       "test-agent",
			ExecPath:   "/opt/test-agent/test-agent",
			WorkingDir: "/opt/test-agent",
		})

		err := mgr.WriteConfig(entry)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !strings.Contains(exec.capturedFileContent, "# daemonpilot: test-agent") {
			t.Errorf("crontab content missing entry, got: %s", exec.capturedFileContent)
		}
	})

	t.Run("no service name", func(t *testing.T) {
		mgr := &crontabManager{executor: newMockExecutor()}
		err := mgr.WriteConfig("content")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "service name not set") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestCrontabReadInstalledConfig(t *testing.T) {
	t.Run("reads entry from live crontab", func(t *testing.T) {
		exec := newMockExecutor()
		crontabContent := "# other stuff\n# daemonpilot: test-agent\n@reboot cd '/opt/test-agent' && '/opt/test-agent/test-agent' run >> /var/log/test-agent.log 2>&1\n"
		exec.setResult("crontab -l", []byte(crontabContent), nil)
		mgr := &crontabManager{serviceName: "test-agent", executor: exec}

		got, err := mgr.ReadInstalledConfig()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(got, "# daemonpilot: test-agent") {
			t.Errorf("expected marker in result, got: %s", got)
		}
		if !strings.Contains(got, "@reboot") {
			t.Errorf("expected schedule line in result, got: %s", got)
		}
	})

	t.Run("service not found in crontab", func(t *testing.T) {
		exec := newMockExecutor()
		exec.setResult("crontab -l", []byte("# some other entry\n"), nil)
		mgr := &crontabManager{serviceName: "test-agent", executor: exec}

		_, err := mgr.ReadInstalledConfig()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "not found in crontab") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("no service name", func(t *testing.T) {
		mgr := &crontabManager{executor: newMockExecutor()}
		_, err := mgr.ReadInstalledConfig()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "service name not set") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("crontab read error", func(t *testing.T) {
		exec := newMockExecutor()
		exec.setResult("crontab -l", []byte("error"), errors.New("crontab failed"))
		mgr := &crontabManager{serviceName: "test-agent", executor: exec}

		_, err := mgr.ReadInstalledConfig()
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestCrontabDetect_LogsWhenDetected(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := &crontabManager{
		executor: newMockExecutor(),
		lookPathFunc: func(name string) (string, error) {
			if name == "crontab" {
				return "/usr/bin/crontab", nil
			}
			return "", errors.New("not found")
		},
	}

	result := m.detect()
	if !result {
		t.Fatal("expected detect() to return true")
	}

	output := buf.String()
	expectedMessages := []string{
		"init detection: crontab: checking crontab detection",
		"init detection: crontab: lookpath crontab -> /usr/bin/crontab",
		"init detection: crontab: detected=true reason=crontab found in PATH (fallback detector: no other detector was selected)",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}
}

func TestCrontabDetect_LogsWhenNotDetected(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := &crontabManager{
		executor: newMockExecutor(),
		lookPathFunc: func(name string) (string, error) {
			return "", errors.New("not found")
		},
	}

	result := m.detect()
	if result {
		t.Fatal("expected detect() to return false")
	}

	output := buf.String()
	expectedMessages := []string{
		"init detection: crontab: checking crontab detection",
		"init detection: crontab: lookpath crontab -> not found",
		"init detection: crontab: detected=false reason=crontab not found",
	}
	for _, msg := range expectedMessages {
		if !strings.Contains(output, msg) {
			t.Errorf("expected log to contain %q, got:\n%s", msg, output)
		}
	}

	if strings.Contains(output, "detected=true") {
		t.Errorf("should not contain detected=true when crontab not detected")
	}
}

func TestCrontabDetect_LogsFallbackWarning(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetDetectionLogger(logger)

	m := &crontabManager{
		executor: newMockExecutor(),
		lookPathFunc: func(name string) (string, error) {
			if name == "crontab" {
				return "/usr/bin/crontab", nil
			}
			return "", errors.New("not found")
		},
	}

	result := m.detect()
	if !result {
		t.Fatal("expected detect() to return true")
	}

	output := buf.String()
	if !strings.Contains(output, "fallback detector") {
		t.Errorf("expected log to contain fallback warning, got:\n%s", output)
	}
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, "fallback detector") && !strings.Contains(line, "level=WARN") {
			t.Errorf("fallback notice logged below warning level: %s", line)
		}
	}
}

// The fallback notice only helps if it survives the log level an install
// actually runs at, which is whatever LOG_LEVEL happens to be — usually unset.
func TestCrontabDetect_FallbackWarningVisibleAtDefaultLogLevel(t *testing.T) {
	tests := []struct {
		name      string
		logLevel  string
		wantDebug bool
	}{
		{name: "LOG_LEVEL unset", logLevel: "", wantDebug: false},
		{name: "LOG_LEVEL info", logLevel: "info", wantDebug: false},
		{name: "LOG_LEVEL debug", logLevel: "debug", wantDebug: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldLog := getDetectionLog()
			defer SetDetectionLogger(oldLog)
			t.Setenv("LOG_LEVEL", tt.logLevel)

			var buf bytes.Buffer
			level := slog.LevelWarn
			if tt.wantDebug {
				level = slog.LevelDebug
			}
			SetDetectionLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})))

			m := &crontabManager{
				executor: newMockExecutor(),
				lookPathFunc: func(string) (string, error) {
					return "/usr/bin/crontab", nil
				},
			}
			if !m.detect() {
				t.Fatal("expected detect() to return true")
			}

			if !strings.Contains(buf.String(), "fallback detector") {
				t.Errorf("fallback notice missing at LOG_LEVEL=%q, got:\n%s", tt.logLevel, buf.String())
			}
			gotDebug := strings.Contains(buf.String(), "level=DEBUG")
			if gotDebug != tt.wantDebug {
				t.Errorf("debug diagnostics present = %v, want %v", gotDebug, tt.wantDebug)
			}
		})
	}
}

// NewDetectionLogger is the only thing that decides whether logDetectionWarn is
// reachable in production, so pin the level it picks per LOG_LEVEL.
func TestNewDetectionLogger_KeepsWarningsAtEveryLogLevel(t *testing.T) {
	for _, level := range []string{"", "info", "warn", "error", "trace", "debug"} {
		t.Run("LOG_LEVEL="+level, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", level)
			l, ok := NewDetectionLogger().(*slog.Logger)
			if !ok {
				t.Fatalf("NewDetectionLogger() = %T, want *slog.Logger", NewDetectionLogger())
			}
			if !l.Enabled(context.Background(), slog.LevelWarn) {
				t.Error("warnings are dropped, so logDetectionWarn never reaches the operator")
			}
			wantDebug := level == "trace" || level == "debug"
			if got := l.Enabled(context.Background(), slog.LevelDebug); got != wantDebug {
				t.Errorf("debug enabled = %v, want %v", got, wantDebug)
			}
		})
	}
}

func TestCrontabSupportsCapabilities(t *testing.T) {
	mgr := &crontabManager{}
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

func TestCrontabLogRedirect(t *testing.T) {
	tests := []struct {
		name   string
		scope  Scope
		logDir string
		want   string
	}{
		{
			name:  "system scope keeps the historical path unquoted",
			scope: ScopeSystem,
			want:  ">> /var/log/test-svc.log 2>&1",
		},
		{
			name:   "user scope writes below the resolved log dir",
			scope:  ScopeUser,
			logDir: "/home/alice/.local/state/test-svc",
			want:   ">> /home/alice/.local/state/test-svc/test-svc.log 2>&1",
		},
		{
			name:   "explicit log dir overrides the system default",
			scope:  ScopeSystem,
			logDir: "/srv/logs/test-svc",
			want:   ">> /srv/logs/test-svc/test-svc.log 2>&1",
		},
		{
			name:   "log dir with spaces is shell quoted",
			scope:  ScopeUser,
			logDir: "/home/alice/my logs",
			want:   ">> '/home/alice/my logs/test-svc.log' 2>&1",
		},
		{
			name:   "log dir with a single quote is escaped",
			scope:  ScopeUser,
			logDir: "/home/alice/it's",
			want:   `>> '/home/alice/it'\''s/test-svc.log' 2>&1`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := FormatCrontabEntry(ServiceConfig{
				Name:       "test-svc",
				ExecPath:   "/opt/test-svc/test-svc",
				WorkingDir: "/opt/test-svc",
				Arguments:  []string{"run"},
				Scope:      tt.scope,
				LogDir:     tt.logDir,
			})
			if !strings.Contains(entry, tt.want) {
				t.Errorf("entry missing %q, got:\n%s", tt.want, entry)
			}
		})
	}
}

func TestCrontabSystemEntryIsByteIdentical(t *testing.T) {
	config := ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
		Arguments:  []string{"run"},
	}

	want := "# daemonpilot: test-svc\n" +
		"@reboot cd '/opt/test-svc' && '/opt/test-svc/test-svc' 'run' >> /var/log/test-svc.log 2>&1"
	if got := FormatCrontabEntry(config); got != want {
		t.Errorf("system-scope entry changed:\ngot:  %q\nwant: %q", got, want)
	}
}

// userScopeCrontabLogDir mirrors the per-OS convention resolveCrontabLogDir
// follows, so these tests assert the same rule on every host rather than
// assuming a Linux runner.
func userScopeCrontabLogDir(home, name string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Logs", name)
	}
	return filepath.Join(home, ".local", "state", name)
}

func TestResolveCrontabLogDir(t *testing.T) {
	homeErr := errors.New("no such user")
	tests := []struct {
		name     string
		config   ServiceConfig
		home     string
		homeErr  error
		want     string
		wantErrI error
	}{
		{
			name:   "system scope defaults to /var/log",
			config: ServiceConfig{Name: "svc"},
			want:   "/var/log",
		},
		{
			name:   "explicit log dir wins in system scope",
			config: ServiceConfig{Name: "svc", LogDir: "/srv/logs"},
			want:   "/srv/logs",
		},
		{
			name:   "user scope derives from home",
			config: ServiceConfig{Name: "svc", Scope: ScopeUser},
			home:   "/home/alice",
			want:   userScopeCrontabLogDir("/home/alice", "svc"),
		},
		{
			name:   "explicit log dir wins in user scope",
			config: ServiceConfig{Name: "svc", Scope: ScopeUser, LogDir: "/home/alice/logs"},
			home:   "/home/alice",
			want:   "/home/alice/logs",
		},
		{
			name:     "user scope with empty home",
			config:   ServiceConfig{Name: "svc", Scope: ScopeUser},
			home:     "",
			wantErrI: ErrNoHomeDir,
		},
		{
			name:     "user scope with home error",
			config:   ServiceConfig{Name: "svc", Scope: ScopeUser},
			homeErr:  homeErr,
			wantErrI: ErrNoHomeDir,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveCrontabLogDir(tt.config, func() (string, error) {
				return tt.home, tt.homeErr
			})
			if tt.wantErrI != nil {
				if !errors.Is(err, tt.wantErrI) {
					t.Fatalf("error = %v, want %v", err, tt.wantErrI)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveCrontabLogDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCrontabInstall_CreatesLogDir(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(""), nil)

	var created []string
	mgr := &crontabManager{
		executor: exec,
		mkdirAllFunc: func(path string, perm os.FileMode) error {
			created = append(created, path)
			if perm != 0o755 {
				t.Errorf("MkdirAll perm = %v, want 0755", perm)
			}
			return nil
		},
	}

	config := ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
		Scope:      ScopeUser,
		LogDir:     "/home/alice/.local/state/test-svc",
	}
	if err := mgr.Install(config); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(created) != 1 || created[0] != "/home/alice/.local/state/test-svc" {
		t.Errorf("MkdirAll calls = %v, want the resolved log dir", created)
	}
}

// WriteConfig is the path the config-patcher install and update-config take;
// it never calls Install, so it has to create the redirect parent itself or the
// entry's `>>` fails and the service never starts.
func TestCrontabWriteConfig_CreatesLogDir(t *testing.T) {
	tests := []struct {
		name string
		mgr  func(record func(string) error) *crontabManager
		want string
	}{
		{
			name: "explicit log dir from ManagerOptions",
			mgr: func(record func(string) error) *crontabManager {
				return &crontabManager{
					serviceName: "test-svc", scope: ScopeUser,
					logDir: "/home/alice/.local/state/test-svc", mkdirAllFunc: recordMkdir(record),
				}
			},
			want: "/home/alice/.local/state/test-svc",
		},
		{
			name: "system scope falls back to /var/log",
			mgr: func(record func(string) error) *crontabManager {
				return &crontabManager{serviceName: "test-svc", mkdirAllFunc: recordMkdir(record)}
			},
			want: "/var/log",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var created []string
			mgr := tt.mgr(func(path string) error {
				created = append(created, path)
				return nil
			})
			exec := newMockExecutor()
			exec.setResult("crontab -l", []byte(""), nil)
			mgr.executor = exec

			if err := mgr.WriteConfig("# daemonpilot: test-svc\n@reboot true"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(created) != 1 || created[0] != tt.want {
				t.Errorf("MkdirAll calls = %v, want [%s]", created, tt.want)
			}
		})
	}
}

// A re-install overwrites the manifest describing the entry that is still
// installed. If the crontab write then fails, that entry stays in place, so the
// manifest has to be restored rather than deleted.
func TestCrontabInstall_RollbackRestoresPreviousManifest(t *testing.T) {
	installDir := t.TempDir()
	const previous = "# daemonpilot: test-svc\n@reboot 'old-entry'"

	mgr := &crontabManager{serviceName: "test-svc", installDir: installDir}
	if err := mgr.writeManifestFile(previous); err != nil {
		t.Fatalf("setup: %v", err)
	}
	manifestPath := mgr.manifestFilePath()

	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(previous+"\n"), nil)
	exec.fallbackErr = errors.New("exit status 1")
	exec.fallbackOutput = []byte("crontab: installing new crontab failed")

	install := &crontabManager{
		executor:     exec,
		installDir:   installDir,
		mkdirAllFunc: func(string, os.FileMode) error { return nil },
	}
	err := install.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
		InstallDir: installDir,
	})
	if err == nil {
		t.Fatal("expected the crontab write to fail")
	}

	got, readErr := os.ReadFile(manifestPath)
	if readErr != nil {
		t.Fatalf("manifest was removed instead of restored: %v", readErr)
	}
	if string(got) != previous {
		t.Errorf("manifest = %q, want the pre-install content %q", got, previous)
	}
}

// A manifest that cannot be read must not be mistaken for a missing one: the
// rollback would then delete a manifest describing an entry still live in the
// crontab. The read happens before anything is mutated, so Install fails
// outright and never reaches `crontab <file>`.
func TestCrontabInstall_UnreadableManifestAbortsBeforeTouchingCrontab(t *testing.T) {
	installDir := t.TempDir()

	mgr := &crontabManager{serviceName: "test-svc", installDir: installDir}
	// A directory in the manifest's place makes ReadFile fail with something
	// other than fs.ErrNotExist, on any host and for any uid.
	if err := os.Mkdir(mgr.manifestFilePath(), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(""), nil)

	install := &crontabManager{
		executor:     exec,
		installDir:   installDir,
		mkdirAllFunc: func(string, os.FileMode) error { return nil },
	}
	err := install.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
		InstallDir: installDir,
	})
	if err == nil {
		t.Fatal("expected the unreadable manifest to fail the install")
	}
	if !strings.Contains(err.Error(), "reading crontab manifest") {
		t.Errorf("unexpected error: %v", err)
	}

	for _, c := range exec.calls {
		if c.Name == "crontab" && len(c.Args) == 1 && c.Args[0] != "-l" {
			t.Errorf("crontab was replaced despite the unreadable manifest: %v", c.Args)
		}
	}
}

func TestCrontabInstall_RollbackRemovesManifestWhenThereWasNone(t *testing.T) {
	installDir := t.TempDir()

	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(""), nil)
	exec.fallbackErr = errors.New("exit status 1")
	exec.fallbackOutput = []byte("crontab: installing new crontab failed")

	mgr := &crontabManager{
		executor:     exec,
		installDir:   installDir,
		mkdirAllFunc: func(string, os.FileMode) error { return nil },
	}
	err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
		InstallDir: installDir,
	})
	if err == nil {
		t.Fatal("expected the crontab write to fail")
	}

	if _, statErr := os.Stat(mgr.manifestFilePath()); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("manifest should have been removed, stat error = %v", statErr)
	}
}

// cron splits its file on newlines before any shell quoting applies, and
// rewrites an unescaped %% into a newline, so neither can be allowed into the
// redirect target: the first appends an entry uninstall would never find again,
// the second silently truncates the command.
func TestCrontabRejectsUnsafeLogDir(t *testing.T) {
	tests := []struct {
		name    string
		logDir  string
		wantErr string
	}{
		{
			name:    "newline would append a second crontab entry",
			logDir:  "/tmp/logs\n* * * * * /bin/sh -c evil",
			wantErr: "newline",
		},
		{
			name:    "carriage return",
			logDir:  "/tmp/logs\r",
			wantErr: "newline",
		},
		{
			name:    "percent is a cron newline",
			logDir:  "/srv/logs/100%",
			wantErr: "%",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := ServiceConfig{
				Name:       "test-svc",
				ExecPath:   "/opt/test-svc/test-svc",
				WorkingDir: "/opt/test-svc",
				LogDir:     tt.logDir,
			}

			exec := newMockExecutor()
			exec.setResult("crontab -l", []byte(""), nil)
			mgr := &crontabManager{
				executor:     exec,
				mkdirAllFunc: func(string, os.FileMode) error { return nil },
			}

			err := mgr.Install(config)
			if err == nil {
				t.Fatal("Install accepted an unsafe log directory")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %v does not mention %q", err, tt.wantErr)
			}
			if exec.called("crontab", "-l") {
				t.Error("Install must reject the log directory before touching the crontab")
			}

			if _, err := mgr.GenerateConfig(config); err == nil {
				t.Error("GenerateConfig accepted an unsafe log directory")
			}
		})
	}
}

// Shell quoting is applied after cron has already split the file on newlines
// and rewritten `%`, so every value reaching the command field needs the same
// rejection the redirect target gets — a newline in an argument installs a
// second crontab line that uninstall cannot find again.
func TestCrontabRejectsUnsafeCommandFields(t *testing.T) {
	base := ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
		LogDir:     "/var/log/test-svc",
	}

	tests := []struct {
		name    string
		mutate  func(*ServiceConfig)
		wantErr string
		// mustOmit is a substring the error must not carry, for the values that
		// may hold a secret.
		mustOmit string
	}{
		{
			name:    "newline in an argument",
			mutate:  func(c *ServiceConfig) { c.Arguments = []string{"run\n* * * * * /bin/sh -c evil"} },
			wantErr: "argument must not contain newline",
		},
		{
			name:    "percent in an argument",
			mutate:  func(c *ServiceConfig) { c.Arguments = []string{"--threshold=90%"} },
			wantErr: "argument must not contain %",
		},
		{
			name:    "newline in an environment value",
			mutate:  func(c *ServiceConfig) { c.Environment = map[string]string{"TOKEN": "a\nb"} },
			wantErr: "environment value for TOKEN must not contain newline",
		},
		{
			name:    "percent in an environment value",
			mutate:  func(c *ServiceConfig) { c.Environment = map[string]string{"URL": "http://x/%2f"} },
			wantErr: "environment value for URL must not contain %",
		},
		{
			name:     "the rejected environment value is not printed",
			mutate:   func(c *ServiceConfig) { c.Environment = map[string]string{"TOKEN": "s3cr3t%40x"} },
			wantErr:  "environment value for TOKEN must not contain %",
			mustOmit: "s3cr3t",
		},
		{
			name:    "newline in the executable path",
			mutate:  func(c *ServiceConfig) { c.ExecPath = "/opt/test-svc/test-svc\nevil" },
			wantErr: "executable path must not contain newline",
		},
		{
			name:    "newline in the working directory",
			mutate:  func(c *ServiceConfig) { c.WorkingDir = "/opt/test-svc\nevil" },
			wantErr: "working directory must not contain newline",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := base
			tt.mutate(&config)

			exec := newMockExecutor()
			exec.setResult("crontab -l", []byte(""), nil)
			mgr := &crontabManager{
				executor:     exec,
				mkdirAllFunc: func(string, os.FileMode) error { return nil },
			}

			err := mgr.Install(config)
			if err == nil {
				t.Fatal("Install accepted an unsafe command field")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %v does not contain %q", err, tt.wantErr)
			}
			if tt.mustOmit != "" && strings.Contains(err.Error(), tt.mustOmit) {
				t.Errorf("error %v leaks %q", err, tt.mustOmit)
			}
			if exec.called("crontab", "-l") {
				t.Error("Install must reject the field before touching the crontab")
			}

			if _, err := mgr.GenerateConfig(config); err == nil {
				t.Error("GenerateConfig accepted an unsafe command field")
			}
		})
	}
}

func recordMkdir(record func(string) error) func(string, os.FileMode) error {
	return func(path string, perm os.FileMode) error {
		if perm != 0o755 {
			return fmt.Errorf("MkdirAll perm = %v, want 0755", perm)
		}
		return record(path)
	}
}

func TestCrontabInstall_LogDirFailureLeavesNothingInstalled(t *testing.T) {
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(""), nil)

	mgr := &crontabManager{
		executor:     exec,
		installDir:   t.TempDir(),
		mkdirAllFunc: func(string, os.FileMode) error { return errors.New("permission denied") },
	}

	err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
		InstallDir: mgr.installDir,
		LogDir:     "/srv/logs/test-svc",
	})
	if err == nil || !strings.Contains(err.Error(), "creating log directory") {
		t.Fatalf("Install() error = %v, want a log directory error", err)
	}
	for _, c := range exec.calls {
		if c.Name == "crontab" && len(c.Args) == 1 && c.Args[0] != "-l" {
			t.Error("crontab was replaced despite the log directory failing")
		}
	}
}

// manifestWatchingExecutor records whether the manifest already existed at the
// moment the crontab was replaced.
type manifestWatchingExecutor struct {
	*mockExecutor

	manifestPath          string
	manifestExistedOnCall bool
}

func (e *manifestWatchingExecutor) Run(name string, args ...string) ([]byte, error) {
	if name == "crontab" && len(args) == 1 && args[0] != "-l" {
		_, err := os.Stat(e.manifestPath)
		e.manifestExistedOnCall = err == nil
	}
	return e.mockExecutor.Run(name, args...)
}

func TestCrontabInstall_WritesManifestBeforeCrontab(t *testing.T) {
	installDir := t.TempDir()
	inner := newMockExecutor()
	inner.setResult("crontab -l", []byte(""), nil)
	exec := &manifestWatchingExecutor{
		mockExecutor: inner,
		manifestPath: filepath.Join(installDir, ".daemonpilot-crontab-test-svc.entry"),
	}

	mgr := &crontabManager{
		executor:     exec,
		mkdirAllFunc: func(string, os.FileMode) error { return nil },
	}
	if err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
		InstallDir: installDir,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !exec.manifestExistedOnCall {
		t.Error("manifest was written after the crontab was replaced")
	}
}

func TestCrontabInstall_RemovesManifestWhenCrontabFails(t *testing.T) {
	installDir := t.TempDir()
	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(""), nil)
	exec.fallbackErr = errors.New("crontab: installing new crontab failed")

	mgr := &crontabManager{
		executor:     exec,
		mkdirAllFunc: func(string, os.FileMode) error { return nil },
	}
	err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
		InstallDir: installDir,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	manifest := filepath.Join(installDir, ".daemonpilot-crontab-test-svc.entry")
	if _, statErr := os.Stat(manifest); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("manifest still present after a failed crontab write: %v", statErr)
	}
}

// WriteConfig is the install path whenever ConfigPatchers is set, and the
// update-config write path, so it needs the same ordering guarantee Install has.
func TestCrontabWriteConfig_WritesManifestBeforeCrontab(t *testing.T) {
	installDir := t.TempDir()
	inner := newMockExecutor()
	inner.setResult("crontab -l", []byte(""), nil)
	exec := &manifestWatchingExecutor{
		mockExecutor: inner,
		manifestPath: filepath.Join(installDir, ".daemonpilot-crontab-test-svc.entry"),
	}

	mgr := &crontabManager{
		serviceName:  "test-svc",
		installDir:   installDir,
		executor:     exec,
		mkdirAllFunc: func(string, os.FileMode) error { return nil },
	}
	if err := mgr.WriteConfig("# daemonpilot: test-svc\n@reboot cd /opt/test-svc && ./test-svc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !exec.manifestExistedOnCall {
		t.Error("manifest was written after the crontab was replaced")
	}
}

func TestCrontabWriteConfig_RejectsUnsafeEntryContent(t *testing.T) {
	valid := "# daemonpilot: test-svc\n@reboot cd /opt/test-svc && ./test-svc"

	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{name: "the generated entry is accepted", content: valid},
		{name: "a trailing newline is accepted", content: valid + "\n"},
		{
			name:    "a patcher-appended third line is rejected",
			content: valid + "\n@reboot /usr/bin/attacker",
			wantErr: true,
		},
		{
			name:    "a marker for another service is rejected",
			content: "# daemonpilot: other-svc\n@reboot cd /opt/test-svc && ./test-svc",
			wantErr: true,
		},
		{
			name:    "a missing marker is rejected",
			content: "@reboot cd /opt/test-svc && ./test-svc",
			wantErr: true,
		},
		{
			name:    "a percent in the command line is rejected",
			content: "# daemonpilot: test-svc\n@reboot cd /opt/test-svc && ./test-svc --pass=a%b",
			wantErr: true,
		},
		{
			name:    "a carriage return is rejected",
			content: "# daemonpilot: test-svc\r\n@reboot cd /opt/test-svc && ./test-svc",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installDir := t.TempDir()
			exec := newMockExecutor()
			exec.setResult("crontab -l", []byte(""), nil)
			mgr := &crontabManager{
				serviceName:  "test-svc",
				installDir:   installDir,
				executor:     exec,
				mkdirAllFunc: func(string, os.FileMode) error { return nil },
			}

			err := mgr.WriteConfig(tt.content)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				// Nothing may be mutated by a rejected entry.
				if _, statErr := os.Stat(filepath.Join(installDir, ".daemonpilot-crontab-test-svc.entry")); !os.IsNotExist(statErr) {
					t.Error("manifest was written for a rejected entry")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCrontabWriteConfig_RestoresPreviousManifestWhenCrontabFails(t *testing.T) {
	tests := []struct {
		name        string
		before      string
		wantContent string
		wantExists  bool
	}{
		{
			name:        "re-write restores the manifest it overwrote",
			before:      "# daemonpilot: test-svc\n@reboot old-entry",
			wantContent: "# daemonpilot: test-svc\n@reboot old-entry",
			wantExists:  true,
		},
		{
			name:       "first write leaves no manifest behind",
			wantExists: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installDir := t.TempDir()
			manifest := filepath.Join(installDir, ".daemonpilot-crontab-test-svc.entry")
			if tt.before != "" {
				if err := os.WriteFile(manifest, []byte(tt.before), 0o600); err != nil {
					t.Fatalf("seeding manifest: %v", err)
				}
			}

			exec := newMockExecutor()
			exec.setResult("crontab -l", []byte(""), nil)
			exec.fallbackErr = errors.New("crontab: installing new crontab failed")

			mgr := &crontabManager{
				serviceName:  "test-svc",
				installDir:   installDir,
				executor:     exec,
				mkdirAllFunc: func(string, os.FileMode) error { return nil },
			}
			if err := mgr.WriteConfig("# daemonpilot: test-svc\n@reboot new-entry"); err == nil {
				t.Fatal("expected an error")
			}

			got, err := os.ReadFile(manifest)
			if !tt.wantExists {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("manifest still present after a failed crontab write: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("reading manifest: %v", err)
			}
			if string(got) != tt.wantContent {
				t.Errorf("manifest = %q, want %q — the entry still in the crontab has no manifest describing it",
					got, tt.wantContent)
			}
		})
	}
}

// The caller is itself rolling back, so the manifest on disk describes the entry
// being rolled back away from. Restoring it would leave the manifest naming the
// configuration the rollback exists to remove.
func TestCrontabWriteConfigForRollback_KeepsTheRolledBackManifest(t *testing.T) {
	installDir := t.TempDir()
	manifest := filepath.Join(installDir, ".daemonpilot-crontab-test-svc.entry")
	if err := os.WriteFile(manifest, []byte("# daemonpilot: test-svc\n@reboot rejected"), 0o600); err != nil {
		t.Fatalf("seeding manifest: %v", err)
	}

	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(""), nil)
	exec.fallbackErr = errors.New("crontab: installing new crontab failed")

	mgr := &crontabManager{
		serviceName:  "test-svc",
		installDir:   installDir,
		executor:     exec,
		mkdirAllFunc: func(string, os.FileMode) error { return nil },
	}
	const previous = "# daemonpilot: test-svc\n@reboot previous"
	if err := mgr.WriteConfigForRollback(previous); err == nil {
		t.Fatal("expected an error")
	}

	got, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	if string(got) != previous {
		t.Errorf("manifest = %q, want the rolled-back entry %q", got, previous)
	}
}

// Without an install dir manifestFilePath is a bare relative name, so any Remove
// of it would delete a dot-file from whatever directory the CLI was invoked in.
func TestCrontabManifestRemoval_SkippedWithoutInstallDir(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	decoy := filepath.Join(cwd, ".daemonpilot-crontab-test-svc.entry")
	if err := os.WriteFile(decoy, []byte("not ours"), 0o600); err != nil {
		t.Fatalf("seeding decoy: %v", err)
	}

	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(""), nil)
	mgr := &crontabManager{serviceName: "test-svc", executor: exec}

	if err := mgr.Uninstall(); !errors.Is(err, ErrCrontabEntryNotFound) {
		t.Fatalf("Uninstall() error = %v, want ErrCrontabEntryNotFound", err)
	}
	if _, err := os.Stat(decoy); err != nil {
		t.Errorf("uninstall deleted a file in the working directory: %v", err)
	}
}

func TestCrontabRebootScheduleCaveat(t *testing.T) {
	tests := []struct {
		name     string
		schedule string
		want     bool
	}{
		{name: "empty schedule defaults to @reboot", schedule: "", want: true},
		{name: "explicit @reboot", schedule: "@reboot", want: true},
		{name: "padded @reboot", schedule: " @reboot ", want: true},
		{name: "time based schedule", schedule: "*/5 * * * *", want: false},
		{name: "other keyword", schedule: "@daily", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CrontabRebootScheduleCaveat(tt.schedule)
			if (got != "") != tt.want {
				t.Fatalf("CrontabRebootScheduleCaveat(%q) = %q, want caveat = %v", tt.schedule, got, tt.want)
			}
			if tt.want && !strings.Contains(got, "FEATURE_CROND_SPECIAL_TIMES") {
				t.Errorf("caveat should name the BusyBox feature flag, got: %q", got)
			}
			if tt.want && !strings.Contains(got, "Config.CrontabSchedule") {
				t.Errorf("caveat should point at Config.CrontabSchedule, got: %q", got)
			}
		})
	}
}

// pkg/daemonpilot emits the @reboot caveat through the caller's own Logger
// (TestHandleInstall_* covers that). The detection logger now reaches the same
// terminal at default log levels, so Install repeating it here would print the
// paragraph twice for one install.
func TestCrontabInstall_DoesNotRepeatRebootCaveat(t *testing.T) {
	oldLog := getDetectionLog()
	defer SetDetectionLogger(oldLog)
	var buf bytes.Buffer
	SetDetectionLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	exec := newMockExecutor()
	exec.setResult("crontab -l", []byte(""), nil)
	mgr := &crontabManager{
		executor:     exec,
		mkdirAllFunc: func(string, os.FileMode) error { return nil },
	}
	if err := mgr.Install(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(buf.String(), "FEATURE_CROND_SPECIAL_TIMES") {
		t.Errorf("@reboot caveat emitted from the daemon layer too, so an install prints it twice:\n%s", buf.String())
	}
}

func TestCrontabGenerateConfig_UserScopeLogPath(t *testing.T) {
	mgr := &crontabManager{
		executor: newMockExecutor(),
		homeFunc: func() (string, error) { return "/home/alice", nil },
	}

	entry, err := mgr.GenerateConfig(ServiceConfig{
		Name:       "test-svc",
		ExecPath:   "/opt/test-svc/test-svc",
		WorkingDir: "/opt/test-svc",
		Scope:      ScopeUser,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ">> " + filepath.Join(userScopeCrontabLogDir("/home/alice", "test-svc"), "test-svc.log") + " 2>&1"
	if !strings.Contains(entry, want) {
		t.Errorf("entry missing %q, got:\n%s", want, entry)
	}
}
