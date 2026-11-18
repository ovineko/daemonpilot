package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// This file is deliberately NOT build-tagged, unlike windows.go beside it. A
// Windows service has no manifest FILE — its definition lives in the SCM database
// — so the parts that can be got wrong are the rendering, the parsing and above
// all the command-line quoting, none of which need Windows to run. Keeping them
// here is what lets them be tested on any host; windows.go is then a thin layer of
// SCM calls that cannot be tested without Windows anyway.

// windowsManifestSuffix names the file that records what we registered. The SCM is
// the source of truth for the RUNNING service; this file is what daemonpilot's
// install/rollback/digest machinery reads, exactly as it reads a systemd unit.
const windowsManifestSuffix = "-service.conf"

// windowsServiceDef is the service definition, in the shape the SCM needs it.
type windowsServiceDef struct {
	Name        string
	DisplayName string
	Description string
	BinaryPath  string // fully quoted command line: exe + arguments
	// ImagePath is the executable ALONE, unquoted. It is carried separately
	// rather than re-parsed out of BinaryPath so the registration can STAT it.
	// The SCM reports a missing image only when the service is STARTED, and the
	// install path downgrades a failed start to a warning -- so without this a
	// service registered against a file that does not exist reads as a
	// successful install, and the node sits at "pending" with no heartbeat and
	// nothing anywhere saying why. That is not hypothetical: it is exactly what
	// the first Windows enrollment did.
	ImagePath string
	// Arguments are the service's own arguments, unquoted and in order.
	//
	// They are carried SEPARATELY because the two x/sys calls that write a
	// service take the command line in DIFFERENT shapes, and getting that
	// backwards is invisible until the service is started:
	//
	//   mgr.CreateService(name, exepath, cfg, args...) IGNORES cfg.BinaryPathName
	//   entirely and builds the stored path itself as
	//   EscapeArg(exepath) + " " + EscapeArg(arg)... — so handing it an
	//   already-quoted command line escapes the WHOLE string into one token, and
	//   the SCM then looks for a file whose name contains quotes and a space.
	//
	//   Service.UpdateConfig(cfg) uses cfg.BinaryPathName VERBATIM, as the full
	//   command line.
	//
	// Registration succeeds either way. The only symptom is "The system cannot
	// find the file specified" at start — which reads as a missing binary, and
	// sends you to look at a file that is sitting exactly where it should be.
	Arguments []string
}

// GenerateWindowsServiceConfig renders the manifest recording what will be (or
// was) registered with the Service Control Manager.
//
// Environment is REFUSED rather than dropped. A Windows service takes no
// environment through CreateService, and the registry mechanisms that look like
// they would are either driver-only or undocumented for this case — so the honest
// options are "refuse" and "accept it and silently run without it". The second
// produces a service that installs green and behaves as if the operator had never
// set the variable, which is the exact class of silent failure this project spends
// its comments on. A caller that needs configuration on Windows passes arguments or
// writes a config file.
func GenerateWindowsServiceConfig(config ServiceConfig, execPath string) (string, error) {
	if err := validateServiceText(config, execPath, "windows"); err != nil {
		return "", err
	}
	if config.Name == "" {
		return "", errors.New("windows: service name is required")
	}
	if len(config.Environment) > 0 {
		keys := make([]string, 0, len(config.Environment))
		for k := range config.Environment {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return "", fmt.Errorf(
			"windows: per-service environment is not supported by the Service Control Manager (refusing to drop %s silently); pass arguments or a config file instead",
			strings.Join(keys, ", "))
	}
	if config.User != "" || config.Group != "" {
		return "", errors.New("windows: User/Group are POSIX identities; a Windows service runs as LocalSystem (refusing rather than ignoring them)")
	}

	def := windowsServiceDef{
		Name:        config.Name,
		DisplayName: config.Name,
		Description: config.Description,
		BinaryPath:  WindowsCommandLine(execPath, config.Arguments),
		ImagePath:   execPath,
		Arguments:   append([]string(nil), config.Arguments...),
	}

	var b strings.Builder
	// A deterministic, order-stable rendering: it is hashed for the rollback
	// digest, so a map iteration or a reordered field would report drift on every
	// reconcile of an unchanged service.
	b.WriteString("# daemonpilot windows service definition\n")
	fmt.Fprintf(&b, "Name=%s\n", def.Name)
	fmt.Fprintf(&b, "DisplayName=%s\n", def.DisplayName)
	fmt.Fprintf(&b, "Description=%s\n", def.Description)
	fmt.Fprintf(&b, "BinaryPath=%s\n", def.BinaryPath)
	fmt.Fprintf(&b, "ImagePath=%s\n", def.ImagePath)
	// One line per argument, in order: an argument may contain spaces, and a
	// single joined line would need quoting rules that the parser would then have
	// to undo — which is the very mistake this field exists to avoid.
	for _, arg := range def.Arguments {
		fmt.Fprintf(&b, "Argument=%s\n", arg)
	}
	return b.String(), nil
}

// parseWindowsServiceConfig reads back what GenerateWindowsServiceConfig wrote.
// Unknown keys are ignored so a manifest written by a newer version still applies;
// a missing Name or BinaryPath is an error, because registering a service with
// either one empty is not a smaller registration but a broken one.
func parseWindowsServiceConfig(content string) (windowsServiceDef, error) {
	var def windowsServiceDef
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Name":
			def.Name = value
		case "DisplayName":
			def.DisplayName = value
		case "Description":
			def.Description = value
		case "BinaryPath":
			def.BinaryPath = value
		case "ImagePath":
			def.ImagePath = value
		case "Argument":
			def.Arguments = append(def.Arguments, value)
		}
	}
	if def.Name == "" {
		return windowsServiceDef{}, errors.New("windows: manifest has no Name")
	}
	if def.BinaryPath == "" {
		return windowsServiceDef{}, errors.New("windows: manifest has no BinaryPath")
	}
	if def.DisplayName == "" {
		def.DisplayName = def.Name
	}
	return def, nil
}

func updateWindowsServiceImagePath(content, imagePath string) (string, error) {
	def, err := parseWindowsServiceConfig(content)
	if err != nil {
		return "", err
	}
	config := ServiceConfig{Name: def.Name, Description: def.Description, Arguments: def.Arguments}
	if err := validateServiceText(config, imagePath, "windows"); err != nil {
		return "", err
	}
	binaryPath := WindowsCommandLine(imagePath, def.Arguments)
	lines := strings.SplitAfter(content, "\n")
	imagePathSeen, binaryPathSeen := false, false
	for i, line := range lines {
		text := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		ending := line[len(text):]
		switch {
		case strings.HasPrefix(text, "ImagePath="):
			if imagePathSeen {
				return "", errors.New("windows: manifest has duplicate ImagePath fields")
			}
			imagePathSeen = true
			lines[i] = "ImagePath=" + imagePath + ending
		case strings.HasPrefix(text, "BinaryPath="):
			if binaryPathSeen {
				return "", errors.New("windows: manifest has duplicate BinaryPath fields")
			}
			binaryPathSeen = true
			lines[i] = "BinaryPath=" + binaryPath + ending
		}
	}
	if !binaryPathSeen {
		return "", errors.New("windows: manifest has no BinaryPath")
	}
	if !imagePathSeen {
		if len(lines) > 0 && !strings.HasSuffix(lines[len(lines)-1], "\n") {
			lines[len(lines)-1] += "\n"
		}
		lines = append(lines, "ImagePath="+imagePath+"\n")
	}
	return strings.Join(lines, ""), nil
}

// WindowsCommandLine builds the single string the SCM stores as a service's binary
// path, quoted so CommandLineToArgvW — which is what the started process parses it
// back with — recovers exactly exe and args.
//
// The unquoted path is the classic Windows service vulnerability AND an ordinary
// bug: `C:\Program Files\x\a.exe` unquoted is offered to the loader as
// `C:\Program.exe` with `Files\x\a.exe` as an argument, so the service either fails
// to start or starts something else entirely.
func WindowsCommandLine(exe string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, windowsQuoteArg(exe))
	for _, a := range args {
		parts = append(parts, windowsQuoteArg(a))
	}
	return strings.Join(parts, " ")
}

// windowsQuoteArg quotes one argument per the CommandLineToArgvW rules: a run of
// backslashes is doubled only when it precedes a quote (or the closing quote), and
// an embedded quote is escaped with a backslash. Halving the backslashes of an
// ordinary path — the mistake the naive `strings.ReplaceAll` version makes — turns
// `C:\dir\` into `C:\dir` and every path below it into a different one.
func windowsQuoteArg(arg string) string {
	if arg == "" {
		return `""`
	}
	if !strings.ContainsAny(arg, " \t\n\v\"") {
		return arg
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := range len(arg) {
		switch c := arg[i]; c {
		case '\\':
			slashes++
		case '"':
			// Escape the run of backslashes, then the quote itself.
			b.WriteString(strings.Repeat(`\`, slashes*2+1))
			slashes = 0
			b.WriteByte('"')
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			slashes = 0
			b.WriteByte(c)
		}
	}
	// Backslashes immediately before the closing quote must be doubled, or they
	// would escape it.
	b.WriteString(strings.Repeat(`\`, slashes*2))
	b.WriteByte('"')
	return b.String()
}

// windowsManifestPath is where the definition file lives: beside the binary, so an
// install that is wiped by deleting its directory leaves nothing behind.
func windowsManifestPath(installDir, name string) string {
	if installDir == "" {
		installDir = filepath.Join(`C:\ProgramData`, name)
	}
	return filepath.Join(installDir, name+windowsManifestSuffix)
}

// windowsExecPath resolves the service binary, appending the `.exe` Windows needs:
// CreateProcess does not add it to an absolute path, so an extensionless
// BinaryPathName is a service that can never start.
func windowsExecPath(config ServiceConfig) string {
	path := config.ExecPath
	if path == "" {
		installDir := config.InstallDir
		if installDir == "" {
			installDir = filepath.Join(`C:\ProgramData`, config.Name)
		}
		path = filepath.Join(installDir, config.Name)
	}
	if !strings.EqualFold(filepath.Ext(path), ".exe") {
		path += ".exe"
	}
	return path
}

// windowsInitSystemName is the manager's Name(), the detector's name and the value
// persisted in install state — one spelling, because NewServiceManagerByName looks
// a manager up by exactly the string that was recorded.
const windowsInitSystemName = "windows-service"
