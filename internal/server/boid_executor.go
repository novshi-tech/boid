package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

type boidBuiltinExecutor struct {
	workflow *api.TaskWorkflowService
	tasks    *api.TaskAppService
}

func newBoidBuiltinExecutor(workflow *api.TaskWorkflowService, tasks *api.TaskAppService) sandbox.BoidExecutor {
	if workflow == nil && tasks == nil {
		return nil
	}
	return &boidBuiltinExecutor{
		workflow: workflow,
		tasks:    tasks,
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
		if req.ProjectID == "" {
			req.ProjectID = ctx.ProjectID
		}
		if req.ProjectID == "" {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task create requires a project"}
		}
		if !ctx.AllowsProject(req.ProjectID) {
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid task create is restricted to the current workspace"}
		}
		createReq := api.CreateTaskRequest{
			ProjectID:        req.ProjectID,
			Title:            req.Title,
			Behavior:         req.Behavior,
			Description:      req.Description,
			Payload:          req.Payload,
			Ref:              req.Ref,
			ParentID:         req.ParentID,
			DependsOn:        req.DependsOn,
			DependsOnPayload: req.DependsOnPayload,
			AutoStart:        req.AutoStart,
		}
		if req.BehaviorSpec != nil {
			createReq.BehaviorSpec = &orchestrator.BehaviorSpec{
				Name:           req.BehaviorSpec.Name,
				Traits:         req.BehaviorSpec.Traits,
				Readonly:       req.BehaviorSpec.Readonly,
				Worktree:       req.BehaviorSpec.Worktree,
				BranchPrefix:   req.BehaviorSpec.BranchPrefix,
				BaseBranch:     req.BehaviorSpec.BaseBranch,
				DefaultPayload: orchestrator.RawPayload(req.BehaviorSpec.DefaultPayload),
			}
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
		updateReq := api.UpdateTaskRequest{
			Title:       req.Title,
			Description: req.Description,
		}
		if len(req.Payload) > 0 {
			updateReq.Payload = req.Payload
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
	default:
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("unsupported boid op %q", req.Op)}
	}
}
