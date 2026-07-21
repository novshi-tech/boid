package api

import (
	"context"
	"net/http"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ReplayHook replays a single hook for the given task. If req.Status is non-empty
// the task's status is overwritten before dispatch. Running jobs on the same task
// cause a 409 Conflict.
func (s *TaskWorkflowService) ReplayHook(ctx context.Context, taskID string, req ReplayHookRequest) (*ReplayHookResult, error) {
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	meta, ok := s.Meta.Get(task.ProjectID)
	if !ok {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "project meta not loaded: " + task.ProjectID}
	}

	// Check for running jobs.
	jobs, err := s.Jobs.ListJobsByTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	for _, j := range jobs {
		if j.Status == JobStatusRunning {
			return nil, &StatusError{Code: http.StatusConflict, Message: "task has a running job; wait for it to complete before replaying"}
		}
	}

	// Optional status override.
	if req.Status != "" {
		task.Status = orchestrator.TaskStatus(req.Status)
		if err := s.Tasks.UpdateTask(task); err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
	}

	sm := orchestrator.DefaultMachine()
	replay, err := s.Coordinator.ReplayHook(ctx, task, meta, sm, req.HookID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	// Persist payload and optional status advance. Merge replay.PayloadDelta
	// (only what the replayed hook itself wrote) onto a freshly re-read task
	// row — never wholesale-assign replay.FinalPayload, which is built from
	// the task snapshot taken BEFORE the hook ran and would silently discard
	// any out-of-band write the hook made mid-flight (e.g. via
	// `boid task update --payload-patch`), exactly the bug
	// DispatchResult.PayloadDelta's doc comment describes for
	// runDispatchLoop (Phase 5b PR7 codex review Blocker 1, wiring-seams.md
	// #17) — this callsite had the same class of bug in an even blunter
	// form (assignment, not even a merge).
	if err := s.Tx.WithinTx(func(tx TxStore) error {
		latest, err := tx.GetTask(taskID)
		if err != nil {
			return err
		}
		if len(replay.PayloadDelta) > 0 {
			merged, mergeErr := orchestrator.MergePayload(latest.Payload, replay.PayloadDelta)
			if mergeErr != nil {
				return mergeErr
			}
			latest.Payload = merged
		}
		if replay.NewStatus != "" {
			latest.Status = replay.NewStatus
		}
		return tx.UpdateTask(latest)
	}); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	s.persistFiredEvents(taskID, task.Status, replay.FiredEvents)

	// Re-fetch to return the persisted state.
	updated, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return &ReplayHookResult{Task: updated, FiredEvents: replay.FiredEvents}, nil
}

// ListHooksForStatus returns hooks that match the given status for the task.
// If status is empty, the task's current status is used.
func (s *TaskWorkflowService) ListHooksForStatus(taskID, status string) ([]orchestrator.Hook, error) {
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	meta, ok := s.Meta.Get(task.ProjectID)
	if !ok {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "project meta not loaded: " + task.ProjectID}
	}
	effectiveStatus := task.Status
	if status != "" {
		effectiveStatus = orchestrator.TaskStatus(status)
	}
	hooks := orchestrator.ListHooksForStatus(meta, task, effectiveStatus)
	if hooks == nil {
		hooks = []orchestrator.Hook{}
	}
	return hooks, nil
}
