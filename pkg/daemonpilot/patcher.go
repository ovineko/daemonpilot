package daemonpilot

// PatchInfo provides context about the service and platform to config patchers.
type PatchInfo struct {
	OS          string
	Arch        string
	InitSystem  string
	ServiceName string
	FilePath    string
}

// ConfigPatcher is a function that modifies generated init-system configuration.
// It receives the config file path, the generated content, and platform/service
// context via PatchInfo. It returns the modified content or an error.
// Patchers are applied in order after config generation, before the file is written.
type ConfigPatcher func(path string, content string, info PatchInfo) (string, error)
