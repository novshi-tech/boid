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

// SweepWake implements 決定12's wake evaluation as a periodic, decision-only
// sweep (clock + task DB only, no agent judgment) — REDUCED, as of card
// machine v2 (docs/plans/suggestion-as-state-transition-impl.md §3.4, §C),
// to a pure fact-recording role. v1's SweepWake resolved a parked task's
// origin (via ParkedFrom) and applied the matching wake_triaged/wake_ready/
// wake_working transition itself — the daemon deciding, on its own, what a
// re-surfaced card should become. Design doc §3.3's "機械遷移ゼロ" retires
// that: v2's card machine has exactly one park origin (working) and no
// wake_* rules at all, so there is nothing left to resolve. SweepWake now
// only records that orchestrator.ShouldWake fired (recordWakeDue) — a
// wake_due action, no transition, no suggestion. khi reads wake_due off the
// action log and decides what (if anything) to suggest.
//
// A failure evaluating or recording one task is logged and does not abort
// the sweep — the same posture v1 established. Returns the ids of tasks
// wake_due was recorded for this tick.
func (s *TaskWorkflowService) SweepWake(ctx context.Context, now time.Time) (woken []string, err error) {
	if s.Tasks == nil || s.TaskTriage == nil || s.Tx == nil {
		return nil, nil
	}
	// 論点11: this is the machine-driven wake sweep (決定12, no human in the
	// loop) — must never be confused with a human action.
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
// action, self-recorded the same way child_dispatched/child_closed are
// (tx.CreateAction directly — never routed through sm.Apply/ApplyAction; see
// machine_card.go's own doc comment for why wake_due is registered Manual:false,
// FromStatus "*" purely so IsManualAction/Apply still treat the name as
// "known"). Clears tt.WakeAt/WakeTaskID in the SAME transaction — the fields
// that made orchestrator.ShouldWake return true in the first place — which is
// what stops this from firing every tick forever: with no transition to rely
// on (v1's wake_* rules consumed the condition implicitly, by moving the task
// OUT of parked), consuming the condition here is the only mechanism left.
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
		return tx.CreateAction(action)
	})
}

// SweepReconcileChildren re-checks every dispatched child of every working
// card against its REAL current task status, self-healing any whose
// task_triage.detail entry still says "dispatched" after the real task
// already reached done/aborted (PR #987 review, HIGH 8). This is bare
// bookkeeping over detail.children[].status, NOT a card transition — it does
// not touch task.Status, so it does not reopen design doc §3.3's "機械遷移
// ゼロ" (card machine v2 has zero rules moving a card's own status; this
// sweep only keeps a CHILD's recorded status from drifting from reality).
//
// v1's SweepTriage did this same reconciliation as a side effect of
// evaluating auto-done every tick (決定15) — deleting auto-done along with
// it (see this PR's orchestrator-layer commit) took the reconciliation with
// it too, which was collateral, not intentional: without a periodic
// re-check, a "dispatched" entry that never gets its child_closed
// self-record (recordChildClosedOnParent hitting a transient DB error, or a
// child GC'd out from under it — orchestrator/store.go's GCTasks deletes a
// terminal task's row independent of any parent) sticks forever, and khi
// reads "a child is still running" off that stale entry and never suggests
// "done" for a card that has, in reality, already finished (a permanently
// stale working card).
//
// A failure evaluating one task is logged and does not abort the sweep —
// the same posture SweepWake established.
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
				// PR #987 review round 2, MEDIUM N3: mutating children[i] here
				// alone is NOT enough — this local slice is discarded by
				// SweepReconcileChildren's caller (it never calls
				// UpsertTaskTriage with it), so without this persistence call
				// the "closed" mapping never survives past this one function
				// call, and the sweep re-derives (and re-discards) the exact
				// same no-op result every single tick forever. v1's
				// ShouldAutoDone consumed this same in-memory mutation within
				// the SAME tick it was made — v2 has no such consumer, so the
				// mutation must persist itself now.
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
// mapping onto its parent's task_triage sidecar (PR #987 review round 2,
// MEDIUM N3) — the vanished-child sibling of recordChildClosedOnParent
// (workflow_triage.go), which cannot be reused directly here since it takes
// a real child *orchestrator.Task (task.ID/task.ParentID/task.Status), and a
// vanished child has none of those available — only the parent's own task ID
// and the dangling TaskRef string survive in task_triage.detail.children.
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
		// existing child_closed payload key exactly (workflow_triage.go) — a
		// future consumer reading this payload by key must not silently miss
		// vanished-child rows because they alone spelled the same value
		// differently (coordinator review, LOW: payload key mismatch).
		payload, _ := json.Marshal(map[string]string{"child_id": childTaskRef, "child_status": "vanished"})
		action := &orchestrator.Action{
			TaskID:     parentTaskID,
			Type:       "child_closed",
			FromStatus: parentTask.Status,
			ToStatus:   parentTask.Status,
			Payload:    payload,
			Actor:      orchestrator.ActorDaemon,
		}
		return tx.CreateAction(action)
	}); err != nil {
		slog.Error("vanished child_closed self-record failed", "task_id", parentTaskID, "child_task_id", childTaskRef, "error", err)
	}
}

// QueueSweepStore is the interface QueueSweepLoop needs — narrowed from
// *TaskWorkflowService so the loop can be unit tested against a fake.
//
// SweepTriage (決定15/16) and SweepReopen (I-5) are GONE as of card machine
// v2 — both were machine-driven transitions (auto-done, auto-reopen) the
// redesign retires entirely (design doc §3.3: "機械遷移ゼロ"). done/reopen
// are now suggested by khi and applied only by a human's accept, through the
// ordinary ApplyAction/accept(verb) path — there is nothing left for a
// periodic sweep to decide on their behalf. SweepReconcileChildren (above)
// is NOT one of those machine-driven transitions — it survives because it
// never touches task.Status.
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
