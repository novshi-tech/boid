package orchestrator

import "encoding/json"

// JobKind is the business category of a job. It is used for DB labels / TUI
// display only — dispatcher's sandbox construction logic must not branch on
// it (use Visibility / HostCommands / Instruction instead).
type JobKind string

const (
	JobKindHook JobKind = "hook"
	JobKindGate JobKind = "gate"
	JobKindExec JobKind = "exec"
)

// JobSpec is the orchestrator-owned, sandbox-agnostic execution request.
// It is written purely in business vocabulary: "what to run with what
// visibility and permissions". All sandbox construction details (mounts,
// env, exit scripts, proxy wiring, stdin routing) are left to dispatcher.
//
// dispatcher is the only layer that bridges JobSpec and sandbox.Spec.
type JobSpec struct {
	// Identity used by dispatcher for Job DB persistence and state-machine
	// notification. TaskID and HandlerID are empty for boid-exec jobs.
	TaskID    string
	ProjectID string
	HandlerID string

	// Kind labels the job for persistence / observability. Dispatcher must
	// NOT read this to decide mount layout — that comes from Visibility and
	// the other primitives below.
	Kind JobKind

	// Argv is the command to execute. Argv[0] is either a host absolute path
	// (hook / gate scripts) or a bare command name resolved via broker shim
	// (boid exec). Everything after Argv[0] is passed as-is.
	Argv []string

	// Instruction, when non-nil, materializes at $HOME/.boid/context/instructions.yaml
	// and identifies the agent that should pick up the job. Agent-less jobs
	// (gate scripts, boid exec) leave this nil.
	Instruction *RoutedInstruction

	// Task, when non-nil, materializes at $HOME/.boid/context/task.yaml. Gate
	// jobs typically leave this nil and receive task data through PrimaryInput
	// instead; boid-exec jobs always leave it nil.
	Task *TaskSnapshot

	// PrimaryInput is the payload the script expects to read on stdin (or,
	// when Instruction.Interactive is true, at $HOME/.boid/context/payload.json).
	// nil means the job reads nothing from boid (e.g. boid-exec with a user TTY).
	PrimaryInput json.RawMessage

	// Visibility describes which host directories the sandbox sees and whether
	// they are writable.
	Visibility Visibility

	// BuiltinPolicies authorises broker-mediated builtin operations (boid, git).
	BuiltinPolicies map[string]BuiltinPolicy

	// HostCommands authorises broker-mediated host command invocations. hook
	// jobs leave this empty; gate and exec jobs populate it from behavior.
	HostCommands map[string]CommandDef

	// SecretNamespace scopes the broker's secret resolver.
	SecretNamespace string

	// Env carries extra environment variables the orchestrator wants to export
	// (e.g. behavior-level overrides). dispatcher merges these with its own
	// HOME/PATH/proxy/broker settings.
	Env map[string]string
}

// Visibility captures which host paths the sandbox sees and whether they are
// writable. orchestrator sets this once per JobSpec; dispatcher turns it into
// mount entries with no further role-aware logic.
type Visibility struct {
	// ProjectDir is the host path to the project working directory. Empty
	// means the project filesystem is not visible at all (gate jobs).
	ProjectDir string

	// UseWorktree asks dispatcher to replace ProjectDir with a per-task git
	// worktree obtained from its WorktreeManager.
	UseWorktree bool

	// WorkspacePeers maps peer project IDs to host paths. All peers are
	// mounted read-only.
	WorkspacePeers map[string]string

	// AdditionalBindings lists extra host bind-mounts (e.g. kit-provided CLIs
	// like the claude binary) that must be visible inside the sandbox.
	AdditionalBindings []BindMount

	// Writable permits writes to ProjectDir / the resolved worktree. When
	// ProjectDir is empty, this field has no effect.
	Writable bool
}

// TaskSnapshot is the business metadata that materializes at
// $HOME/.boid/context/task.yaml. Fields mirror the subset historically
// produced by planner's buildTaskYAML helper.
type TaskSnapshot struct {
	ID          string
	Title       string
	Status      string
	Behavior    string
	Description string
}

// CleanupFunc releases transient resources created while planning a JobSpec
// (e.g. staging directories for hook/gate scripts). dispatcher invokes it
// after the sandbox process has exited. A nil CleanupFunc means nothing to
// release.
type CleanupFunc func()
