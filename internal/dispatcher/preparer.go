package dispatcher

import "github.com/novshi-tech/boid/internal/sandbox"

// HookFile describes a single hook file to bind-mount into the sandbox.
type HookFile struct {
	Source     string // host-side absolute path
	TargetName string // filename inside sandbox .boid/hooks/
}

// SandboxSpec is the dispatcher-owned execution spec required to prepare a sandbox launch.
type SandboxSpec struct {
	JobID              string
	TaskID             string
	ProjectID          string
	ProjectDir         string
	HomeDir            string
	HookFiles          []HookFile
	GatesDir           string
	HookScript         string
	Command            string
	BoidBinary         string
	ServerSocket       string
	BrokerSocket       string
	BrokerToken        string
	Env                map[string]string
	BuiltinPolicies    map[string]sandbox.BuiltinPolicy
	HostCommands       []string
	AdditionalBindings []BindMount
	WorkspaceDirs      map[string]string
	ProxyPort          int
	StagingDir         string
	TTY                bool
	Interactive        bool
	WorktreeDir        string
	Role               string
	PayloadJSON        string
	TaskJSON           string
	Readonly           bool
	InstructionsJSON   string
	TaskYAML           string
	EnvironmentYAML    string
	Model              string
	InvokedRole        string // instruction map key name
	InvokedName        string // instruction.Name value (empty if unset)
	InvokedType        string // instruction.Type value
}

// PreparedSandbox is the concrete launch artifact returned by a provider.
type PreparedSandbox struct {
	OuterPath string
}

// SandboxPreparer prepares concrete launch artifacts from the dispatcher-owned sandbox spec.
type SandboxPreparer interface {
	PrepareSandbox(spec SandboxSpec) (*PreparedSandbox, error)
}
