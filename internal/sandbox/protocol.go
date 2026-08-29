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

	// BoidOpTaskWait blocks until the named task reaches a terminal status and
	// reports how it ended (exit 0 for done, non-zero otherwise). It exists so
	// a trigger's `run:` command can live exactly as long as the task it
	// started: with the launcher exiting in seconds, the trigger's own
	// single-flight measures the launcher rather than the work, and a failed
	// round never reaches TriggerLoop.trackFailStreak at all — the workspace
	// has to re-derive it. Waiting closes both gaps with no bookkeeping of its
	// own.
	//
	// Scoped like every other task op: the broker resolves the task and the
	// executor rejects one outside the caller's workspace. The wait ends only
	// on a terminal status or on the broker cancelling the request context
	// (daemon shutdown / sandbox disconnect), so it cannot outlive its caller.
	BoidOpTaskWait BoidOp = "task_wait"

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

	// BoidOpProjectBehaviors backs `boid project behaviors <project-ref>` from
	// inside the sandbox — added so a "meta project" task (one whose job is
	// to look across multiple projects, e.g. cross-project issue triage) can
	// discover another project's task_behaviors without a host-only `boid
	// project` CLI invocation. Scoped like BoidOpTaskList/BoidOpTaskCreate:
	// the broker resolves BoidRequest.ProjectID (UUID, exact name, or partial
	// name) via ProjectResolver and then enforces
	// TokenContext.AllowsProject, so a caller can only ever see behaviors for
	// projects within its own token's workspace scope — never an arbitrary
	// project on the daemon.
	BoidOpProjectBehaviors BoidOp = "project_behaviors"

	// BoidOpProjectList backs `boid project list` from inside the sandbox —
	// lets a "meta project" task discover which projects it can even ask
	// BoidOpProjectBehaviors about, rather than requiring it to already know
	// every project ref up front. Unlike BoidOpProjectBehaviors there is no
	// caller-supplied ref to resolve: the executor enumerates
	// TokenContext.AllowedProjectIDs (falling back to the caller's own
	// ProjectID when that is empty, same fallback BoidOpTaskList's
	// no-project/no-workspace branch uses) — a value stamped into the token
	// at dispatch time from the workspace's peer-project set
	// (dispatcher.allowedProjectIDs), never something a sandboxed request
	// can widen. So the result is unconditionally scoped to the caller's own
	// workspace; there is intentionally no way to list projects daemon-wide
	// the way the host-only `boid project list` does.
	BoidOpProjectList BoidOp = "project_list"

	// BoidOpCardGet / BoidOpCardList back `boid card get <task-id>` and
	// `boid card list [--status S] [--project-id P] [--workspace-id W]` from
	// inside the sandbox (docs/plans/cross-project-issue-triage.md Phase 1
	// PR-5a; renamed from task_triage_get/list and `boid task triage` by
	// docs/plans/card-model-cleanup.md PR-3 §4 — wire rename only, scoping
	// and shape are unchanged).
	//
	// 決定14 makes the daemon the SOLE source of truth for a triage task's
	// state — khi retires its own decisions log and fold, keeping only claims
	// and the note body. That only works if the workspace side can read the
	// state back, and before PR-5a there was no read path at all: the sidecar
	// was reachable only from inside the daemon (Web UI enrichment), and
	// BoidOpTaskGet returns orchestrator.Task's own columns, which
	// deliberately exclude everything triage-specific.
	//
	// These are strictly READ ops over data the calling workspace already
	// owns, so they do not widen 決定2's boundary (the same reasoning that
	// already allows brokered task_list of the caller's own workspace —
	// 実測結果 項10). Scoping matches BoidOpActionSend: the
	// broker only checks shape, the executor enforces
	// TokenContext.AllowsProject before returning anything.
	BoidOpCardGet  BoidOp = "card_get"
	BoidOpCardList BoidOp = "card_list"

	// BoidOpTaskIdentityLink / BoidOpTaskIdentityUnlink / BoidOpTaskIdentityResolve
	// back `boid task identity link/unlink/resolve` from inside the sandbox
	// (docs/plans/ingestion-identity.md PR-1, B-1): the identity index
	// (task_identities table, external key -> task, I-1/I-2/I-3). This PR
	// only wires the index itself — nothing pushes into it yet (PR-2 wires
	// the actual observation-resolution path); a workspace's own bulk
	// migration script is the first real caller of Link.
	//
	// Scoping is broker-authoritative, matching BoidOpTaskCreate exactly:
	// ProjectID defaults from the token's own context when omitted, gets
	// resolved via ProjectResolver, and is checked against
	// TokenContext.AllowsProject BEFORE the executor ever sees the request
	// (broker.go). This is deliberate given the review history on this
	// exact class of bug (PR-4/PR-5 codex review, 3 separate rounds):
	// scoping a project-id-bearing op only in the executor and leaving the
	// broker to pass it through unscoped. BoidOpTaskIdentityLink additionally
	// carries a caller-supplied TaskID (not itself the scope, since it names
	// an EXISTING task rather than the project being written into); its
	// ownership is checked in the executor the same way as BoidOpActionSend's
	// TaskID (GetTask + AllowsProject), since the broker has no TaskStore to
	// resolve it against — but unlike action_send, which merely
	// re-look-up their TaskID downstream and so tolerate a short (>=8-char)
	// prefix transparently, Link writes the id it's given straight into
	// task_identities.task_id, an FK column. It MUST use GetTask's resolved
	// existing.ID there, never the caller-supplied req.TaskID verbatim — a
	// still-a-prefix write is a raw SQLite FOREIGN KEY constraint failure,
	// not a clean error.
	//
	// BoidOpTaskIdentityResolve represents "no such binding" via a distinct
	// ExecResponse.ExitCode (IdentityNotFoundExitCode below) rather than the
	// generic ExitCode:1 error path every other op uses for failure — the
	// design doc calls this out explicitly ("未登録は「見つからない」を exit
	// code で表し、エラーにしない"): a caller doing get-or-create needs to
	// tell "not found" apart from a real error without parsing stderr text.
	// BoidOpTaskIdentityLink has its own distinguished code for the OTHER
	// direction the design doc calls out (PR-2 section): a conflicting link
	// (IdentityConflictExitCode below) must also be machine-distinguishable,
	// not just a stderr string a caller has to pattern-match.
	BoidOpTaskIdentityLink    BoidOp = "task_identity_link"
	BoidOpTaskIdentityUnlink  BoidOp = "task_identity_unlink"
	BoidOpTaskIdentityResolve BoidOp = "task_identity_resolve"

	// BoidOpTaskResolveOrCapture backs `boid task resolve-or-capture
	// <identity> [--title T] [--description D|--description-file F]` from
	// inside the sandbox (docs/plans/ingestion-identity.md PR-2, B-2): the
	// destination-resolution half of I-4 — resolve Identity to an existing
	// task, or atomically create a new `captured` triage task and link
	// Identity to it when unresolved. This is deliberately a SEPARATE op
	// from BoidOpTaskIdentityLink/BoidOpActionSend, not a variant of either
	// — "解決と記録を 1 op に混ぜない" (PR-2 節): the record vocabulary
	// (attrs_set / child_added / …) already exists on action_send, and
	// mixing destination-resolution into it would give that op two
	// responsibilities. When the result's Created is false, the caller is
	// expected to follow up with a normal BoidOpActionSend (attrs_set) —
	// this op never touches actions.
	//
	// Scoping is broker-authoritative, matching BoidOpTaskIdentityLink
	// exactly (default from ctx, resolve via ProjectResolver, AllowsProject
	// BEFORE the executor ever sees the request — broker.go). Unlike Link,
	// there is no caller-supplied TaskID to separately verify: every task
	// this op can return or create is scoped to req.ProjectID by
	// construction (a fresh task is created WITH ProjectID=req.ProjectID;
	// an existing one is found via ResolveIdentity(req.ProjectID, ...), so
	// it can only ever be a task already scoped to that same project) — no
	// analogue of Link's "existing.ProjectID != req.ProjectID" cross-check
	// is needed here.
	//
	// Conflict (Identity already bound to a DIFFERENT task) is represented
	// the same way BoidOpTaskIdentityLink represents it: IdentityConflictExitCode
	// below, not a generic ExitCode:1 — the design doc requires PR-1's
	// ErrIdentityConflict to survive machine-readably ("identity 衝突時の
	// エラー語彙は PR-1 の ErrIdentityConflict をそのまま返す") so the
	// caller can route it to the integration judgment call it names
	// (「統合」の判断) rather than treating it as a generic failure.
	BoidOpTaskResolveOrCapture BoidOp = "task_resolve_or_capture"

	// BoidOpActionList backs `boid action list` from inside the sandbox
	// (docs/plans/ingestion-identity.md PR-3, B-3): the workspace-scoped,
	// since-cursor read over actions — the missing read half of
	// BoidOpActionSend (「action_send で書けるのに読めない」, 本 doc 3 節).
	//
	// Deliberately a SINGLE workspace-wide read, not per-task
	// (ListActionsByTask's existing shape): a tick script that has to list
	// every triage task and read each one's actions separately would issue
	// O(N) brokered ops per cycle (「B-3: per-task だけだと tick が O(N) に
	// なる」節) — this op lets the "反応型はスクリプトが書ける" (J-3) argument
	// hold at the actual implementation-cost level, not just in principle.
	//
	// Scoping is broker-authoritative, matching BoidOpCardList EXACTLY
	// (project_id resolve+check / workspace_id equality check / neither ->
	// inject the token's own WorkspaceID — see broker.go's case). TaskID
	// additionally narrows to one task's actions; unlike
	// BoidOpTaskIdentityLink's caller-supplied TaskID (which the broker
	// cannot verify and must defer to the executor's GetTask+AllowsProject),
	// this TaskID is always ANDed together with the already-broker-verified
	// project/workspace scope in the SQL query itself
	// (orchestrator.ListActionsSince) — a TaskID outside the caller's scope
	// simply matches zero rows, it can never widen what comes back.
	BoidOpActionList BoidOp = "action_list"

	// BoidOpSignalList / BoidOpSignalAck back `boid signal list [--claim]
	// [--source ...] [--state ...] [--limit N]` / `boid signal ack <id>...`
	// from inside the sandbox (docs/plans/signal-ingest-detailed-design.md
	// §3.2, PR-3): the judgment-side read/decide surface over the signal
	// inbox (signals/signal_cursors, migration 0046, PR-1's signal_store.go).
	// Both are part of the general boidPolicy (policy.go) — any hook/exec job
	// may scan its own workspace's inbox and ack a Signal once it has
	// written a judgment for it, the same "workspace membership is the
	// gate, not role" posture as BoidOpCardList/BoidOpActionList.
	//
	// BoidOpSignalList's --claim flag routes to ClaimSignals (attempts++)
	// instead of the plain read ListSignals — see BoidRequest.Claim.
	// BoidOpSignalAck is idempotent by construction: AckSignals only ever
	// sets acked_at WHERE acked_at IS NULL, so acking an already-acked id is
	// a no-op success, not an error (design doc §2, Q14).
	BoidOpSignalList BoidOp = "signal_list"
	BoidOpSignalAck  BoidOp = "signal_ack"

	// BoidOpSignalIngest / BoidOpSignalCursorGet back `boid signal ingest`
	// (stdin JSONL) / `boid signal cursor` from inside a connector's exec
	// job (design doc §3.2, §5.3 connector プロセス契約). Declared here
	// (protocol / mirror / escape-manifest) for completeness, but
	// DELIBERATELY NOT added to the general boidPolicy in policy.go's
	// boidPolicy: design doc §3.2 is explicit that granting these two ops is
	// PR-5's job (a connector-scoped, reduced policy handed only to derived
	// trigger exec jobs). As of PR-3 these are "nobody can call them" ops —
	// fully wired end-to-end (broker scoping + executor logic + tests) but
	// unreachable because no policy names them yet. See boidPolicy's own
	// doc comment in policy.go for the matching note from the policy side.
	BoidOpSignalIngest    BoidOp = "signal_ingest"
	BoidOpSignalCursorGet BoidOp = "signal_cursor_get"
)

// IdentityNotFoundExitCode is BoidOpTaskIdentityResolve's distinguished exit
// code for "no task is bound to this identity" — deliberately NOT 0
// (success) or 1 (every other op's generic failure code), so a caller can
// tell "not found" apart from a real error (broken broker connection,
// unavailable executor, ...) purely from the exit code, without parsing
// stderr text. See BoidOpTaskIdentityResolve's own doc comment.
const IdentityNotFoundExitCode = 2

// IdentityConflictExitCode is BoidOpTaskIdentityLink's distinguished exit
// code for "this identity is already bound to a DIFFERENT task"
// (orchestrator.ErrIdentityConflict) — its own code, distinct from 0
// (success), 1 (generic failure), and IdentityNotFoundExitCode (a resolve
// miss). docs/plans/ingestion-identity.md's PR-2 section requires this to be
// machine-readable ("identity 衝突時のエラー語彙は PR-1 の ErrIdentityConflict
// をそのまま返す。workspace はこれを受けて統合の判断へ回す") — a caller must be
// able to tell a conflict apart from any other failure without depending on
// Stderr's exact wording, which is not a public contract.
const IdentityConflictExitCode = 3

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

	// Identity carries the opaque external-key string for
	// BoidOpTaskIdentityLink / BoidOpTaskIdentityUnlink /
	// BoidOpTaskIdentityResolve / BoidOpTaskResolveOrCapture
	// (docs/plans/ingestion-identity.md PR-1/PR-2). Never interpreted by the
	// daemon (I-2) — validated only for non-emptiness. Link additionally
	// uses TaskID (the task to bind); scope is ProjectID for all four, same
	// field every other project-scoped op already uses.
	Identity string `json:"identity,omitempty"`

	// Title / Description carry BoidOpTaskResolveOrCapture's new-task fields
	// (docs/plans/ingestion-identity.md PR-2, B-2) — used ONLY when Identity
	// is unresolved and a fresh `captured` task is created; ignored when the
	// identity already resolves to an existing task. Deliberately plain
	// strings rather than a CreatePatch (unlike BoidOpTaskCreate) — the
	// design doc scopes this op's input to "project + identity + 新規時の
	// title / description" only, not the full CreateTaskRequest surface
	// (behavior/traits/readonly/etc. all come from the resolved default
	// behavior, same as khi's existing captured/triaged ensure_task calls).
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`

	// Since carries BoidOpActionList's opaque cursor (I-9's since カーソル,
	// orchestrator.EncodeActionCursor's output) — empty means "from the
	// beginning". TaskID/ProjectID/WorkspaceID/Limit (already declared
	// above) round out the op's other inputs; no new fields are needed for
	// those.
	Since string `json:"since,omitempty"`

	// Signal ops fields (docs/plans/signal-ingest-detailed-design.md §3.2,
	// PR-3). WorkspaceID (already declared above) is the scope for all four
	// signal ops, but is NEVER caller-supplied for them — the shim never
	// sets it (there is no --workspace-id flag in the sandbox-side signal
	// grammar) and the broker unconditionally overwrites whatever value (if
	// any) a hand-crafted request carries with entry.Context.WorkspaceID
	// (design doc: "引数で workspace を指定させない").
	//
	// Service/Connector serve two different ops in two different ways:
	//   - BoidOpSignalList: Connector is the optional --source filter
	//     (Service has no sandbox-side flag — host CLI only, PR-2). Not
	//     broker-enforced beyond normal read scoping — a filter narrowing
	//     what the caller's OWN workspace shows is not a security boundary.
	//   - BoidOpSignalIngest / BoidOpSignalCursorGet: both fields are the
	//     connector's own identity. The shim populates them from the
	//     BOID_SIGNAL_SERVICE / BOID_SIGNAL_CONNECTOR environment variables
	//     (never a CLI flag), but that alone is only a well-behaved-shim
	//     convention, not enforcement — the broker is what actually makes
	//     this a security boundary: BoidOpSignalIngest/BoidOpSignalCursorGet's
	//     broker.go case unconditionally OVERWRITES these fields with
	//     TokenContext.Service/Connector (M2 of PR #1014's review: an
	//     earlier version of this comment claimed the env-only shim path
	//     alone prevented a connector from addressing another source's
	//     inbox/cursor, which was false — nothing stopped a hand-crafted
	//     ExecRequest that bypassed the shim from setting these fields to
	//     an arbitrary value; only the broker's overwrite closes that).
	Service   string `json:"service,omitempty"`
	Connector string `json:"connector,omitempty"`

	// Claim selects BoidOpSignalList's ClaimSignals path (attempts++
	// included) over the plain ListSignals read — `boid signal list
	// --claim`.
	Claim bool `json:"claim,omitempty"`

	// SignalState filters BoidOpSignalList by orchestrator.SignalState
	// (pending|dead|acked|all) — `boid signal list --state <state>`. Empty
	// defaults to "pending" (design doc §3.2's "未 ack の Signal" framing),
	// the same default orchestrator.ListSignals itself applies for an empty
	// SignalFilter.State. A distinct field from Status (which carries task
	// status for the task_* ops) — the two are unrelated vocabularies and
	// sharing a field would silently conflate them.
	SignalState string `json:"signal_state,omitempty"`

	// SignalIDs carries `boid signal ack <id>...`'s positional id list (1 or
	// more). AckSignals matches by (workspace_id, id) only and is idempotent
	// per id (Q14) — repeating an already-acked id here is a no-op success.
	SignalIDs []string `json:"signal_ids,omitempty"`

	// IngestPayload carries the raw JSONL bytes `boid signal ingest` reads
	// from stdin, capped by the shim at PayloadPatchMaxBytes (design doc
	// §3.2: "既存 PayloadPatchMaxBytes と同値" — reusing that constant rather
	// than defining a second one for the same 10 MiB limit). Parsing each
	// line into orchestrator.SignalIngestRow and validating the required
	// fields (id/occurred_at/identity) happens server-side in the executor,
	// not in the shim — matching how BoidOpTaskUpdatePayloadPatch's
	// PayloadPatch is parsed server-side rather than in boid_shim.go.
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
	// spec.Visibility.ProjectDir (which drives sandbox mount layout and is
	// intentionally empty for gate jobs): host-side operations the broker
	// performs on behalf of the sandbox (host-command cwd) have their own
	// notion of "which project are we operating on" that doesn't care
	// whether the sandbox itself can see the tree.
	ProjectDir string
	// Service / Connector are the token-registration-time-stamped identity
	// a connector job is authorized to ingest/read the cursor for
	// (docs/plans/signal-ingest-detailed-design.md §3.2/§5.2, PR-3's M2
	// review fix). BoidOpSignalIngest / BoidOpSignalCursorGet's broker case
	// (broker.go) unconditionally overwrites BoidRequest.Service/Connector
	// with these values — the SAME "broker-injected, never
	// caller-supplied" pattern WorkspaceID already uses for every signal
	// op. Before this fix, the shim-populated BoidRequest.Service/Connector
	// (read from the BOID_SIGNAL_SERVICE/BOID_SIGNAL_CONNECTOR env inside
	// the job) were trusted as-is by the broker — a correctly-behaving shim
	// never lets an agent override those env vars via a CLI flag, but
	// nothing stopped a hand-crafted ExecRequest (bypassing the shim
	// entirely) from claiming an arbitrary Service/Connector and reading or
	// writing another source's inbox rows/cursor. Empty for every job today
	// (PR-3 ships no caller that sets these — that's PR-5's job, when it
	// registers a connector-scoped token), which is exactly why
	// signal_ingest/signal_cursor_get stay unreachable in practice even
	// once PR-5 eventually grants the op via its own reduced policy, until
	// PR-5 ALSO populates these two fields at registration time.
	Service   string
	Connector string
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
