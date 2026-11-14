package daemonpilot

import (
	"slices"
	"strings"
	"testing"
)

// An application whose CLI already owns "run" and "worker" puts daemonpilot's
// verbs behind a prefix, and every path that executes the binary again has to
// carry it — the service manifest most of all, since a manifest that runs the
// application's own "run" would start something else entirely.
func TestCommandPrefixReachesTheServiceManifest(t *testing.T) {
	tests := []struct {
		name      string
		prefix    []string
		arguments []string
		want      []string
	}{
		{name: "no prefix is the verb alone", want: []string{"run"}},
		{
			name:   "one word",
			prefix: []string{"daemon"},
			want:   []string{"daemon", "run"},
		},
		{
			name:      "prefix comes before the verb, arguments after it",
			prefix:    []string{"daemon", "service"},
			arguments: []string{"--verbose"},
			want:      []string{"daemon", "service", "run", "--verbose"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Name:             "myapp",
				CommandPrefix:    tt.prefix,
				ServiceArguments: tt.arguments,
			}

			if got := buildServiceConfig(cfg).Arguments; !slices.Equal(got, tt.want) {
				t.Fatalf("manifest arguments = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDaemonpilotArgs(t *testing.T) {
	prefix := []string{"daemon", "service"}

	got := DaemonpilotArgs(prefix, "run", "--exec-through")
	want := []string{"daemon", "service", "run", "--exec-through"}

	if !slices.Equal(got, want) {
		t.Fatalf("DaemonpilotArgs() = %v, want %v", got, want)
	}

	// The caller's slice is never written through: two spawns building on one
	// prefix would otherwise overwrite each other's verbs.
	if !slices.Equal(prefix, []string{"daemon", "service"}) {
		t.Fatalf("DaemonpilotArgs() wrote into the prefix: %v", prefix)
	}
}

func TestValidateRejectsAnUnusablePrefix(t *testing.T) {
	tests := []struct {
		name   string
		prefix []string
		want   string
	}{
		{name: "NUL", prefix: []string{"daemon\x00"}, want: "contains NUL"},
		{name: "an empty word", prefix: []string{"daemon", ""}, want: "single word"},
		{name: "surrounding space", prefix: []string{" daemon"}, want: "single word"},
		{
			// A flag before the verb would be read by the application's own
			// parser, not by daemonpilot.
			name: "a flag", prefix: []string{"--daemon"}, want: "not a flag",
		},
		{
			// cron splits on newlines long before any quoting applies.
			name: "a newline", prefix: []string{"daemon\nrm -rf /"}, want: "CommandPrefix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Name: "myapp", CommandPrefix: tt.prefix, WorkerFunc: dummyWorker}

			err := cfg.Validate()
			if err == nil {
				err = cfg.validateCrontabFields()
			}

			if err == nil {
				t.Fatalf("Validate() accepted %q", tt.prefix)
			}

			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want %q in it", err, tt.want)
			}
		})
	}
}
