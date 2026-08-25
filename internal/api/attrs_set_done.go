package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- docs/plans/ingestion-identity.md PR-5 (B-6): I-5b の service 層ガード ----
//
// I-5 (auto-reopen) needs observed.source_closed to keep updating on a DONE
// triage task, but machine.go's attrs_set rule set is deliberately scoped to
// preExecutionStatuses ("captured","triaged","parked","ready","working") —
// NOT "done" — per 論点6-3's explicit "never *" discipline: attrs_set must
// stay unreachable against an ordinary task's done/executing/aborted/etc, and
// preExecutionStatuses is a single shared FromStatus list across FIVE action
// verbs (attrs_set/child_added/child_specced/noted/answered), so widening it
// to include "done" would open the door for ALL FIVE, not just attrs_set.
//
// So this is intentionally NOT a machine.go rule change. It is a
// service-layer special case that runs BEFORE sm.Apply, scoped to exactly
// one (action, status) pair: attrs_set against a task currently "done". The
// 判定 key (12節 B-6 既定案) is "does this task carry a task_triage sidecar
// row" — the same signal resolveReopenVariant already uses to tell a triage
// card apart from an ordinary done task. Everything else (a different
// action type, a different status, OR attrs_set against a done task with NO
// row) falls through to sm.Apply's existing rejection unchanged.
//
// Why "row exists" is safe as the判定 key, despite CreateTask technically
// allowing ANY project to request initial_status=parked (task_create.go's
// allowedCreateInitialStatuses is not scoped to the meta project) AND
// despite the row NOT always starting out with real content — CreateTask's
// own createCardTask (task_create.go) and ResolveOrCapture
// (task_resolve_or_capture.go) both build a task with EMPTY CardAttrs
// ({} detail, no attrs) up front, unconditional on the caller ever sending
// an attrs_set at all (2026-08-19 correction, Opus review — the previous
// text here claimed attrs_set's payload contract makes an empty row
// impossible; that is not true, and a careful reviewer will find both
// construction sites independently).
//
// card-model-cleanup PR-2 (docs/plans/card-model-cleanup.md, migration
// 0045) strengthens this further than the original argument needed: getTriage
// (CardStore.GetTaskTriage) now queries the SAME tasks row filtered to
// `type = 'card'` — there is no longer a separate task_triage sidecar table
// at all, so "the row exists" and "this task's type is card" are now the
// exact same fact, not two facts kept in sync by convention. A card can
// never be rowless post-migration: it is type='card' from the moment
// CreateTask/ResolveOrCapture inserts it (design doc §3.6 — SeedTaskTriage,
// the old best-effort post-hoc seeding step this comment used to reference,
// is gone entirely).
//
// PR #987 review (LOW 12) correction, superseding this section's original
// "reachability" argument: card machine v2 (docs/plans/
// suggestion-as-state-transition-impl.md §3.3) deleted triage_done and its
// ShouldAutoDone/SourceClosed auto-approval condition entirely — NewCardMachine
// now has exactly ONE rule reaching "done" (working→done, Manual:true, no
// Condition at all), reachable by a plain human click OR accept("done") on a
// khi-suggested "done" verb. Neither path requires source_closed, or any
// task_triage content at all, to be true first. So — unlike the ORIGINAL
// (v1) version of this argument, which is now WRONG and must not be
// trusted — a triage-carrying task CAN legitimately reach "done" while its
// task_triage row is still the pristine empty seed (e.g. a human clicks
// go→working→done on a card nobody ever ran attrs_set/park/note against).
// That is fine: "row exists" was ALWAYS safe here for the reason given above
// alone — a card row is created ONLY by a genuine card-creation path
// (createCardTask / ResolveOrCapture), never by accident on an ordinary
// task — and that fact does not depend on WHEN or via WHICH rule the task
// later reaches "done".
// The row's mere existence, empty or not, already proves this is a real
// triage card; there was never a need to additionally prove it is non-empty
// by the time done arrives.
//
// PR-B reachability note (docs/plans/suggestion-as-state-transition-impl.md
// §2, PR #986 review): api.machineFor (machine_select.go) now performs THIS
// SAME getTriage lookup even earlier than this function does, to pick
// NewCardMachine vs NewExecutionMachine for the whole ApplyAction call. For
// the PRE-Tx call site (workflow_action.go, right after the task loads), a
// "done"-status task only ever reaches this function on NewCardMachine —
// which machineFor only selects for "done" when its OWN lookup found a row
// (err == nil; done/aborted are deliberately excluded from machineFor's
// status-based fallback, see machineFor's own doc comment). So the
// sql.ErrNoRows branch and the `default:` genuine-error branch below are, via
// that call site, reachable ONLY if this function's getTriage call disagrees
// with the lookup machineFor just made moments earlier for the identical
// task/store — i.e. a flaky store returning two different answers back to
// back, not a real state change (nothing in this codebase deletes a
// task_triage row once created — DeleteTaskTriage, orchestrator/card.go,
// currently has no production caller). The IN-Tx re-validation call
// (workflow_action.go's skipTaskUpdate branch, using tx.GetTaskTriage) is the
// one place these two branches remain meaningfully reachable: a genuine race
// between the pre-Tx read and this Tx opening could still surface a
// transient lookup error there, even though a real concurrent row deletion
// is not something any current caller performs.
func resolveAttrsSetDoneTransition(sm *orchestrator.StateMachine, task *orchestrator.Task, action *orchestrator.Action, getTriage func(string) (*orchestrator.CardAttrs, error)) (*orchestrator.Task, *StatusError) {
	if action.Type == "attrs_set" && task.Status == orchestrator.TaskStatusDone && getTriage != nil {
		_, err := getTriage(task.ID)
		switch {
		case err == nil:
			// A task_triage row exists: treat this exactly like every other
			// preExecutionStatuses attrs_set — non-transitioning, status
			// unchanged. The actual fold happens later in
			// applyAttrsSetSideEffect, same as always.
			//
			// orchestrator.CloneTaskShallow (not a bare `*task` copy — PR-2
			// review, matching that helper's own doc comment listing this
			// call site): the caller downstream doesn't currently mutate
			// noop.Card, but a bare copy would silently alias noop.Card back
			// into task.Card the moment it started, exactly the "縫い目" bug
			// class CloneTaskShallow exists to close off everywhere, not just
			// where a mutation is known today.
			return orchestrator.CloneTaskShallow(task), nil
		case errors.Is(err, sql.ErrNoRows):
			// No row: an ordinary done task (or the theoretical initial_status
			// edge case that never wrote any attrs). Falls through to
			// sm.Apply below, which rejects it — this is what pins "task_triage
			// 行を持たない done の通常 task には発火しない".
		default:
			// A genuine lookup failure must not be silently reinterpreted as
			// "no row, reject" — same ErrNoRows-vs-other split
			// statusErrorForGetTaskErr/applyAttrsSetSideEffect already use.
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: "attrs_set: check task_triage: " + err.Error()}
		}
	}
	applied, aerr := sm.Apply(task, action)
	if aerr != nil {
		return nil, &StatusError{Code: http.StatusConflict, Message: aerr.Error()}
	}
	return applied, nil
}

// logAttrsSetOnDoneTriage is I-5c's visibility half: EVERY attrs_set that
// lands on a done triage task via the guard above gets a log line.
//
// slog.Debug, NOT slog.Warn (PR #987 review, MEDIUM 11 — downgraded from
// v1's Warn): when this log line was introduced (I-5c, docs/plans/
// ingestion-identity.md PR-5), a done card receiving attrs_set was
// specifically the signal that SweepReopen (I-5, auto-reopen) might have a
// candidate on its next tick — a "this card needs a human's attention"
// event worth Warn, mirroring 決定16's MissingCanonicalSourceGuidance
// precedent (queue_sweep.go's now-also-deleted logCanonicalSourceBreaches).
// Card machine v2 (docs/plans/suggestion-as-state-transition-impl.md §3.3)
// retires auto-reopen entirely: khi writing observed/suggestion data onto a
// done card (most commonly a "reopen" suggestion for a human to accept, see
// NewCardMachine's own doc comment on why "answered" reaches done/dropped)
// is now the ORDINARY, EXPECTED way a done card receives new information —
// not a signal anything is wrong or needs attention. Keeping this at Warn
// would flood the daemon log with an entry for every routine done-card
// observation once khi's real ingestion volume hits it. The line survives
// at Debug purely as an ops trace (did an attrs_set actually land here,
// and was the source reported closed at the time) — not a call to action.
func logAttrsSetOnDoneTriage(taskTriage CardStore, taskID string) {
	if taskTriage == nil {
		return
	}
	tt, err := taskTriage.GetTaskTriage(taskID)
	if err != nil {
		slog.Debug("attrs_set landed on a done triage task, but re-reading task_triage failed", "task_id", taskID, "error", err)
		return
	}
	slog.Debug("attrs_set landed on a done triage task",
		"task_id", taskID, "source_closed", orchestrator.SourceClosed(tt.Detail))
}
