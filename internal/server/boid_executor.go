package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// jobContextProvider resolves the Phase 5b PR1 task-context RPC data
// (docs/plans/phase5-shim-and-task-context.md) that has no standalone DB
// representation — the reduced environment view + trait-filtered payload —
// tracked per job by dispatcher.Runner at Dispatch() time. Kept as a narrow
// interface (rather than depending on *dispatcher.Runner's full surface) so
// boid_executor's dependency on dispatcher stays this one method;
// *dispatcher.Runner satisfies it structurally.
type jobContextProvider interface {
	JobContext(jobID string) (dispatcher.JobContextSnapshot, bool)
}

// projectLookup is narrowed from *api.ProjectAppService (which satisfies it
// structurally) down to the single method BoidOpProjectBehaviors and
// BoidOpProjectList need, mirroring jobContextProvider's narrowing of
// *dispatcher.Runner — keeps boid_executor's dependency surface minimal and
// its tests free of the full ProjectAppService's Meta-store interface.
type projectLookup interface {
	GetProject(id string) (*orchestrator.Project, error)
}

// resolveOrCaptureService is narrowed from api.WorkflowService down to the
// single method BoidOpTaskResolveOrCapture needs
// (docs/plans/ingestion-identity.md PR-2, B-2), mirroring
// jobContextProvider/projectLookup's own narrowing above. Deliberately kept
// OUT of api.WorkflowService itself (rather than adding a 6th method there)
// so the 3 existing WorkflowService test doubles in this package and
// internal/api/web_test.go (recordingWorkflow, askWorkflowStub,
// stubWorkflowService) do not all need a new pass-through method just to
// keep compiling — newBoidBuiltinExecutor below does a runtime interface
// check against the SAME workflow value callers already pass in, so
// production wiring (wire.go's *api.TaskWorkflowService) picks this up with
// zero constructor/wire.go changes once TaskWorkflowService.ResolveOrCapture
// exists, while every test double that doesn't implement it simply leaves
// this field nil (→ "unavailable", same convention as every other optional
// dependency here).
type resolveOrCaptureService interface {
	ResolveOrCapture(ctx context.Context, req api.ResolveOrCaptureRequest) (*api.ResolveOrCaptureResult, error)
}

// actionListService is narrowed from api.WorkflowService down to the single
// method BoidOpActionList needs (docs/plans/ingestion-identity.md PR-3, B-3),
// mirroring resolveOrCaptureService's own narrowing immediately above — kept
// OUT of api.WorkflowService itself for the identical reason (every existing
// WorkflowService test double in this package and internal/api/web_test.go
// would otherwise need a new pass-through method just to keep compiling).
// newBoidBuiltinExecutor does the same runtime interface check against the
// SAME workflow value callers already pass in, so production wiring picks
// this up with zero constructor/wire.go changes once
// TaskWorkflowService.ListActions exists (it already does — action_list_read.go),
// while every test double that doesn't implement it leaves this field nil
// (→ "unavailable"), same convention as every other optional dependency here.
type actionListService interface {
	ListActions(filter orchestrator.ActionListFilter) (*api.ActionListResult, error)
}

// projectSummary is BoidOpProjectList's per-project JSON shape — deliberately
// leaner than BoidOpProjectBehaviors' output (no task_behaviors): the list op
// is for discovery ("what projects can I even ask about"), and a caller that
// needs a given project's behaviors calls BoidOpProjectBehaviors on it by id.
//
// CloneURL/ReferencePath/CloneDir (peer-discovery feature) are populated
// only for a workspace peer whose id appears in this job's
// JobContextSnapshot.WorkspacePeerAdvertise — never for the caller's own
// (self) entry, and never overwriting Name (which stays proj.Meta.Name;
// dispatcher.PeerAdvertise.Name has a different meaning — the upstream_url
// repo basename). Empty when the job isn't in clone mode, the gateway isn't
// wired, or the peer has no resolvable upstream_url — a caller must not
// assume these are always populated; fall back to a plain `git clone
// <upstream_url>` when empty. ReferencePath in particular may point to a
// path that was never actually mounted (git-URL-registered / bare-repo
// projects have no `.git` at their WorkDir for cloneMounts to bind) — check
// it exists before passing it to `git clone --reference`. CloneDir is a
// suggestion only and may collide with another peer's or self's directory
// name; pick a different target if it already exists.
type projectSummary struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	UpstreamURL   string `json:"upstream_url,omitempty"`
	CloneURL      string `json:"clone_url,omitempty"`
	ReferencePath string `json:"reference_path,omitempty"`
	CloneDir      string `json:"clone_dir,omitempty"`
}

type boidBuiltinExecutor struct {
	workflow    api.WorkflowService
	tasks       *api.TaskAppService
	jobs        api.JobStore
	logReader   api.JobLogReader
	jobContexts jobContextProvider
	// attachmentsRoot is the data-home directory under which per-task
	// attachments live (`<attachmentsRoot>/tasks/<task_id>/attachments`),
	// backing the Phase 5b PR2 attachments RPCs
	// (docs/plans/phase5-shim-and-task-context.md). It is the same value
	// wire.go threads into api.WebHandler.AttachmentsRoot (the upload path)
	// — see wiring-seams.md #15 — so the RPC reply can never drift from what
	// the upload path writes. Empty disables the two ops with an
	// "unavailable" error rather than panicking.
	attachmentsRoot string
	// projects backs BoidOpProjectBehaviors (`boid project behaviors` from
	// inside the sandbox). nil disables the op with an "unavailable" error
	// rather than panicking, same convention as every other optional
	// dependency here.
	projects projectLookup
	// resolveOrCapture backs BoidOpTaskResolveOrCapture
	// (docs/plans/ingestion-identity.md PR-2, B-2). Populated in
	// newBoidBuiltinExecutor via a runtime interface check against the SAME
	// workflow value (see resolveOrCaptureService's own doc comment for
	// why) — nil disables the op with an "unavailable" error, same
	// convention as every other optional dependency here.
	resolveOrCapture resolveOrCaptureService
	// actionList backs BoidOpActionList (docs/plans/ingestion-identity.md
	// PR-3, B-3). Same runtime-interface-check wiring as resolveOrCapture
	// above — see actionListService's own doc comment.
	actionList actionListService
	// signals backs BoidOpSignalList / BoidOpSignalAck / BoidOpSignalIngest /
	// BoidOpSignalCursorGet (docs/plans/signal-ingest-detailed-design.md
	// §3.2, PR-3).
	//
	// [M1, review of PR #1014, 2026-08-26] This field was ORIGINALLY wired
	// with a runtime-interface-check against `workflow`
	// (`if sig, ok := workflow.(api.SignalStore); ok`), mirroring
	// resolveOrCapture/actionList below — but that pattern only works when
	// SOME concrete type passed as `workflow` at production wire-up time
	// actually implements the target interface's methods. Unlike
	// resolveOrCaptureService/actionListService (whose methods were added
	// directly to *api.TaskWorkflowService in the same PR that introduced
	// the check), api.SignalStore's 6 methods have never been added to
	// *api.TaskWorkflowService, and no other PR was ever assigned to do so
	// — production wire.go passes a *api.TaskWorkflowService that does NOT
	// implement api.SignalStore, so the check always resolved to nil and
	// every signal op silently replied "unavailable" from the moment it
	// shipped. Confirmed by a failing
	// TestNewBoidBuiltinExecutor_WiresSignalsFromRealTaskRepository-shaped
	// probe against the real production type before this fix.
	//
	// Fixed by taking `signals` as an EXPLICIT constructor parameter
	// instead: wire.go passes `taskRepo` directly (the same
	// *orchestrator.TaskRepository value already in scope there, which
	// PR-1's `var _ SignalStore = (*orchestrator.TaskRepository)(nil)`
	// assertion in internal/api/store.go proves satisfies this interface) —
	// no runtime type assertion, no possibility of the check silently
	// resolving to nil. Nil is still tolerated here (every other optional
	// dependency in this struct follows the same "nil disables the op with
	// an 'unavailable' error" convention), but nil now means "the caller
	// deliberately passed no store" rather than "a type assertion that
	// nobody verified against the real production type happened to fail".
	signals api.SignalStore
}

func newBoidBuiltinExecutor(workflow api.WorkflowService, tasks *api.TaskAppService, jobs api.JobStore, logReader api.JobLogReader, jobContexts jobContextProvider, attachmentsRoot string, projects projectLookup, signals api.SignalStore) sandbox.BoidExecutor {
	if workflow == nil && tasks == nil {
		return nil
	}
	var resolveOrCapture resolveOrCaptureService
	if roc, ok := workflow.(resolveOrCaptureService); ok {
		resolveOrCapture = roc
	}
	var actionList actionListService
	if al, ok := workflow.(actionListService); ok {
		actionList = al
	}
	return &boidBuiltinExecutor{
		workflow:         workflow,
		tasks:            tasks,
		jobs:             jobs,
		logReader:        logReader,
		jobContexts:      jobContexts,
		attachmentsRoot:  attachmentsRoot,
		projects:         projects,
		resolveOrCapture: resolveOrCapture,
		actionList:       actionList,
		signals:          signals,
	}
}

// validateTaskListStatus rejects status values ListTasks doesn't understand
// before they reach the DB layer, where they were previously passed through
// unvalidated (docs/plans/cross-project-issue-triage.md Phase 1 実測結果 項10).
// Empty (no filter), the special keywords ("open"/"closed"/"cards_live"/
// "triage" — the last two are the SAME predicate, docs/plans/
// webui-detail-list-redesign.md PR-4 §3.6 renamed "triage" to "cards_live"
// and kept "triage" as a compatibility alias for any explicit caller), and
// any exact orchestrator.TaskStatus value are accepted.
//
// "queue_next" is ALSO still accepted here (unlike "queue" below) even
// though PR-4 removed its special-cased predicate from store.go's
// ListTasks entirely: it now falls through to store.go's generic
// `t.status = ?` literal-equality branch, which can never match a real row
// (queue_next is not a valid tasks.status value) and so deterministically
// returns an empty list — the decided §5 論点3 answer (empty, not an
// error; see store.go's own doc comment and
// orchestrator.TestListTasks_QueueNext_ReturnsEmpty). Rejecting it here
// would contradict that decision by turning a query that used to return
// real rows into a hard CLI error instead of the chosen "quietly empty"
// behavior.
//
// "queue" (the old broad pre-execution-status superset, PR-1 of
// cross-project-issue-triage.md) is REMOVED as of PR-2: the Web UI never
// used it (only queue_next/parked/open/closed reach web.go's TaskList) and
// store.go's ListTasks no longer has a dedicated branch for it. Rejecting it
// here surfaces the removal as a clear "unknown status" error rather than
// silently falling through to the generic `t.status = 'queue'` literal
// match, which can never match any real row and would otherwise look like a
// permanently-empty list instead of an error.
func validateTaskListStatus(status string) error {
	switch status {
	case "", "open", "closed", "queue_next", "cards_live", "triage":
		return nil
	}
	if _, ok := orchestrator.ParseTaskStatus(status); ok {
		return nil
	}
	return fmt.Errorf("boid task list: unknown status %q", status)
}

func (e *boidBuiltinExecutor) ExecuteBoidBuiltin(goCtx context.Context, ctx sandbox.TokenContext, req *sandbox.BoidRequest) *sandbox.ExecResponse {
	if req == nil {
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: "missing boid request"}
	}

	// docs/plans/boid-internal-signal-inbox.md §4.3/§6.2: this is the ONE
	// place in the whole daemon that ever calls WithWriterProjectID — every
	// sandbox-originated write reaches CreateAction's ingest step (via
	// whichever api.* call below it takes) carrying ctx.ProjectID as its
	// "who wrote this" fact, so orchestrator.CreateAction can tell a
	// metaproject's own self-authored write (must not ingest — loop risk)
	// from everything else, without threading sandbox.TokenContext itself
	// through every intermediate call. Every OTHER ctx-building path in this
	// codebase (HTTP handlers, daemon loops) never touches this key, so
	// WriterProjectIDFromContext naturally reports "not a sandbox write" for
	// all of them with zero special-casing there. Below, every call site
	// that used to pass a bare context.Background() now passes goCtx (or a
	// value built from it) so this stays attached all the way down.
	goCtx = orchestrator.WithWriterProjectID(goCtx, ctx.ProjectID)

	switch req.Op {
	case sandbox.BoidOpJobDone:
		if e.workflow == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid job done unavailable"}
		}
		if _, err := e.workflow.CompleteJob(goCtx, req.JobID, api.JobDoneRequest{
			ExitCode: req.ExitCode,
			Output:   req.Output,
		}); err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{
			Stdout: fmt.Sprintf("job %s completed (exit_code=%d)\n", req.JobID, req.ExitCode),
		}
	case sandbox.BoidOpAgentStop:
		// agent stop: ask the daemon to deliver SIGUSR1 to the runtime pgrp.
		// claude.Adapter.Run()'s signal.Notify handler translates that into a
		// SIGTERM toward the claude child while the surrounding runner-inner-
		// child keeps running and posts `boid job done` through the broker
		// directly (internal/sandbox/runner.postJobDone) — that callback is
		// the sole CompleteJob path, so the broker token must stay valid
		// until then. Mirrors NotifyTask's StopAgent path; do NOT call
		// CompleteJob here.
		if e.workflow == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid agent stop unavailable"}
		}
		if e.jobs == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid agent stop unavailable (no job store)"}
		}
		job, err := e.jobs.GetJob(req.JobID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		if job.RuntimeID == "" {
			// No runtime to signal — likely a host-foreground job that
			// shouldn't have called agent stop in the first place. Treat as
			// a no-op success so the caller can `exit` afterwards if needed.
			return &sandbox.ExecResponse{
				Stdout: fmt.Sprintf("agent stop: job %s has no runtime\n", req.JobID),
			}
		}
		e.workflow.StopAgent(job.RuntimeID)
		return &sandbox.ExecResponse{
			Stdout: fmt.Sprintf("agent stop signalled for job %s\n", req.JobID),
		}
	case sandbox.BoidOpTaskCreate:
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task create unavailable"}
		}
		var createReq api.CreateTaskRequest
		if len(req.CreatePatch) > 0 {
			if err := json.Unmarshal(req.CreatePatch, &createReq); err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task create: invalid create_patch: " + err.Error()}
			}
		}
		// InitialStatus (docs/plans/cross-project-issue-triage.md Phase 1
		// PR-1) was deliberately NOT allowed through the brokered path until
		// PR-4 (論点1/4/7, Fable レビュー第9版): this is the "ingestion push
		// opens up" change. What makes it safe now:
		//
		//   - resolveInitialStatus's allowlist (task_create.go) still limits
		//     the reachable statuses to captured/triaged/pending — a
		//     sandboxed job can never fabricate a task that's already
		//     ready/working/done/etc.
		//   - createReq.ProjectID is resolved to a UUID above (from the
		//     broker's own resolution or ctx.ProjectID — never trusted
		//     verbatim from the sandboxed caller's create_patch), and the
		//     ctx.AllowsProject check right below still gates against the
		//     token's own workspace scope. A sandboxed job cannot claim
		//     "create a triaged task in a project outside my workspace" any
		//     more than it could before this PR — AllowsProject is the same
		//     workspace-scoping check task_get/task_update/action_send etc.
		//     already use, so ingestion push does not trust any self-declared
		//     workspace claim from the create_patch payload.
		//   - dedup (論点7): CreateTask's Ref get-or-create (task_create.go),
		//     now open to root tasks too, makes a crash-and-resend from khi
		//     idempotent instead of planting a duplicate card.
		//
		// broker が req.ProjectID を UUID に解決済みの場合は必ず優先する。
		// CreatePatch.project_id は元の名前のまま (未上書き) のため使用しない。
		if req.ProjectID != "" {
			createReq.ProjectID = req.ProjectID
		} else if createReq.ProjectID == "" {
			createReq.ProjectID = ctx.ProjectID
		}
		if createReq.ProjectID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task create requires a project"}
		}
		if createReq.ParentID == orchestrator.ParentIDSentinelRoot {
			createReq.ParentID = ""
		} else if createReq.ParentID == "" {
			createReq.ParentID = ctx.TaskID
		}
		if createReq.ParentID != "" && createReq.Ref == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "child create requires a stable ref; pass ref: <slug> in the task spec"}
		}
		if !ctx.AllowsProject(createReq.ProjectID) {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task create is restricted to the current workspace"}
		}
		task, err := e.tasks.CreateTask(createReq)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{
			Stdout: fmt.Sprintf("task created: %s (%s)\n", task.ID, task.Status),
		}
	case sandbox.BoidOpTaskGet:
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task show unavailable"}
		}
		// broker only defaults an *empty* TaskID from the token context
		// (BoidOpTaskGet's own case in broker.go) and otherwise passes an
		// explicit caller-supplied one through untouched — unlike task_update
		// / task_notify / task_answer / task_delete, which all look the
		// target task up here before touching it, this op used to skip that
		// check entirely, letting a caller read another workspace's task
		// fields (title/description/status/payload/...) as long as it knew
		// the task UUID. Look the task up and enforce the same
		// AllowsProject gate the other task-scoped ops use.
		existing, err := e.tasks.GetTask(req.TaskID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		if !ctx.AllowsProject(existing.ProjectID) {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task show is restricted to the current workspace"}
		}
		value, err := e.tasks.GetTaskField(req.TaskID, req.TaskField)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{Stdout: value}
	case sandbox.BoidOpTaskUpdate:
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task update unavailable"}
		}
		if req.TaskID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task update requires a task id"}
		}
		existing, err := e.tasks.GetTask(req.TaskID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		if !ctx.AllowsProject(existing.ProjectID) {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task update is restricted to the current workspace"}
		}
		var updateReq api.UpdateTaskRequest
		if len(req.UpdatePatch) > 0 {
			if err := json.Unmarshal(req.UpdatePatch, &updateReq); err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task update: invalid update_patch: " + err.Error()}
			}
		}
		task, err := e.tasks.UpdateTask(req.TaskID, updateReq)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{
			Stdout: fmt.Sprintf("task updated: %s (%s)\n", task.ID, task.Status),
		}
	case sandbox.BoidOpTaskReopen:
		if e.workflow == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task reopen unavailable"}
		}
		if req.TaskID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task reopen requires a task id"}
		}
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task reopen unavailable"}
		}
		// Same authorization gap as BoidOpTaskGet above: this op used to call
		// ApplyAction directly with no lookup of the target task's
		// project_id, letting a caller reopen (and thereby dispatch) another
		// workspace's task as long as it knew the task UUID. Look the task up
		// and enforce the same AllowsProject gate task_update / task_notify /
		// task_answer / task_delete already use.
		existing, err := e.tasks.GetTask(req.TaskID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		if !ctx.AllowsProject(existing.ProjectID) {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task reopen is restricted to the current workspace"}
		}
		applyReq := api.ApplyActionRequest{Type: "reopen"}
		if req.Message != "" {
			payload, err := json.Marshal(map[string]any{
				"instruction": map[string]any{
					"message": req.Message,
				},
			})
			if err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task reopen: marshal instruction: %s", err)}
			}
			applyReq.Payload = payload
		}
		if _, err := e.workflow.ApplyAction(orchestrator.WithActor(goCtx, orchestrator.ActorTask(ctx.TaskID)), req.TaskID, applyReq); err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{
			Stdout: fmt.Sprintf("task %s reopened\n", req.TaskID),
		}
	case sandbox.BoidOpTaskNotify:
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task notify unavailable"}
		}
		if req.TaskID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task notify requires a task id"}
		}
		existing, err := e.tasks.GetTask(req.TaskID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		if !ctx.AllowsProject(existing.ProjectID) {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task notify is restricted to the current workspace"}
		}
		if err := e.tasks.NotifyTask(goCtx, req.TaskID, req.Message, req.Ask, req.QuestionID, req.Progress, req.Done, req.Fail); err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{
			Stdout: fmt.Sprintf("notified: %s\n", req.TaskID),
		}
	case sandbox.BoidOpTaskAnswer:
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task answer unavailable"}
		}
		if req.TaskID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task answer requires a task id"}
		}
		existing, err := e.tasks.GetTask(req.TaskID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		if !ctx.AllowsProject(existing.ProjectID) {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task answer is restricted to the current workspace"}
		}
		if err := e.tasks.AnswerTask(orchestrator.WithActor(goCtx, orchestrator.ActorTask(ctx.TaskID)), req.TaskID, req.QuestionID, req.Answer); err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{
			Stdout: fmt.Sprintf("answered: %s\n", req.TaskID),
		}
	case sandbox.BoidOpTaskAsk:
		// Harness-independent blocking Q&A: AskTaskBlocking transitions the task
		// to awaiting and blocks (on goCtx) until the user/supervisor answers.
		// goCtx is cancelled by the broker on daemon shutdown / sandbox
		// disconnect, so the wait cannot hang forever.
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task ask unavailable"}
		}
		taskID := req.TaskID
		if taskID == "" {
			taskID = ctx.TaskID
		}
		if taskID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task ask requires a task id"}
		}
		existing, err := e.tasks.GetTask(taskID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		if !ctx.AllowsProject(existing.ProjectID) {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task ask is restricted to the current workspace"}
		}
		answer, err := e.tasks.AskTaskBlocking(goCtx, taskID, req.Question)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{Stdout: answer}
	case sandbox.BoidOpTaskList:
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task list unavailable"}
		}
		if err := validateTaskListStatus(req.Status); err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		var tasks []*orchestrator.Task
		if req.ProjectID != "" {
			listed, err := e.tasks.ListTasks(orchestrator.TaskFilter{ProjectID: req.ProjectID, Status: req.Status})
			if err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
			}
			tasks = listed
		} else if req.WorkspaceID != "" {
			listed, err := e.tasks.ListTasks(orchestrator.TaskFilter{WorkspaceID: req.WorkspaceID, Status: req.Status})
			if err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
			}
			tasks = listed
		} else {
			// workspace 未割当: AllowedProjectIDs でフィルタ (= self project のみ)
			projectIDs := ctx.AllowedProjectIDs
			if len(projectIDs) == 0 {
				projectIDs = []string{ctx.ProjectID}
			}
			for _, pid := range projectIDs {
				listed, err := e.tasks.ListTasks(orchestrator.TaskFilter{ProjectID: pid, Status: req.Status})
				if err != nil {
					return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
				}
				tasks = append(tasks, listed...)
			}
		}
		var sb strings.Builder
		for _, t := range tasks {
			fmt.Fprintf(&sb, "%-36s %-12s %s\n", t.ID, t.Status, t.Title)
		}
		return &sandbox.ExecResponse{Stdout: sb.String()}
	case sandbox.BoidOpTaskImport:
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task import unavailable"}
		}
		var reqs []api.CreateTaskRequest
		for i, raw := range req.ImportTasks {
			var r api.CreateTaskRequest
			if err := json.Unmarshal(raw, &r); err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task import: line %d: invalid task json: %s", i+1, err)}
			}
			if req.ImportProjectOverride != "" {
				r.ProjectID = req.ImportProjectOverride
			}
			if r.ProjectID == "" {
				r.ProjectID = ctx.ProjectID
			}
			reqs = append(reqs, r)
		}
		result, err := e.tasks.ImportTasks(reqs)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		stdout := fmt.Sprintf("Created: %d, Skipped: %d, Errors: %d\n", result.Created, result.Skipped, len(result.Errors))
		var stderrBuf strings.Builder
		for _, importErr := range result.Errors {
			fmt.Fprintf(&stderrBuf, "error line %d (remote_id=%s): %s\n", importErr.Line, importErr.RemoteID, importErr.Error)
		}
		return &sandbox.ExecResponse{Stdout: stdout, Stderr: stderrBuf.String()}
	case sandbox.BoidOpProjectBehaviors:
		if e.projects == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid project behaviors unavailable"}
		}
		proj, err := e.projects.GetProject(req.ProjectID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		out, err := json.MarshalIndent(proj.Meta.TaskBehaviors, "", "  ")
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{Stdout: string(out) + "\n"}
	case sandbox.BoidOpProjectList:
		if e.projects == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid project list unavailable"}
		}
		// No caller-supplied scope: ctx.AllowedProjectIDs is the token's
		// stamped-at-dispatch workspace peer set (dispatcher.allowedProjectIDs),
		// so enumerating it can never surface a project outside the caller's
		// own workspace. Falls back to the caller's own ProjectID, mirroring
		// BoidOpTaskList's no-project/no-workspace branch, for a token with no
		// workspace assigned at all (a single-project scope of one).
		projectIDs := ctx.AllowedProjectIDs
		if len(projectIDs) == 0 {
			projectIDs = []string{ctx.ProjectID}
		}
		// Peer-discovery feature: merge this job's tracked
		// WorkspacePeerAdvertise (clone URL / reference path / clone dir per
		// workspace peer) into the corresponding summaries below. Fail-soft
		// by design — e.JobID unset/unknown to jobContexts (host-side
		// caller, or a token whose job already completed) just means no
		// clone info gets merged, never an error; the plain id/name/
		// upstream_url listing must keep working regardless.
		var peerAdvertise map[string]dispatcher.PeerAdvertise
		if e.jobContexts != nil {
			if snap, ok := e.jobContexts.JobContext(req.JobID); ok {
				peerAdvertise = snap.WorkspacePeerAdvertise
			}
		}
		summaries := make([]projectSummary, 0, len(projectIDs))
		for _, pid := range projectIDs {
			if pid == "" {
				continue
			}
			proj, err := e.projects.GetProject(pid)
			if err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
			}
			summary := projectSummary{
				ID:          proj.ID,
				Name:        proj.Meta.Name,
				UpstreamURL: proj.UpstreamURL,
			}
			if adv, ok := peerAdvertise[pid]; ok {
				summary.CloneURL = adv.CloneURL
				summary.ReferencePath = adv.ReferencePath
				summary.CloneDir = adv.CloneDir
			}
			summaries = append(summaries, summary)
		}
		out, err := json.MarshalIndent(summaries, "", "  ")
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{Stdout: string(out) + "\n"}
	case sandbox.BoidOpActionSend:
		if req.TaskID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid action send requires a task id"}
		}
		if e.tasks != nil {
			existing, err := e.tasks.GetTask(req.TaskID)
			if err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
			}
			if !ctx.AllowsProject(existing.ProjectID) {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid action send is restricted to the current workspace"}
			}
		}
		// child_specced (docs/plans/cross-project-issue-triage.md Phase 1
		// PR-4, codex review Blocker fix): the ONLY workspace scoping check
		// above verifies the parent card's own project — it says nothing
		// about the "project" field the child_specced payload itself
		// carries, which is where TaskWorkflowService.Dispatch later
		// creates and auto-starts the real child task
		// (orchestrator.TaskTriageChildSpec.Project). Without this check a
		// job in workspace A could specc a child aimed at ANY project the
		// daemon knows about, and Dispatch (triggered later by anyone,
		// including nose clicking Go) would create and run work there with
		// no further authorization check — a workspace-scope bypass. The
		// payload's project must already be a resolved UUID (see
		// TaskTriageChildSpec's own doc comment: PR-2 does not do
		// project-ref fuzzy resolution for children), so an exact
		// AllowsProject membership check is the correct (and only) gate
		// here — no broker-side name resolution to lean on, unlike
		// BoidOpTaskCreate/BoidOpProjectBehaviors.
		if req.ActionType == "child_specced" {
			var childPayload struct {
				Project string `json:"project"`
			}
			if err := json.Unmarshal(req.Payload, &childPayload); err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid action send: invalid child_specced payload: " + err.Error()}
			}
			if childPayload.Project == "" || !ctx.AllowsProject(childPayload.Project) {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid action send: child_specced project is restricted to the current workspace"}
			}
		}
		if e.workflow == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid action send unavailable"}
		}
		if _, err := e.workflow.ApplyAction(orchestrator.WithActor(goCtx, orchestrator.ActorTask(ctx.TaskID)), req.TaskID, api.ApplyActionRequest{
			Type:    req.ActionType,
			Payload: req.Payload,
		}); err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{
			Stdout: fmt.Sprintf("action applied: %s\n", req.ActionType),
		}
	case sandbox.BoidOpCardGet:
		// docs/plans/cross-project-issue-triage.md Phase 1 PR-5a: the read
		// half of 決定14 (daemon が state の唯一の正). Same scoping pattern as
		// BoidOpTaskGet — look the task up, then enforce AllowsProject before
		// returning anything, so a caller that happens to know another
		// workspace's task UUID still learns nothing about it.
		if req.TaskID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid card get requires a task id"}
		}
		if e.tasks == nil || e.workflow == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid card get unavailable"}
		}
		existing, err := e.tasks.GetTask(req.TaskID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		if !ctx.AllowsProject(existing.ProjectID) {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid card get is restricted to the current workspace"}
		}
		view, err := e.workflow.GetCard(req.TaskID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		encoded, err := json.MarshalIndent(view, "", "  ")
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{Stdout: string(encoded) + "\n"}
	case sandbox.BoidOpCardList:
		if e.workflow == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid card list unavailable"}
		}
		if err := validateTaskListStatus(req.Status); err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		// Scoping mirrors BoidOpTaskList exactly, in BOTH layers: the broker
		// resolves the project ref, checks AllowsProject, and rejects a
		// workspace_id that isn't the caller's own (see broker.go's
		// BoidOpCardList case — that is where task_list's scoping lives
		// too, so putting it only here would leave --workspace-id unchecked);
		// the AllowsProject re-check below is the defense-in-depth second
		// layer for a handwritten request that bypassed the shim. With no
		// filter at all the listing is assembled per allowed project rather
		// than running unscoped.
		var views []*api.CardView
		switch {
		case req.ProjectID != "":
			if !ctx.AllowsProject(req.ProjectID) {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid card list is restricted to the current workspace"}
			}
			listed, err := e.workflow.ListCards(orchestrator.TaskFilter{ProjectID: req.ProjectID, Status: req.Status})
			if err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
			}
			views = listed
		case req.WorkspaceID != "":
			// Defense in depth behind the broker's own equality check: the
			// WorkspaceID filter INNER JOINs project_workspaces, so an
			// unchecked value here genuinely crosses the compartment 決定2
			// exists to keep (every other workspace's card titles/summaries).
			if ctx.WorkspaceID != "" && req.WorkspaceID != ctx.WorkspaceID {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid card list is restricted to the current workspace"}
			}
			listed, err := e.workflow.ListCards(orchestrator.TaskFilter{WorkspaceID: req.WorkspaceID, Status: req.Status})
			if err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
			}
			views = listed
		default:
			projectIDs := ctx.AllowedProjectIDs
			if len(projectIDs) == 0 {
				// An empty ProjectID here would make the fallback
				// ListCards(ProjectID: "") a DAEMON-WIDE listing of every
				// card's title and detail blob (Opus review round 2). Job
				// tokens always carry a ProjectID, so this is insurance rather
				// than a live hole — but the card payload is far richer than
				// task_list's id/status/title, so it is refused explicitly
				// instead of relying on that invariant holding forever.
				if ctx.ProjectID == "" {
					return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid card list: no project scope available for an unfiltered listing"}
				}
				projectIDs = []string{ctx.ProjectID}
			}
			for _, pid := range projectIDs {
				listed, err := e.workflow.ListCards(orchestrator.TaskFilter{ProjectID: pid, Status: req.Status})
				if err != nil {
					return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
				}
				views = append(views, listed...)
			}
		}
		if views == nil {
			views = []*api.CardView{}
		}
		encoded, err := json.MarshalIndent(views, "", "  ")
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{Stdout: string(encoded) + "\n"}
	case sandbox.BoidOpTaskIdentityLink:
		// docs/plans/ingestion-identity.md PR-1 (B-1). req.ProjectID is
		// already broker-resolved and workspace-checked (broker.go); the
		// TaskID it links to is a SEPARATE scope the broker cannot verify
		// (no TaskStore there), so the GetTask + AllowsProject pattern below
		// matches BoidOpActionSend's own — EXCEPT that this
		// op writes the task id straight into task_identities.task_id, an
		// FK column, so it must pass LinkIdentity the GetTask call's
		// RESOLVED existing.ID (which absorbs GetTask's own >=8-char prefix
		// fallback, internal/orchestrator/store.go) rather than the raw
		// req.TaskID a caller supplied — action_send never writes
		// their TaskID anywhere, only re-look it up downstream, so a short
		// prefix silently keeps working for them where it would hard-fail
		// here with a raw SQLite FOREIGN KEY constraint error.
		if req.TaskID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task identity link requires a task id"}
		}
		if req.Identity == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task identity link requires an identity"}
		}
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task identity link unavailable"}
		}
		existing, err := e.tasks.GetTask(req.TaskID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		if !ctx.AllowsProject(existing.ProjectID) {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task identity link is restricted to the current workspace"}
		}
		// I-3: identity is scoped to a single project. AllowsProject above
		// only checks the task's project is somewhere in the caller's
		// workspace — it does NOT check the task's project matches the
		// SPECIFIC req.ProjectID the identity is being linked under. Without
		// this, a caller could bind proj-1's identity to a task that
		// actually lives in proj-2 (same workspace, so AllowsProject alone
		// never objects), and PR-2's resolve would then hand proj-1 an
		// observation for a task it doesn't own.
		if existing.ProjectID != req.ProjectID {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task identity link: task belongs to a different project"}
		}
		if err := e.tasks.LinkIdentity(req.ProjectID, req.Identity, existing.ID); err != nil {
			if errors.Is(err, orchestrator.ErrIdentityConflict) {
				return &sandbox.ExecResponse{ExitCode: sandbox.IdentityConflictExitCode, Stderr: err.Error()}
			}
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{Stdout: fmt.Sprintf("linked: %s -> %s\n", req.Identity, existing.ID)}
	case sandbox.BoidOpTaskIdentityUnlink:
		if req.Identity == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task identity unlink requires an identity"}
		}
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task identity unlink unavailable"}
		}
		if err := e.tasks.UnlinkIdentity(req.ProjectID, req.Identity); err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{Stdout: fmt.Sprintf("unlinked: %s\n", req.Identity)}
	case sandbox.BoidOpTaskIdentityResolve:
		// The design doc's explicit contract: "not found" is represented as
		// a distinguished exit code (sandbox.IdentityNotFoundExitCode), NOT
		// the generic ExitCode:1 error path — a get-or-create caller needs
		// to tell the two apart without parsing stderr text. Only
		// orchestrator.ErrTaskNotFound gets this treatment; every other
		// error (store unavailable, DB failure, ...) stays generic.
		if req.Identity == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task identity resolve requires an identity"}
		}
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task identity resolve unavailable"}
		}
		resolved, err := e.tasks.ResolveIdentity(req.ProjectID, req.Identity)
		if err != nil {
			if errors.Is(err, orchestrator.ErrTaskNotFound) {
				return &sandbox.ExecResponse{ExitCode: sandbox.IdentityNotFoundExitCode}
			}
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		// Every other op that hands a task back to the caller re-checks
		// AllowsProject on the task it actually got, not just the project it
		// was asked about (BoidOpTaskGet/BoidOpCardGet/BoidOpActionSend
		// all do this). Resolve is no different — the broker
		// only validated req.ProjectID itself; verify the task ResolveIdentity
		// actually returned still belongs to it.
		if !ctx.AllowsProject(resolved.ProjectID) {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task identity resolve is restricted to the current workspace"}
		}
		// Only the task's own ID and status — never the full task (the
		// design doc is explicit: resolve is a delivery-address lookup, not
		// a task-detail read; workspace already owns whatever more it needs
		// via card_get/BoidOpTaskGet).
		out, err := json.Marshal(struct {
			TaskID string `json:"task_id"`
			Status string `json:"status"`
		}{TaskID: resolved.ID, Status: string(resolved.Status)})
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{Stdout: string(out) + "\n"}
	case sandbox.BoidOpTaskResolveOrCapture:
		// docs/plans/ingestion-identity.md PR-2 (B-2): resolves req.Identity
		// (scoped to req.ProjectID, already broker-resolved+workspace-
		// checked — see the op's own doc comment in protocol.go) to an
		// existing task, or atomically creates a new `captured` triage task
		// and links it when unresolved (I-4). No separate AllowsProject
		// re-check on the result is needed here the way
		// BoidOpTaskIdentityResolve does one — every task this call can
		// return or create is scoped to req.ProjectID by construction (see
		// TaskWorkflowService.ResolveOrCapture's own doc comment).
		if req.Identity == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task resolve-or-capture requires an identity"}
		}
		if e.resolveOrCapture == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task resolve-or-capture unavailable"}
		}
		result, err := e.resolveOrCapture.ResolveOrCapture(goCtx, api.ResolveOrCaptureRequest{
			ProjectID:   req.ProjectID,
			Identity:    req.Identity,
			Title:       req.Title,
			Description: req.Description,
		})
		if err != nil {
			// PR-2 節: identity 衝突時は PR-1 の ErrIdentityConflict をその
			// まま返す — machine-readable via IdentityConflictExitCode
			// (BoidOpTaskIdentityLink and this op share the SAME exit code,
			// since both represent the identical underlying condition), not
			// a generic ExitCode:1 a caller has to pattern-match stderr for.
			if errors.Is(err, orchestrator.ErrIdentityConflict) {
				return &sandbox.ExecResponse{ExitCode: sandbox.IdentityConflictExitCode, Stderr: err.Error()}
			}
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		out, err := json.Marshal(struct {
			TaskID  string `json:"task_id"`
			Created bool   `json:"created"`
		}{TaskID: result.TaskID, Created: result.Created})
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{Stdout: string(out) + "\n"}
	case sandbox.BoidOpActionList:
		// docs/plans/ingestion-identity.md PR-3 (B-3). req.ProjectID/
		// req.WorkspaceID are already broker-resolved+workspace-checked
		// (broker.go's BoidOpActionList case, mirroring BoidOpCardList
		// exactly) — this builds the orchestrator.ActionListFilter the SAME
		// three-branch shape decides between (project_id / workspace_id /
		// neither -> ctx.AllowedProjectIDs), but as ONE ProjectIDs slice
		// rather than BoidOpCardList's per-project loop: a single SQL
		// IN(...) query keeps cursor pagination correct across the whole
		// scope in one pass (see orchestrator.ActionListFilter's own doc
		// comment for why a loop cannot merge cursors correctly here).
		if e.actionList == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid action list unavailable"}
		}
		filter := orchestrator.ActionListFilter{
			TaskID: req.TaskID,
			Since:  req.Since,
			Limit:  req.Limit,
		}
		switch {
		case req.ProjectID != "":
			if !ctx.AllowsProject(req.ProjectID) {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid action list is restricted to the current workspace"}
			}
			filter.ProjectIDs = []string{req.ProjectID}
		case req.WorkspaceID != "":
			// Defense in depth behind the broker's own equality check —
			// same rationale as BoidOpCardList's WorkspaceID branch:
			// an unchecked value here would cross the compartment 決定2
			// exists to keep.
			if ctx.WorkspaceID != "" && req.WorkspaceID != ctx.WorkspaceID {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid action list is restricted to the current workspace"}
			}
			filter.WorkspaceID = req.WorkspaceID
		default:
			projectIDs := ctx.AllowedProjectIDs
			if len(projectIDs) == 0 {
				// Mirrors BoidOpCardList's own insurance (Opus review
				// round 2): an empty ProjectIDs AND empty WorkspaceID would
				// make ListActionsSince refuse via ErrActionListUnscoped —
				// which is safe — but only if ctx.ProjectID is also empty;
				// when it's set, fall back to it exactly like every other
				// "no explicit scope" branch in this file does.
				if ctx.ProjectID == "" {
					return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid action list: no project scope available for an unfiltered listing"}
				}
				projectIDs = []string{ctx.ProjectID}
			}
			filter.ProjectIDs = projectIDs
		}
		result, err := e.actionList.ListActions(filter)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		encoded, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{Stdout: string(encoded) + "\n"}
	case sandbox.BoidOpSignalList:
		// docs/plans/signal-ingest-detailed-design.md §3.2 (PR-3). WorkspaceID
		// is already broker-injected from the job token (broker.go's
		// BoidOpSignalList case) — the "requires a workspace" check below is
		// defense in depth for a handwritten request that bypassed the
		// broker (e.g. a direct ExecuteBoidBuiltin caller in a future test),
		// mirroring every other op's own re-check convention in this file.
		if e.signals == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal list unavailable"}
		}
		if req.WorkspaceID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal list requires a workspace"}
		}
		if req.Claim {
			// ClaimSignals (orchestrator/signal_store.go) has no
			// connector/state filter — it always selects pending signals
			// workspace-wide. --source and a non-pending --state are
			// refused outright rather than silently ignored, so a caller
			// doesn't mistakenly believe the filter was honored.
			if req.Connector != "" {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal list: --claim does not support --source (ClaimSignals has no connector filter)"}
			}
			if req.SignalState != "" && req.SignalState != string(orchestrator.SignalStatePending) {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal list: --claim always claims pending signals; --state is not supported with --claim"}
			}
			claimed, err := e.signals.ClaimSignals(req.WorkspaceID, req.Limit, 0)
			if err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
			}
			return signalsJSONResponse(claimed)
		}
		listed, err := e.signals.ListSignals(orchestrator.SignalFilter{
			WorkspaceID: req.WorkspaceID,
			Connector:   req.Connector,
			State:       orchestrator.SignalState(req.SignalState),
			Limit:       req.Limit,
		})
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return signalsJSONResponse(listed)
	case sandbox.BoidOpSignalAck:
		if e.signals == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal ack unavailable"}
		}
		if req.WorkspaceID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal ack requires a workspace"}
		}
		if len(req.SignalIDs) == 0 {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal ack requires at least one id"}
		}
		// AckSignals is idempotent per id (Q14): acking an id already acked
		// by a prior call is a no-op success, not an error — nothing extra
		// is needed here to preserve that, since it's a plain passthrough.
		if err := e.signals.AckSignals(req.WorkspaceID, req.SignalIDs); err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{ExitCode: 0}
	case sandbox.BoidOpSignalIngest:
		// Unreachable via the general policy as of PR-3 (policy.go's
		// boidPolicy deliberately excludes this op — see its own doc
		// comment) — implemented now so PR-5's connector-scoped reduced
		// policy can grant it later with zero executor changes.
		if e.signals == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal ingest unavailable"}
		}
		if req.WorkspaceID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal ingest requires a workspace"}
		}
		if req.Service == "" || req.Connector == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal ingest requires a service and connector"}
		}
		rows, err := parseSignalIngestPayload(req.IngestPayload)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		if err := e.signals.IngestSignals(req.WorkspaceID, req.Service, req.Connector, rows); err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{ExitCode: 0}
	case sandbox.BoidOpSignalCursorGet:
		// Unreachable via the general policy as of PR-3 — see
		// BoidOpSignalIngest's case above for the same note.
		if e.signals == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal cursor unavailable"}
		}
		if req.WorkspaceID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal cursor requires a workspace"}
		}
		if req.Service == "" || req.Connector == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal cursor requires a service and connector"}
		}
		cursor, err := e.signals.GetSignalCursor(req.WorkspaceID, req.Service, req.Connector)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		encoded, err := json.MarshalIndent(struct {
			Cursor string `json:"cursor"`
		}{Cursor: cursor}, "", "  ")
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{Stdout: string(encoded) + "\n"}
	case sandbox.BoidOpJobList:
		if e.jobs == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid job list unavailable"}
		}
		if req.TaskID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid job list requires a task id"}
		}
		if e.tasks != nil {
			existing, err := e.tasks.GetTask(req.TaskID)
			if err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
			}
			if !ctx.AllowsProject(existing.ProjectID) {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid job list is restricted to the current workspace"}
			}
		}
		jobs, err := e.jobs.ListJobsByTask(req.TaskID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "%-36s %-24s %-8s %-10s %-4s %-19s\n", "ID", "HANDLER", "ROLE", "STATUS", "EXIT", "UPDATED")
		for _, j := range jobs {
			exit := "-"
			if j.Status == api.JobStatusCompleted || j.Status == api.JobStatusFailed {
				exit = fmt.Sprintf("%d", j.ExitCode)
			}
			updated := "-"
			if !j.UpdatedAt.IsZero() {
				updated = j.UpdatedAt.Format(time.DateTime)
			}
			handler := j.HandlerID
			if len(handler) > 24 {
				handler = handler[:21] + "..."
			}
			fmt.Fprintf(&sb, "%-36s %-24s %-8s %-10s %-4s %-19s\n",
				j.ID, handler, j.Role, string(j.Status), exit, updated)
		}
		return &sandbox.ExecResponse{Stdout: sb.String()}
	case sandbox.BoidOpJobShow:
		if e.jobs == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid job show unavailable"}
		}
		if req.JobID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid job show requires a job id"}
		}
		j, err := e.jobs.GetJob(req.JobID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		if !ctx.AllowsProject(j.ProjectID) {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid job show is restricted to the current workspace"}
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "ID:         %s\n", j.ID)
		fmt.Fprintf(&sb, "Task:       %s\n", j.TaskID)
		fmt.Fprintf(&sb, "Project:    %s\n", j.ProjectID)
		fmt.Fprintf(&sb, "Handler:    %s\n", j.HandlerID)
		fmt.Fprintf(&sb, "Role:       %s\n", j.Role)
		runtimeVal := j.RuntimeID
		if runtimeVal == "" {
			runtimeVal = "-"
		}
		fmt.Fprintf(&sb, "Runtime:    %s\n", runtimeVal)
		fmt.Fprintf(&sb, "Status:     %s\n", j.Status)
		exitVal := "-"
		if j.Status == api.JobStatusCompleted || j.Status == api.JobStatusFailed {
			exitVal = fmt.Sprintf("%d", j.ExitCode)
		}
		fmt.Fprintf(&sb, "Exit Code:  %s\n", exitVal)
		fmt.Fprintf(&sb, "Created At: %s\n", j.CreatedAt.Format(time.DateTime))
		fmt.Fprintf(&sb, "Updated At: %s\n", j.UpdatedAt.Format(time.DateTime))
		return &sandbox.ExecResponse{Stdout: sb.String()}
	case sandbox.BoidOpJobLog:
		if e.jobs == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid job log unavailable"}
		}
		if req.JobID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid job log requires a job id"}
		}
		j, err := e.jobs.GetJob(req.JobID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		if !ctx.AllowsProject(j.ProjectID) {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid job log is restricted to the current workspace"}
		}
		// Both "no log" answers name their own reason, in the same words
		// GET /jobs/{id}/log uses (api.JobLog*Message). A bare "log not
		// available" reads as "it was swept" for a job whose sandbox never
		// started, which is a different problem with a different fix — see
		// api.JobLogNoSuchJobMessage's own doc comment for the dogfood
		// session that motivated splitting them.
		if j.RuntimeID == "" {
			return &sandbox.ExecResponse{Stdout: api.JobLogNoRuntimeMessage(req.JobID, j.Status) + "\n"}
		}
		if e.logReader == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid job log unavailable"}
		}
		data, err := e.logReader.ReadJobLog(j.RuntimeID)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return &sandbox.ExecResponse{Stdout: api.JobLogTranscriptGoneMessage(j.RuntimeID) + "\n"}
			}
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{Stdout: string(data)}
	case sandbox.BoidOpTaskDelete:
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task delete unavailable"}
		}
		if req.TaskID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task delete requires a task id"}
		}
		existing, err := e.tasks.GetTask(req.TaskID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		if !ctx.AllowsProject(existing.ProjectID) {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task delete is restricted to the current workspace"}
		}
		if err := e.tasks.DeleteTask(req.TaskID, req.Force); err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{
			Stdout: fmt.Sprintf("task deleted: %s\n", req.TaskID),
		}

	// --- Phase 5b PR1 task-context RPCs (docs/plans/phase5-shim-and-task-context.md) ---
	// `boid task current` / `instructions` are live re-derivations from the
	// task row (api.TaskAppService); `env` / `payload` are backed by the
	// per-job JobContextSnapshot dispatcher.Runner tracks at Dispatch() time
	// (see jobContextProvider's doc comment for why the split exists).

	case sandbox.BoidOpTaskCurrent:
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task current unavailable"}
		}
		if req.TaskField != "" {
			value, err := e.tasks.GetTaskCurrentField(req.TaskID, req.TaskField)
			if err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
			}
			return &sandbox.ExecResponse{Stdout: value}
		}
		snap, err := e.tasks.GetTaskCurrent(req.TaskID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return marshalTaskContextResponse(snap)

	case sandbox.BoidOpTaskInstructions:
		// Job-scoped, NOT task-row-derived: api.TaskAppService.GetInstructions
		// (task-row-derived, kept for other potential task-level callers —
		// see its own doc comment) must not back this RPC. Two agent-kind
		// hooks for different agents can be dispatched from the same task in
		// one evaluation round; only jobContexts (populated from this job's
		// own JobSpec.Instruction at Dispatch time) tells them apart. Fixed
		// during codex review on PR #797 before merge — see
		// wiring-seams.md #13.
		if e.jobContexts == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task instructions unavailable"}
		}
		snap, ok := e.jobContexts.JobContext(req.JobID)
		if !ok {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task instructions: no context tracked for job %q", req.JobID)}
		}
		if req.TaskField != "" {
			value, err := resolveTaskContextField(snap.Instructions, req.TaskField)
			if err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
			}
			return &sandbox.ExecResponse{Stdout: value}
		}
		return marshalTaskContextResponse(snap.Instructions)

	case sandbox.BoidOpTaskEnv:
		if e.jobContexts == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task env unavailable"}
		}
		snap, ok := e.jobContexts.JobContext(req.JobID)
		if !ok {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task env: no context tracked for job %q", req.JobID)}
		}
		if req.TaskField != "" {
			value, err := resolveTaskContextField(snap.Env, req.TaskField)
			if err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
			}
			return &sandbox.ExecResponse{Stdout: value}
		}
		return marshalTaskContextResponse(snap.Env)

	case sandbox.BoidOpTaskPayload:
		if e.jobContexts == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task payload unavailable"}
		}
		snap, ok := e.jobContexts.JobContext(req.JobID)
		if !ok {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task payload: no context tracked for job %q", req.JobID)}
		}
		payload := snap.Payload
		if len(payload) == 0 {
			payload = json.RawMessage("{}")
		}
		if req.TaskField != "" {
			value, err := api.ResolveJSONField(payload, req.TaskField)
			if err != nil {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
			}
			return &sandbox.ExecResponse{Stdout: value}
		}
		return &sandbox.ExecResponse{Stdout: string(payload)}

	// --- Phase 5b PR2 attachments RPCs (docs/plans/phase5-shim-and-task-context.md) ---
	// Both read straight from disk via api.ListAttachments/api.ReadAttachment
	// (AttachmentsRootForTask, the same helper the upload path writes
	// through) — no DB or JobContextSnapshot involved, since attachments are
	// keyed by TaskID alone (see broker.go's guard). The Phase 5b PR6 cutover
	// retired the parallel dispatch-time RO bind these two ops used to run
	// alongside — this RPC pair is now the sole read path.

	case sandbox.BoidOpTaskAttachmentsList:
		if e.attachmentsRoot == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task attachments list unavailable"}
		}
		names, err := api.ListAttachments(e.attachmentsRoot, req.TaskID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return marshalTaskContextResponse(names)

	case sandbox.BoidOpTaskAttachmentsGet:
		if e.attachmentsRoot == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task attachments get unavailable"}
		}
		data, err := api.ReadAttachment(e.attachmentsRoot, req.TaskID, req.AttachmentName)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		// Binary transport: base64-encode into Stdout (ExecResponse only
		// carries strings over the JSON broker wire — no separate byte-array
		// field, and no chunked-streaming protocol for this op; the existing
		// 10 MB/file write-time cap (AttachmentMaxFileBytes, re-checked
		// independently by ReadAttachment) keeps a single JSON round trip
		// well-bounded). The shim decodes this before writing to --output or
		// the real process stdout.
		return &sandbox.ExecResponse{Stdout: base64.StdEncoding.EncodeToString(data)}

	// --- Phase 5b PR7 job_done payload_patch direct-pass RPC
	// (docs/plans/phase5-shim-and-task-context.md) ---
	case sandbox.BoidOpTaskUpdatePayloadPatch:
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task update --payload-patch unavailable"}
		}
		// allowedTraits comes from the JobContextSnapshot captured at
		// dispatch time (JobSpec.HookTraitsProduces), never a live
		// re-lookup against current project meta — codex review caught a
		// TOCTOU staleness bug in an early cut that re-resolved the firing
		// hook by ID at merge time, which could silently apply a
		// post-dispatch-edit trait list (or fail open) if project.yaml
		// changed between dispatch and this call. See wiring-seams.md #17's
		// Major 1 finding and JobContextSnapshot's own doc comment.
		if e.jobContexts == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task update --payload-patch unavailable"}
		}
		snap, ok := e.jobContexts.JobContext(req.JobID)
		if !ok {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task update --payload-patch: no context tracked for job %q", req.JobID)}
		}
		task, err := e.tasks.UpdateTaskPayloadPatch(req.JobID, req.PayloadPatch, snap.PayloadPatchAllowedTraits)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{
			Stdout: fmt.Sprintf("task updated: %s (%s)\n", task.ID, task.Status),
		}

	default:
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("unsupported boid op %q", req.Op)}
	}
}

// signalsJSONResponse renders a []*orchestrator.Signal (from ListSignals or
// ClaimSignals) as apiwire.ListSignalsResponse — the SAME v0 envelope shape
// PR-2's `GET /api/signals` host API replies with (M3, PR #1014 review: an
// earlier version marshaled orchestrator.Signal directly, which has no json
// tags and so replied with raw Go field names — a DIFFERENT, undocumented
// shape depending on whether `boid signal list` ran on the host or inside a
// sandbox). toWireSignals never returns nil (json.MarshalIndent renders an
// empty slice as "[]", never "null", inside the "signals" field).
func signalsJSONResponse(signals []*orchestrator.Signal) *sandbox.ExecResponse {
	encoded, err := json.MarshalIndent(apiwire.ListSignalsResponse{Signals: toWireSignals(signals)}, "", "  ")
	if err != nil {
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
	}
	return &sandbox.ExecResponse{Stdout: string(encoded) + "\n"}
}

// toWireSignals shapes store rows into the v0 envelope (signal-driven-
// review.md §5.2), splitting each row's stored "<pack>/<connector>"
// composite Connector back into the envelope's separate
// source.pack/source.connector fields. Mirrors internal/api/signal_handler.go's
// own toWireSignals exactly (PR-2, docs/plans/signal-ingest-detailed-design.md
// §3.1) — duplicated here rather than shared because it's an unexported
// helper in a different package (internal/api), the same "small local
// helper rather than reusing an unexported cross-package one" precedent
// internal/api/signal_handler.go's own dedupeStrings doc comment describes
// for orchestrator's dedupeStringsPreserveOrder.
func toWireSignals(signals []*orchestrator.Signal) []apiwire.Signal {
	out := make([]apiwire.Signal, 0, len(signals))
	for _, s := range signals {
		pack, connector := splitSignalPackConnector(s.Connector)
		out = append(out, apiwire.Signal{
			ID:         s.ID,
			OccurredAt: s.OccurredAt,
			Source: apiwire.SignalSource{
				Pack:      pack,
				Connector: connector,
				Service:   s.Service,
			},
			Identity:   s.Identity,
			URL:        s.URL,
			Author:     s.Author,
			Title:      s.Title,
			ReceivedAt: s.ReceivedAt,
			Attempts:   s.Attempts,
			AckedAt:    s.AckedAt,
		})
	}
	return out
}

// splitSignalPackConnector splits a stored "<pack>/<connector>" composite
// into its two halves. A value with no "/" (should never happen for a real
// connector-produced row, but the wire layer must not panic on it) comes
// back as an empty Pack with the whole string as Connector.
func splitSignalPackConnector(composite string) (pack, connector string) {
	pack, connector, ok := strings.Cut(composite, "/")
	if !ok {
		return "", composite
	}
	return pack, connector
}

// parseSignalIngestPayload parses `boid signal ingest`'s stdin body
// (BoidRequest.IngestPayload) as JSONL — one orchestrator.SignalIngestRow
// per non-blank line (design doc §5.3: "1 行 = {id, occurred_at, identity,
// url?, author?, title?}") — server-side, matching where
// BoidOpTaskUpdatePayloadPatch's PayloadPatch is parsed. A malformed line
// fails the whole call (nothing partially applied — IngestSignals itself is
// an all-or-nothing single tx per design doc §2, so a payload that can't
// even be parsed must not reach it at all) with the 1-indexed line number in
// the error for connector-side debugging.
// parseSignalIngestPayload returns an empty (nil) slice, not an error, when
// payload has no signal rows (blank/whitespace-only stdin, or no stdin at
// all) — matching orchestrator.IngestSignals' own contract for an empty
// rows slice ("if len(rows) == 0 { return nil }", signal_store.go), so the
// two layers agree rather than the shim/executor boundary silently
// tightening a no-op into a hard failure. [m2, review of PR #1014,
// 2026-08-26]: an earlier version of this function returned an error here,
// which meant a connector that polls its source and finds nothing new —
// the ordinary, expected outcome of most polling cycles, not an error
// condition — would get exit != 0 from `boid signal ingest` if it always
// piped its (possibly empty) batch through rather than conditionally
// skipping the call, incrementing failStreak for doing nothing wrong. A
// malformed (non-empty, non-JSON) line is still a real error and still
// fails the whole call, unchanged.
func parseSignalIngestPayload(payload []byte) ([]orchestrator.SignalIngestRow, error) {
	var rows []orchestrator.SignalIngestRow
	lines := strings.Split(string(payload), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var row orchestrator.SignalIngestRow
		if err := json.Unmarshal([]byte(trimmed), &row); err != nil {
			return nil, fmt.Errorf("boid signal ingest: line %d: invalid json: %w", i+1, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// marshalTaskContextResponse renders v (a task-current snapshot, routed
// instructions list, or reduced environment view) as the full-object form
// of a Phase 5b PR1 task-context RPC response: canonical JSON in Stdout. The
// CLI (internal/sandbox's shim) is responsible for any client-side
// `--format yaml` re-rendering — the broker always speaks JSON on the wire.
func marshalTaskContextResponse(v any) *sandbox.ExecResponse {
	raw, err := json.Marshal(v)
	if err != nil {
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("marshal response: %s", err)}
	}
	return &sandbox.ExecResponse{Stdout: string(raw)}
}

// resolveTaskContextField JSON-marshals v and resolves path against it via
// api.ResolveJSONField, giving `boid task env --field` the same --field
// contract (missing path → "", scalar → unquoted/stringified, object/array
// → compact JSON) as `boid task current` / `instructions` / `payload`.
func resolveTaskContextField(v any, path string) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal response: %w", err)
	}
	return api.ResolveJSONField(raw, path)
}
