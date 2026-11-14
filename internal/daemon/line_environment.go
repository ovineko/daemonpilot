package daemon

import (
	"fmt"
	"sort"
	"strings"
)

func formatLineEnvironment(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("%s=%s", k, env[k]))
	}
	return strings.Join(lines, "\n") + "\n"
}

func validateLineEnvironment(env map[string]string) error {
	if err := validateEnvKeys(env); err != nil {
		return err
	}
	for key, value := range env {
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("environment variable %s contains a line break or NUL", key)
		}
	}
	return nil
}
