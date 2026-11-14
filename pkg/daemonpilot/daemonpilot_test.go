package daemonpilot

import (
	"context"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	s, err := New(Config{
		Name:       "testapp",
		WorkerFunc: func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("New() returned nil")
	}
	if s.config.Name != "testapp" {
		t.Errorf("config.Name = %q, want %q", s.config.Name, "testapp")
	}
}

func TestNewSetsDefaults(t *testing.T) {
	s, err := New(Config{
		Name:       "testapp",
		WorkerFunc: func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	if s.config.GracefulTimeout == 0 {
		t.Error("expected GracefulTimeout to be set by defaults")
	}
	if s.config.Logger == nil {
		t.Error("expected Logger to be set by defaults")
	}
	if s.config.Version == "" {
		t.Error("expected Version to be set by defaults")
	}
}

func TestNewValidationError_MissingName(t *testing.T) {
	_, err := New(Config{
		WorkerFunc: func(context.Context) error { return nil },
	})
	if err == nil {
		t.Fatal("New() expected error for missing Name")
	}
	if !strings.Contains(err.Error(), "invalid config") {
		t.Errorf("error = %q, want it to contain 'invalid config'", err.Error())
	}
}

func TestNewValidationError_MissingWorkerFunc(t *testing.T) {
	_, err := New(Config{
		Name: "testapp",
	})
	if err == nil {
		t.Fatal("New() expected error for missing WorkerFunc")
	}
	if !strings.Contains(err.Error(), "invalid config") {
		t.Errorf("error = %q, want it to contain 'invalid config'", err.Error())
	}
}

func TestRunWithCommand(t *testing.T) {
	called := false
	s, err := New(Config{
		Name: "testapp",
		WorkerFunc: func(context.Context) error {
			called = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	orig := osArgs
	osArgs = func() []string { return []string{"testapp", "worker"} }
	defer func() { osArgs = orig }()

	origSetup := setupSignalHandler
	setupSignalHandler = func(ctx context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(ctx)
		return ctx, cancel
	}
	defer func() { setupSignalHandler = origSetup }()

	err = s.Run()
	if err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}
	if !called {
		t.Error("expected WorkerFunc to be called")
	}
}

func TestRunInvalidCommand(t *testing.T) {
	s, err := New(Config{
		Name:       "testapp",
		WorkerFunc: func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	orig := osArgs
	osArgs = func() []string { return []string{"testapp", "bogus"} }
	defer func() { osArgs = orig }()

	err = s.Run()
	if err == nil {
		t.Fatal("Run() expected error for invalid command")
	}
	if !strings.Contains(err.Error(), "command error") {
		t.Errorf("error = %q, want it to contain 'command error'", err.Error())
	}
}

func TestRunNoArgs(t *testing.T) {
	s, err := New(Config{
		Name:       "testapp",
		WorkerFunc: func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	orig := osArgs
	osArgs = func() []string { return []string{"testapp"} }
	defer func() { osArgs = orig }()

	err = s.Run()
	if err == nil {
		t.Fatal("Run() expected error for no args")
	}
}

func TestRunVersionCommand(t *testing.T) {
	s, err := New(Config{
		Name:       "testapp",
		WorkerFunc: func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	orig := osArgs
	osArgs = func() []string { return []string{"testapp", "version"} }
	defer func() { osArgs = orig }()

	err = s.Run()
	if err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}
}

func TestRunWatchdogVersionCommand(t *testing.T) {
	s, err := New(Config{
		Name:       "testapp",
		WorkerFunc: func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	orig := osArgs
	osArgs = func() []string { return []string{"testapp", "watchdog-version"} }
	defer func() { osArgs = orig }()

	err = s.Run()
	if err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}
}

func TestRunUpdateConfigCommand(t *testing.T) {
	s, err := New(Config{
		Name:       "testapp",
		WorkerFunc: func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	orig := osArgs
	osArgs = func() []string { return []string{"testapp", "update-config"} }
	defer func() { osArgs = orig }()

	origLoadState := loadStateFn
	loadStateFn = func(_, _ string) (*StateData, error) {
		return nil, nil
	}
	defer func() { loadStateFn = origLoadState }()

	err = s.Run()
	if err == nil {
		t.Fatal("Run() expected error for update-config with no state")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("error = %q, want it to contain 'not installed'", err.Error())
	}
}

func TestRunSetsExecThroughFromFlag(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		envVal string
		envSet bool
		want   bool
	}{
		{
			name: "no flag",
			args: []string{"testapp", "run"},
			want: false,
		},
		{
			name: "with flag",
			args: []string{"testapp", "run", "--exec-through"},
			want: true,
		},
		{
			name:   "with env var",
			args:   []string{"testapp", "run"},
			envVal: "1",
			envSet: true,
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origArgs := osArgs
			osArgs = func() []string { return tt.args }
			defer func() { osArgs = origArgs }()

			origLookupEnv := lookupEnv
			lookupEnv = func(key string) (string, bool) {
				if key == "DAEMONPILOT_EXEC_THROUGH" {
					return tt.envVal, tt.envSet
				}
				return "", false
			}
			defer func() { lookupEnv = origLookupEnv }()

			origSetup := setupSignalHandler
			setupSignalHandler = context.WithCancel
			defer func() { setupSignalHandler = origSetup }()

			var capturedExecThrough bool
			origWatchdogFn := newWatchdogFn
			newWatchdogFn = func(cfg *Config) Runner {
				capturedExecThrough = cfg.ExecThrough
				return &mockRunner{}
			}
			defer func() { newWatchdogFn = origWatchdogFn }()

			s, err := New(Config{
				Name:       "testapp",
				WorkerFunc: func(context.Context) error { return nil },
			})
			if err != nil {
				t.Fatalf("New() unexpected error: %v", err)
			}

			_ = s.Run()
			if capturedExecThrough != tt.want {
				t.Errorf("ExecThrough = %v, want %v", capturedExecThrough, tt.want)
			}
		})
	}
}

func TestRunSelftestCommand(t *testing.T) {
	s, err := New(Config{
		Name:       "testapp",
		WorkerFunc: func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	orig := osArgs
	osArgs = func() []string { return []string{"testapp", "selftest"} }
	defer func() { osArgs = orig }()

	err = s.Run()
	if err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}
}

func TestRunModeUsesExplicitArguments(t *testing.T) {
	original := osArgs
	osArgs = func() []string { t.Fatal("RunMode read os.Args"); return nil }
	defer func() { osArgs = original }()
	t.Setenv(EnvDisableParentMonitor, "1")
	t.Setenv("DAEMONPILOT_EXEC_THROUGH", "")
	for _, tc := range []struct {
		mode    string
		args    []string
		wantErr bool
		worker  bool
	}{
		{mode: "worker", worker: true},
		{mode: "version"},
		{mode: "watchdog-version"},
		{mode: "selftest"},
		{mode: "unknown", wantErr: true},
		{mode: "", wantErr: true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			called := false
			s, err := New(Config{Name: "runmode-test", InstallDir: t.TempDir(), WorkerFunc: func(context.Context) error { called = true; return nil }})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RunMode(tc.mode, tc.args...); (err != nil) != tc.wantErr {
				t.Fatalf("RunMode: %v", err)
			}
			if called != tc.worker {
				t.Fatalf("worker called: %v", called)
			}
		})
	}
	for _, flag := range []bool{false, true} {
		s, err := New(Config{Name: "runmode-test", InstallDir: t.TempDir(), WorkerFunc: func(context.Context) error { return nil }})
		if err != nil {
			t.Fatal(err)
		}
		originalFactory := newWatchdogFn
		newWatchdogFn = func(cfg *Config) Runner {
			if cfg.ExecThrough != flag {
				t.Errorf("ExecThrough = %v, want %v", cfg.ExecThrough, flag)
			}
			return &mockRunner{}
		}
		args := []string{}
		if flag {
			args = append(args, "--exec-through")
		}
		err = s.RunMode("run", args...)
		newWatchdogFn = originalFactory
		if err != nil {
			t.Fatal(err)
		}
	}
}
