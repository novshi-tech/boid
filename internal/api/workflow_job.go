package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func (s *TaskWorkflowService) CompleteJob(_ context.Context, jobID string, req JobDoneRequest) (*Job, error) {
	job, err := s.Jobs.GetJob(jobID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	// Idempotency: a second CompleteJob call (e.g. EXIT trap after agent-driven
	// SIGTERM) must not corrupt the already-terminal job or re-fire lifecycle events.
	if job.Status == JobStatusCompleted || job.Status == JobStatusFailed {
		return job, nil
	}

	if req.ExitCode == 0 {
		job.Status = JobStatusCompleted
	} else {
		job.Status = JobStatusFailed
	}
	job.ExitCode = req.ExitCode
	job.Output = req.Output

	finalize := func() {
		if s.Lifecycle == nil {
			return
		}
		s.Lifecycle.CompleteJob(job.ID, JobCompletion{
			Output:   req.Output,
			ExitCode: req.ExitCode,
		})
		s.Lifecycle.UnregisterJob(job.ID)
	}

	if err := s.Jobs.UpdateJob(job); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	defer finalize()

	// Stop the runtime so the agent process receives SIGTERM immediately after
	// calling `boid job done` explicitly, rather than waiting for natural bash
	// exit. The EXIT trap that fires afterward is absorbed by the idempotency
	// guard above. A no-op when RuntimeID is unset or the process has already
	// exited (LocalRuntime.Stop handles that gracefully).
	if job.RuntimeID != "" && s.Lifecycle != nil {
		runtimeID := job.RuntimeID
		go s.Lifecycle.StopJobRuntime(runtimeID)
	}

	// Successful job completion: no state transition here.
	// The runDispatchLoop (hooks → gates → auto-advance) is responsible for
	// evaluating conditions and advancing the task state once all handlers
	// have completed. Transitioning in CompleteJob would race with the gate
	// execution and clean up the worktree before gates can run.
	//
	// Broadcast the running→completed transition so the web timeline can
	// recolor the marker (green) immediately — without waiting for the
	// downstream hook_fired action to land later. The failure path below
	// gets its own broadcast alongside the job_failed action (task-status
	// transition is a separate visual signal).
	if req.ExitCode == 0 {
		if s.Hub != nil {
			s.Hub.Broadcast(job.TaskID, TaskEvent{
				Kind: "job",
				Payload: map[string]any{
					"job_id":     job.ID,
					"new_status": string(job.Status),
				},
			})
		}
		return job, nil
	}

	// Failed job: apply job_failed → aborted.
	task, err := s.Tasks.GetTask(job.TaskID)
	if err != nil {
		slog.Error("job done: task not found", "task_id", job.TaskID)
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "task not found: " + err.Error()}
	}

	if _, ok := s.Meta.Get(job.ProjectID); !ok {
		slog.Error("job done: project meta not loaded", "project_id", job.ProjectID)
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "project meta not loaded: " + job.ProjectID}
	}

	sm := orchestrator.DefaultMachine()

	jobFailedFrom := task.Status
	action := &orchestrator.Action{TaskID: task.ID, Type: "job_failed"}
	newTask, err := sm.Apply(task, action)
	if err != nil {
		slog.Warn("job done: job_failed transition not applicable", "error", err)
		return job, nil
	}
	action.FromStatus = jobFailedFrom
	action.ToStatus = newTask.Status

	if err := s.Tx.WithinTx(func(tx TxStore) error {
		if err := tx.UpdateTask(newTask); err != nil {
			return err
		}
		return tx.CreateAction(action)
	}); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	if s.Hub != nil {
		s.Hub.Broadcast(job.TaskID, TaskEvent{
			Kind: "job",
			Payload: map[string]any{
				"job_id":    job.ID,
				"new_state": string(newTask.Status),
			},
		})
	}

	// job_failed moves the task out of executing — release the project lock so
	// queued tasks on the same project can advance. Idempotent.
	if newTask.Status != orchestrator.TaskStatusExecuting {
		s.releaseProjectLock(newTask.ID)
	}

	slog.Info("job done: job_failed applied", "job_id", job.ID, "new_status", newTask.Status)
	s.cleanupWorktree(newTask.ID, job.ProjectID, newTask.Status)
	return job, nil
}
