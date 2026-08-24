package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/novshi-tech/boid/internal/notify"
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
	// autoDone only ever runs against a triage card (SweepTriage/
	// recordChildClosedOnParent's caller already found a task_triage row
	// before calling in here), so NewCardMachine is unambiguous — no
	// machineFor lookup needed (PR-B).
	sm := orchestrator.NewCardMachine()
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

	// 2026-08-14 追記: このパスは daemon 内部の headless sweep が直接
	// working→done を適用するだけで、`boid task notify --done` 相当の呼び出しは
	// 一度も発生しない — 通常タスクの done は agent 自身が
	// task_notify.go の NotifyTask を叩くので fireUserNotify (root task ゲート)
	// を通るが、triage card にはそもそも叩く agent が存在しない。ゲートを
	// 通らない以前に「呼び出し自体が無い」ので、ここで明示的に notify する。
	// autoDone のドキュメントコメントが言う通り card は常に root task
	// (parent_id=="") なので、そのゲートと矛盾しない。
	s.notifyTriageAutoDone(ctx, newTask)

	return &ActionApplication{Task: newTask, Action: action}, nil
}

// notifyTriageAutoDone fires a best-effort user notification when
// SweepTriage's autoDone completes a triage card without any agent session
// ever running (see autoDone's own call site comment for why the ordinary
// task_notify.go path never reaches this case). Mirrors
// notifyIfUrgencyNow's own nil-tolerant, timeout-bounded shape — s.Notifier
// is the same optional dependency, and a hung notify.command must not stall
// the sweep loop.
func (s *TaskWorkflowService) notifyTriageAutoDone(ctx context.Context, task *orchestrator.Task) {
	if s.Notifier == nil || task == nil {
		return
	}
	ev := notify.Event{
		TaskID:    task.ID,
		TaskTitle: task.Title,
		ProjectID: task.ProjectID,
		Message:   "queue: triage task が完了しました (children 全 closed)",
		URLPath:   "/tasks/" + task.ID,
	}
	notifyCtx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	if err := s.Notifier.Notify(notifyCtx, ev); err != nil {
		slog.Warn("triage sweep: auto-done notify failed", "task_id", task.ID, "error", err)
	}
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
	// PR-B (docs/plans/suggestion-as-state-transition-impl.md §2): the
	// sidecar-existence judgment itself now lives in hasTaskTriageRow
	// (machine_select.go), shared with machineFor — this function's own
	// nil-store / error-message behavior is otherwise unchanged.
	isCard, err := hasTaskTriageRow(s.TaskTriage, task.ID)
	if err != nil {
		return "", &StatusError{
			Code:    http.StatusServiceUnavailable,
			Message: fmt.Sprintf("reopen: cannot determine whether task %s is a triage task: %v", task.ID, err),
		}
	}
	if isCard {
		return "reopen_triaged", nil
	}
	return actionType, nil
}

// CanonicalSourceBreach is one 決定16 violation: a triage task that has
// reached triaged-or-beyond without a source that can ever report closure.
type CanonicalSourceBreach struct {
	TaskID    string
	ProjectID string
	Title     string
	Guidance  string
}

// ---- docs/plans/ingestion-identity.md PR-5 (B-6): auto-reopen (I-5) ----

// ReopenSweepResult is one SweepReopen tick's outcome — 決定15's mirror of
// TriageSweepResult: Reopened is what I-5 actually reopened this tick;
// Flapped is 12節 B-6 のフラップ対策 candidates a repeat flip did NOT
// auto-reopen (surfaced via notify instead — see notifyNewlyFlapped).
type ReopenSweepResult struct {
	Reopened []string
	Flapped  []string
}

// errAutoReopenFlapped is autoReopen's signal that ShouldAutoReopen said no
// specifically because of 12節 B-6's フラップ対策 (CountAutoReopens > 0),
// as opposed to any other reason a reopen no longer applies (the source
// re-closed, the task raced out of "done", ...). SweepReopen uses this to
// decide notify-instead-of-warn.
var errAutoReopenFlapped = errors.New("auto reopen: already auto-reopened this task once (フラップ対策)")

// SweepReopen is I-5's periodic evaluation, sharing QueueSweepLoop's tick
// with SweepWake/SweepTriage per the design doc's own placement instruction
// ("評価契機は QueueSweepLoop — 決定15のSweepDoneと同じ場所に並べる", 12節
// B-6) — no dedicated timer, same InitialDelay/Interval staggering the
// existing loops already use.
//
// For every DONE task carrying a task_triage sidecar row (12節 B-6 既定案の
// 判定 key — see resolveAttrsSetDoneTransition's doc comment, attrs_set_done.go,
// for why "row exists" is safe here too), this reads the SAME single
// detail.attrs.observed.source_closed key SweepTriage's ShouldAutoDone
// reads (I-5's own "読むキーは増やさない"promise) and, when it has flipped
// back to false, reopens the card via reopen_triaged (決定17's routing
// target, machine.go) — UNLESS this exact task has already been
// auto-reopened once before (フラップ対策), in which case it is surfaced via
// Notifier instead of acted on again.
//
// A task whose canonical source is STILL reported closed is not a candidate
// at all here — that "配送はされたが reopen しない" case (I-5c) is made
// visible at attrs_set-apply time instead (logAttrsSetOnDoneTriage,
// workflow_action.go), not by this periodic sweep; see that function's own
// doc comment for why an event-time log fits I-5c better than a per-tick
// state check would.
//
// A failure evaluating or reopening one task is logged and does not abort
// the sweep — the same posture SweepWake/SweepTriage already establish.
func (s *TaskWorkflowService) SweepReopen(ctx context.Context, _ time.Time) (ReopenSweepResult, error) {
	var result ReopenSweepResult
	// s.Tx is checked here too (Opus review N-7), not just s.Tasks/s.TaskTriage:
	// without it, a construction gap that leaves Tx nil would let every done
	// triage task with source_closed==false reach autoReopen below, which
	// immediately fails with "Transactor not configured" — 500 warned PER TASK,
	// EVERY tick, for as long as the daemon runs with that gap. Failing the
	// whole sweep once here (mirroring SweepTriage/SweepWake's own posture:
	// they don't check Tx because neither of THEM calls into a Tx-requiring
	// helper before doing per-task work) is one line instead of a warning storm.
	if s.Tasks == nil || s.TaskTriage == nil || s.Tx == nil {
		return result, nil
	}
	ctx = orchestrator.WithActor(ctx, orchestrator.ActorDaemon)

	// docs/plans/ingestion-identity.md PR-5 (B-6), I-5 (Opus review — 修正必須1):
	// "done_triage" (store.go's ListTasks) INNER JOINs task_triage so the scan
	// is bounded by the number of triage CARDS (tens), not every done task in
	// the system (dev tasks + their children, unbounded — done is the largest
	// status set GC ever leaves standing, 30-day retention, and J-10 lets
	// description carry up to 64 KiB). Before this, `Status:
	// string(orchestrator.TaskStatusDone)` fell through to the generic
	// `t.status = ?` branch: no LIMIT, taskSelectCols' full
	// description/payload/instructions columns, plus taskChildCountCols' 4
	// correlated subqueries PER ROW — on a tick that fires every minute
	// (QueueSweepLoop.Interval, wire.go). See store.go's "done_triage" branch
	// doc comment for the full query-shape rationale.
	tasks, err := s.Tasks.ListTasks(orchestrator.TaskFilter{Status: "done_triage"})
	if err != nil {
		return result, fmt.Errorf("sweep reopen: list done triage tasks: %w", err)
	}
	for _, t := range tasks {
		tt, ttErr := s.TaskTriage.GetTaskTriage(t.ID)
		if ttErr != nil {
			if !errors.Is(ttErr, sql.ErrNoRows) {
				slog.Warn("reopen sweep: get task_triage failed", "task_id", t.ID, "error", ttErr)
			}
			// No sidecar row: an ordinary done task (never a triage card) —
			// nothing to evaluate. Mirrors SweepTriage/SweepWake's own
			// sql.ErrNoRows handling exactly.
			continue
		}
		if orchestrator.SourceClosed(tt.Detail) {
			continue // canonical source still closed: nothing to reopen here (I-5c's log already ran at write time)
		}

		// N-9 (Opus review, 2026-08-19): a card whose source stays reported
		// OPEN while already フラップ-blocked (s.lastFlappedReopen[t.ID] true)
		// re-enters autoReopen below and opens a fresh Tx EVERY tick until a
		// human acts — a real, deliberately-accepted cost, not an oversight.
		// Judged not worth an extra pre-Tx short-circuit (e.g. skipping via
		// s.lastFlappedReopen here) for two reasons: (1) 修正必須1's
		// "done_triage" filter above already bounds the population this loop
		// walks to the number of triage CARDS (tens), not every done task in
		// the system — the per-tick cost this would save is one extra Tx open
		// + ListActionsByTask per ALREADY-SMALL candidate, not per thousands
		// of rows; (2) the 2026-08-19 episode-scoping fix
		// (orchestrator.CountAutoReopens's own doc comment) makes a
		// PERSISTENT flap materially rarer than it was under lifetime
		// counting — every full triaged→ready→working→triage_done cycle
		// resets the budget, so a card can only stay flapped by genuinely
		// double-flipping within one still-open episode, not merely by
		// having been auto-reopened once, ever, in its whole history (the
		// old semantics, under which EVERY multi-cycle card eventually
		// became permanently flapped). Revisit if a workload emerges where
		// many cards sit flapped for long stretches at once.
		if _, aErr := s.autoReopen(ctx, t.ID); aErr != nil {
			if errors.Is(aErr, errAutoReopenFlapped) {
				result.Flapped = append(result.Flapped, t.ID)
				continue
			}
			var statusErr *StatusError
			if errors.As(aErr, &statusErr) && statusErr.Code == http.StatusConflict {
				// Raced back to closed, or the task left "done" concurrently —
				// self-resolved, nothing to warn about (same tolerance
				// autoDone's own StatusConflict branch gets from SweepTriage).
				continue
			}
			slog.Warn("reopen sweep: auto-reopen failed", "task_id", t.ID, "error", aErr)
			continue
		}
		result.Reopened = append(result.Reopened, t.ID)
	}

	s.notifyNewlyFlapped(ctx, result.Flapped)
	return result, nil
}

// autoReopen applies the machine-internal reopen_triaged transition (done →
// triaged), re-checking ShouldAutoReopen fresh INSIDE the transaction — same
// "re-read and re-validate inside the Tx" discipline as autoDone above: a
// card that raced back to source_closed=true, or that a concurrent human
// action already moved out of "done", must never be reopened out from under
// that change.
//
// The フラップ count is derived from tx.ListActionsByTask (決定13: no
// dedicated counter column — see orchestrator.CountAutoReopens's own doc
// comment) rather than a value passed in from the pre-Tx caller, so the
// re-check is not just re-reading stale data computed before the Tx opened.
func (s *TaskWorkflowService) autoReopen(ctx context.Context, taskID string) (*ActionApplication, error) {
	if s.Tx == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "auto reopen: Transactor not configured"}
	}
	// autoReopen only ever runs against a triage card (SweepReopen's
	// "done_triage" filter already INNER JOINs task_triage before calling in
	// here), so NewCardMachine is unambiguous — no machineFor lookup needed
	// (PR-B).
	sm := orchestrator.NewCardMachine()
	var newTask *orchestrator.Task
	action := &orchestrator.Action{TaskID: taskID, Type: "reopen_triaged", Actor: orchestrator.ActorFromContext(ctx)}

	if err := s.Tx.WithinTx(func(tx TxStore) error {
		fresh, ferr := tx.GetTask(taskID)
		if ferr != nil {
			return statusErrorForGetTaskErr(ferr)
		}
		if fresh.Status != orchestrator.TaskStatusDone {
			return &StatusError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("auto reopen: task status changed to %q before the done->triaged transition could commit", fresh.Status),
			}
		}
		tt, tErr := tx.GetTaskTriage(taskID)
		if tErr != nil {
			if errors.Is(tErr, sql.ErrNoRows) {
				// The sidecar row vanished between the pre-Tx read (which
				// found it, or SweepReopen would never have called in here)
				// and this Tx opening — e.g. a concurrent `drop` released it
				// (DeleteTaskTriage, applyDropSideEffect). Self-resolved,
				// same as the fresh.Status-changed branch above: not this
				// sweep's problem to warn about (Opus review N-4 — this
				// used to fall to the generic fmt.Errorf below, surfacing as
				// a 500 + slog.Warn per tick for as long as the race
				// persisted, even though nothing is actually wrong).
				return &StatusError{Code: http.StatusConflict, Message: "auto reopen: task_triage row no longer exists"}
			}
			return fmt.Errorf("auto reopen: get task_triage: %w", tErr)
		}
		actions, aErr := tx.ListActionsByTask(taskID)
		if aErr != nil {
			return fmt.Errorf("auto reopen: list actions: %w", aErr)
		}
		closedNow := orchestrator.SourceClosed(tt.Detail)
		prior := orchestrator.CountAutoReopens(actions)
		if !orchestrator.ShouldAutoReopen(tt.Detail, prior) {
			if !closedNow && prior > 0 {
				return errAutoReopenFlapped
			}
			return &StatusError{Code: http.StatusConflict, Message: "auto reopen: conditions no longer hold"}
		}

		applied, aerr := sm.Apply(fresh, action)
		if aerr != nil {
			return &StatusError{Code: http.StatusConflict, Message: aerr.Error()}
		}
		newTask = applied
		action.FromStatus = fresh.Status
		action.ToStatus = newTask.Status
		if err := tx.UpdateTask(newTask); err != nil {
			return err
		}
		return tx.CreateAction(action)
	}); err != nil {
		if errors.Is(err, errAutoReopenFlapped) {
			return nil, errAutoReopenFlapped
		}
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

	// docs/plans/ingestion-identity.md PR-5 (B-6) — 修正必須2 (Opus review):
	// autoDone's mirror. This is the SAME headless-sweep gap
	// notifyTriageAutoDone exists to close (see its own doc comment,
	// 2026-08-14 追記, above): the daemon applies done→triaged entirely on
	// its own tick, so no `boid task notify` call — and no fireUserNotify
	// gate — ever runs for this path either. Without an explicit call here,
	// a now-urgency card that just came BACK into the queue would land
	// silently: attrs_set on a done triage task never fires
	// notifyUrgencyRaised (queue_notify.go's own gate reads
	// queueMemberStatus(task.Status), and "done" is not a queue-member
	// status — I-5b's whole point is that attrs_set on a done card is
	// non-transitioning), so nothing else in the system observes this
	// arrival at all.
	//
	// Reusing notifyQueueEntryIfUrgent (rule 4's existing "now は即時" path)
	// rather than a new notifyTriageAutoReopen: newTask.Status is "triaged"
	// (a queueMemberStatus) and fromStatus is explicitly "done" (never a
	// queueMemberStatus), so the entry-transition check
	// (!queueMemberStatus(task.Status) || queueMemberStatus(fromStatus))
	// always evaluates to "this is an entry" for every reopen — the SAME
	// notify path a human's own reopen already gets via ApplyAction
	// (workflow_action.go's own call, after ordinary `reopen`/`reopen_triaged`
	// commits). This also already wraps the notify.Service call in
	// notifyTimeout (queue_notify.go's notifyIfUrgencyNow), so a hung
	// notify.command cannot stall SweepReopen's per-task loop.
	s.notifyQueueEntryIfUrgent(ctx, newTask, orchestrator.TaskStatusDone)

	return &ActionApplication{Task: newTask, Action: action}, nil
}

// notifyNewlyFlapped sends 12節 B-6 のフラップ対策's notify — "2回目以降は
// 通知して人に見せる" — but only for a task NEWLY entering the flapped set
// this tick, mirroring queue_sweep.go's logCanonicalSourceBreaches
// fingerprint-on-change discipline (same underlying flood: this loop ticks
// every minute, and a flapped task's blocked state persists until a human
// acts, so an unconditional per-tick notify would re-fire the configured
// notify.command — an actual external side effect, unlike a log line — every
// single tick forever).
//
// s.lastFlappedReopen is safe unguarded by a mutex for the SAME reason
// QueueSweepLoop.lastBreachFingerprint is (queue_sweep.go): SweepReopen has
// exactly one caller in production, QueueSweepLoop.runOnce, which the loop
// invokes sequentially from a single goroutine. It lives on
// TaskWorkflowService rather than on the loop only because Notifier does —
// the two pieces of state that decide "notify or not" belong together.
func (s *TaskWorkflowService) notifyNewlyFlapped(ctx context.Context, flapped []string) {
	next := make(map[string]bool, len(flapped))
	for _, id := range flapped {
		next[id] = true
		if s.lastFlappedReopen[id] {
			continue // already notified for this same episode
		}
		s.notifyReopenFlap(ctx, id)
	}
	s.lastFlappedReopen = next
}

// notifyReopenFlap sends a best-effort notification for one フラップ-blocked
// task. Mirrors notifyTriageAutoDone's own nil-tolerant, timeout-bounded
// shape (s.Notifier is the same optional dependency, and a hung
// notify.command must not stall the sweep loop). Re-fetches the task for
// its title — SweepReopen only carries task IDs in ReopenSweepResult.Flapped
// — tolerating a lookup failure by falling back to a title-less event rather
// than dropping the notification entirely.
func (s *TaskWorkflowService) notifyReopenFlap(ctx context.Context, taskID string) {
	if s.Notifier == nil || s.Tasks == nil {
		return
	}
	ev := notify.Event{
		TaskID:  taskID,
		Message: "queue: triage task の canonical source が再度動きましたが、既に自動 reopen 済みのため今回は自動 reopen しません — 確認してください",
		URLPath: "/tasks/" + taskID,
	}
	if t, terr := s.Tasks.GetTask(taskID); terr == nil && t != nil {
		ev.TaskTitle = t.Title
		ev.ProjectID = t.ProjectID
	}
	notifyCtx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	if err := s.Notifier.Notify(notifyCtx, ev); err != nil {
		slog.Warn("reopen sweep: flap notify failed", "task_id", taskID, "error", err)
	}
}
