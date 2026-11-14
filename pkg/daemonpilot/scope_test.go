package daemonpilot

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

func TestInstallScopeString(t *testing.T) {
	tests := []struct {
		scope InstallScope
		want  string
	}{
		{ScopeSystem, "system"},
		{ScopeUser, "user"},
		{InstallScope(""), ""},
	}
	for _, tt := range tests {
		if got := tt.scope.String(); got != tt.want {
			t.Errorf("InstallScope(%q).String(): got %q, want %q", string(tt.scope), got, tt.want)
		}
	}
}

func TestDefaultInstallDir(t *testing.T) {
	tests := []struct {
		name  string
		goos  string
		scope InstallScope
		home  string
		want  string
	}{
		{"linux system", "linux", ScopeSystem, "/home/alice", "/opt/myapp"},
		{"linux system ignores home", "linux", ScopeSystem, "", "/opt/myapp"},
		{"darwin system", "darwin", ScopeSystem, "/Users/alice", "/usr/local/bin"},
		{"darwin system ignores home", "darwin", ScopeSystem, "", "/usr/local/bin"},
		{"freebsd system", "freebsd", ScopeSystem, "/home/alice", "/opt/myapp"},
		{"windows system", "windows", ScopeSystem, "", "/opt/myapp"},
		{"linux user", "linux", ScopeUser, "/home/alice", "/home/alice/.local/share/myapp"},
		{"darwin user", "darwin", ScopeUser, "/Users/alice", "/Users/alice/Library/Application Support/myapp"},
		{"freebsd user", "freebsd", ScopeUser, "/home/alice", "/home/alice/.local/share/myapp"},
		{"linux user empty home", "linux", ScopeUser, "", ".local/share/myapp"},
		{"darwin user empty home", "darwin", ScopeUser, "", "Library/Application Support/myapp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultInstallDir(tt.goos, "myapp", tt.scope, tt.home)
			if got != tt.want {
				t.Errorf("defaultInstallDir(%q, myapp, %q, %q): got %q, want %q",
					tt.goos, tt.scope, tt.home, got, tt.want)
			}
		})
	}
}

func TestDefaultLogDir(t *testing.T) {
	tests := []struct {
		name  string
		goos  string
		scope InstallScope
		home  string
		want  string
	}{
		{"linux system keeps manager constant", "linux", ScopeSystem, "/home/alice", ""},
		{"darwin system keeps manager constant", "darwin", ScopeSystem, "/Users/alice", ""},
		{"freebsd system keeps manager constant", "freebsd", ScopeSystem, "", ""},
		{"zero scope keeps manager constant", "linux", InstallScope(""), "/home/alice", ""},
		{"linux user", "linux", ScopeUser, "/home/alice", "/home/alice/.local/state/myapp"},
		{"darwin user", "darwin", ScopeUser, "/Users/alice", "/Users/alice/Library/Logs/myapp"},
		{"freebsd user", "freebsd", ScopeUser, "/home/alice", "/home/alice/.local/state/myapp"},
		{"linux user empty home", "linux", ScopeUser, "", ".local/state/myapp"},
		{"darwin user empty home", "darwin", ScopeUser, "", "Library/Logs/myapp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultLogDir(tt.goos, "myapp", tt.scope, tt.home)
			if got != tt.want {
				t.Errorf("defaultLogDir(%q, myapp, %q, %q): got %q, want %q",
					tt.goos, tt.scope, tt.home, got, tt.want)
			}
		})
	}
}

// System scope must resolve to an empty LogDir so every existing manifest keeps
// the location its manager has always used.
func TestConfigSetDefaults_SystemScopeLogDirStaysEmpty(t *testing.T) {
	cfg := Config{Name: "myapp"}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}
	if cfg.LogDir != "" {
		t.Errorf("LogDir: got %q, want empty", cfg.LogDir)
	}
}

func TestConfigSetDefaults_UserScopeLogDir(t *testing.T) {
	restore := userHomeDir
	userHomeDir = func() (string, error) { return "/home/alice", nil }
	t.Cleanup(func() { userHomeDir = restore })

	cfg := Config{Name: "myapp", Scope: ScopeUser}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}
	want := defaultLogDir(runtime.GOOS, "myapp", ScopeUser, "/home/alice")
	if cfg.LogDir != want {
		t.Errorf("LogDir: got %q, want %q", cfg.LogDir, want)
	}
}

func TestConfigSetDefaults_ExplicitLogDirOverridesDefault(t *testing.T) {
	tests := []struct {
		name  string
		scope InstallScope
	}{
		{"system scope", ScopeSystem},
		{"user scope", ScopeUser},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := userHomeDir
			userHomeDir = func() (string, error) { return "/home/alice", nil }
			t.Cleanup(func() { userHomeDir = restore })

			cfg := Config{Name: "myapp", Scope: tt.scope, LogDir: "/srv/logs/myapp"}
			if err := cfg.SetDefaults(); err != nil {
				t.Fatalf("SetDefaults: %v", err)
			}
			if cfg.LogDir != "/srv/logs/myapp" {
				t.Errorf("LogDir: got %q, want /srv/logs/myapp", cfg.LogDir)
			}
		})
	}
}

func TestBuildServiceConfig_CarriesLogDir(t *testing.T) {
	cfg := &Config{Name: "myapp", InstallDir: "/opt/myapp", LogDir: "/var/log/myapp"}
	if got := buildServiceConfig(cfg).LogDir; got != "/var/log/myapp" {
		t.Errorf("ServiceConfig.LogDir: got %q, want /var/log/myapp", got)
	}
}

func TestConfigSetDefaults_NormalizesZeroScopeToSystem(t *testing.T) {
	cfg := Config{Name: "myapp"}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}
	if cfg.Scope != ScopeSystem {
		t.Errorf("Scope: got %q, want %q", cfg.Scope, ScopeSystem)
	}
}

func TestConfigSetDefaults_PreservesExplicitScope(t *testing.T) {
	restore := userHomeDir
	userHomeDir = func() (string, error) { return "/home/alice", nil }
	t.Cleanup(func() { userHomeDir = restore })

	cfg := Config{Name: "myapp", Scope: ScopeUser}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}
	if cfg.Scope != ScopeUser {
		t.Errorf("Scope: got %q, want %q", cfg.Scope, ScopeUser)
	}
	want := defaultInstallDir(runtime.GOOS, "myapp", ScopeUser, "/home/alice")
	if cfg.InstallDir != want {
		t.Errorf("InstallDir: got %q, want %q", cfg.InstallDir, want)
	}
}

func TestConfigSetDefaults_UserScopeWithoutHome(t *testing.T) {
	tests := []struct {
		name string
		home func() (string, error)
	}{
		{"lookup fails", func() (string, error) { return "", errors.New("$HOME is not defined") }},
		{"empty home", func() (string, error) { return "", nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := userHomeDir
			userHomeDir = tt.home
			t.Cleanup(func() { userHomeDir = restore })

			cfg := Config{Name: "myapp", Scope: ScopeUser}
			err := cfg.SetDefaults()
			if !errors.Is(err, ErrNoHomeDir) {
				t.Fatalf("SetDefaults: got %v, want ErrNoHomeDir", err)
			}
		})
	}
}

// An explicit InstallDir does not excuse an unresolvable $HOME under user
// scope: the unit file is placed under $HOME regardless of where the binary
// lives, so accepting the config here would surface later as an empty manifest
// path recorded in the state file.
func TestConfigSetDefaults_UserScopeRequiresHomeEvenWithExplicitInstallDir(t *testing.T) {
	restore := userHomeDir
	userHomeDir = func() (string, error) { return "", errors.New("$HOME is not defined") }
	t.Cleanup(func() { userHomeDir = restore })

	cfg := Config{Name: "myapp", Scope: ScopeUser, InstallDir: "/srv/myapp"}
	err := cfg.SetDefaults()
	if !errors.Is(err, ErrNoHomeDir) {
		t.Fatalf("SetDefaults: got %v, want ErrNoHomeDir", err)
	}
}

// An explicit InstallDir is still honored when $HOME resolves.
func TestConfigSetDefaults_UserScopeKeepsExplicitInstallDir(t *testing.T) {
	restore := userHomeDir
	userHomeDir = func() (string, error) { return "/home/alice", nil }
	t.Cleanup(func() { userHomeDir = restore })

	cfg := Config{Name: "myapp", Scope: ScopeUser, InstallDir: "/srv/myapp"}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}
	if cfg.InstallDir != "/srv/myapp" {
		t.Errorf("InstallDir: got %q, want /srv/myapp", cfg.InstallDir)
	}
}

func TestConfigValidate_Scope(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"empty scope allowed", func(c *Config) { c.Scope = "" }, ""},
		{"system scope allowed", func(c *Config) { c.Scope = ScopeSystem }, ""},
		{"user scope allowed", func(c *Config) { c.Scope = ScopeUser }, ""},
		{"unknown scope rejected", func(c *Config) { c.Scope = InstallScope("auto") }, "Config.Scope"},
		{
			"user scope rejects ServiceUser",
			func(c *Config) { c.Scope = ScopeUser; c.ServiceUser = "svc" },
			"Config.ServiceUser",
		},
		{
			"user scope rejects ServiceGroup",
			func(c *Config) { c.Scope = ScopeUser; c.ServiceGroup = "svc" },
			"Config.ServiceGroup",
		},
		{
			"system scope allows ServiceUser",
			func(c *Config) { c.Scope = ScopeSystem; c.ServiceUser = "svc"; c.ServiceGroup = "svc" },
			"",
		},
		{
			"user scope rejects a raw User directive",
			func(c *Config) {
				c.Scope = ScopeUser
				c.SystemdDirectives = map[string]string{"User": "svc"}
			},
			"Config.SystemdDirectives sets User=",
		},
		{
			"user scope rejects a raw Group directive with surrounding whitespace",
			func(c *Config) {
				c.Scope = ScopeUser
				c.SystemdDirectives = map[string]string{"Group ": "svc"}
			},
			"Config.SystemdDirectives sets Group=",
		},
		{
			"user scope allows unrelated raw directives",
			func(c *Config) {
				c.Scope = ScopeUser
				c.SystemdDirectives = map[string]string{"user": "svc", "Nice": "5"}
			},
			"",
		},
		{
			"system scope allows a raw User directive",
			func(c *Config) {
				c.Scope = ScopeSystem
				c.SystemdDirectives = map[string]string{"User": "svc"}
			},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Name:       "myapp",
				WorkerFunc: func(ctx context.Context) error { return nil },
			}
			tt.mutate(&cfg)

			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: got %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate: got nil, want error containing %q", tt.wantErr)
			}
			if !contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate: got %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}
