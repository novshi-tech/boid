package dispatcher

// BindMount is a plain shared DTO at the dispatcher boundary.
// It carries only mount source/mode data and does not encode provider behavior.
type BindMount struct {
	Source string
	Mode   string
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
	HooksDir           string
	GatesDir           string
	HookScript         string
	BoidBinary         string
	ServerSocket       string
	Env                map[string]string
	BuiltinCommands    []string
	HostCommands       map[string]CommandDef
	AdditionalBindings []BindMount
	WorkspaceDirs      map[string]string
	ProxyPort          int
	StagingDir         string
	WorktreeDir        string
	PayloadJSON        string
	TaskJSON           string
	Readonly           bool
}
