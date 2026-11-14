package daemon

import (
	"strings"
	"testing"
)

func FuzzCrontabEntries(f *testing.F) {
	f.Fuzz(func(t *testing.T, input string) {
		kept, found := ParseCrontabEntries(input, "fuzz")
		again, second := ParseCrontabEntries(kept, "fuzz")
		if second || again != kept {
			t.Fatal("removal is not idempotent")
		}
		if !found && kept != strings.TrimSpace(input) { // Parser normalizes trailing newlines only.
			if strings.TrimSpace(kept) != strings.TrimSpace(input) {
				t.Fatal("unrelated crontab changed")
			}
		}
		command, err := extractCrontabCommand(input)
		if err == nil {
			prefix := strings.TrimSpace(strings.TrimSuffix(input, command))
			if command == "" || ValidateCrontabSchedule(prefix) != nil || strings.ContainsAny(input, "\r\n\x00") {
				t.Fatalf("garbage crontab accepted: %q", input)
			}
		}
	})
}
