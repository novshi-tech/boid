package dispatcher

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// DispatchPlan is the fully resolved execution plan consumed by the runner.
// M5: the plan carries the original orchestrator.DispatchRequest so Runner
// can forward it to orchestrator.BuildSandboxSpec without round-trip
// translation through intermediate dispatcher types.
type DispatchPlan struct {
	// Request is the original orchestrator-built request. Runner passes it
	// directly to orchestrator.BuildSandboxSpec for sandbox.Spec rendering.
	Request *orchestrator.DispatchRequest

	// The fields below mirror Request and are consumed by dispatcher-specific
	// code (broker, runtime, gate staging, etc.). They use orchestrator types
	// directly so no translation is needed.
	TaskID             string
	ProjectID          string
	WorkspaceID        string
	HandlerID          string
	Role               string
	ProjectDir         string
	HomeDir            string
	HookFiles          []orchestrator.HookFile
	GatesDir           string
	ProjectGatesDir    string                  // source dir for project-side gate scripts (dispatcher-side staging)
	KitGatesDirs       []orchestrator.KitGatesInfo // per-kit gate script source dirs (dispatcher-side staging)
	HookScript         string
	BoidBinary         string
	ServerSocket       string
	Env                map[string]string
	BuiltinPolicies    map[string]sandbox.BuiltinPolicy
	HostCommands       map[string]orchestrator.CommandDef
	AdditionalBindings []orchestrator.BindMount
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
