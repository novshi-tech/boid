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
	JobKindHook JobKind = "hook"
	JobKindExec JobKind = "exec"
	// JobKindSession is the Phase 3-d label for a user-initiated agent
	// session not tied to a task lifecycle (WebUI [New Session] / `boid
	// agent` CLI). It is distinct from JobKindHook (state-machine driven)
	// and JobKindExec (sandboxed argv with no harness), so the TUI / Web UI
	// can present it as a separate top-level concept.
	JobKindSession JobKind = "session"
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
	TaskID      string
	ProjectID   string
	HandlerID   string
	DisplayName string // human-readable label (hook name or command-session name); persisted to jobs.display_name

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

	// HarnessType identifies which HarnessAdapter implementation the runner
	// hands the agent process off to. Phase 3-d made this invariant
	// non-empty for every dispatched job:
	//   - "shell" for hooks without an `agent:` declaration, every `boid
	//     exec`, and the fall-through for unknown agents
	//   - "claude" / "codex" / "opencode" for the corresponding agent hooks
	//     and user-initiated sessions
	// dispatcher bridges this into sandbox.Spec.HarnessType and the
	// runner-inner-child resolves the adapter via the registry.
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

	// ProjectName is project.yaml's `meta.name` (kebab-case by convention,
	// not enforced), threaded through from the workspace-hydrated
	// ProjectMeta at JobSpec-build time (PlanHook / BuildSessionJobSpec).
	// dispatcher uses it — falling back to filepath.Base(ProjectDir) when
	// empty — to name the sandbox-internal clone directory under the
	// /workspace parent dir (workspace 親化リファクタリング, nose
	// 2026-07-13 decision): every project previously shared the exact same
	// sandbox cwd ("/workspace"), which collided Claude Code's
	// `~/.claude/projects/-workspace/` session-log slug across every boid
	// project. Empty is a legitimate value (a project with no `name:` in
	// project.yaml, or a dispatch path — e.g. gate/hook jobs with no
	// project visible at all — that never resolves one); dispatcher's
	// fallback degrades gracefully rather than erroring.
	ProjectName string

	// AdditionalBindings lists extra host bind-mounts (e.g. kit-provided CLIs
	// like the claude binary) that must be visible inside the sandbox.
	AdditionalBindings []BindMount

	// Writable permits writes to ProjectDir. When ProjectDir is empty, this
	// field has no effect.
	Writable bool

	// DockerEnabled, when true, indicates capabilities.docker was declared in
	// project.yaml. Dispatcher uses this to start a per-sandbox docker proxy.
	DockerEnabled bool

	// Clone declares the sandbox-internal-clone branch state for real dispatch
	// (docs/plans/git-gateway-cutover.md PR5 「5. branch 宣言の JobSpec 化」・
	// PR6 cutover). nil leaves dispatch unaffected (test-only JobSpecs that
	// don't exercise clone-mode). When non-nil, dispatcher does not resolve
	// the branch itself against a host repo — it carries this declaration
	// through to the sandbox so the runner resolves it (rev-parse /
	// merge-base / checkout -B) after cloning inside the sandbox.
	Clone *CloneDeclaration
}

// CloneDeclaration declares the working-branch state a sandbox-internal
// clone should end up in, without resolving it — resolution (rev-parse /
// merge-base / checkout -B) is deferred to the runner, which performs it
// against the freshly cloned repo (docs/plans/git-gateway-cutover.md: 「dispatcher
// は JobSpec に宣言のみ載せる...runner が clone 完了後に解決」).
//
// docs/plans/branch-policy-simplification.md Phase 1 retired the per-task
// "boid/<id8>" branch and its fork-point: every task, root or child, checks
// out BaseBranch directly (CheckoutOnly is always true now). See
// BuildCloneDeclaration's doc comment for the rationale.
type CloneDeclaration struct {
	// Branch is the branch the runner ends up on inside the clone. Always
	// equal to BaseBranch — every task occupies its own BaseBranch directly.
	Branch string

	// BaseBranch is the upstream branch this task's work is based on
	// (task.BaseBranch). Always required.
	BaseBranch string

	// CheckoutOnly is always true (Branch is checked out directly rather than
	// created fresh from a fork point). Kept as an explicit field — rather
	// than collapsed away — so CloneSpec / runner-state.json's declaration
	// shape stays self-describing.
	CheckoutOnly bool

	// BaseBranchForkPoint (ClassifyBaseBranch case 3): the start point used
	// to create BaseBranch locally when it exists on neither the clone's
	// origin nor locally. Empty falls back to refs/remotes/origin/HEAD,
	// resolved by the runner after clone (no extra fetch needed: `git clone`
	// already brings every remote branch's ref). This is independent of the
	// retired per-task fork point: it addresses BaseBranch not existing yet
	// anywhere, not task-to-task isolation.
	BaseBranchForkPoint string
}

// TaskSnapshot is the business metadata that materializes at
// $HOME/.boid/context/task.yaml, and (Phase 5b PR1,
// docs/plans/phase5-shim-and-task-context.md) is also what `boid task
// current` returns over the broker RPC. JSON tags are lowercase to match
// task.yaml's existing key casing (marshalTaskYAML in sandbox_builder.go)
// and the snake_case convention every other boid RPC response uses.
type TaskSnapshot struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	Behavior    string `json:"behavior"`
	Description string `json:"description,omitempty"`
}

// CleanupFunc releases transient resources created while planning a JobSpec
// (e.g. staging directories for hook scripts). dispatcher invokes it
// after the sandbox process has exited. A nil CleanupFunc means nothing to
// release.
type CleanupFunc func()
