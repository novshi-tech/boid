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
}

func newBoidBuiltinExecutor(workflow api.WorkflowService, tasks *api.TaskAppService, jobs api.JobStore, logReader api.JobLogReader, jobContexts jobContextProvider, attachmentsRoot string) sandbox.BoidExecutor {
	if workflow == nil && tasks == nil {
		return nil
	}
	return &boidBuiltinExecutor{
		workflow:        workflow,
		tasks:           tasks,
		jobs:            jobs,
		logReader:       logReader,
		jobContexts:     jobContexts,
		attachmentsRoot: attachmentsRoot,
	}
}

func (e *boidBuiltinExecutor) ExecuteBoidBuiltin(goCtx context.Context, ctx sandbox.TokenContext, req *sandbox.BoidRequest) *sandbox.ExecResponse {
	if req == nil {
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: "missing boid request"}
	}

	switch req.Op {
	case sandbox.BoidOpJobDone:
		if e.workflow == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid job done unavailable"}
		}
		if _, err := e.workflow.CompleteJob(context.Background(), req.JobID, api.JobDoneRequest{
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
		if _, err := e.workflow.ApplyAction(context.Background(), req.TaskID, applyReq); err != nil {
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
		if err := e.tasks.NotifyTask(context.Background(), req.TaskID, req.Message, req.Ask, req.QuestionID, req.Progress, req.Done, req.Fail); err != nil {
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
		if err := e.tasks.AnswerTask(context.Background(), req.TaskID, req.QuestionID, req.Answer); err != nil {
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
		if e.workflow == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid action send unavailable"}
		}
		if _, err := e.workflow.ApplyAction(context.Background(), req.TaskID, api.ApplyActionRequest{
			Type:    req.ActionType,
			Payload: req.Payload,
		}); err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		return &sandbox.ExecResponse{
			Stdout: fmt.Sprintf("action applied: %s\n", req.ActionType),
		}
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
		if j.RuntimeID == "" {
			return &sandbox.ExecResponse{Stdout: "log not available\n"}
		}
		if e.logReader == nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid job log unavailable"}
		}
		data, err := e.logReader.ReadJobLog(j.RuntimeID)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return &sandbox.ExecResponse{Stdout: "log not available\n"}
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
