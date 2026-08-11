package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// statusErrorForGetTaskErr classifies a GetTask failure: only "the row
// genuinely doesn't exist" (orchestrator.ErrTaskNotFound) is a 404. Anything
// else (a DB connectivity error, a scan failure, ...) is a 500 — collapsing
// every GetTask error into 404 (as Dispatch originally did) would report a
// transient DB outage as "task not found", which is misleading and, worse,
// indistinguishable from the real not-found case in logs/monitoring (codex
// review round 1, Minor).
func statusErrorForGetTaskErr(err error) *StatusError {
	if errors.Is(err, orchestrator.ErrTaskNotFound) {
		return &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
}

// parkPayload is the optional shape of the "park" action's payload:
// {"wake_at": "<RFC3339>", "wake_task_id": "<task id>"}. Both fields are
// optional; a park with no payload still gets a task_triage row (see
// applyParkSideEffect) so ParkedFrom always has somewhere to read from.
type parkPayload struct {
	WakeAt     string `json:"wake_at,omitempty"`
	WakeTaskID string `json:"wake_task_id,omitempty"`
}

// parseParkPayload validates the park action's payload BEFORE the
// transaction opens, so a malformed payload surfaces as 400 rather than
// being swallowed into WithinTx's generic 500 error wrapping.
func parseParkPayload(payload json.RawMessage) (*parkPayload, error) {
	var p parkPayload
	if len(payload) == 0 {
		return &p, nil
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "invalid park payload: " + err.Error()}
	}
	if p.WakeAt != "" {
		if _, err := time.Parse(time.RFC3339, p.WakeAt); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "invalid park payload: wake_at: " + err.Error()}
		}
	}
	return &p, nil
}

// applyParkSideEffect upserts wake_at/wake_task_id into the task_triage
// sidecar as part of the same transaction that records the park action,
// preserving any existing kind/urgency/detail. This is the "real writer"
// PR-1's park/wake vertical slice needed (docs/plans/cross-project-issue-triage.md
// Phase 1 PR-1, Opus指摘#1/#12) — park's origin status itself is NOT
// duplicated here; it's derived later from the actions log via ParkedFrom.
// p must already be validated via parseParkPayload.
func applyParkSideEffect(tx TxStore, taskID string, p *parkPayload) error {
	// Only "no existing row" should start a fresh sidecar. Any other error
	// (DB connectivity, scan failure, ...) was previously swallowed the same
	// way, which would silently blow away an existing row's kind/urgency/
	// detail on a transient failure instead of surfacing it (codex review
	// round 1, Minor).
	tt, err := tx.GetTaskTriage(taskID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("park: get task_triage: %w", err)
		}
		tt = &orchestrator.TaskTriage{TaskID: taskID}
	}

	tt.WakeAt = nil
	if p.WakeAt != "" {
		parsed, err := time.Parse(time.RFC3339, p.WakeAt)
		if err != nil {
			// Unreachable in practice (already validated by parseParkPayload),
			// kept defensive since this runs inside a transaction.
			return &StatusError{Code: http.StatusBadRequest, Message: "invalid park payload: wake_at: " + err.Error()}
		}
		tt.WakeAt = &parsed
	}
	tt.WakeTaskID = p.WakeTaskID

	if err := tx.UpsertTaskTriage(tt); err != nil {
		return fmt.Errorf("park: upsert task_triage: %w", err)
	}
	return nil
}

// Wake is the single user/PR-2/3-facing verb for reviving a parked task. It
// resolves the origin (triaged vs ready) via ParkedFrom — derived from the
// actions log, not a stored column — and applies the matching internal
// action (wake_triaged/wake_ready, rejected if sent directly through the
// public ApplyAction path — see StateMachine.IsManualAction, used by
// ApplyAction's guard in workflow_action.go). This exists specifically so no
// caller can wake a task to the wrong status: getting triaged vs ready wrong
// would silently promote a task past Go without nose's judgment (決定9/逆輸入2).
//
// Unlike ApplyAction's general shape (task read, then a separate
// WithinTx write), Wake re-reads the task AND resolves ParkedFrom from
// inside the SAME transaction as the write. Splitting those into a pre-tx
// read and a later write left a window where a concurrent park→wake→park
// cycle on the same task could change the actions-log origin between the
// read and the write, so a wake in flight could apply against a stale
// origin (codex review round 1, Major). Reading everything transactionally
// closes that window: whichever origin is committed at write time is the
// one that decides wake_triaged vs wake_ready, with no gap to race into.
func (s *TaskWorkflowService) Wake(ctx context.Context, taskID string) (*ActionApplication, error) {
	if s.Tx == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "wake: Transactor not configured"}
	}

	sm := orchestrator.DefaultMachine()
	var newTask *orchestrator.Task
	var action *orchestrator.Action

	if err := s.Tx.WithinTx(func(tx TxStore) error {
		fresh, err := tx.GetTask(taskID)
		if err != nil {
			return statusErrorForGetTaskErr(err)
		}
		if fresh.Status != orchestrator.TaskStatusParked {
			return &StatusError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("cannot wake task in status %q (must be parked)", fresh.Status),
			}
		}
		from, err := tx.ParkedFrom(taskID)
		if err != nil {
			return &StatusError{Code: http.StatusConflict, Message: "wake: cannot resolve park origin: " + err.Error()}
		}
		var resolvedType string
		switch from {
		case orchestrator.TaskStatusTriaged:
			resolvedType = "wake_triaged"
		case orchestrator.TaskStatusReady:
			resolvedType = "wake_ready"
		default:
			return &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("wake: unexpected park origin %q", from)}
		}

		action = &orchestrator.Action{TaskID: taskID, Type: resolvedType}
		newTask, err = sm.Apply(fresh, action)
		if err != nil {
			return &StatusError{Code: http.StatusConflict, Message: err.Error()}
		}
		action.FromStatus = fresh.Status
		action.ToStatus = newTask.Status

		if err := tx.UpdateTask(newTask); err != nil {
			return err
		}
		return tx.CreateAction(action)
	}); err != nil {
		var statusErr *StatusError
		if errors.As(err, &statusErr) {
			return nil, statusErr
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
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

	return &ActionApplication{Task: newTask, Action: action}, nil
}

// Dispatch is the machine-driven second stage of Go (docs/plans/
// cross-project-issue-triage.md Phase 1 PR-2, 逆輸入2: 「Go 操作 = ready 遷移 +
// 機械 dispatch の 2 段で実装する」). It always moves a ready task straight to
// working — 逆輸入2 defines working as covering BOTH "dispatched な子あり" and
// "nose が手動対応中で子は open のみ", so the transition does not depend on
// there being any specced children to task-ify. Along the way it task-ifies
// every `specced` entry in task_triage.detail.children into a real child boid
// task (逆輸入1); `open` entries are left untouched, per PR-1's plan ("open/
// specced な子は... dispatch 時にのみ task 化する").
//
// Like Wake, this is Manual:false in the state machine (see machine.go's
// "dispatch" rule) and therefore unreachable through the public ApplyAction
// endpoint — StateMachine.IsManualAction gates it out. Unlike Wake, there is
// currently only one caller: ApplyAction chains into this automatically right
// after a "ready" action commits (workflow_action.go), so Dispatch is not
// exposed as a standalone HTTP/CLI entry point in PR-2.
//
// Child task creation happens BEFORE the state-transition transaction opens:
// TaskCreator.CreateTask does its own non-transactional work (project/meta
// lookups, its own TaskStore.CreateTask write) that must not run nested
// inside this method's own WithinTx — SQLite only tolerates one open write
// transaction at a time, so a second writer invoked from inside an
// already-open one would deadlock against it. This means full atomicity
// across "create N child tasks" + "flip ready→working" is NOT guaranteed the
// way Wake's read-decide-write is: a crash (or a concurrent drop/park racing
// in) between child creation and the final transaction could leave orphan
// child tasks whose task_triage entry never got marked dispatched. Given the
// only caller today is ApplyAction("ready")'s own synchronous follow-up, this
// window is accepted as a known PR-2 gap rather than closed here — a future
// PR that adds a queue sweep / retry path (PR-3) would need a reconciliation
// step for it. The final transaction re-checks the task is still ready
// immediately before committing, so at least the transition itself never
// silently applies against a status that moved out from under it.
func (s *TaskWorkflowService) Dispatch(ctx context.Context, taskID string) (*ActionApplication, error) {
	if s.Tx == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "dispatch: Transactor not configured"}
	}

	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, statusErrorForGetTaskErr(err)
	}
	if task.Status != orchestrator.TaskStatusReady {
		return nil, &StatusError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("cannot dispatch task in status %q (must be ready)", task.Status),
		}
	}

	var children []orchestrator.TaskTriageChild
	if s.TaskTriage != nil {
		tt, ttErr := s.TaskTriage.GetTaskTriage(taskID)
		if ttErr != nil {
			if !errors.Is(ttErr, sql.ErrNoRows) {
				return nil, &StatusError{Code: http.StatusInternalServerError, Message: "dispatch: get task_triage: " + ttErr.Error()}
			}
			// No task_triage row at all: treat as "no children" (a triage
			// task that reached ready without ever getting a sidecar row —
			// e.g. created directly with initial_status=ready in a test —
			// still has to be dispatchable).
		} else {
			children, err = orchestrator.DetailChildren(tt.Detail)
			if err != nil {
				return nil, &StatusError{Code: http.StatusInternalServerError, Message: "dispatch: parse children: " + err.Error()}
			}
		}
	}

	// Validate every child's status up front, before creating anything.
	// Without this an unrecognized value (e.g. a "specced" typo) would just
	// silently fall through the switch below as "not specced, skip" — the
	// child never gets task-ified and nothing ever surfaces that mismatch
	// (codex review round 1, Minor).
	for i := range children {
		switch children[i].Status {
		case orchestrator.TaskTriageChildStatusOpen,
			orchestrator.TaskTriageChildStatusSpecced,
			orchestrator.TaskTriageChildStatusDispatched,
			orchestrator.TaskTriageChildStatusClosed:
		default:
			return nil, &StatusError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("dispatch: child %q has unrecognized status %q", children[i].ID, children[i].Status),
			}
		}
	}

	childrenChanged := false
	for i := range children {
		if children[i].Status != orchestrator.TaskTriageChildStatusSpecced {
			continue
		}
		if children[i].Spec == nil {
			return nil, &StatusError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("dispatch: child %q is specced but has no spec", children[i].ID),
			}
		}
		if s.TaskCreator == nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: "dispatch: TaskCreator not configured"}
		}
		var instructions json.RawMessage
		if children[i].Spec.Instruction != "" {
			marshaled, mErr := json.Marshal([]orchestrator.Instruction{{Message: children[i].Spec.Instruction}})
			if mErr != nil {
				return nil, &StatusError{Code: http.StatusInternalServerError, Message: "dispatch: marshal instruction: " + mErr.Error()}
			}
			instructions = marshaled
		}
		// AutoStart: true (codex review round 1, Major) — without it the
		// created child sits in "pending" forever: nothing else in PR-2's
		// scope (no queue sweep, no cron) ever starts it, so "task-ified"
		// would silently mean "created but never runs". A child whose
		// auto-start itself fails (childTask.Status still pending on return
		// — see TaskAppService.CreateTask's own auto_start guard, which
		// logs and continues rather than erroring) is treated as a dispatch
		// failure below rather than being marked dispatched with a
		// task_ref pointing at a task that will never execute.
		// Ref: children[i].ID makes this call idempotent (codex review round
		// 2, Major): CreateTask's own (ref, parent_id) get-or-create dedup
		// (task_create.go, guarded by the idx_tasks_ref_parent unique index)
		// means that if an earlier child in this loop already got created
		// and a LATER child then fails (e.g. its own auto-start comes back
		// pending), the caller has no way to retry Dispatch from within
		// PR-2's scope today — but if a future caller ever does retry it
		// (PR-3's queue sweep is the obvious candidate), replaying this loop
		// from the top must not re-create-and-restart the already-succeeded
		// child. Without a stable Ref, CreateTask would insert a brand-new
		// row for the same child.ID on replay, duplicating the work and
		// starting it a second time.
		childTask, cErr := s.TaskCreator.CreateTask(CreateTaskRequest{
			ProjectID:    children[i].Spec.Project,
			Title:        children[i].Title,
			Behavior:     children[i].Spec.Behavior,
			Instructions: instructions,
			ParentID:     taskID,
			Ref:          children[i].ID,
			AutoStart:    true,
		})
		if cErr != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("dispatch: create child task %q: %v", children[i].ID, cErr)}
		}
		if childTask.Status == orchestrator.TaskStatusPending {
			return nil, &StatusError{
				Code:    http.StatusInternalServerError,
				Message: fmt.Sprintf("dispatch: child task %q (%s) was created but failed to auto-start (still pending)", children[i].ID, childTask.ID),
			}
		}
		children[i].Status = orchestrator.TaskTriageChildStatusDispatched
		children[i].TaskRef = childTask.ID
		childrenChanged = true
	}

	sm := orchestrator.DefaultMachine()
	action := &orchestrator.Action{TaskID: taskID, Type: "dispatch"}
	var newTask *orchestrator.Task

	if err := s.Tx.WithinTx(func(tx TxStore) error {
		fresh, ferr := tx.GetTask(taskID)
		if ferr != nil {
			return statusErrorForGetTaskErr(ferr)
		}
		if fresh.Status != orchestrator.TaskStatusReady {
			return &StatusError{
				Code: http.StatusConflict,
				Message: fmt.Sprintf(
					"dispatch: task status changed to %q before the ready->working transition could commit",
					fresh.Status),
			}
		}

		var applyErr error
		newTask, applyErr = sm.Apply(fresh, action)
		if applyErr != nil {
			return &StatusError{Code: http.StatusConflict, Message: applyErr.Error()}
		}
		action.FromStatus = fresh.Status
		action.ToStatus = newTask.Status

		if err := tx.UpdateTask(newTask); err != nil {
			return err
		}
		if err := tx.CreateAction(action); err != nil {
			return err
		}
		if childrenChanged {
			tt, gErr := tx.GetTaskTriage(taskID)
			if gErr != nil {
				return fmt.Errorf("dispatch: get task_triage for children update: %w", gErr)
			}
			newDetail, sErr := orchestrator.SetDetailChildren(tt.Detail, children)
			if sErr != nil {
				return sErr
			}
			tt.Detail = newDetail
			return tx.UpsertTaskTriage(tt)
		}
		return nil
	}); err != nil {
		var statusErr *StatusError
		if errors.As(err, &statusErr) {
			return nil, statusErr
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
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

	return &ActionApplication{Task: newTask, Action: action}, nil
}
