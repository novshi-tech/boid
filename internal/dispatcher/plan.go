package dispatcher

// BindMount describes a host path to bind-mount into the sandbox.
type BindMount struct {
	Source string
	Mode   string
}

// CommandDef defines a host command that can be executed via the broker.
type CommandDef struct {
	Name                string
	Path                string
	AllowedPatterns     []string
	DeniedPatterns      []string
	AllowedSubcommands  []string
	AllowStdin          bool
	Env                 map[string]string
	ExtractSubcommandFn string
	RequireCwd          bool
	AllowedCwdPrefixes  []string
}

// DispatchPlan is the fully resolved execution plan consumed by the runner.
type DispatchPlan struct {
	TaskID             string
	ProjectID          string
	HandlerID          string
	Role               string
	ProjectDir         string
	HomeDir            string
	HooksDir           string
	HookScript         string
	BoidBinary         string
	ServerSocket       string
	Env                map[string]string
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
