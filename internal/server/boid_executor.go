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

// jobContextProvider resolves per-job task-context RPC data that has no
// standalone DB representation — the reduced environment view + trait-
// filtered payload — tracked per job by dispatcher.Runner at Dispatch()
// time. Kept as a narrow interface (rather than depending on
// *dispatcher.Runner's full surface) so boid_executor's dependency on
// dispatcher stays this one method; *dispatcher.Runner satisfies it
// structurally.
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
// single method BoidOpTaskResolveOrCapture needs, mirroring
// jobContextProvider/projectLookup's own narrowing above. Kept out of
// api.WorkflowService itself; newBoidBuiltinExecutor does a runtime
// interface check against the SAME workflow value callers already pass in,
// so a test double that doesn't implement it just leaves this field nil
// (→ "unavailable"), same convention as every other optional dependency
// here.
type resolveOrCaptureService interface {
	ResolveOrCapture(ctx context.Context, req api.ResolveOrCaptureRequest) (*api.ResolveOrCaptureResult, error)
}

// actionListService is narrowed from api.WorkflowService down to the single
// method BoidOpActionList needs, mirroring resolveOrCaptureService's own
// narrowing and runtime-interface-check wiring immediately above.
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
	// attachments live (`<attachmentsRoot>/tasks/<task_id>/attachments`).
	// It is the same value wire.go threads into
	// api.WebHandler.AttachmentsRoot (the upload path), so the RPC reply
	// can never drift from what the upload path writes. Empty disables the
	// two ops with an "unavailable" error rather than panicking.
	attachmentsRoot string
	// projects backs BoidOpProjectBehaviors (`boid project behaviors` from
	// inside the sandbox). nil disables the op with an "unavailable" error
	// rather than panicking, same convention as every other optional
	// dependency here.
	projects projectLookup
	// resolveOrCapture backs BoidOpTaskResolveOrCapture. Populated in
	// newBoidBuiltinExecutor via a runtime interface check against the SAME
	// workflow value (see resolveOrCaptureService's own doc comment) — nil
	// disables the op with an "unavailable" error, same convention as
	// every other optional dependency here.
	resolveOrCapture resolveOrCaptureService
	// actionList backs BoidOpActionList. Same runtime-interface-check
	// wiring as resolveOrCapture above — see actionListService's own doc
	// comment.
	actionList actionListService
	// signals backs BoidOpSignalList / BoidOpSignalAck / BoidOpSignalIngest /
	// BoidOpSignalCursorGet. Taken as an explicit constructor parameter
	// (wire.go passes taskRepo directly) rather than a runtime interface
	// check against workflow, since *api.TaskWorkflowService does not
	// implement api.SignalStore. Nil disables these ops with an
	// "unavailable" error, same convention as every other optional
	// dependency here.
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

// validateTaskListStatus rejects status values ListTasks doesn't
// understand before they reach the DB layer. Accepted: empty (no filter),
// the special keywords "open"/"closed"/"cards_live" (plus "triage" as a
// compatibility alias for "cards_live"), "queue_next" (falls through to a
// literal-equality match that can never hit a real row, so it
// deterministically returns an empty list rather than erroring), and any
// exact orchestrator.TaskStatus value. "queue" is rejected outright — it
// has no special handling anymore, so it must surface as an unknown-status
// error rather than silently returning an empty list.
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

	// This is the ONE place in the daemon that calls WithWriterProjectID:
	// every sandbox-originated write carries ctx.ProjectID as its "who
	// wrote this" fact, so orchestrator.CreateAction can tell a
	// metaproject's own self-authored write (must not ingest — loop risk)
	// from everything else. Every call site below must pass goCtx (or a
	// value built from it), not a bare context.Background(), to keep this
	// attached.
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
		// InitialStatus is restricted by resolveInitialStatus's allowlist
		// (task_create.go) to captured/triaged/pending — a sandboxed job
		// can never fabricate an already-ready/working/done task.
		// createReq.ProjectID is resolved to a UUID above and gated by
		// ctx.AllowsProject below, so a sandboxed job cannot create a task
		// outside its own workspace. CreateTask's Ref get-or-create also
		// makes a crash-and-resend idempotent instead of planting a
		// duplicate card.
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
		// explicit caller-supplied one through untouched. Look the task up
		// and enforce the same AllowsProject gate the other task-scoped ops
		// use, so a caller cannot read another workspace's task fields just
		// by knowing the UUID.
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
		// Look the task up and enforce the same AllowsProject gate
		// task_update / task_notify / task_answer / task_delete already use.
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
	case sandbox.BoidOpTaskWait:
		// Blocks until the task ends, then reports how it ended through the
		// exit code — `done` is the only success. This is what lets a trigger's
		// `run:` live as long as the task it started, so the trigger's own
		// single-flight and failStreak cover the work instead of the launcher
		// (see sandbox.BoidOpTaskWait's own doc comment).
		//
		// goCtx is cancelled by the broker on daemon shutdown and, because
		// task_wait is listed in sandbox.isBlockingBoidRequest, on sandbox
		// disconnect too — so the wait cannot outlive its caller. It is not a
		// timeout: a task that parks in a non-terminal resting state waits
		// indefinitely, and bounding that is the trigger's job.
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task wait unavailable"}
		}
		if req.TaskID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task wait requires a task id"}
		}
		existing, err := e.tasks.GetTask(req.TaskID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		if !ctx.AllowsProject(existing.ProjectID) {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task wait is restricted to the current workspace"}
		}
		// Compare the RESOLVED id, never the raw one the caller typed: GetTask
		// falls back to a `id LIKE <prefix>%` match for anything >= 8 chars, so
		// `boid task wait <first 8 chars of my own id>` resolves to the calling
		// task while the raw strings differ — the guard would not fire and the
		// job would block on itself forever, which is the deadlock it exists to
		// prevent. Prefixes are ordinary input here (`boid task show <prefix>`
		// works), so this is reachable by accident. Same rule as the resolved-id
		// note on the identity ops further down this file.
		if existing.ID == ctx.TaskID {
			// Waiting on yourself never returns: this job IS the reason the
			// task has not reached a terminal status. Fail rather than hold the
			// caller open until something else kills it.
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task wait cannot wait on the calling task"}
		}
		// Record what this job is parked on for the duration of the wait, so
		// the trigger sweep can end the TASK (the work) rather than only this
		// job (the launcher) when a round outruns its trigger's `timeout` —
		// see api.TaskWaitRegistry. A missing job id or registry just means the
		// wait is unattributable, never that it misbehaves.
		// Register runs NOW (defer evaluates the call and its arguments at the
		// defer statement); only the returned release is deferred. Deferred
		// rather than called after the wait purely so the release cannot be
		// skipped by a future early return added between here and the end of
		// this case — ExecuteBoidBuiltin returns immediately below today, so
		// the timing is otherwise identical.
		defer e.tasks.TaskWaits.Register(ctx.JobID, existing.ID)()
		outcome, err := e.tasks.WaitTaskTerminal(goCtx, existing.ID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		if outcome.Succeeded() {
			return &sandbox.ExecResponse{Stdout: fmt.Sprintf("task %s: %s\n", outcome.ID, outcome.Status)}
		}
		stderr := fmt.Sprintf("task %s: %s", outcome.ID, outcome.Status)
		if reason := api.FormatAbortReason(outcome); reason != "" {
			stderr += " (" + reason + ")"
		}
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: stderr + "\n"}
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
		// The workspace scoping check above only verifies the parent
		// card's own project — it says nothing about the "project" field the
		// child_specced payload itself carries, which is where
		// TaskWorkflowService.Dispatch later creates and auto-starts the real
		// child task (orchestrator.TaskTriageChildSpec.Project). Without this
		// check a job in workspace A could specc a child aimed at any project
		// the daemon knows about. The payload's project must already be a
		// resolved UUID, so an exact AllowsProject membership check is the
		// correct gate here.
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
		// Same scoping pattern as BoidOpTaskGet — look the task up, then
		// enforce AllowsProject before returning anything, so a caller that
		// happens to know another workspace's task UUID still learns
		// nothing about it.
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
			// unchecked value here would leak titles/summaries across
			// workspaces.
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
				// ListCards(ProjectID: "") a daemon-wide listing of every
				// card's title and detail blob. Job tokens always carry a
				// ProjectID, so this is insurance rather than a live hole.
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
		// req.ProjectID is already broker-resolved and workspace-checked
		// (broker.go); the TaskID it links to is a separate scope the
		// broker cannot verify, so the GetTask + AllowsProject pattern
		// below matches BoidOpActionSend's own — except this op writes the
		// GetTask call's RESOLVED existing.ID (not the raw req.TaskID) into
		// task_identities.task_id, an FK column, so a short id prefix that
		// action_send tolerates would otherwise hard-fail here with a raw
		// SQLite FOREIGN KEY constraint error.
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
		// identity is scoped to a single project: AllowsProject above only
		// checks the task's project is somewhere in the caller's
		// workspace — it does NOT check the task's project matches the
		// SPECIFIC req.ProjectID the identity is being linked under.
		// Without this, a caller could bind proj-1's identity to a task
		// that actually lives in proj-2 (same workspace, so AllowsProject
		// alone never objects).
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
		// Resolves req.Identity (scoped to req.ProjectID, already
		// broker-resolved+workspace-checked) to an existing task, or
		// atomically creates a new `captured` triage task and links it
		// when unresolved. No separate AllowsProject re-check on the
		// result is needed here — every task this call can return or
		// create is scoped to req.ProjectID by construction.
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
			// identity 衝突時は ErrIdentityConflict をそのまま返す —
			// machine-readable via IdentityConflictExitCode (shared with
			// BoidOpTaskIdentityLink), not a generic ExitCode:1 a caller
			// has to pattern-match stderr for.
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
		// req.ProjectID/req.WorkspaceID are already broker-resolved and
		// workspace-checked (broker.go's BoidOpActionList case, mirroring
		// BoidOpCardList). Builds a single ProjectIDs slice rather than a
		// per-project loop, since a single SQL IN(...) query keeps cursor
		// pagination correct across the whole scope in one pass (see
		// orchestrator.ActionListFilter's own doc comment for why a loop
		// cannot merge cursors correctly here).
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
			// same rationale as BoidOpCardList's WorkspaceID branch: an
			// unchecked value here would leak titles/summaries across
			// workspaces.
			if ctx.WorkspaceID != "" && req.WorkspaceID != ctx.WorkspaceID {
				return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid action list is restricted to the current workspace"}
			}
			filter.WorkspaceID = req.WorkspaceID
		default:
			projectIDs := ctx.AllowedProjectIDs
			if len(projectIDs) == 0 {
				// Mirrors BoidOpCardList's own insurance: an empty
				// ProjectIDs and WorkspaceID would be safely refused via
				// ErrActionListUnscoped, but only when ctx.ProjectID is
				// also empty; otherwise fall back to it.
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
		// WorkspaceID is already broker-injected from the job token
		// (broker.go's BoidOpSignalList case) — the "requires a workspace"
		// check below is defense in depth for a handwritten request that
		// bypassed the broker.
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
	case sandbox.BoidOpSignalClaim:
		if e.signals == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal claim unavailable"}
		}
		if req.WorkspaceID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal claim requires a workspace"}
		}
		if len(req.SignalIDs) == 0 {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid signal claim requires at least one id"}
		}
		if err := e.signals.ClaimSignalIDs(req.WorkspaceID, req.SignalIDs); err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{ExitCode: 0}
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
		// AckSignals is idempotent per id: acking an id already acked by a
		// prior call is a no-op success, not an error.
		if err := e.signals.AckSignals(req.WorkspaceID, req.SignalIDs); err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{ExitCode: 0}
	case sandbox.BoidOpSignalIngest:
		// Unreachable via the general policy — boidPolicy deliberately
		// excludes this op (see its own doc comment); implemented so a
		// connector-scoped reduced policy can grant it later with zero
		// executor changes.
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
		// Unreachable via the general policy — see BoidOpSignalIngest's
		// case above for the same note.
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
		// GET /jobs/{id}/log uses (api.JobLog*Message) — a bare "log not
		// available" would read as "it was swept" for a job whose sandbox
		// never started, which is a different problem with a different
		// fix.
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

	// --- task-context RPCs ---
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
		// own JobSpec.Instruction at Dispatch time) tells them apart.
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

	// --- attachments RPCs ---
	// Both read straight from disk via api.ListAttachments/api.ReadAttachment
	// (AttachmentsRootForTask, the same helper the upload path writes
	// through) — no DB or JobContextSnapshot involved, since attachments are
	// keyed by TaskID alone (see broker.go's guard).

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

	// --- job_done payload_patch direct-pass RPC ---
	case sandbox.BoidOpTaskUpdatePayloadPatch:
		if e.tasks == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task update --payload-patch unavailable"}
		}
		// allowedTraits comes from the JobContextSnapshot captured at
		// dispatch time (JobSpec.HookTraitsProduces), never a live
		// re-lookup against current project meta — a live lookup could
		// silently apply a post-dispatch-edit trait list if project.yaml
		// changed between dispatch and this call.
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
// ClaimSignals) as apiwire.ListSignalsResponse, the same envelope shape
// `GET /api/signals` replies with. toWireSignals never returns nil
// (json.MarshalIndent renders an empty slice as "[]", never "null", inside
// the "signals" field).
func signalsJSONResponse(signals []*orchestrator.Signal) *sandbox.ExecResponse {
	encoded, err := json.MarshalIndent(apiwire.ListSignalsResponse{Signals: toWireSignals(signals)}, "", "  ")
	if err != nil {
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
	}
	return &sandbox.ExecResponse{Stdout: string(encoded) + "\n"}
}

// toWireSignals shapes store rows into the wire envelope, splitting each
// row's stored "<pack>/<connector>" composite Connector back into the
// envelope's separate source.pack/source.connector fields. Mirrors
// internal/api/signal_handler.go's own toWireSignals; duplicated here as a
// small local helper rather than shared across packages.
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

// parseSignalIngestPayload parses `boid signal ingest`'s stdin body as
// JSONL — one orchestrator.SignalIngestRow per non-blank line. A malformed
// line fails the whole call with its 1-indexed line number in the error. An
// empty (or blank) payload returns a nil slice, not an error — matching
// orchestrator.IngestSignals' own no-op contract for an empty rows slice,
// so a connector that polls and finds nothing new is not penalized for
// piping through an empty batch.
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
// instructions list, or reduced environment view) as canonical JSON in
// Stdout. The CLI (internal/sandbox's shim) is responsible for any
// client-side `--format yaml` re-rendering — the broker always speaks JSON
// on the wire.
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
