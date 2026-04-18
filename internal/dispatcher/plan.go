package dispatcher

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// Type alias keeps the DispatchPlan struct definition self-contained and
// avoids an extra indirection when callers access plan.Request.
type orchestratorDispatchRequestAlias = orchestrator.DispatchRequest

// BindMount is a plain shared DTO at the dispatcher boundary.
// It carries mount source/target/mode data and does not encode provider behavior.
type BindMount struct {
	Source string
	Target string // if empty, defaults to Source
	Mode   string // "rw" | "" (ro default)
	IsFile bool
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
// M5: the plan carries the original orchestrator.DispatchRequest so Runner
// can forward it to orchestrator.BuildSandboxSpec without round-trip
// translation through intermediate dispatcher types.
type DispatchPlan struct {
	// Request is the original orchestrator-built request. Runner passes it
	// directly to orchestrator.BuildSandboxSpec for sandbox.Spec rendering.
	Request *orchestratorDispatchRequestAlias

	// The fields below mirror Request but use dispatcher-local types where
	// they are consumed by dispatcher-specific code (broker, runtime, etc.).
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
	InvokedRole        string
	InvokedName        string
	InvokedType        string
}
