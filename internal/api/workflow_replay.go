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

	// Hydrate with workspace.yaml so workspace-level task_behaviors are
	// visible to the replayed hook, matching the dispatch-loop's own meta.
	// No fallback to bare Get here (unlike CreateTask): a bare-Get miss
	// already 500'd before this switch, so GetWithWorkspace erroring for ANY
	// reason keeps landing on the exact same 500.
	meta, err := s.Meta.GetWithWorkspace(ctx, task.ProjectID)
	if err != nil {
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

	// ReplayHook is a generic per-task endpoint (any task ID, including a
	// card's), so — unlike Wake/Dispatch/autoDone/autoReopen/CompleteJob,
	// each of which only ever runs against one definite side of the split —
	// the governing machine must be resolved dynamically per task.
	// machineFor is a pure function of task.Type, so it cannot fail. A card
	// reaching this far still fails fast just below —
	// Coordinator.ReplayHook's lookupBehavior(meta, task) returns
	// (TaskBehavior{}, false) for a nil task.Exec, which surfaces as
	// "behavior not found" (400), the same as a card with an unmatched
	// behavior name always did.
	sm := machineFor(task)
	replay, err := s.Coordinator.ReplayHook(ctx, task, meta, sm, req.HookID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	// Persist payload and optional status advance. Merge replay.PayloadDelta
	// (only what the replayed hook itself wrote) onto a freshly re-read task
	// row — never wholesale-assign replay.FinalPayload, which is built from
	// the task snapshot taken BEFORE the hook ran and would silently discard
	// any out-of-band write the hook made mid-flight (e.g. via
	// `boid task update --payload-patch`), the same class of bug
	// DispatchResult.PayloadDelta's doc comment describes for
	// runDispatchLoop.
	if err := s.Tx.WithinTx(func(tx TxStore) error {
		latest, err := tx.GetTask(taskID)
		if err != nil {
			return err
		}
		if len(replay.PayloadDelta) > 0 && latest.Exec != nil {
			merged, mergeErr := orchestrator.MergePayload(latest.Exec.Payload, replay.PayloadDelta)
			if mergeErr != nil {
				return mergeErr
			}
			latest.Exec.Payload = merged
		}
		if replay.NewStatus != "" {
			latest.Status = replay.NewStatus
		}
		return tx.UpdateTask(latest)
	}); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	s.persistFiredEvents(ctx, taskID, task.Status, replay.FiredEvents)

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
	// Same switch (and same "no fallback" rationale) as ReplayHook above.
	meta, err := s.Meta.GetWithWorkspace(context.Background(), task.ProjectID)
	if err != nil {
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
