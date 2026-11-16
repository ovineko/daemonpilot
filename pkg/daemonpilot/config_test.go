package daemonpilot

import (
	"context"
	"crypto/ed25519"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func dummyWorker(context.Context) error { return nil }

func TestConfigValidate_MissingName(t *testing.T) {
	cfg := Config{WorkerFunc: dummyWorker}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for missing Name")
	}
}

func TestConfigValidate_MissingWorkerFunc(t *testing.T) {
	cfg := Config{Name: "test"}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for missing WorkerFunc")
	}
}

func TestConfigValidate_Valid(t *testing.T) {
	cfg := Config{Name: "test", WorkerFunc: dummyWorker}
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// Neither character stays inside the field it is rendered into: the crontab
// manager writes LogDir to a command line cron splits on newlines before any
// shell quoting applies, and cron turns an unescaped % into a newline of its
// own. Both have to be refused here rather than at install time, which runs
// after the binary has been copied and leaves that copy orphaned.
func TestConfigValidate_LogDirRejectsCronMetacharacters(t *testing.T) {
	tests := []struct {
		name   string
		logDir string
		wantOK bool
	}{
		{name: "plain path", logDir: "/var/log/test", wantOK: true},
		{name: "empty", logDir: "", wantOK: true},
		{name: "path with a space", logDir: "/var/log/my test", wantOK: true},
		{name: "newline injects a crontab entry", logDir: "/var/log\n* * * * * /bin/sh -c evil"},
		{name: "carriage return", logDir: "/var/log\r"},
		{name: "percent truncates the cron command", logDir: "/var/log/%Y"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Name: "test", WorkerFunc: dummyWorker, LogDir: tt.logDir}
			err := cfg.validateCrontabFields()
			if tt.wantOK {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error for LogDir %q", tt.logDir)
			}
			if !strings.Contains(err.Error(), "LogDir") {
				t.Errorf("error %v does not mention LogDir", err)
			}
		})
	}
}

// The crontab-safety checks are gated on a crontab install and must never run
// from Validate, which New calls at every process start: % is legal everywhere
// except a crontab command field, so rejecting it there would stop an existing
// systemd or launchd service carrying a percent-encoded value from booting.
func TestConfigValidate_AllowsCronMetacharactersOutsideCrontab(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		Name:               "test",
		WorkerFunc:         dummyWorker,
		LogDir:             filepath.Join(root, "var", "log", "%Y"),
		InstallDir:         filepath.Join(root, "opt", "100%"),
		ServiceArguments:   []string{"--url=https://example.test/a%20b"},
		ServiceEnvironment: map[string]string{"DSN": "postgres://u:p%40ss@h/db"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := cfg.validateCrontabFields(); err == nil {
		t.Error("expected the crontab-gated check to refuse the same config")
	}
}

func TestConfigValidate_LogDirMustBeAbsolute(t *testing.T) {
	cfg := Config{Name: "test", WorkerFunc: dummyWorker, LogDir: "logs"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected an error for a relative LogDir")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error %v does not mention the absolute-path requirement", err)
	}
}

// An unusable environment key is refused by the crontab manager from Install,
// after the binary has been copied — so it orphans the copy exactly as a
// rejected LogDir would, and has to be refused on the same pre-copy path.
func TestValidateCrontabFields_RejectsInvalidEnvKeys(t *testing.T) {
	cfg := Config{
		Name: "test", WorkerFunc: dummyWorker,
		ServiceEnvironment: map[string]string{"MY-VAR": "1"},
	}
	err := cfg.validateCrontabFields()
	if err == nil {
		t.Fatal("expected an error for an invalid environment key")
	}
	if !strings.Contains(err.Error(), "MY-VAR") {
		t.Errorf("error %v does not name the offending key", err)
	}
}

// The schedule is validated by the crontab manager from Install, which runs
// after the binary has been copied — so a rejected schedule orphans the copy
// exactly as a rejected LogDir would. It has to be refused here too.
func TestConfigValidate_CrontabSchedule(t *testing.T) {
	tests := []struct {
		name     string
		schedule string
		wantOK   bool
	}{
		{name: "unset keeps the default", schedule: "", wantOK: true},
		{name: "reboot keyword", schedule: "@reboot", wantOK: true},
		{name: "five fields", schedule: "*/5 * * * *", wantOK: true},
		{name: "unknown keyword", schedule: "@sometimes"},
		{name: "too few fields", schedule: "* * * *"},
		{name: "newline injects a crontab entry", schedule: "@reboot\n* * * * * /bin/sh -c evil"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Name: "test", WorkerFunc: dummyWorker, CrontabSchedule: tt.schedule}
			err := cfg.validateCrontabFields()
			if tt.wantOK {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error for CrontabSchedule %q", tt.schedule)
			}
			if !strings.Contains(err.Error(), "CrontabSchedule") {
				t.Errorf("error %v does not mention CrontabSchedule", err)
			}
		})
	}
}

// LogDir is not the only value that reaches a crontab command field: InstallDir
// becomes the ExecPath and WorkingDir, and the service arguments and
// environment values are rendered there verbatim. All of them have to be
// refused before the binary is copied, for the same reason LogDir is.
func TestConfigValidate_CommandFieldsRejectCronMetacharacters(t *testing.T) {
	const inject = "/opt\n* * * * * /bin/sh -c evil"

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantOK  bool
		mention string
	}{
		{name: "clean install dir", mutate: func(c *Config) { c.InstallDir = "/opt/test" }, wantOK: true},
		{
			name: "newline in install dir", mutate: func(c *Config) { c.InstallDir = inject },
			mention: "InstallDir",
		},
		{
			name: "percent in install dir", mutate: func(c *Config) { c.InstallDir = "/opt/%Y" },
			mention: "InstallDir",
		},
		{
			name: "clean arguments", mutate: func(c *Config) { c.ServiceArguments = []string{"--flag", "value"} },
			wantOK: true,
		},
		{
			name: "newline in an argument", mutate: func(c *Config) { c.ServiceArguments = []string{"--flag", inject} },
			mention: "ServiceArguments[1]",
		},
		{
			name: "carriage return in an argument", mutate: func(c *Config) { c.ServiceArguments = []string{"a\rb"} },
			mention: "ServiceArguments[0]",
		},
		{
			name: "clean environment", mutate: func(c *Config) { c.ServiceEnvironment = map[string]string{"K": "v"} },
			wantOK: true,
		},
		{
			name:    "newline in an environment value",
			mutate:  func(c *Config) { c.ServiceEnvironment = map[string]string{"K": inject} },
			mention: "ServiceEnvironment[K]",
		},
		{
			name:    "percent in an environment value",
			mutate:  func(c *Config) { c.ServiceEnvironment = map[string]string{"K": "100%"} },
			mention: "ServiceEnvironment[K]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Name: "test", WorkerFunc: dummyWorker}
			tt.mutate(&cfg)
			err := cfg.validateCrontabFields()
			if tt.wantOK {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.mention) {
				t.Errorf("error %v does not mention %s", err, tt.mention)
			}
		})
	}
}

// Environment values are where tokens and passwords live, and a percent-encoded
// secret trips this check, so the rejection must name the variable without
// printing what it held: install output ends up in CI and provisioning logs.
func TestConfigValidate_DoesNotPrintRejectedEnvironmentValues(t *testing.T) {
	const secret = "s3cr3t%40token"
	cfg := Config{
		Name:               "test",
		WorkerFunc:         dummyWorker,
		ServiceEnvironment: map[string]string{"API_TOKEN": secret},
	}

	err := cfg.validateCrontabFields()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "ServiceEnvironment[API_TOKEN]") {
		t.Errorf("error %v does not name the variable", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error %v leaks the environment value", err)
	}
}

func TestConfigValidate_InvalidUpdateConfig(t *testing.T) {
	cfg := Config{
		Name:       "test",
		WorkerFunc: dummyWorker,
		Update:     &UpdateConfig{},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for invalid UpdateConfig")
	}
}

func TestConfigValidate_ValidUpdateConfig(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	cfg := Config{
		Name:       "test",
		WorkerFunc: dummyWorker,
		Update: &UpdateConfig{
			Source:    mockSource{},
			PublicKey: pub,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUpdateConfigValidate_MissingSource(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	uc := UpdateConfig{PublicKey: pub}
	if err := uc.Validate(); err == nil {
		t.Error("expected error for missing Source")
	}
}

func TestUpdateConfigValidate_MissingPublicKey(t *testing.T) {
	uc := UpdateConfig{Source: mockSource{}}
	if err := uc.Validate(); err == nil {
		t.Error("expected error for missing PublicKey")
	}
}

func TestUpdateConfigValidate_Valid(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	uc := UpdateConfig{Source: mockSource{}, PublicKey: pub}
	if err := uc.Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUpdateConfigValidate_NegativeDurations(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	tests := []struct {
		name string
		uc   UpdateConfig
	}{
		{"negative CheckInterval", UpdateConfig{Source: mockSource{}, PublicKey: pub, CheckInterval: -1 * time.Second}},
		{"negative SelftestTimeout", UpdateConfig{Source: mockSource{}, PublicKey: pub, SelftestTimeout: -1 * time.Second}},
		{"negative RollbackTimeout", UpdateConfig{Source: mockSource{}, PublicKey: pub, RollbackTimeout: -1 * time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.uc.Validate(); err == nil {
				t.Error("expected error for negative duration")
			}
		})
	}
}

func TestConfigSetDefaults_AllDefaults(t *testing.T) {
	cfg := Config{Name: "myapp"}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}

	if cfg.Version != Version {
		t.Errorf("Version: got %q, want %q", cfg.Version, Version)
	}
	if cfg.WatchdogVersion != WatchdogVersion {
		t.Errorf("WatchdogVersion: got %q, want %q", cfg.WatchdogVersion, WatchdogVersion)
	}
	if cfg.GracefulTimeout != 30*time.Second {
		t.Errorf("GracefulTimeout: got %v, want 30s", cfg.GracefulTimeout)
	}
	if cfg.StableThreshold != 5*time.Minute {
		t.Errorf("StableThreshold: got %v, want 5m", cfg.StableThreshold)
	}
	if cfg.MaxRestartDelay != 60*time.Second {
		t.Errorf("MaxRestartDelay: got %v, want 60s", cfg.MaxRestartDelay)
	}

	if runtime.GOOS == "darwin" {
		if cfg.InstallDir != "/usr/local/bin" {
			t.Errorf("InstallDir (darwin): got %q, want /usr/local/bin", cfg.InstallDir)
		}
	} else {
		if cfg.InstallDir != "/opt/myapp" {
			t.Errorf("InstallDir (linux): got %q, want /opt/myapp", cfg.InstallDir)
		}
	}
}

func TestConfigSetDefaults_WatchdogLogCollection(t *testing.T) {
	cfg := Config{CollectWatchdogLogs: true}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}
	if cfg.WatchdogLogBufferBytes != DefaultWatchdogLogBufferBytes {
		t.Fatalf("WatchdogLogBufferBytes = %d, want %d", cfg.WatchdogLogBufferBytes, DefaultWatchdogLogBufferBytes)
	}

	disabled := Config{}
	if err := disabled.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults disabled: %v", err)
	}
	if disabled.WatchdogLogBufferBytes != 0 {
		t.Fatalf("disabled WatchdogLogBufferBytes = %d, want 0", disabled.WatchdogLogBufferBytes)
	}
}

func TestConfigSetDefaults_PreservesExistingValues(t *testing.T) {
	cfg := Config{
		Name:            "myapp",
		Version:         "1.0.0",
		WatchdogVersion: "2",
		InstallDir:      "/custom/path",
		GracefulTimeout: 10 * time.Second,
		StableThreshold: 2 * time.Minute,
		MaxRestartDelay: 30 * time.Second,
	}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}

	if cfg.Version != "1.0.0" {
		t.Errorf("Version should be preserved, got %q", cfg.Version)
	}
	if cfg.WatchdogVersion != "2" {
		t.Errorf("WatchdogVersion should be preserved, got %q", cfg.WatchdogVersion)
	}
	if cfg.InstallDir != "/custom/path" {
		t.Errorf("InstallDir should be preserved, got %q", cfg.InstallDir)
	}
	if cfg.GracefulTimeout != 10*time.Second {
		t.Errorf("GracefulTimeout should be preserved, got %v", cfg.GracefulTimeout)
	}
	if cfg.StableThreshold != 2*time.Minute {
		t.Errorf("StableThreshold should be preserved, got %v", cfg.StableThreshold)
	}
	if cfg.MaxRestartDelay != 30*time.Second {
		t.Errorf("MaxRestartDelay should be preserved, got %v", cfg.MaxRestartDelay)
	}
}

func TestConfigSetDefaults_UpdateConfigDefaults(t *testing.T) {
	cfg := Config{
		Name:   "myapp",
		Update: &UpdateConfig{},
	}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}

	if cfg.Update.CheckInterval != 5*time.Minute {
		t.Errorf("CheckInterval: got %v, want 5m", cfg.Update.CheckInterval)
	}
	if cfg.Update.SelftestTimeout != 60*time.Second {
		t.Errorf("SelftestTimeout: got %v, want 60s", cfg.Update.SelftestTimeout)
	}
	if cfg.Update.RollbackTimeout != 5*time.Minute {
		t.Errorf("RollbackTimeout: got %v, want 5m", cfg.Update.RollbackTimeout)
	}
}

func TestConfigSetDefaults_UpdateConfigPreservesValues(t *testing.T) {
	cfg := Config{
		Name: "myapp",
		Update: &UpdateConfig{
			CheckInterval:   10 * time.Minute,
			SelftestTimeout: 30 * time.Second,
			RollbackTimeout: 10 * time.Minute,
		},
	}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}

	if cfg.Update.CheckInterval != 10*time.Minute {
		t.Errorf("CheckInterval should be preserved, got %v", cfg.Update.CheckInterval)
	}
	if cfg.Update.SelftestTimeout != 30*time.Second {
		t.Errorf("SelftestTimeout should be preserved, got %v", cfg.Update.SelftestTimeout)
	}
	if cfg.Update.RollbackTimeout != 10*time.Minute {
		t.Errorf("RollbackTimeout should be preserved, got %v", cfg.Update.RollbackTimeout)
	}
}

func TestConfigSetDefaults_NilUpdate(t *testing.T) {
	cfg := Config{Name: "myapp"}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}
	if cfg.Update != nil {
		t.Error("Update should remain nil")
	}
}

func TestIsValidServiceName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"lowercase letters", "myservice", true},
		{"lowercase with hyphen", "my-service", true},
		{"lowercase with underscore", "test_01", true},
		{"digits only", "123", true},
		{"mixed lowercase digits hyphen underscore", "my-service_01", true},
		{"uppercase start", "MyService", false},
		{"mixed case", "Test-01", false},
		{"all uppercase", "MYSERVICE", false},
		{"empty string", "", false},
		{"space in name", "my service", false},
		{"dot in name", "my.service", false},
		{"slash in name", "my/service", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidServiceName(tt.input)
			if got != tt.want {
				t.Errorf("isValidServiceName(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestConfigValidate_UppercaseNameRejected(t *testing.T) {
	cfg := Config{Name: "MyService", WorkerFunc: dummyWorker}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for uppercase service name")
	}
	if err != nil {
		want := "lowercase only"
		if got := err.Error(); !contains(got, want) {
			t.Errorf("error message should mention %q, got %q", want, got)
		}
	}
}

func TestConfigValidate_HealthCheckFields(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "valid with HealthFunc and defaults",
			cfg: Config{
				Name:       "test",
				WorkerFunc: dummyWorker,
				HealthFunc: func(context.Context) error { return nil },
			},
			wantErr: false,
		},
		{
			name: "valid with custom HealthInterval and HealthFailures",
			cfg: Config{
				Name:           "test",
				WorkerFunc:     dummyWorker,
				HealthFunc:     func(context.Context) error { return nil },
				HealthInterval: 10 * time.Second,
				HealthFailures: 5,
			},
			wantErr: false,
		},
		{
			name: "valid without HealthFunc",
			cfg: Config{
				Name:       "test",
				WorkerFunc: dummyWorker,
			},
			wantErr: false,
		},
		{
			name: "negative HealthInterval",
			cfg: Config{
				Name:           "test",
				WorkerFunc:     dummyWorker,
				HealthInterval: -1 * time.Second,
			},
			wantErr: true,
		},
		{
			name: "negative HealthFailures",
			cfg: Config{
				Name:           "test",
				WorkerFunc:     dummyWorker,
				HealthFailures: -1,
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestConfigSetDefaults_HealthCheckDefaults(t *testing.T) {
	cfg := Config{
		Name:       "myapp",
		HealthFunc: func(context.Context) error { return nil },
	}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}

	if cfg.HealthInterval != 30*time.Second {
		t.Errorf("HealthInterval: got %v, want 30s", cfg.HealthInterval)
	}
	if cfg.HealthFailures != 3 {
		t.Errorf("HealthFailures: got %d, want 3", cfg.HealthFailures)
	}
}

func TestConfigSetDefaults_HealthCheckPreservesValues(t *testing.T) {
	cfg := Config{
		Name:           "myapp",
		HealthFunc:     func(context.Context) error { return nil },
		HealthInterval: 10 * time.Second,
		HealthFailures: 5,
	}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}

	if cfg.HealthInterval != 10*time.Second {
		t.Errorf("HealthInterval should be preserved, got %v", cfg.HealthInterval)
	}
	if cfg.HealthFailures != 5 {
		t.Errorf("HealthFailures should be preserved, got %d", cfg.HealthFailures)
	}
}

func TestConfigSetDefaults_HealthCheckNoDefaultsWithoutFunc(t *testing.T) {
	cfg := Config{Name: "myapp"}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}

	if cfg.HealthInterval != 0 {
		t.Errorf("HealthInterval should remain zero without HealthFunc, got %v", cfg.HealthInterval)
	}
	if cfg.HealthFailures != 0 {
		t.Errorf("HealthFailures should remain zero without HealthFunc, got %d", cfg.HealthFailures)
	}
}

func TestConfigValidate_ResourceLimitFields(t *testing.T) {
	intPtr := func(v int) *int { return &v }

	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "valid with no resource limits",
			cfg: Config{
				Name:       "test",
				WorkerFunc: dummyWorker,
			},
			wantErr: false,
		},
		{
			name: "valid MaxMemoryBytes",
			cfg: Config{
				Name:           "test",
				WorkerFunc:     dummyWorker,
				MaxMemoryBytes: 256 * 1024 * 1024,
			},
			wantErr: false,
		},
		{
			name: "valid OOMScoreAdj zero",
			cfg: Config{
				Name:        "test",
				WorkerFunc:  dummyWorker,
				OOMScoreAdj: intPtr(0),
			},
			wantErr: false,
		},
		{
			name: "valid OOMScoreAdj positive",
			cfg: Config{
				Name:        "test",
				WorkerFunc:  dummyWorker,
				OOMScoreAdj: intPtr(500),
			},
			wantErr: false,
		},
		{
			name: "valid OOMScoreAdj negative",
			cfg: Config{
				Name:        "test",
				WorkerFunc:  dummyWorker,
				OOMScoreAdj: intPtr(-500),
			},
			wantErr: false,
		},
		{
			name: "valid OOMScoreAdj max boundary",
			cfg: Config{
				Name:        "test",
				WorkerFunc:  dummyWorker,
				OOMScoreAdj: intPtr(1000),
			},
			wantErr: false,
		},
		{
			name: "valid OOMScoreAdj min boundary",
			cfg: Config{
				Name:        "test",
				WorkerFunc:  dummyWorker,
				OOMScoreAdj: intPtr(-1000),
			},
			wantErr: false,
		},
		{
			name: "negative MaxMemoryBytes",
			cfg: Config{
				Name:           "test",
				WorkerFunc:     dummyWorker,
				MaxMemoryBytes: -1,
			},
			wantErr: true,
		},
		{
			name: "OOMScoreAdj too high",
			cfg: Config{
				Name:        "test",
				WorkerFunc:  dummyWorker,
				OOMScoreAdj: intPtr(1001),
			},
			wantErr: true,
		},
		{
			name: "OOMScoreAdj too low",
			cfg: Config{
				Name:        "test",
				WorkerFunc:  dummyWorker,
				OOMScoreAdj: intPtr(-1001),
			},
			wantErr: true,
		},
		{
			name: "nil OOMScoreAdj is valid",
			cfg: Config{
				Name:        "test",
				WorkerFunc:  dummyWorker,
				OOMScoreAdj: nil,
			},
			wantErr: false,
		},
		{
			name: "valid both resource limits set",
			cfg: Config{
				Name:           "test",
				WorkerFunc:     dummyWorker,
				MaxMemoryBytes: 512 * 1024 * 1024,
				OOMScoreAdj:    intPtr(100),
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// mockSource implements UpdateSource for testing.
type mockSource struct{}

func (mockSource) CheckUpdate(_ context.Context, _ string) (*UpdateInfo, error) { return nil, nil }
func (mockSource) DownloadUpdate(_ context.Context, _ *UpdateInfo) (io.ReadCloser, []byte, error) {
	return nil, nil, nil
}

func TestUpdateRetryDefaultsAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		attempts  int
		base, max time.Duration
		invalid   bool
	}{
		{name: "defaults"},
		{name: "custom", attempts: 2, base: time.Second, max: time.Minute},
		{name: "negative attempts", attempts: -1, invalid: true},
		{name: "negative base", base: -time.Second, invalid: true},
		{name: "negative max", max: -time.Second, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := UpdateConfig{Source: &mockSource{}, PublicKey: make(ed25519.PublicKey, ed25519.PublicKeySize), RetryMaxAttempts: tc.attempts, RetryBaseBackoff: tc.base, RetryMaxBackoff: tc.max}
			cfg.setDefaults()
			if (cfg.Validate() != nil) != tc.invalid {
				t.Fatalf("Validate: %v", cfg.Validate())
			}
			if tc.name == "defaults" && (cfg.RetryMaxAttempts != 6 || cfg.RetryBaseBackoff != 30*time.Second || cfg.RetryMaxBackoff != 15*time.Minute || cfg.DisableRetryBudget) {
				t.Fatalf("defaults: %+v", cfg)
			}
			if tc.name == "custom" && (cfg.RetryMaxAttempts != tc.attempts || cfg.RetryBaseBackoff != tc.base || cfg.RetryMaxBackoff != tc.max) {
				t.Fatalf("overwrote settings: %+v", cfg)
			}
		})
	}
}

func TestValidateCgroupSettings(t *testing.T) {
	cfg := func() *Config {
		c := &Config{Name: "app", WorkerFunc: func(context.Context) error { return nil }}
		return c
	}
	if err := cfg().Validate(); err != nil {
		t.Fatalf("zero cgroup settings must validate: %v", err)
	}
	negative := cfg()
	negative.WorkerCgroupMemoryMax = -1
	err := negative.Validate()
	if err == nil || !strings.Contains(err.Error(), "WorkerCgroupMemoryMax") {
		t.Fatalf("negative memory limit must be rejected, got %v", err)
	}
	if runtime.GOOS != "linux" {
		nonLinux := cfg()
		nonLinux.WorkerCgroupMemoryMax = 64 << 20
		if err := nonLinux.Validate(); err == nil {
			t.Fatal("cgroup settings must be rejected off Linux")
		}
	}
}
