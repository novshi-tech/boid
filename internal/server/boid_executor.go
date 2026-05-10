package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

type boidBuiltinExecutor struct {
	workflow  *api.TaskWorkflowService
	tasks     *api.TaskAppService
	jobs      api.JobStore
	logReader api.JobLogReader
}

func newBoidBuiltinExecutor(workflow *api.TaskWorkflowService, tasks *api.TaskAppService, jobs api.JobStore, logReader api.JobLogReader) sandbox.BoidExecutor {
	if workflow == nil && tasks == nil {
		return nil
	}
	return &boidBuiltinExecutor{
		workflow:  workflow,
		tasks:     tasks,
		jobs:      jobs,
		logReader: logReader,
	}
}

func (e *boidBuiltinExecutor) ExecuteBoidBuiltin(ctx sandbox.TokenContext, req *sandbox.BoidRequest) *sandbox.ExecResponse {
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
		if createReq.ParentID == "" {
			createReq.ParentID = ctx.TaskID
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
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task get unavailable"}
		}
		task, err := e.tasks.GetTask(req.TaskID)
		if err != nil {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
		var value string
		switch req.TaskField {
		case "title":
			value = task.Title
		case "description":
			value = task.Description
		case "status":
			value = string(task.Status)
		default:
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("unknown task field %q", req.TaskField)}
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
		if _, err := e.workflow.ApplyAction(context.Background(), req.TaskID, api.ApplyActionRequest{Type: "reopen"}); err != nil {
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
		if err := e.tasks.NotifyTask(context.Background(), req.TaskID, req.Message, req.Ask, req.QuestionID, req.SessionID, req.Progress); err != nil {
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
			if req.ImportDatasourceOverride != "" {
				r.DataSourceID = req.ImportDatasourceOverride
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
	default:
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("unsupported boid op %q", req.Op)}
	}
}
