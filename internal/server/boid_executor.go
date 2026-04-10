package server

import (
	"context"
	"fmt"

	"github.com/novshi-tech/boid/internal/api"
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
		task, err := e.tasks.CreateTask(api.CreateTaskRequest{
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
		})
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
	default:
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("unsupported boid op %q", req.Op)}
	}
}
