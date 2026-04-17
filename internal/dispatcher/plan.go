package dispatcher

import "github.com/novshi-tech/boid/internal/sandbox"

// BindMount is a plain shared DTO at the dispatcher boundary.
// It carries only mount source/mode data and does not encode provider behavior.
type BindMount struct {
	Source string
	Mode   string
}

// KitGatesSource is per-kit gate scripts directory carried through the dispatch
// plan. Dispatcher uses it to stage gate scripts under a per-job unique path.
type KitGatesSource struct {
	GatesDir string
}

// CommandDef is a dispatcher-side transport shape for sandbox command policy input.
// sandbox remains the canonical owner of how these policy fields are interpreted.
type CommandDef struct {
	Name               string
	Path               string
	AllowedPatterns    []string
	DeniedPatterns     []string
	AllowedSubcommands []string
	AllowStdin         bool
	Env                map[string]string
}

// DispatchPlan is the fully resolved execution plan consumed by the runner.
type DispatchPlan struct {
	TaskID             string
	ProjectID          string
	WorkspaceID        string
	HandlerID          string
	Role               string
	ProjectDir         string
	HomeDir            string
	HookFiles          []HookFile
	GatesDir           string
	ProjectGatesDir    string           // source dir for project-side gate scripts (dispatcher-side staging)
	KitGatesDirs       []KitGatesSource // per-kit gate script source dirs (dispatcher-side staging)
	HookScript         string
	BoidBinary         string
	ServerSocket       string
	Env                map[string]string
	BuiltinPolicies    map[string]sandbox.BuiltinPolicy
	HostCommands       map[string]CommandDef
	AdditionalBindings []BindMount
	WorkspaceDirs      map[string]string
	ProxyPort          int
	StagingDir         string
	WorktreeDir        string
	PayloadJSON        string
	TaskJSON           string
	Readonly           bool
	Interactive        bool
	InstructionsJSON   string
	SecretNamespace    string
	TaskYAML           string
	EnvironmentYAML    string
	Model              string
	InvokedRole        string // instruction map key name
	InvokedName        string // instruction.Name value (empty if unset)
	InvokedType        string // instruction.Type value
}
