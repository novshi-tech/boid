package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- Phase 1 PR-5b (docs/plans/cross-project-issue-triage.md 決定15/16) ----

// SweepTriage is the periodic, decision-only evaluation of 決定15 (done の
// 自動落ち) and 決定16 (canonical source の検知), in the same shape as
// SweepWake: for every working triage task it evaluates
// orchestrator.ShouldAutoDone (全子 closed ∧ observed.source_closed) and
// applies the machine-internal triage_done transition to any that qualify,
// while collecting every card that can never satisfy that rule because it
// carries no canonical source.
//
// 決定16's enforcement is a REPORT rather than a create-time reject: a reject
// would let an ingestion bug swallow the card entirely (the direction that
// loses work), while a breach leaves the card fully visible and only says
// "this one can never finish".
//
// Scope note (stated rather than silently omitted, per the plan doc's
// 「テストが無い step には理由を明示する」 discipline): queue 節 rule 7 wants
// guidance surfaced as a 案内 card in the queue. No guidance-presentation
// surface exists yet — orchestrator.WatchdogGuidance, PR-3's staleness
// primitive, has no caller either. Rather than inventing a presentation for
// one guidance kind alone, this PR returns the breaches for the caller to log;
// the queue presentation lands together with rule 7's, for both kinds at once.
//
// 決定14 is what makes this necessary: khi's evaluate.py used to own the done
// judgment, and retiring khi's fold takes that with it. Without a daemon-side
// rule, every card would pile up in working forever — done/dropped could no
// longer be told apart (which is how 成功の定義 is measured at all), and the
// 30-day GC, which only ever reaches done/aborted, would never collect them.
//
// A failure on one task is logged and does not abort the sweep (SweepWake's
// same posture): one malformed detail blob must not stop every other card
// from finishing. Returns the ids that were transitioned.
func (s *TaskWorkflowService) SweepTriage(ctx context.Context, _ time.Time) (TriageSweepResult, error) {
	var result TriageSweepResult
	if s.Tasks == nil || s.TaskTriage == nil {
		return result, nil
	}
	// 論点11: machine-driven, never a human pressing a button.
	ctx = orchestrator.WithActor(ctx, orchestrator.ActorDaemon)

	// ONE pass over each status set (Opus review round 2): the done rule and
	// the 決定16 breach check read exactly the same rows, so evaluating both in
	// the same walk halves the per-tick query count instead of listing
	// `working` twice a minute with a GetTaskTriage per row each time.
	//
	//   working : both checks
	//   queue   : breach check only (a pre-execution card has not started, so
	//             it cannot be finished) — "queue" is ListTasks' existing
	//             pre-execution branch, one query for all four statuses.
	for _, statusFilter := range []string{string(orchestrator.TaskStatusWorking), "queue"} {
		tasks, err := s.Tasks.ListTasks(orchestrator.TaskFilter{Status: statusFilter})
		if err != nil {
			return result, fmt.Errorf("sweep triage: list %s tasks: %w", statusFilter, err)
		}
		for _, t := range tasks {
			tt, ttErr := s.TaskTriage.GetTaskTriage(t.ID)
			if ttErr != nil {
				if !errors.Is(ttErr, sql.ErrNoRows) {
					slog.Warn("triage sweep: get task_triage failed", "task_id", t.ID, "error", ttErr)
				}
				// No sidecar row: not a triage task — nothing to evaluate.
				continue
			}

			if guidance := orchestrator.MissingCanonicalSourceGuidance(t.Status, tt.Kind, tt.Detail); guidance != "" {
				result.Breaches = append(result.Breaches, CanonicalSourceBreach{
					TaskID:    t.ID,
					ProjectID: t.ProjectID,
					Title:     t.Title,
					Guidance:  guidance,
				})
			}

			if t.Status != orchestrator.TaskStatusWorking {
				continue
			}
			children, cErr := orchestrator.DetailChildren(tt.Detail)
			if cErr != nil {
				slog.Warn("triage sweep: parse children failed", "task_id", t.ID, "error", cErr)
				continue
			}
			// Reconcile the blob against reality before judging it (Opus review
			// round 3). ShouldAutoDone reads ONLY the blob, so any child whose
			// real task is terminal while its entry still says "dispatched"
			// pins the card in working forever. Dispatch's own post-commit loop
			// already heals the specific race it creates (a child finishing
			// before its TaskRef commits), but 決定15 makes the blob
			// load-bearing rather than cosmetic — so the sweep re-checks every
			// dispatched child each tick instead of trusting that one path to
			// have caught everything.
			s.reconcileDispatchedChildren(t.ID, children)
			if !orchestrator.ShouldAutoDone(children, tt.Detail) {
				continue
			}
			if _, aErr := s.autoDone(ctx, t.ID); aErr != nil {
				slog.Warn("triage sweep: auto-done failed", "task_id", t.ID, "error", aErr)
				continue
			}
			result.Completed = append(result.Completed, t.ID)
		}
	}
	return result, nil
}

// reconcileDispatchedChildren marks any child closed whose referenced task has
// actually reached a terminal status, mutating children in place and
// persisting through the same self-record path Dispatch and finalizeTerminal
// use (so the child_closed action is written exactly once — MarkDetailChildClosed
// reports changed=false for an already-closed child).
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
				children[i].Status = orchestrator.TaskTriageChildStatusClosed
			} else {
				slog.Warn("triage sweep: get dispatched child failed", "task_id", taskID, "child_task_id", children[i].TaskRef, "error", err)
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

// TriageSweepResult is one tick's outcome: the cards 決定15 finished, and the
// cards in breach of 決定16's canonical-source contract.
type TriageSweepResult struct {
	Completed []string
	Breaches  []CanonicalSourceBreach
}

// autoDone applies the machine-internal triage_done transition (working →
// done). Like Wake, it re-reads the task from INSIDE the transaction and
// re-checks the predicate there, so a card that raced out of working (a
// concurrent working→ready re-Go, 論点8) is never finished out from under
// that transition.
func (s *TaskWorkflowService) autoDone(ctx context.Context, taskID string) (*ActionApplication, error) {
	if s.Tx == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "auto done: Transactor not configured"}
	}
	sm := orchestrator.DefaultMachine()
	var newTask *orchestrator.Task
	action := &orchestrator.Action{TaskID: taskID, Type: "triage_done", Actor: orchestrator.ActorFromContext(ctx)}

	if err := s.Tx.WithinTx(func(tx TxStore) error {
		fresh, ferr := tx.GetTask(taskID)
		if ferr != nil {
			return statusErrorForGetTaskErr(ferr)
		}
		if fresh.Status != orchestrator.TaskStatusWorking {
			return &StatusError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("auto done: task status changed to %q before the working->done transition could commit", fresh.Status),
			}
		}
		// Re-check the predicate against the in-Tx sidecar for the same
		// reason: a concurrent child_added could have introduced an unfinished
		// child between the sweep's read and this write.
		tt, tErr := tx.GetTaskTriage(taskID)
		if tErr != nil {
			return fmt.Errorf("auto done: get task_triage: %w", tErr)
		}
		children, cErr := orchestrator.DetailChildren(tt.Detail)
		if cErr != nil {
			return fmt.Errorf("auto done: parse children: %w", cErr)
		}
		if !orchestrator.ShouldAutoDone(children, tt.Detail) {
			return &StatusError{Code: http.StatusConflict, Message: "auto done: conditions no longer hold"}
		}

		applied, aErr := sm.Apply(fresh, action)
		if aErr != nil {
			return &StatusError{Code: http.StatusConflict, Message: aErr.Error()}
		}
		newTask = applied
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

	// Route through the same terminal funnel every other done/aborted path
	// uses (Opus review round 2, Low). Today the visible effect is only
	// CleanupTaskWindow, because a card is always a root task — but
	// child_added/child_specced make a card-under-a-card representable, and
	// the moment one exists, a nested card finishing here would never
	// self-record child_closed on its parent, leaving THAT parent permanently
	// unable to satisfy 決定15. Keeping the funnel invariant intact costs one
	// call; discovering the omission later costs a stuck queue.
	s.finalizeTerminal(ctx, newTask)

	return &ActionApplication{Task: newTask, Action: action}, nil
}

// resolveReopenVariant implements 決定17's routing: `reopen` stays the single
// verb every caller uses, and the daemon picks between the ordinary rule
// (done/aborted → executing) and the triage rule (done/aborted → triaged)
// by looking at whether the task has a task_triage sidecar row. It returns
// the action type to actually apply.
//
// Routing rather than rejecting-with-advice, because the state machine cannot
// make this call itself (it matches on status, and a done triage task is
// indistinguishable there from a done executor task) and neither can the
// caller be trusted to: the Web UI renders a reopen button on every done
// task, so a single click on a card would otherwise flip it into `executing`
// and try to RUN the meta project's card. This mirrors Wake, which resolves
// wake_triaged vs wake_ready internally for exactly the same reason — "so no
// caller can wake a task to the wrong status".
//
// `reopen` from `dropped` is untouched: that rule (dropped→triaged) is
// already part of the triage vocabulary — the recovery path from a mistaken
// 破棄 — so only the statuses where the ordinary-task rule would fire
// (done/aborted) get routed.
//
// ONLY sql.ErrNoRows means "ordinary task" (Opus review round 2, Medium). A
// genuine lookup failure (SQLITE_BUSY, a scan error) must NOT fall through to
// the ordinary verb: that is the DANGEROUS direction — it would dispatch the
// card as a job, the exact outcome 決定17 exists to prevent — so an
// indeterminate answer fails the request instead of guessing. Same posture,
// same reason, as CreateTask's seed guard and applyParkSideEffect's.
func (s *TaskWorkflowService) resolveReopenVariant(task *orchestrator.Task, actionType string) (string, error) {
	if actionType != "reopen" {
		return actionType, nil
	}
	if task.Status != orchestrator.TaskStatusDone && task.Status != orchestrator.TaskStatusAborted {
		return actionType, nil
	}
	if s.TaskTriage == nil {
		// A nil store is even LESS determinate than a failed query, so it
		// takes the same safe branch (Opus review round 3): falling through to
		// the ordinary verb here would mean any construction path that forgets
		// to wire the field turns every reopen of a done card into
		// done→executing — dispatching the meta project's card as a job.
		return "", &StatusError{
			Code:    http.StatusServiceUnavailable,
			Message: fmt.Sprintf("reopen: cannot determine whether task %s is a triage task (no task_triage store wired)", task.ID),
		}
	}
	_, err := s.TaskTriage.GetTaskTriage(task.ID)
	switch {
	case err == nil:
		return "reopen_triaged", nil
	case errors.Is(err, sql.ErrNoRows):
		return actionType, nil
	default:
		return "", &StatusError{
			Code:    http.StatusServiceUnavailable,
			Message: fmt.Sprintf("reopen: cannot determine whether task %s is a triage task: %v", task.ID, err),
		}
	}
}

// CanonicalSourceBreach is one 決定16 violation: a triage task that has
// reached triaged-or-beyond without a source that can ever report closure.
type CanonicalSourceBreach struct {
	TaskID    string
	ProjectID string
	Title     string
	Guidance  string
}
