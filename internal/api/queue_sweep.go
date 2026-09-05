package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// SweepWake is a periodic, decision-only sweep (clock + task DB only, no
// agent judgment) that only records that orchestrator.ShouldWake fired
// (recordWakeDue) — a wake_due action, no transition, no suggestion. khi
// reads wake_due off the action log and decides what (if anything) to
// suggest. Card machine v2 has exactly one park origin (working) and no
// wake_* rules at all, so there is nothing for this sweep to resolve
// beyond recording the fact.
//
// A failure evaluating or recording one task is logged and does not abort
// the sweep. Returns the ids of tasks wake_due was recorded for this tick.
func (s *TaskWorkflowService) SweepWake(ctx context.Context, now time.Time) (woken []string, err error) {
	if s.Tasks == nil || s.TaskTriage == nil || s.Tx == nil {
		return nil, nil
	}
	// This is the machine-driven wake sweep, no human in the loop — must
	// never be confused with a human action.
	ctx = orchestrator.WithActor(ctx, orchestrator.ActorDaemon)
	parked, err := s.Tasks.ListTasks(orchestrator.TaskFilter{Status: string(orchestrator.TaskStatusParked)})
	if err != nil {
		return nil, fmt.Errorf("sweep wake: list parked tasks: %w", err)
	}
	for _, t := range parked {
		tt, ttErr := s.TaskTriage.GetTaskTriage(t.ID)
		if ttErr != nil {
			if !errors.Is(ttErr, sql.ErrNoRows) {
				slog.Warn("queue sweep: get task_triage failed", "task_id", t.ID, "error", ttErr)
			}
			// sql.ErrNoRows: a parked task with no sidecar row at all has no
			// wake condition to evaluate — not an error, just nothing to do.
			continue
		}

		wakeTaskFound := false
		var wakeTaskStatus orchestrator.TaskStatus
		if tt.WakeTaskID != "" {
			wt, wErr := s.Tasks.GetTask(tt.WakeTaskID)
			switch {
			case wErr == nil:
				wakeTaskFound = true
				wakeTaskStatus = wt.Status
			case errors.Is(wErr, orchestrator.ErrTaskNotFound):
				// wakeTaskFound stays false: ShouldWake treats a vanished
				// reference as terminal (fail-open, see its own doc comment).
			default:
				slog.Warn("queue sweep: get wake_task_id failed", "task_id", t.ID, "wake_task_id", tt.WakeTaskID, "error", wErr)
				continue
			}
		}

		if !orchestrator.ShouldWake(now, tt, wakeTaskFound, wakeTaskStatus) {
			continue
		}
		if recErr := s.recordWakeDue(ctx, t.ID); recErr != nil {
			slog.Warn("queue sweep: record wake_due failed", "task_id", t.ID, "error", recErr)
			continue
		}
		woken = append(woken, t.ID)
	}
	return woken, nil
}

// recordWakeDue is SweepWake's actual write: a non-transitioning "wake_due"
// action, self-recorded directly via tx.CreateAction (never routed through
// sm.Apply/ApplyAction). Clears tt.WakeAt/WakeTaskID in the same
// transaction — the fields that made orchestrator.ShouldWake return true
// in the first place — which is what stops this from firing every tick
// forever: with no transition to consume the condition implicitly,
// clearing it here is the only mechanism left.
func (s *TaskWorkflowService) recordWakeDue(ctx context.Context, taskID string) error {
	return s.Tx.WithinTx(func(tx TxStore) error {
		tt, err := tx.GetTaskTriage(taskID)
		if err != nil {
			return fmt.Errorf("record wake_due: get task_triage: %w", err)
		}
		tt.WakeAt = nil
		tt.WakeTaskID = ""
		if err := tx.UpsertTaskTriage(tt); err != nil {
			return fmt.Errorf("record wake_due: upsert task_triage: %w", err)
		}
		fresh, ferr := tx.GetTask(taskID)
		if ferr != nil {
			return fmt.Errorf("record wake_due: get task: %w", ferr)
		}
		action := &orchestrator.Action{
			TaskID:     taskID,
			Type:       "wake_due",
			FromStatus: fresh.Status,
			ToStatus:   fresh.Status,
			Actor:      orchestrator.ActorFromContext(ctx),
		}
		return tx.CreateAction(ctx, action)
	})
}

// SweepReconcileChildren re-checks every dispatched child of every working
// card against its real current task status, self-healing any whose
// task_triage.detail entry still says "dispatched" after the real task
// already reached done/aborted. This is bare bookkeeping over
// detail.children[].status, NOT a card transition — it never touches
// task.Status.
//
// Without this periodic re-check, a "dispatched" entry that never gets its
// child_closed self-record (a transient DB error, or a child GC'd out
// from under it) would stick forever, and khi would read "a child is still
// running" off that stale entry and never suggest "done" for a card that
// has, in reality, already finished.
//
// A failure evaluating one task is logged and does not abort the sweep.
func (s *TaskWorkflowService) SweepReconcileChildren(ctx context.Context, _ time.Time) error {
	if s.Tasks == nil || s.TaskTriage == nil {
		return nil
	}
	working, err := s.Tasks.ListTasks(orchestrator.TaskFilter{Status: string(orchestrator.TaskStatusWorking)})
	if err != nil {
		return fmt.Errorf("sweep reconcile children: list working tasks: %w", err)
	}
	for _, t := range working {
		tt, ttErr := s.TaskTriage.GetTaskTriage(t.ID)
		if ttErr != nil {
			if !errors.Is(ttErr, sql.ErrNoRows) {
				slog.Warn("sweep reconcile children: get task_triage failed", "task_id", t.ID, "error", ttErr)
			}
			// No sidecar row: not a triage card — nothing to reconcile.
			continue
		}
		children, cErr := orchestrator.DetailChildren(tt.Detail)
		if cErr != nil {
			slog.Warn("sweep reconcile children: parse children failed", "task_id", t.ID, "error", cErr)
			continue
		}
		s.reconcileDispatchedChildren(t.ID, children)
	}
	return nil
}

// reconcileDispatchedChildren marks any child closed whose referenced task
// has actually reached a terminal status, mutating children in place (for
// any FUTURE caller that also wants the reconciled snapshot in-memory — the
// current sole caller, SweepReconcileChildren, does not) and persisting
// through the same self-record path accept(go) and finalizeTerminal use
// (recordChildClosedOnParent), so the child_closed action is written exactly
// once — MarkDetailChildClosed reports changed=false for an already-closed
// child.
func (s *TaskWorkflowService) reconcileDispatchedChildren(taskID string, children []orchestrator.TaskTriageChild) {
	for i := range children {
		if children[i].Status != orchestrator.TaskTriageChildStatusDispatched || children[i].TaskRef == "" {
			continue
		}
		childTask, err := s.Tasks.GetTask(children[i].TaskRef)
		if err != nil {
			// A vanished child (deleted, GC'd) is treated as closed, matching
			// ShouldWake's fail-open posture toward a missing reference: a
			// reference that no longer exists must not strand the card forever.
			if errors.Is(err, orchestrator.ErrTaskNotFound) {
				// Mutating children[i] alone is not enough: this local slice is
				// discarded by the caller (it never calls UpsertTaskTriage with
				// it), so without persisting here the "closed" mapping never
				// survives past this call, and the sweep re-derives the same
				// no-op result every tick forever.
				children[i].Status = orchestrator.TaskTriageChildStatusClosed
				s.recordVanishedChildClosedOnParent(taskID, children[i].TaskRef)
			} else {
				slog.Warn("sweep reconcile children: get dispatched child failed", "task_id", taskID, "child_task_id", children[i].TaskRef, "error", err)
			}
			continue
		}
		if childTask.Status != orchestrator.TaskStatusDone && childTask.Status != orchestrator.TaskStatusAborted {
			continue
		}
		children[i].Status = orchestrator.TaskTriageChildStatusClosed
		s.recordChildClosedOnParent(childTask)
	}
}

// recordVanishedChildClosedOnParent persists a vanished child's "closed"
// mapping onto its parent's task_triage sidecar — the vanished-child
// sibling of recordChildClosedOnParent (workflow_card.go), which cannot be
// reused directly here since it takes a real child *orchestrator.Task, and
// a vanished child has none of those fields available — only the parent's
// own task ID and the dangling TaskRef string survive.
func (s *TaskWorkflowService) recordVanishedChildClosedOnParent(parentTaskID, childTaskRef string) {
	if s.Tx == nil {
		return
	}
	if err := s.Tx.WithinTx(func(tx TxStore) error {
		tt, err := tx.GetTaskTriage(parentTaskID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// Parent's row vanished too (or never existed) between the
				// read above and this Tx opening — nothing left to record.
				return nil
			}
			return fmt.Errorf("sweep reconcile children: get parent task_triage: %w", err)
		}
		newDetail, changed, merr := orchestrator.MarkDetailChildClosed(tt.Detail, childTaskRef)
		if merr != nil {
			return fmt.Errorf("sweep reconcile children: mark detail child closed: %w", merr)
		}
		if !changed {
			return nil
		}
		tt.Detail = newDetail
		if err := tx.UpsertTaskTriage(tt); err != nil {
			return fmt.Errorf("sweep reconcile children: upsert parent task_triage: %w", err)
		}
		parentTask, gErr := tx.GetTask(parentTaskID)
		if gErr != nil {
			return fmt.Errorf("sweep reconcile children: get parent task: %w", gErr)
		}
		// "child_id" (not "child_task_id"), matching recordChildClosedOnParent's
		// existing child_closed payload key exactly (workflow_card.go) — a
		// future consumer reading this payload by key must not silently miss
		// vanished-child rows because they alone spelled the value differently.
		payload, _ := json.Marshal(map[string]string{"child_id": childTaskRef, "child_status": "vanished"})
		action := &orchestrator.Action{
			TaskID:     parentTaskID,
			Type:       "child_closed",
			FromStatus: parentTask.Status,
			ToStatus:   parentTask.Status,
			Payload:    payload,
			Actor:      orchestrator.ActorDaemon,
		}
		// context.Background(): this sweep is daemon-originated bookkeeping
		// (never a sandbox write — see the Actor above), so there is no
		// TokenContext-carried writer project to thread through.
		if err := tx.CreateAction(context.Background(), action); err != nil {
			return err
		}
		// Same updated_at bump recordChildClosedOnParent's real-child path
		// applies (workflow_card.go): a vanished child is still a
		// child_closed event from the parent's point of view.
		return tx.TouchTaskUpdatedAt(parentTaskID)
	}); err != nil {
		slog.Error("vanished child_closed self-record failed", "task_id", parentTaskID, "child_task_id", childTaskRef, "error", err)
	}
}

// QueueSweepStore is the interface QueueSweepLoop needs — narrowed from
// *TaskWorkflowService so the loop can be unit tested against a fake.
//
// SweepTriage and SweepReopen are gone as of card machine v2: both were
// machine-driven transitions (auto-done, auto-reopen) the redesign retires
// entirely. done/reopen are now suggested by khi and applied only by a
// human's accept, through the ordinary ApplyAction/accept(verb) path —
// there is nothing left for a periodic sweep to decide on their behalf.
// SweepReconcileChildren (above) is not one of those transitions — it
// survives because it never touches task.Status.
type QueueSweepStore interface {
	SweepWake(ctx context.Context, now time.Time) ([]string, error)
	SweepReconcileChildren(ctx context.Context, now time.Time) error
}

// QueueSweepLoop periodically calls SweepWake, mirroring
// orchestrator.GCLoop's shape (internal/orchestrator/gc_loop.go) — same
// InitialDelay-then-Interval idiom, errors logged and never fatal to the
// loop.
type QueueSweepLoop struct {
	Store        QueueSweepStore
	Interval     time.Duration
	InitialDelay time.Duration
}

// Run blocks until ctx is done. It waits InitialDelay before the first
// sweep, then calls Store.SweepWake every Interval.
func (l *QueueSweepLoop) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(l.InitialDelay):
	}

	l.runOnce(ctx)

	ticker := time.NewTicker(l.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.runOnce(ctx)
		}
	}
}

func (l *QueueSweepLoop) runOnce(ctx context.Context) {
	now := time.Now()

	woken, err := l.Store.SweepWake(ctx, now)
	if err != nil {
		slog.Warn("queue wake sweep failed", "error", err)
		// Deliberately NOT a return: SweepReconcileChildren is independent
		// bookkeeping (detail.children[].status, never task.Status) — a wake
		// sweep failure must not also block it.
	} else if len(woken) > 0 {
		slog.Info("queue wake sweep recorded wake_due", "count", len(woken), "task_ids", woken)
	}

	if err := l.Store.SweepReconcileChildren(ctx, now); err != nil {
		slog.Warn("queue child reconciliation sweep failed", "error", err)
	}
}
