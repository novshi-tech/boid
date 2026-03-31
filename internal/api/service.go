package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return e.Message
}

type ActionApplication struct {
	Task   *orchestrator.Task   `json:"task"`
	Action *orchestrator.Action `json:"action"`
}

type TaskWorkflowService struct {
	Tasks       TaskStore
	Jobs        JobStore
	Projects    ProjectRepository
	Tx          Transactor
	Meta        MetaStore
	Resolver    TransitionResolver
	Coordinator DispatchCoordinator
	Lifecycle   JobLifecycle
	Worktrees   WorktreeCleaner
}

func (s *TaskWorkflowService) ApplyAction(ctx context.Context, taskID string, req ApplyActionRequest) (*ActionApplication, error) {
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	meta, ok := s.Meta.Get(task.ProjectID)
	if !ok {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "project meta not loaded: " + task.ProjectID}
	}

	sm, err := s.Resolver.Resolve(meta, task.Behavior)
	if err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	action := &orchestrator.Action{
		TaskID:  task.ID,
		Type:    req.Type,
		Payload: req.Payload,
	}
	newTask, err := sm.Apply(task, action)
	if err != nil {
		return nil, &StatusError{Code: http.StatusConflict, Message: err.Error()}
	}

	merged, err := orchestrator.MergePayload(task.Payload, action.Payload)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "payload merge: " + err.Error()}
	}
	newTask.Payload = merged

	if err := s.Tx.WithinTx(func(tx TxStore) error {
		if err := tx.UpdateTask(newTask); err != nil {
			return err
		}
		return tx.CreateAction(action)
	}); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	s.cleanupWorktree(newTask.ID, task.ProjectID, newTask.Status)

	if s.Coordinator != nil {
		behavior, _ := meta.TaskBehaviors[newTask.Behavior]
		go s.runDispatchLoop(ctx, newTask, meta, &behavior, sm)
	}

	return &ActionApplication{
		Task:   newTask,
		Action: action,
	}, nil
}

func (s *TaskWorkflowService) CompleteJob(_ context.Context, jobID string, req JobDoneRequest) (*dispatcher.Job, error) {
	job, err := s.Jobs.GetJob(jobID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	if req.ExitCode == 0 {
		job.Status = dispatcher.JobStatusCompleted
	} else {
		job.Status = dispatcher.JobStatusFailed
	}
	job.ExitCode = req.ExitCode
	job.Output = req.Output

	actionType := "job_completed"
	if req.ExitCode != 0 {
		actionType = "job_failed"
	}

	task, err := s.Tasks.GetTask(job.TaskID)
	if err != nil {
		slog.Error("job done: task not found", "task_id", job.TaskID)
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "task not found: " + err.Error()}
	}

	meta, ok := s.Meta.Get(job.ProjectID)
	if !ok {
		slog.Error("job done: project meta not loaded", "project_id", job.ProjectID)
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "project meta not loaded: " + job.ProjectID}
	}

	sm, err := s.Resolver.Resolve(meta, task.Behavior)
	if err != nil {
		slog.Error("job done: resolve transition", "error", err)
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "resolve transition: " + err.Error()}
	}

	action := &orchestrator.Action{TaskID: task.ID, Type: actionType}
	newTask, err := sm.Apply(task, action)
	if err != nil {
		slog.Warn("job done: transition not applicable", "action", actionType, "error", err)
		if err := s.Jobs.UpdateJob(job); err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		return job, nil
	}

	if err := s.Tx.WithinTx(func(tx TxStore) error {
		if err := tx.UpdateJob(job); err != nil {
			return err
		}
		if err := tx.UpdateTask(newTask); err != nil {
			return err
		}
		return tx.CreateAction(action)
	}); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	slog.Info("job done: auto-applied action", "job_id", job.ID, "action", actionType, "new_status", newTask.Status)

	if s.Lifecycle != nil {
		s.Lifecycle.CompleteJob(job.ID, dispatcher.JobCompletionResult{
			Output:   req.Output,
			ExitCode: req.ExitCode,
		})
		s.Lifecycle.UnregisterJob(job.ID)
	}

	s.cleanupWorktree(newTask.ID, job.ProjectID, newTask.Status)
	return job, nil
}

func (s *TaskWorkflowService) runDispatchLoop(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, behavior *orchestrator.TaskBehavior, sm *orchestrator.StateMachine) {
	const maxCycles = 10
	current := task

	for cycle := 0; cycle < maxCycles; cycle++ {
		result, err := s.Coordinator.DispatchAndAdvance(ctx, current, meta, behavior, sm)
		if err != nil {
			slog.Error("dispatch loop error", "task_id", current.ID, "cycle", cycle, "error", err)
			return
		}

		if len(result.FinalPayload) > 0 {
			current.Payload = result.FinalPayload
			if err := s.Tx.WithinTx(func(tx TxStore) error {
				return tx.UpdateTask(current)
			}); err != nil {
				slog.Error("persist payload failed", "task_id", current.ID, "error", err)
				return
			}
		}

		if result.NewStatus == "" {
			return
		}

		action := &orchestrator.Action{TaskID: current.ID, Type: "auto_advance"}
		current.Status = result.NewStatus
		if err := s.Tx.WithinTx(func(tx TxStore) error {
			if err := tx.UpdateTask(current); err != nil {
				return err
			}
			return tx.CreateAction(action)
		}); err != nil {
			slog.Error("auto-advance persist failed", "task_id", current.ID, "error", err)
			return
		}

		slog.Info("auto-advanced", "task_id", current.ID, "new_status", current.Status, "cycle", cycle)
		s.cleanupWorktree(current.ID, current.ProjectID, current.Status)

		if current.Status == orchestrator.TaskStatusDone || current.Status == orchestrator.TaskStatusAborted {
			if s.Lifecycle != nil {
				s.Lifecycle.CleanupTaskWindow(current.ID)
			}
			return
		}
	}

	slog.Warn("dispatch loop max cycles reached", "task_id", current.ID, "max", maxCycles)
}

func (s *TaskWorkflowService) cleanupWorktree(taskID, projectID string, status orchestrator.TaskStatus) {
	if s.Projects == nil || s.Worktrees == nil || projectID == "" {
		return
	}

	project, err := s.Projects.GetProject(projectID)
	if err != nil {
		slog.Warn("worktree cleanup project lookup failed", "task_id", taskID, "project_id", projectID, "error", err)
		return
	}
	if err := s.Worktrees.CleanupForTask(taskID, project.WorkDir, string(status)); err != nil {
		slog.Warn("worktree cleanup failed", "task_id", taskID, "project_id", projectID, "error", err)
	}
}
