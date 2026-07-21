package sandbox

import (
	"encoding/json"
	"strings"
)

// CreatePatch / UpdatePatch fields below replace the old individual fields
// (Title, Description, Behavior, BehaviorSpec, BaseBranch, Ref, ParentID,
// AutoStart) that were previously hand-crafted into BoidRequest.
// The patch is a JSON-serialised api.CreateTaskRequest or api.UpdateTaskRequest
// and is passed through verbatim to the executor, which unmarshals it and
// fills in context-derived defaults (ProjectID, ParentID).

// ExecRequest carries a broker-mediated host command invocation. The broker
// never wires caller-provided stdin into the host process (see the host
// command gate in broker.go / broker_streaming_linux.go); host commands
// always run with stdin connected to /dev/null.
type ExecRequest struct {
	Command   string        `json:"command"`
	Args      []string      `json:"args"`
	Cwd       string        `json:"cwd,omitempty"`
	Token     string        `json:"token"`
	Boid      *BoidRequest  `json:"boid,omitempty"`
	Fetch     *FetchRequest `json:"fetch,omitempty"`
	Streaming bool          `json:"streaming,omitempty"`
}

// FetchRequest carries the parameters for a broker-mediated HTTP GET.
// Only GET is supported; the broker performs the request on the host and
// returns the response body (HTML converted to markdown, other types verbatim).
type FetchRequest struct {
	URL string `json:"url"`
}

type ExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// StreamChunk is one unit in the streaming host-command protocol (Streaming=true).
//
// Broker → Shim: type "stdout"/"stderr" carry Data; type "exit" carries ExitCode.
// Shim → Broker: type "kill" requests SIGTERM on the host process group.
const (
	StreamTypeStdout = "stdout"
	StreamTypeStderr = "stderr"
	StreamTypeExit   = "exit"
	StreamTypeKill   = "kill"
)

type StreamChunk struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

type BoidOp string

const (
	BoidOpJobDone    BoidOp = "job_done"
	BoidOpJobList    BoidOp = "job_list"
	BoidOpJobShow    BoidOp = "job_show"
	BoidOpJobLog     BoidOp = "job_log"
	BoidOpActionSend BoidOp = "action_send"
	BoidOpAgentStop  BoidOp = "agent_stop"
	BoidOpTaskCreate BoidOp = "task_create"
	BoidOpTaskGet    BoidOp = "task_get"
	BoidOpTaskUpdate BoidOp = "task_update"
	BoidOpTaskImport BoidOp = "task_import"
	BoidOpTaskReopen BoidOp = "task.reopen"
	BoidOpTaskList   BoidOp = "task_list"
	BoidOpTaskNotify BoidOp = "task_notify"
	BoidOpTaskAnswer BoidOp = "task_answer"
	BoidOpTaskAsk    BoidOp = "task_ask"
	BoidOpTaskDelete BoidOp = "task_delete"

	// Phase 5b PR1 task-context RPCs (docs/plans/phase5-shim-and-task-context.md):
	// pull-based replacements for the dispatch-time context files
	// ($HOME/.boid/context/{task,instructions,environment,payload}.{yaml,json}).
	// The 5b-6 cutover PR retired that file-based materialization entirely
	// (sandbox_builder.go's contextFiles/buildEnvironmentYAML) — these four
	// ops are now the sole source of task/instructions/environment/payload
	// data for an in-sandbox caller.
	BoidOpTaskCurrent      BoidOp = "task_current"
	BoidOpTaskInstructions BoidOp = "task_instructions"
	BoidOpTaskEnv          BoidOp = "task_env"
	BoidOpTaskPayload      BoidOp = "task_payload"

	// Phase 5b PR2 attachments RPCs (docs/plans/phase5-shim-and-task-context.md):
	// pull-based replacement for the dispatch-time attachments bind
	// (`~/.boid/attachments`, sandbox_builder.go's former per-task RO mount
	// of `<AttachmentsRoot>/tasks/<task_id>/attachments`). The 5b-6 cutover
	// PR retired that bind entirely — these two ops are now the sole
	// in-sandbox read path for attachments.
	BoidOpTaskAttachmentsList BoidOp = "task_attachments_list"
	BoidOpTaskAttachmentsGet  BoidOp = "task_attachments_get"

	// Phase 5b PR7 (docs/plans/phase5-shim-and-task-context.md): the
	// job_done payload_patch direct-pass RPC. `boid task update
	// --payload-patch @-` sends this instead of the agent writing
	// $HOME/.boid/output/payload_patch.json for postJobDone/JobDone to pick
	// up later — it applies immediately, with the SAME merge semantics
	// (orchestrator.MergePayloadPatch, gated by the firing hook's own
	// Traits.Produces) rather than BoidOpTaskUpdate's simpler top-level
	// shallow merge. JobID-scoped like TaskInstructions/Env/Payload (not
	// TaskID-scoped): the allowedTraits gate is sourced from the
	// dispatcher.JobContextSnapshot captured for the CALLING job at dispatch
	// time (never re-resolved live against project meta — a TOCTOU
	// staleness bug codex review caught, see wiring-seams.md #17's Major 1),
	// which only exists per-job. The file-based fallback (decision 6/7,
	// wiring-seams.md #13's PR6 update) is untouched by this op and remains
	// available as a secondary path — full retirement is deferred to Phase 6.
	BoidOpTaskUpdatePayloadPatch BoidOp = "task_update_payload_patch"
)

// PayloadPatchMaxBytes caps the size of a single BoidOpTaskUpdatePayloadPatch
// request's PayloadPatch content (whether read from a file, stdin, or an
// inline CLI value). Unlike most of the shim's other file-reading flags
// (--payload-file, --patch-file, ...), this content crosses the broker RPC
// boundary into the daemon process — a shared, long-lived process — so an
// unbounded read is a real OOM vector, not just a local-runner concern.
// Enforced at two independent points (defense in depth, Phase 5b PR7 codex
// review Major 3, wiring-seams.md #17): the shim's own read
// (internal/sandbox/boid_shim.go's readPayloadPatchSource, so an oversized
// input never even reaches the wire) and the broker's request handler
// (internal/sandbox/broker.go), which re-checks independently so a shim
// bypass or a future second caller can't skip the limit. Matches
// api.AttachmentMaxFileBytes's existing 10 MB precedent (Phase 5b PR2).
const PayloadPatchMaxBytes = 10 * 1024 * 1024

type BoidRequest struct {
	Op        BoidOp `json:"op"`
	JobID     string `json:"job_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	TaskField string `json:"task_field,omitempty"`
	// ProjectID is extracted by the shim for broker authorization / project
	// resolver; it is also present inside CreatePatch when the YAML includes
	// project_id. The executor prefers createReq.ProjectID from CreatePatch
	// and falls back to this field, then to ctx.ProjectID.
	ProjectID string `json:"project_id,omitempty"`

	ExitCode int    `json:"exit_code,omitempty"`
	Output   string `json:"output,omitempty"`

	// CreatePatch is a JSON-serialised api.CreateTaskRequest. The shim builds
	// it from the full YAML map so that every field (including previously
	// dropped ones such as instructions, traits, readonly, worktree,
	// branch_prefix, id) is forwarded without enumeration.
	CreatePatch json.RawMessage `json:"create_patch,omitempty"`

	// UpdatePatch is a JSON-serialised api.UpdateTaskRequest. The shim
	// assembles it from --patch-file and/or individual flags (--title,
	// --description, --payload-file).
	UpdatePatch json.RawMessage `json:"update_patch,omitempty"`

	// task import fields
	ImportTasks           []json.RawMessage `json:"import_tasks,omitempty"`
	ImportProjectOverride string            `json:"import_project_override,omitempty"`

	// task list fields
	WorkspaceID string `json:"workspace_id,omitempty"`
	Status      string `json:"status,omitempty"`
	Limit       int    `json:"limit,omitempty"`

	// task notify fields
	Message    string `json:"message,omitempty"`
	Ask        string `json:"ask,omitempty"`
	QuestionID string `json:"question_id,omitempty"`
	Progress   string `json:"progress,omitempty"`
	Done       string `json:"done,omitempty"`
	Fail       string `json:"fail,omitempty"`

	// task answer fields
	Answer string `json:"answer,omitempty"`

	// task ask fields. Question carries the blocking-RPC question text for
	// `boid task ask <text>`. The broker holds the connection open until the
	// answer arrives; the answer is returned verbatim in ExecResponse.Stdout
	// (the boid builtin reply framing is ExecResponse, so no separate
	// BoidResponse type is needed).
	Question string `json:"question,omitempty"`

	// task delete fields
	Force bool `json:"force,omitempty"`

	// action send fields
	ActionType string `json:"action_type,omitempty"`
	Payload    []byte `json:"payload,omitempty"`

	// task attachments get fields. AttachmentName addresses one attachment
	// by its exact basename — see api.ReadAttachment for the traversal
	// guard (no path separators, no ".."). Unused by
	// BoidOpTaskAttachmentsList.
	AttachmentName string `json:"attachment_name,omitempty"`

	// PayloadPatch carries the raw patch body for BoidOpTaskUpdatePayloadPatch
	// — the JSON that would otherwise go inside a file-based
	// {"payload_patch": ...} envelope (docs/*/reference/hook-contract.md).
	// Unlike UpdatePatch (a JSON-serialised api.UpdateTaskRequest consumed by
	// a top-level shallow merge), this is merged via
	// orchestrator.MergePayloadPatch — see api.TaskAppService.UpdateTaskPayloadPatch.
	PayloadPatch json.RawMessage `json:"payload_patch,omitempty"`
}

type TokenContext struct {
	JobID             string
	TaskID            string
	ProjectID         string
	WorkspaceID       string
	AllowedProjectIDs []string
	Role              string
	// ProjectDir is the project's host-side work directory. Independent of
	// spec.Visibility.ProjectDir (which drives sandbox mount layout and is
	// intentionally empty for gate jobs): host-side operations the broker
	// performs on behalf of the sandbox (host-command cwd) have their own
	// notion of "which project are we operating on" that doesn't care
	// whether the sandbox itself can see the tree.
	ProjectDir string
	// SandboxRoot is the sandbox-internal (not host-side) root directory a
	// clone-mode job's filesystem lives under — a name-scoped subdirectory
	// of the neutral parent path "/workspace" (docs/plans/git-gateway-cutover.md
	// PR6 cutover; workspace 親化リファクタリング, nose 2026-07-13 decision),
	// set by dispatcher when spec.Visibility.Clone != nil. Unlike ProjectDir
	// this is never a host path: clone-mode jobs have no host directory the
	// sandbox's own filesystem corresponds to, so cwd-based authorization
	// (validateBoidBuiltinCwd's entryRoot) must compare against this
	// sandbox-side path instead. Empty for every non-clone job.
	SandboxRoot string
}

func (c TokenContext) AllowsProject(projectID string) bool {
	if projectID == "" {
		projectID = c.ProjectID
	}
	if projectID == "" {
		return false
	}
	if len(c.AllowedProjectIDs) == 0 {
		return projectID == c.ProjectID
	}
	for _, allowed := range c.AllowedProjectIDs {
		if allowed == projectID {
			return true
		}
	}
	return false
}

// ProjectResolver maps a project reference (UUID, exact name, or partial name)
// to a concrete project UUID. The broker calls it just before the UUID-based
// AllowsProject authorization check so that sandbox-side callers (e.g. plan
// agents) can use project names while the broker continues to enforce
// workspace isolation in UUID space. When nil, the broker passes refs through
// verbatim (tests and UUID-only callers).
type ProjectResolver func(ref string) (uuid string, err error)

// BuiltinPolicy defines which operations are permitted for a named builtin command.
// It is stamped at token registration time by the planner and checked at dispatch time
// by the broker, keeping all role-based authorization logic outside the broker itself.
type BuiltinPolicy struct {
	AllowedOps map[string]struct{}
	// AllowedCwdRoots lists additional cwd roots permitted for this builtin
	// beyond the per-token entry root (project/worktree dir). Used so that
	// e.g. gate jobs can target /tmp or the host project dir without the
	// broker needing to know the role itself.
	AllowedCwdRoots []string
}

// Allows reports whether op is in the allowed set.
func (p BuiltinPolicy) Allows(op string) bool {
	_, ok := p.AllowedOps[op]
	return ok
}

// AllowsCwd reports whether cwd is within any of the policy's additional cwd roots.
func (p BuiltinPolicy) AllowsCwd(cwd string) bool {
	for _, root := range p.AllowedCwdRoots {
		if root == "" {
			continue
		}
		if cwd == root {
			return true
		}
		if strings.HasPrefix(cwd, root+"/") {
			return true
		}
	}
	return false
}
