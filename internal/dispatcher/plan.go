package dispatcher

import "github.com/novshi-tech/boid/internal/sandbox"

// BindMount is owned by sandbox and reused at the dispatcher boundary.
type BindMount = sandbox.BindMount

// CommandDef is owned by sandbox and reused at the dispatcher boundary.
type CommandDef = sandbox.CommandDef

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
