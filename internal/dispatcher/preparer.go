package dispatcher

// SandboxSpec is the dispatcher-owned execution spec required to prepare a sandbox launch.
type SandboxSpec struct {
	JobID              string
	TaskID             string
	ProjectID          string
	ProjectDir         string
	HomeDir            string
	HooksDir           string
	GatesDir           string
	HookScript         string
	Command            string
	BoidBinary         string
	ServerSocket       string
	BrokerSocket       string
	BrokerToken        string
	Env                map[string]string
	BuiltinCommands    []string
	HostCommands       []string
	AdditionalBindings []BindMount
	WorkspaceDirs      map[string]string
	ProxyPort          int
	StagingDir         string
	TTY                bool
	WorktreeDir        string
	Role               string
	PayloadJSON        string
	TaskJSON           string
	Readonly           bool
}

// PreparedSandbox is the concrete launch artifact returned by a provider.
type PreparedSandbox struct {
	OuterPath string
}

// SandboxPreparer prepares concrete launch artifacts from the dispatcher-owned sandbox spec.
type SandboxPreparer interface {
	PrepareSandbox(spec SandboxSpec) (*PreparedSandbox, error)
}
