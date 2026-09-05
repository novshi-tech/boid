package sandbox

import (
	"encoding/json"
	"strings"
)

// CreatePatch / UpdatePatch are JSON-serialised api.CreateTaskRequest /
// api.UpdateTaskRequest, passed through verbatim to the executor, which
// unmarshals them and fills in context-derived defaults (ProjectID, ParentID).

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

	// BoidOpTaskWait blocks until the named task reaches a terminal status and
	// reports how it ended (exit 0 for done, non-zero otherwise). Scoped like
	// every other task op. Not a timeout: a task parked in a non-terminal
	// resting state waits indefinitely; bounding a round belongs to whoever
	// launched it.
	BoidOpTaskWait BoidOp = "task_wait"

	// BoidOpTaskCurrent/Instructions/Env/Payload are the sole source of
	// task/instructions/environment/payload data for an in-sandbox caller
	// (pull-based, no dispatch-time context files).
	BoidOpTaskCurrent      BoidOp = "task_current"
	BoidOpTaskInstructions BoidOp = "task_instructions"
	BoidOpTaskEnv          BoidOp = "task_env"
	BoidOpTaskPayload      BoidOp = "task_payload"

	// BoidOpTaskAttachmentsList/Get are the sole in-sandbox read path for
	// task attachments (pull-based, no dispatch-time bind mount).
	BoidOpTaskAttachmentsList BoidOp = "task_attachments_list"
	BoidOpTaskAttachmentsGet  BoidOp = "task_attachments_get"

	// BoidOpTaskUpdatePayloadPatch backs `boid task update --payload-patch
	// <value>`: applies a payload patch immediately via
	// orchestrator.MergePayloadPatch (gated by the firing hook's own
	// Traits.Produces), rather than BoidOpTaskUpdate's simpler top-level
	// shallow merge. JobID-scoped like TaskInstructions/Env/Payload: the
	// allowedTraits gate is sourced from the dispatcher.JobContextSnapshot
	// captured for the calling job at dispatch time, not re-resolved live.
	BoidOpTaskUpdatePayloadPatch BoidOp = "task_update_payload_patch"

	// BoidOpProjectBehaviors backs `boid project behaviors <project-ref>`.
	// Scoped like BoidOpTaskList/BoidOpTaskCreate: the broker resolves
	// BoidRequest.ProjectID via ProjectResolver and enforces
	// TokenContext.AllowsProject.
	BoidOpProjectBehaviors BoidOp = "project_behaviors"

	// BoidOpProjectList backs `boid project list`. Unlike
	// BoidOpProjectBehaviors there is no caller-supplied ref: the executor
	// enumerates TokenContext.AllowedProjectIDs, so the result is
	// unconditionally scoped to the caller's own workspace.
	BoidOpProjectList BoidOp = "project_list"

	// BoidOpCardGet / BoidOpCardList back `boid card get <task-id>` and
	// `boid card list [--status S] [--project-id P] [--workspace-id W]` —
	// strictly READ ops over data the calling workspace already owns.
	// Scoping matches BoidOpActionSend: the broker only checks shape, the
	// executor enforces TokenContext.AllowsProject before returning anything.
	BoidOpCardGet  BoidOp = "card_get"
	BoidOpCardList BoidOp = "card_list"

	// BoidOpTaskIdentityLink / BoidOpTaskIdentityUnlink / BoidOpTaskIdentityResolve
	// back `boid task identity link/unlink/resolve` — the identity index
	// (task_identities table, external key -> task).
	//
	// Scoping is broker-authoritative, matching BoidOpTaskCreate: ProjectID
	// defaults from the token's own context when omitted, is resolved via
	// ProjectResolver, and is checked against TokenContext.AllowsProject
	// before the executor ever sees the request. BoidOpTaskIdentityLink also
	// carries a caller-supplied TaskID checked in the executor
	// (GetTask + AllowsProject); it must use GetTask's resolved existing.ID
	// (not the caller-supplied prefix) since it writes into an FK column.
	//
	// BoidOpTaskIdentityResolve represents "no such binding" via a distinct
	// ExecResponse.ExitCode (IdentityNotFoundExitCode below), not the generic
	// ExitCode:1 failure path, so a get-or-create caller can tell "not
	// found" apart from a real error without parsing stderr. Link has its
	// own distinguished code for a conflicting link (IdentityConflictExitCode).
	BoidOpTaskIdentityLink    BoidOp = "task_identity_link"
	BoidOpTaskIdentityUnlink  BoidOp = "task_identity_unlink"
	BoidOpTaskIdentityResolve BoidOp = "task_identity_resolve"

	// BoidOpTaskResolveOrCapture backs `boid task resolve-or-capture
	// <identity> [--title T] [--description D|--description-file F]`:
	// resolve Identity to an existing task, or atomically create a new
	// `captured` triage task and link Identity to it when unresolved. Kept
	// separate from BoidOpTaskIdentityLink/BoidOpActionSend rather than a
	// variant of either. When Created is false, the caller is expected to
	// follow up with a normal BoidOpActionSend (attrs_set).
	//
	// Scoping matches BoidOpTaskIdentityLink. Unlike Link, there is no
	// caller-supplied TaskID to separately verify: every task this op
	// returns or creates is scoped to req.ProjectID by construction.
	//
	// Conflict (Identity already bound to a different task) is represented
	// the same way BoidOpTaskIdentityLink represents it: IdentityConflictExitCode,
	// not a generic ExitCode:1.
	BoidOpTaskResolveOrCapture BoidOp = "task_resolve_or_capture"

	// BoidOpActionList backs `boid action list`: the workspace-scoped,
	// since-cursor read over actions — the read half of BoidOpActionSend.
	// Deliberately a single workspace-wide read, not per-task, so a caller
	// does not need O(N) brokered calls per tick to cover every task.
	//
	// Scoping is broker-authoritative, matching BoidOpCardList. TaskID
	// additionally narrows to one task's actions, ANDed with the
	// already-broker-verified project/workspace scope in the SQL query
	// itself — a TaskID outside the caller's scope simply matches zero rows.
	BoidOpActionList BoidOp = "action_list"

	// BoidOpSignalList / BoidOpSignalAck back `boid signal list [--claim]
	// [--source ...] [--state ...] [--limit N]` / `boid signal ack <id>...`:
	// the judgment-side read/decide surface over the signal inbox. Both are
	// part of the general boidPolicy (policy.go) — any hook/exec job may
	// scan its own workspace's inbox and ack a Signal once it has written a
	// judgment for it.
	//
	// BoidOpSignalAck is idempotent by construction: AckSignals only ever
	// sets acked_at WHERE acked_at IS NULL.
	//
	// BoidOpSignalList's --claim flag routes to ClaimSignals (attempts++)
	// instead of the plain read ListSignals — see BoidRequest.Claim.
	// --claim is DEPRECATED (2026-08-29): use a plain `signal list` followed
	// by BoidOpSignalClaim naming the rows actually handed to a judgment.
	// Kept working for daemon/workspace version skew; remove once no
	// workspace calls it.
	BoidOpSignalList BoidOp = "signal_list"
	BoidOpSignalAck  BoidOp = "signal_ack"

	// BoidOpSignalClaim backs `boid signal claim <id>...`: charge one
	// attempt against exactly these rows, because they are the ones being
	// handed to a judgment — splitting claim from list keeps a wide read
	// from charging attempts against rows nobody actually judged.
	BoidOpSignalClaim BoidOp = "signal_claim"

	// BoidOpSignalIngest / BoidOpSignalCursorGet back `boid signal ingest`
	// (stdin JSONL) / `boid signal cursor` from inside a connector's exec
	// job. Declared here for completeness but deliberately not added to the
	// general boidPolicy (policy.go) — granting these two ops is a
	// connector-scoped, reduced policy handed only to derived trigger exec
	// jobs, so they are otherwise unreachable.
	BoidOpSignalIngest    BoidOp = "signal_ingest"
	BoidOpSignalCursorGet BoidOp = "signal_cursor_get"
)

// IdentityNotFoundExitCode is BoidOpTaskIdentityResolve's distinguished exit
// code for "no task is bound to this identity", distinct from 0 (success)
// and 1 (generic failure), so a caller can tell "not found" apart from a
// real error purely from the exit code.
const IdentityNotFoundExitCode = 2

// IdentityConflictExitCode is BoidOpTaskIdentityLink's distinguished exit
// code for "this identity is already bound to a different task"
// (orchestrator.ErrIdentityConflict).
const IdentityConflictExitCode = 3

// PayloadPatchMaxBytes caps the size of a single BoidOpTaskUpdatePayloadPatch
// request's PayloadPatch content, since it crosses the broker RPC boundary
// into the daemon process. Enforced independently at both the shim's read
// and the broker's request handler.
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
	// {"payload_patch": ...} envelope. Unlike UpdatePatch (a
	// JSON-serialised api.UpdateTaskRequest consumed by a top-level shallow
	// merge), this is merged via orchestrator.MergePayloadPatch.
	PayloadPatch json.RawMessage `json:"payload_patch,omitempty"`

	// Identity carries the opaque external-key string for
	// BoidOpTaskIdentityLink / BoidOpTaskIdentityUnlink /
	// BoidOpTaskIdentityResolve / BoidOpTaskResolveOrCapture. Never
	// interpreted by the daemon — validated only for non-emptiness. Link
	// additionally uses TaskID (the task to bind); scope is ProjectID for
	// all four.
	Identity string `json:"identity,omitempty"`

	// Title / Description carry BoidOpTaskResolveOrCapture's new-task
	// fields, used only when Identity is unresolved and a fresh `captured`
	// task is created; ignored when the identity already resolves to an
	// existing task. Deliberately plain strings rather than a CreatePatch —
	// behavior/traits/readonly/etc. all come from the resolved default
	// behavior instead.
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`

	// Since carries BoidOpActionList's opaque cursor
	// (orchestrator.EncodeActionCursor's output) — empty means "from the
	// beginning".
	Since string `json:"since,omitempty"`

	// WorkspaceID is the scope for all four signal ops, but is never
	// caller-supplied for them — the broker unconditionally overwrites it
	// with entry.Context.WorkspaceID.
	//
	// Service/Connector serve two different ops in two different ways:
	//   - BoidOpSignalList: Connector is the optional --source filter, not
	//     broker-enforced beyond normal read scoping.
	//   - BoidOpSignalIngest / BoidOpSignalCursorGet: both fields are the
	//     connector's own identity. The shim populates them from
	//     BOID_SIGNAL_SERVICE / BOID_SIGNAL_CONNECTOR, but the broker is
	//     what actually enforces this: its case unconditionally overwrites
	//     these fields with TokenContext.Service/Connector, so a
	//     hand-crafted ExecRequest bypassing the shim can't claim an
	//     arbitrary Service/Connector.
	Service   string `json:"service,omitempty"`
	Connector string `json:"connector,omitempty"`

	// Claim selects BoidOpSignalList's ClaimSignals path (attempts++
	// included) over the plain ListSignals read — `boid signal list
	// --claim`.
	Claim bool `json:"claim,omitempty"`

	// SignalState filters BoidOpSignalList by orchestrator.SignalState
	// (pending|dead|acked|all); empty defaults to "pending". A distinct
	// field from Status (task status for the task_* ops).
	SignalState string `json:"signal_state,omitempty"`

	// SignalIDs carries `boid signal ack <id>...`'s positional id list (1 or
	// more). AckSignals matches by (workspace_id, id) only and is
	// idempotent per id.
	SignalIDs []string `json:"signal_ids,omitempty"`

	// IngestPayload carries the raw JSONL bytes `boid signal ingest` reads
	// from stdin, capped by the shim at PayloadPatchMaxBytes. Parsing each
	// line and validating required fields happens server-side in the
	// executor, not in the shim.
	IngestPayload []byte `json:"ingest_payload,omitempty"`
}

type TokenContext struct {
	JobID             string
	TaskID            string
	ProjectID         string
	WorkspaceID       string
	AllowedProjectIDs []string
	Role              string
	// ProjectDir is the project's host-side work directory. Independent of
	// spec.Visibility.ProjectDir (sandbox mount layout, empty for gate
	// jobs): host-side operations the broker performs on behalf of the
	// sandbox have their own notion of "which project" regardless of what
	// the sandbox itself can see.
	ProjectDir string
	// Service / Connector are the token-registration-time-stamped identity
	// a connector job is authorized to ingest/read the cursor for.
	// BoidOpSignalIngest / BoidOpSignalCursorGet's broker case
	// unconditionally overwrites BoidRequest.Service/Connector with these
	// values, so a hand-crafted ExecRequest bypassing the shim can't claim
	// an arbitrary Service/Connector. Empty until a connector-scoped token
	// registration populates them.
	Service   string
	Connector string
	// SandboxRoot is the sandbox-internal (not host-side) root directory a
	// clone-mode job's filesystem lives under, set by dispatcher when
	// spec.Visibility.Clone != nil. Unlike ProjectDir this is never a host
	// path, so cwd-based authorization must compare against this
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
