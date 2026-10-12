package orchestrator

import "encoding/json"

// JobKind is a DB / observability label for a job. It is NOT a sandbox
// construction signal — dispatcher's mount/env logic must derive everything
// from Visibility / HostCommands / Instruction, never from Kind. Kind exists
// purely so the Job row / TUI can tell apart "agent hook" and "user-initiated
// exec" without re-deriving the category from loosely-related primitives like
// HandlerID.
type JobKind string

const (
	JobKindHook    JobKind = "hook"
	JobKindExec    JobKind = "exec"
	JobKindSession JobKind = "session"
)

// JobSpec is the orchestrator-owned, sandbox-agnostic execution request.
// It is written purely in business vocabulary: "what to run with what
// visibility and permissions". All sandbox construction details (mounts,
// env, exit scripts, proxy wiring, stdin routing) are left to dispatcher.
//
// dispatcher is the only layer that bridges JobSpec and sandbox.Spec.
type JobSpec struct {
	// ID, when non-empty, is the job id dispatcher.Runner.Dispatch must use
	// verbatim instead of generating a fresh one. Caller-supplied ids must
	// be unique (uuid.New().String()). Empty means "generate as usual".
	ID string

	// Identity used by dispatcher for Job DB persistence and state-machine
	// notification. TaskID and HandlerID are empty for boid-exec jobs.
	TaskID             string
	ProjectID          string
	WorkspaceID        string
	HandlerID          string
	DisplayNameDefault bool   // true when DisplayName is generated, rather than explicitly supplied
	DisplayName        string // human-readable label (hook name or command-session name); persisted to jobs.display_name

	// HookTraitsProduces captures the firing hook's own `traits.produces`
	// list (event.Hook.Traits.Produces) AT DISPATCH TIME — the exact value
	// HandlerResult.allowedTraits (coordinator.go) would use to gate this
	// same hook's file-based payload_patch merge. `boid task update
	// --payload-patch` threads this through dispatcher.JobContextSnapshot
	// so it can gate its own merge against the SAME dispatch-time value,
	// never a live re-lookup against the (possibly since-edited/reloaded)
	// project meta, which would be a TOCTOU staleness bug. nil means "this
	// job's firing hook produces unrestricted" — true for both a
	// virtual/synthesized agent-kind hook (orchestrator.synthesizeAgentHook,
	// whose Traits are always the zero value) and an explicitly declared
	// hook with no traits.produces list of its own; both are
	// indistinguishable from "unrestricted" on the file-based path too
	// (MergePayloadPatch(patch, nil) never filters). Empty for non-hook
	// jobs (boid exec).
	HookTraitsProduces []TraitType

	// Kind is a DB-label / TUI-display category. Sandbox construction details
	// MUST NOT branch on this value.
	Kind JobKind

	// Argv is the command to execute. Argv[0] is either a host absolute path
	// (hook scripts) or a bare command name resolved via broker shim
	// (boid exec). Everything after Argv[0] is passed as-is.
	Argv []string

	// Instruction, when non-nil, materializes at $HOME/.boid/context/instructions.yaml
	// and identifies the agent that should pick up the job. Agent-less jobs
	// (boid exec) leave this nil.
	Instruction *RoutedInstruction

	// Task, when non-nil, materializes at $HOME/.boid/context/task.yaml.
	// boid-exec jobs always leave it nil.
	Task *TaskSnapshot

	// PrimaryInput is the payload the script expects to read on stdin (or,
	// for agent jobs, at $HOME/.boid/context/payload.json).
	// nil means the job reads nothing from boid (e.g. boid-exec with a user TTY).
	PrimaryInput json.RawMessage

	// Visibility describes which host directories the sandbox sees and whether
	// they are writable.
	Visibility Visibility

	// BuiltinPolicies authorises broker-mediated builtin operations (boid, git).
	BuiltinPolicies map[string]BuiltinPolicy

	// HostCommands authorises broker-mediated host command invocations.
	// Hook jobs leave this empty; exec jobs populate it from behavior.
	HostCommands map[string]CommandDef

	// SecretNamespace scopes the broker's secret resolver.
	SecretNamespace string

	APIGatewayServices []string

	// SignalService / SignalConnector set sandbox.TokenContext.Service/
	// Connector for this job's broker token registration: the ONLY fields
	// that authorize BoidOpSignalIngest/BoidOpSignalCursorGet's broker-side
	// scoping (internal/sandbox/broker.go) to a specific (service,
	// connector) pair. Set only by BuildSessionJobSpec from
	// SessionJobInput.SignalService/SignalConnector, which in turn only a
	// signal-derived trigger's StartExec call populates
	// (sessionDispatcherAdapter.StartExec, internal/server/wire.go). Empty
	// for every other job — signal_ingest/signal_cursor_get stay
	// unreachable in practice for any job that doesn't ALSO get
	// ConnectorBuiltinPolicies (BuiltinPolicies alone is not enough; both
	// must line up, matching TokenContext.Service's own doc comment).
	SignalService   string
	SignalConnector string

	// CardID / CardRequestID set sandbox.TokenContext.CardID/CardRequestID
	// for this job's broker token registration, mirroring SignalService/
	// SignalConnector's own wiring. Empty for every job that isn't a
	// card-command launcher.
	CardID        string
	CardRequestID string

	// Env carries extra environment variables the orchestrator wants to export
	// (e.g. behavior-level overrides). dispatcher merges these with its own
	// HOME/PATH/proxy/broker settings.
	Env map[string]string

	// ExecutionState records the task.Status at the time this job was dispatched.
	// Stored in the job DB row so TUI can reconstruct replay context.
	ExecutionState string

	// Interactive, when true, forces TTY allocation regardless of whether an
	// Instruction is attached. Used by daemon-side command execution (Web UI)
	// where the caller always expects a PTY-backed terminal.
	Interactive bool

	HarnessType string

	// SandboxProfile selects the filesystem layout strategy for the sandbox.
	// Zero value (sandbox.ProfileDefault) preserves existing behaviour.
	// Set to sandbox.ProfileInit for kit-init / workspace-configure generation
	// scripts that need read access to the full host filesystem.
	// When ProfileInit is set, broker registration and the broker socket mount
	// are both skipped.
	SandboxProfile int // sandbox.Profile — kept as int to avoid a circular import
}

// Visibility captures which host paths the sandbox sees and whether they are
// writable. orchestrator sets this once per JobSpec; dispatcher turns it into
// mount entries with no further role-aware logic.
type Visibility struct {
	// ProjectDir is the host path to the project working directory.
	// Empty means the project filesystem is not visible.
	ProjectDir string

	ProjectName string

	// AdditionalBindings lists extra host bind-mounts (e.g. kit-provided CLIs
	// like the claude binary) that must be visible inside the sandbox.
	AdditionalBindings []BindMount

	// Writable permits writes to ProjectDir. When ProjectDir is empty, this
	// field has no effect.
	Writable bool

	// DockerEnabled, when true, indicates capabilities.docker was declared in
	// workspace configuration. Dispatcher uses this to start a per-sandbox docker proxy.
	DockerEnabled bool

	// Checkout starts the sandbox command in a fresh primary-repository checkout.
	Checkout bool
}

// TaskSnapshot is the business metadata `boid task current` returns over the
// broker RPC. JSON tags stay lowercase, matching the snake_case convention
// every other boid RPC response uses.
//
// Readonly is the mode-determination source the boid-task skill reads via
// `boid task current --field readonly`.
type TaskSnapshot struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	Behavior    string `json:"behavior"`
	Description string `json:"description,omitempty"`
	Readonly    bool   `json:"readonly"`
}

// CleanupFunc releases transient resources created while planning a JobSpec
// (e.g. staging directories for hook scripts). dispatcher invokes it
// after the sandbox process has exited. A nil CleanupFunc means nothing to
// release.
type CleanupFunc func()
