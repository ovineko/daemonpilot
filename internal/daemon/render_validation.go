package daemon

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

func validateServiceText(config ServiceConfig, execPath, backend string) error {
	if err := validateEnvKeys(config.Environment); err != nil {
		return fmt.Errorf("%s: %w", backend, err)
	}
	if config.Name != "" && !manifestNameRE.MatchString(config.Name) {
		return fmt.Errorf("%s: invalid service name", backend)
	}
	values := []string{execPath, config.InstallDir, config.WorkingDir, config.User, config.Group, config.LogDir, config.Description}
	values = append(values, config.Arguments...)
	for _, value := range config.Environment {
		values = append(values, value)
	}
	for _, value := range values {
		if err := validateManifestValue(value, backend); err != nil {
			return err
		}
	}
	if backend == "systemd" {
		for _, value := range []string{config.WorkingDir, config.Description, config.User, config.Group} {
			if strings.ContainsAny(value, "\n\r") || strings.TrimSpace(value) != value || strings.HasSuffix(value, `\`) {
				return errors.New("systemd: setting cannot be represented without changing its value")
			}
		}
	}
	if backend == "sysvinit" && strings.ContainsAny(config.Description, "\r\n") {
		return errors.New("sysvinit: description contains a line break")
	}
	return nil
}

var manifestNameRE = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$`)

func validateManifestValue(value, backend string) error {
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s: NUL cannot be represented", backend)
	}
	switch backend {
	case "launchd":
		if !utf8.ValidString(value) {
			return errors.New("launchd: invalid UTF-8")
		}
		for _, r := range value {
			if r < 32 && r != '\n' && r != '\r' && r != '\t' || r == 0xfffe || r == 0xffff {
				return errors.New("launchd: invalid XML character")
			}
		}
	case "windows", "systemd":
		if !utf8.ValidString(value) {
			return fmt.Errorf("%s: invalid UTF-8", backend)
		}
	}
	if backend == "dinit" || backend == "finit" || backend == "windows" {
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%s: line breaks cannot be represented", backend)
		}
	}
	return nil
}

// Procd lacks a working-directory setting; exec preserves the service PID.
func workingDirectoryArgs(config ServiceConfig, execPath string) []string {
	args := make([]string, 0, 4+len(config.Arguments))
	args = append(args, "-c", "cd "+shellQuote(config.WorkingDir)+" && exec \"$@\"", "daemonpilot", execPath)
	return append(args, config.Arguments...)
}

// Finit splits on spaces before reconstructing quoted arguments and wordexp.
// Its reconstruction loses repeated spaces and mistakes embedded quotes for boundaries.
func validateFinitCommand(config ServiceConfig, execPath string) error {
	if !strings.HasPrefix(execPath, "/") || !shellSafeRe.MatchString(execPath) || strings.HasSuffix(execPath, "--") {
		return errors.New("finit: executable path requires an absolute, unquoted, shell-safe token")
	}
	if config.WorkingDir != "" && !strings.HasPrefix(config.WorkingDir, "/") {
		return errors.New("finit: working directory must be absolute")
	}
	values := append([]string{config.WorkingDir}, config.Arguments...)
	for _, value := range values {
		if strings.ContainsAny(value, "\t\v\f#") || strings.Contains(value, "-- ") {
			return errors.New("finit: command value contains unsupported whitespace, comment or description syntax")
		}
		if strings.Contains(value, " ") && (strings.TrimSpace(value) != value || strings.Contains(value, "  ") || strings.ContainsAny(value, "'\"\\")) {
			return errors.New("finit: command value cannot preserve spaces combined with quotes, escapes or adjacent spaces")
		}
	}
	if strings.ContainsAny(config.Description, "#\\") {
		return errors.New("finit: description contains comment or continuation syntax")
	}
	return nil
}
