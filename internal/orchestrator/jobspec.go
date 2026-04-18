package orchestrator

// JobSpec is the orchestrator-owned, sandbox-agnostic execution request.
// dispatcher translates this into a concrete sandbox.Spec; layers below
// dispatcher must not see this type directly.
type JobSpec struct {
	TaskID             string
	ProjectID          string
	WorkspaceID        string
	HandlerID          string
	Role               Role
	ProjectDir         string
	HomeDir            string
	HookFiles          []HookFile
	GatesDir           string
	ProjectGatesDir    string         // for dispatcher-side gate staging (gate role)
	KitGatesDirs       []KitGatesInfo // for dispatcher-side gate staging (gate role)
	HookScript         string
	BoidBinary         string
	ServerSocket       string
	Env                map[string]string
	BuiltinPolicies    map[string]BuiltinPolicy
	HostCommands       map[string]CommandDef
	AdditionalBindings []BindMount
	WorkspaceDirs      map[string]string
	ProxyPort          int
	StagingDir         string
	WorktreeDir        string
	PayloadJSON        string
	TaskJSON           string
	Readonly           bool
	// Interactive controls payload delivery and PTY allocation for hook dispatch.
	// When true: PayloadJSON is materialized as a context file under
	// $HOME/.boid/context/payload.json (instead of being fed to stdin), the
	// BOID_INTERACTIVE=1 env var is exported, and the sandbox is launched with
	// a PTY. When false: PayloadJSON is fed to stdin and a PTY is still
	// allocated for hook/gate roles because the agent process expects one.
	// (dispatcher computes the final TTY value from this + Role.)
	Interactive      bool
	InstructionsJSON string
	SecretNamespace  string
	TaskYAML         string // serialized task metadata for context/task.yaml
	EnvironmentYAML  string // serialized sandbox environment for context/environment.yaml
	Model            string // AI model to use (from instruction's model field)
	InvokedRole      string // instruction map key name (e.g. "main", "executor", "reviewer_security")
	InvokedName      string // instruction.Name value (e.g. "security", "performance"; empty if unset)
	InvokedType      string // instruction.Type value (e.g. "execution", "rework", "verification")
}
