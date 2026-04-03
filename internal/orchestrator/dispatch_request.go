package orchestrator

// DispatchRequest is the orchestrator-owned execution request model.
// Concrete dispatcher plans are derived from this at the boundary adapter.
type DispatchRequest struct {
	TaskID             string
	ProjectID          string
	WorkspaceID        string
	HandlerID          string
	Role               Role
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
	InstructionsJSON   string
	SecretNamespace    string
	TaskYAML           string // serialized task metadata for context/task.yaml
	EnvironmentYAML    string // serialized sandbox environment for context/environment.yaml
}
