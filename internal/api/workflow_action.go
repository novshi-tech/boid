package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// isShutdownErr reports whether the dispatch failure was caused by the
// dispatch context being canceled (daemon shutdown). Checks both the ctx
// directly and the error chain so wrapped child-ctx cancellations are
// covered.
func isShutdownErr(ctx context.Context, err error) bool {
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return true
	}
	return errors.Is(err, context.Canceled)
}

func (s *TaskWorkflowService) ApplyAction(ctx context.Context, taskID string, req ApplyActionRequest) (*ActionApplication, error) {
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	// Hydrate with workspace.yaml so kit-supplied hooks / env / capabilities
	// are visible to the dispatch loop. Falls back to bare Get if workspace
	// hydration fails (degraded window).
	var meta *orchestrator.ProjectMeta
	if hydrated, err := s.Meta.GetWithWorkspace(ctx, task.ProjectID); err == nil && hydrated != nil {
		meta = hydrated
	} else {
		var ok bool
		meta, ok = s.Meta.Get(task.ProjectID)
		if !ok {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: "project meta not loaded: " + task.ProjectID}
		}
	}

	sm := orchestrator.DefaultMachine()

	if req.Type == "start" {
		if err := checkDependencies(task, s.Tasks.GetTask); err != nil {
			return nil, &StatusError{Code: http.StatusConflict, Message: "dependency not satisfied: " + err.Error()}
		}
	}

	fromStatus := task.Status
	action := &orchestrator.Action{
		TaskID:  task.ID,
		Type:    req.Type,
		Payload: req.Payload,
	}
	newTask, err := sm.Apply(task, action)
	if err != nil {
		return nil, &StatusError{Code: http.StatusConflict, Message: err.Error()}
	}
	action.FromStatus = fromStatus
	action.ToStatus = newTask.Status

	// reopen carries an optional `{"instruction": {...}}` payload that appends a
	// new entry to the task's instruction history. The instruction is recorded
	// only on the action (audit trail) and not merged into task.payload.
	var reopenPayloadConsumed bool
	if req.Type == "reopen" && len(req.Payload) > 0 {
		var p struct {
			Instruction *orchestrator.Instruction `json:"instruction,omitempty"`
		}
		if err := json.Unmarshal(req.Payload, &p); err == nil && p.Instruction != nil {
			inst := *p.Instruction
			if active := task.Instructions.Active(); active != nil {
				if inst.Agent == "" {
					inst.Agent = active.Agent
				}
				if inst.Model == "" {
					inst.Model = active.Model
				}
			}
			newTask.Instructions = orchestrator.AppendInstruction(task.Instructions, inst)
			reopenPayloadConsumed = true
		}
	}

	if !reopenPayloadConsumed {
		merged, err := orchestrator.MergePayload(task.Payload, action.Payload)
		if err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: "payload merge: " + err.Error()}
		}
		newTask.Payload = merged
	}

	if err := s.Tx.WithinTx(func(tx TxStore) error {
		if err := tx.UpdateTask(newTask); err != nil {
			return err
		}
		return tx.CreateAction(action)
	}); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	// Release the project lock whenever the action moves the task out of
	// executing (ask, done, abort, ...). Idempotent — safe when the task did
	// not hold the lock (e.g. readonly/worktree tasks, or repeated abort).
	if newTask.Status != orchestrator.TaskStatusExecuting {
		s.releaseProjectLock(newTask.ID)
	}

	if s.Hub != nil {
		s.Hub.Broadcast(newTask.ID, TaskEvent{
			Kind: "action",
			Payload: map[string]any{
				"action_id":  action.ID,
				"new_status": string(action.ToStatus),
			},
		})
	}

	s.cleanupWorktree(newTask.ID, task.ProjectID, newTask.Status)

	if s.Coordinator != nil {
		dispatchCtx := s.dispatchCtx
		if dispatchCtx == nil {
			dispatchCtx = context.Background()
		}
		s.dispatchWG.Add(1)
		go func() {
			defer s.dispatchWG.Done()
			s.runDispatchLoop(dispatchCtx, newTask, meta, sm)
		}()
	}

	var matchedHooks []string
	if s.Coordinator != nil {
		if coord, ok := s.Coordinator.(*orchestrator.Coordinator); ok && coord.Evaluator != nil {
			if behavior, _, found := orchestrator.LookupBehaviorWithAlias(meta, newTask.Behavior); found {
				for _, hook := range coord.Evaluator.Evaluate(newTask, behavior.Hooks) {
					matchedHooks = append(matchedHooks, hook.ID)
				}
			}
		}
	}

	return &ActionApplication{
		Task:         newTask,
		Action:       action,
		MatchedHooks: matchedHooks,
	}, nil
}

func (s *TaskWorkflowService) runDispatchLoop(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) {
	const maxCycles = 10
	current := task

	// Branch lock — held for the entire executing lifetime so concurrent root
	// tasks on the same base_branch serialize while child tasks (boid/<id8>)
	// always run in parallel. Idempotent: re-spawned dispatch loops for an
	// already-locked task no-op. Only acquired when task.Status == executing;
	// terminal-task dispatch loops skip acquisition. Readonly tasks (supervisor)
	// skip acquisition: their sandbox hooks run on a readonly mount so no
	// write-level conflict exists, and git operations self-serialize via
	// .git/index.lock. Skipping lets supervisors and executors share the same
	// base_branch without the supervisor blocking the executor.
	if s.Locks != nil && current.Status == orchestrator.TaskStatusExecuting && !current.Readonly {
		headBranch := orchestrator.ComputeHeadBranch(current)
		if err := s.Locks.AcquireForTask(ctx, current.ProjectID, headBranch, current.ID); err != nil {
			slog.Warn("dispatch loop: branch lock acquire failed",
				"task_id", current.ID, "project_id", current.ProjectID, "error", err)
			s.abortOnDispatchError(ctx, current, fmt.Errorf("branch lock: %w", err))
			return
		}
	}

	for cycle := 0; cycle < maxCycles; cycle++ {
		result, err := s.Coordinator.DispatchAndAdvance(ctx, current, meta, sm)
		if err != nil {
			// Persist any partial FiredEvents first so the failing hook
			// remains visible in the timeline; abortOnDispatchError then logs
			// the dispatcher-level error and transitions the task to aborted.
			if result != nil {
				s.persistFiredEvents(current.ID, current.Status, result.FiredEvents)
			}
			slog.Error("dispatch loop error", "task_id", current.ID, "cycle", cycle, "error", err)
			s.abortOnDispatchError(ctx, current, err)
			return
		}

		s.persistFiredEvents(current.ID, current.Status, result.FiredEvents)

		// The awaiting trait is owned exclusively by ApplyAction("ask"/"answer")
		// and is persisted to the DB inline as those actions run. The coordinator's
		// FinalPayload, however, derives from a snapshot of task.Payload taken
		// BEFORE the hook executed, so any awaiting value it carries is necessarily
		// stale: if the hook itself called `boid task notify --ask` mid-flight, the
		// fresh awaiting trait is already in the DB and the snapshot's awaiting
		// would clobber it on top-level merge. Strip awaiting from FinalPayload
		// before the merge and apply pending_answer clearing to the DB-fresh row
		// instead.
		result.FinalPayload = orchestrator.StripAwaitingTrait(result.FinalPayload)

		// Persist hook payload. Always refresh the task row so we
		// can detect concurrent terminal transitions (abort/done) and pick up
		// any awaiting trait written by an ApplyAction("ask") that fired during
		// the hook.
		var persisted *orchestrator.Task
		if err := s.Tx.WithinTx(func(tx TxStore) error {
			latest, err := tx.GetTask(current.ID)
			if err != nil {
				return err
			}
			// Clear pending_answer from the (DB-fresh) awaiting trait now that
			// the hook has been spawned and consumed it. session_id, question,
			// and question_id are preserved so the task can be resumed again
			// if the kit emits another ask.
			latest.Payload = orchestrator.ClearPendingAnswer(latest.Payload)
			if len(result.FinalPayload) > 0 {
				merged, mergeErr := orchestrator.MergePayload(latest.Payload, result.FinalPayload)
				if mergeErr != nil {
					return mergeErr
				}
				latest.Payload = merged
			}
			if err := tx.UpdateTask(latest); err != nil {
				return err
			}
			persisted = latest
			return nil
		}); err != nil {
			slog.Error("persist payload failed", "task_id", current.ID, "error", err)
			s.abortOnDispatchError(ctx, current, fmt.Errorf("persist payload: %w", err))
			return
		}
		current = persisted

		// Drop any would-be auto-advance if the task was terminated
		// concurrently (e.g. user abort while a hook was in flight). Finalize
		// here so the caller that set the terminal status does not have to
		// race with us on cleanup.
		if current.Status == orchestrator.TaskStatusDone || current.Status == orchestrator.TaskStatusAborted {
			slog.Info("dispatch loop: task reached terminal concurrently, skipping advance",
				"task_id", current.ID, "status", current.Status, "would_advance_to", result.NewStatus)
			s.finalizeTerminal(ctx, current)
			return
		}

		// If a hook called boid task notify --ask during this cycle, the task
		// transitioned to awaiting. The lifecycle.executed signal computed from
		// the hook exit is stale — do not auto-advance to done. The dispatch
		// loop will re-fire (via AnswerTask → ApplyAction("answer")) once the
		// user replies.
		if current.Status == orchestrator.TaskStatusAwaiting {
			slog.Info("dispatch loop: task is awaiting user answer, skipping auto-advance",
				"task_id", current.ID, "would_advance_to", result.NewStatus)
			// awaiting means the task left executing — release the project
			// lock so other tasks can run. answer will re-acquire on resume.
			s.releaseProjectLock(current.ID)
			return
		}

		if result.NewStatus == "" {
			// No transition this cycle. Finalize if terminal.
			s.finalizeTerminal(ctx, current)
			return
		}

		prevStatus := current.Status
		action := &orchestrator.Action{
			TaskID:     current.ID,
			Type:       "auto_advance",
			FromStatus: prevStatus,
			ToStatus:   result.NewStatus,
			Payload:    result.ActionPayload,
		}
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

		if current.Status == orchestrator.TaskStatusDone || current.Status == orchestrator.TaskStatusAborted {
			s.finalizeTerminal(ctx, current)
			return
		}
	}

	slog.Warn("dispatch loop max cycles reached", "task_id", current.ID, "max", maxCycles)
}

// TriggerDependents は taskID に依存する pending タスクを評価し、
// auto_start=true かつ依存条件が満たされた場合に自動 start する。
// auto_start=false のタスクは依存解決しても pending のまま残り、
// ユーザが手動で start するまで待機する。
func (s *TaskWorkflowService) TriggerDependents(ctx context.Context, taskID string) {
	s.triggerDependentTasks(ctx, taskID)
}

func (s *TaskWorkflowService) triggerDependentTasks(ctx context.Context, taskID string) {
	if s.Tasks == nil {
		return
	}
	dependents, err := s.Tasks.FindDependentTasks(taskID)
	if err != nil {
		slog.Error("trigger dependent tasks: find dependents", "task_id", taskID, "error", err)
		return
	}
	for _, dep := range dependents {
		if !dep.AutoStart {
			continue
		}
		if err := checkDependencies(dep, s.Tasks.GetTask); err != nil {
			continue
		}
		if _, err := s.ApplyAction(ctx, dep.ID, ApplyActionRequest{Type: "start"}); err != nil {
			slog.Warn("trigger dependent tasks: start failed", "dependent_id", dep.ID, "error", err)
		}
	}
}

func (s *TaskWorkflowService) recordDispatchError(taskID string, taskStatus orchestrator.TaskStatus, err error) {
	if s.Tx == nil || taskID == "" || err == nil {
		return
	}

	payload, marshalErr := json.Marshal(map[string]string{"error": err.Error()})
	if marshalErr != nil {
		slog.Error("marshal dispatch error payload failed", "task_id", taskID, "error", marshalErr)
		return
	}

	// dispatch_error は状態遷移を伴わないため from_status = to_status = 現在のステータス
	action := &orchestrator.Action{
		TaskID:     taskID,
		Type:       "dispatch_error",
		Payload:    payload,
		FromStatus: taskStatus,
		ToStatus:   taskStatus,
	}
	if txErr := s.Tx.WithinTx(func(tx TxStore) error {
		return tx.CreateAction(action)
	}); txErr != nil {
		slog.Error("persist dispatch error failed", "task_id", taskID, "error", txErr)
	}
}

// abortOnDispatchError records a dispatch_error action for the audit trail and
// then transitions the task to aborted so the branch lock is released and
// terminal cleanup (worktree removal, lifecycle window) runs. Safe to call
// even when the lock was never acquired — releaseProjectLock is idempotent.
//
// When the dispatch context has been canceled (typically because the daemon
// is shutting down via SIGTERM), the abort is recorded with
// code=daemon_shutdown instead of dispatch_error. The startup auto-reopen
// path looks for this code via the derived lifecycle.abort trait and
// re-dispatches the task on next boot. No dispatch_error action is emitted
// for shutdown — that channel is reserved for genuine hook failures.
func (s *TaskWorkflowService) abortOnDispatchError(ctx context.Context, task *orchestrator.Task, err error) {
	shutdown := isShutdownErr(ctx, err)

	code := "dispatch_error"
	message := err.Error()
	if shutdown {
		code = "daemon_shutdown"
		message = "daemon が停止したため中断されました。 起動時に自動 reopen されます。"
	} else {
		s.recordDispatchError(task.ID, task.Status, err)
	}

	abortPayload, _ := json.Marshal(map[string]string{
		"code":    code,
		"message": message,
	})
	abortAction := &orchestrator.Action{
		TaskID:     task.ID,
		Type:       "abort",
		FromStatus: task.Status,
		ToStatus:   orchestrator.TaskStatusAborted,
		Payload:    abortPayload,
	}
	task.Status = orchestrator.TaskStatusAborted
	if txErr := s.Tx.WithinTx(func(tx TxStore) error {
		if updErr := tx.UpdateTask(task); updErr != nil {
			return updErr
		}
		return tx.CreateAction(abortAction)
	}); txErr != nil {
		slog.Error("abort on dispatch error: persist abort failed",
			"task_id", task.ID, "error", txErr)
	}
	s.finalizeTerminal(ctx, task)
}

func (s *TaskWorkflowService) persistFiredEvents(taskID string, status orchestrator.TaskStatus, events []orchestrator.FiredEvent) {
	if len(events) == 0 || s.Tx == nil {
		return
	}
	if err := s.Tx.WithinTx(func(tx TxStore) error {
		for _, fe := range events {
			payload, _ := json.Marshal(map[string]any{
				"kit_id":       fe.KitID,
				"hook_id":      fe.HandlerID,
				"job_id":       fe.JobID,
				"source_state": fe.SourceState,
				"success":      fe.Success,
				"error":        fe.Error,
			})
			action := &orchestrator.Action{
				TaskID:     taskID,
				Type:       fe.Kind + "_fired",
				Payload:    payload,
				FromStatus: status,
				ToStatus:   status,
			}
			if err := tx.CreateAction(action); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		slog.Warn("persist fired events failed", "task_id", taskID, "error", err)
		return
	}

	if s.Hub != nil {
		for _, fe := range events {
			s.Hub.Broadcast(taskID, TaskEvent{
				Kind: "fired_event",
				Payload: map[string]any{
					"event_name": fe.Kind + "_fired",
					"role":       fe.HandlerID,
					"kit_id":     fe.KitID,
					"success":    fe.Success,
				},
			})
		}
	}
}

// finalizeTerminal runs the per-task cleanup required once a task has reached
// a terminal status. No-op for non-terminal tasks. Safe to call multiple
// times: cleanupWorktree skips already-removed worktrees and
// CleanupTaskWindow atomically drains runtimes.
//
// Branch lifecycle: cleanupWorktree intentionally does NOT delete this task's
// boid/<id8> branch. Instead, sweepChildBranches deletes the boid/* branches
// of this task's direct children — children retain their branches through
// their own finalize so that this (parent) supervisor can merge them into the
// base branch before they're swept. The sweep is one level deep: nested
// supervisors get cleaned in the same way when their own parent terminates.
func (s *TaskWorkflowService) finalizeTerminal(ctx context.Context, task *orchestrator.Task) {
	if task.Status != orchestrator.TaskStatusDone && task.Status != orchestrator.TaskStatusAborted {
		return
	}
	// Release the executing-lifetime project lock first so a queued waiter on
	// the same project can acquire while the cleanup below is still in flight.
	// Idempotent — safe if the task never acquired the lock.
	s.releaseProjectLock(task.ID)
	s.cleanupWorktree(task.ID, task.ProjectID, task.Status)
	s.sweepChildBranches(task.ID, task.ProjectID)
	if s.Lifecycle != nil {
		s.Lifecycle.CleanupTaskWindow(task.ID)
	}
	if task.Status == orchestrator.TaskStatusDone {
		s.triggerDependentTasks(ctx, task.ID)
	}
	if task.ParentID != "" {
		s.triggerDependentTasks(ctx, task.ParentID)
	}
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

// sweepChildBranches deletes the boid/<id8> branches of the given task's
// direct children. No-op when the task has no children (executor / leaf
// task), no recorded children with worktrees, or required services are
// missing. Errors are logged and non-fatal: branch retention is a hygiene
// concern, not a correctness one.
func (s *TaskWorkflowService) sweepChildBranches(parentTaskID, projectID string) {
	if s.Projects == nil || s.Worktrees == nil || s.Tasks == nil || projectID == "" {
		return
	}
	children, err := s.Tasks.ListChildren(parentTaskID)
	if err != nil {
		slog.Warn("sweep child branches: list children failed",
			"parent_task_id", parentTaskID, "error", err)
		return
	}
	if len(children) == 0 {
		return
	}
	project, err := s.Projects.GetProject(projectID)
	if err != nil {
		slog.Warn("sweep child branches: project lookup failed",
			"parent_task_id", parentTaskID, "project_id", projectID, "error", err)
		return
	}
	taskIDs := make([]string, 0, len(children))
	for _, c := range children {
		taskIDs = append(taskIDs, c.ID)
	}
	if err := s.Worktrees.SweepChildBranches(project.WorkDir, taskIDs); err != nil {
		slog.Warn("sweep child branches failed",
			"parent_task_id", parentTaskID, "project_id", projectID, "error", err)
	}
}
