package sandbox

// Sandbox provides an isolated execution environment.
type Sandbox interface {
	Setup(cfg SandboxConfig) error
	Shell(windowName string, cmd string) (string, error)
	Cleanup() error
}

// SandboxConfig holds configuration for sandbox setup.
type SandboxConfig struct {
	ProjectDir      string
	WorkspaceDirs   map[string]string // project-id -> dir (readonly mounts)
	TaskFile        string
	BuiltinCommands []string
	HostCommands    []string
	Bindings        []string
	Env             map[string]string
	BoidBinary      string
	BrokerSocket    string
	ServerSocket    string
}
