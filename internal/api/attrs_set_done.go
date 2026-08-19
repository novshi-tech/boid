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
// allowing ANY project to request initial_status=captured/triaged
// (task_create.go's allowedCreateInitialStatuses is not scoped to the meta
// project) AND despite the row NOT always starting out with real content —
// CreateTask's own SeedTaskTriage call (task_create.go) and
// ResolveOrCapture's tx.SeedTaskTriage (task_resolve_or_capture.go) both
// insert an EMPTY sidecar row ({} detail, no attrs) up front, unconditional
// on the caller ever sending an attrs_set at all (2026-08-19 correction,
// Opus review — the previous text here claimed attrs_set's payload contract
// makes an empty row impossible; that is not true, these two seed calls
// prove it, and a careful reviewer will find them independently).
//
// The real argument is REACHABILITY, not row content: can a task carrying
// such a row ever actually GET to "done" while that row is still empty?
// machine.go's full rule table has exactly THREE rules whose ToStatus is
// "done" — ① the ordinary Manual rule (executing→done / awaiting→done), ②
// its auto-approval sibling (executing→done, machine.go's own Condition
// check), and ③ triage_done (working→done). Rules ① and ② both require
// FromStatus ∈ {executing, awaiting} — and nothing in the ENTIRE rule table
// ever moves a task out of captured/triaged/parked/ready/working (a triage
// card's own statuses) INTO executing or awaiting: `start` only fires from
// pending, `answer` only fires from awaiting, `reopen`/`reopen_triaged` only
// fire FROM done/aborted, never INTO executing. So a triage-carrying task
// can reach "done" only through rule ③, triage_done — and triage_done only
// fires when orchestrator.ShouldAutoDone(children, detail) is true, whose
// first conjunct is SourceClosed(detail)==true (triage_done.go). By the
// time a task_triage row's OWN task is sitting in "done", that row's detail
// necessarily already reports source_closed — it cannot still be the empty
// seed. This is what actually makes "row exists" safe here: not that the
// row was written by attrs_set, but that an empty row's task structurally
// cannot be the task this guard is even asked about.
func resolveAttrsSetDoneTransition(sm *orchestrator.StateMachine, task *orchestrator.Task, action *orchestrator.Action, getTriage func(string) (*orchestrator.TaskTriage, error)) (*orchestrator.Task, *StatusError) {
	if action.Type == "attrs_set" && task.Status == orchestrator.TaskStatusDone && getTriage != nil {
		_, err := getTriage(task.ID)
		switch {
		case err == nil:
			// A task_triage row exists: treat this exactly like every other
			// preExecutionStatuses attrs_set — non-transitioning, status
			// unchanged. The actual fold happens later in
			// applyAttrsSetSideEffect, same as always.
			noop := *task
			return &noop, nil
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
// lands on a done triage task via the guard above gets a log line, whether
// or not it flips source_closed to false (a flip additionally becomes a
// SweepReopen candidate on the next queue-sweep tick; a non-flip — the
// "canonical source は closed のままで Slack にだけ続報が来た" case I-5c
// names explicitly — would otherwise never be visible anywhere at all,
// since nothing else reacts to it).
//
// Chosen form (this PR's own decision, per the design doc's "同じ形にする
// か queue へ出すかをこの PR で決める"): log only, matching the precedent
// 決定16's MissingCanonicalSourceGuidance already set (queue_sweep.go's
// logCanonicalSourceBreaches) — there is no queue-presentation surface for
// a "guidance" kind of entry yet, and inventing one for a single new kind
// here would be scope creep the design doc itself defers ("キューへの提示
// は両方の kind をまとめて着地させる" — see 12節 B-5/B-6's own framing).
// Unlike the breach report, this does NOT need change-fingerprinting: each
// attrs_set is already a discrete, rare event by construction (bounded by
// how often a workspace's ingestion tick runs), not a persistent state that
// would otherwise re-log every sweep tick forever.
//
// slog.Warn, not slog.Info (2026-08-19 fix, Opus review N-1): the precedent
// this comment cites — logCanonicalSourceBreaches — actually logs its real
// breach case at Warn, not Info (queue_sweep.go). I-5c is doc'd as 決定16の
// 契約違反ケースの防御 (the same class of "this card needs a human's
// attention" signal), and boid's default log level is info today — but the
// moment an operator sets `log.level: warn` (config-yaml.md), an Info line
// disappears silently while every OTHER 決定16-class signal in this
// codebase stays visible at Warn. See attrs_set_done_test.go's
// TestLogAttrsSetOnDoneTriage_LogsAtWarnLevel, which fails against an
// Info-level reimplementation.
func logAttrsSetOnDoneTriage(taskTriage TaskTriageStore, taskID string) {
	if taskTriage == nil {
		return
	}
	tt, err := taskTriage.GetTaskTriage(taskID)
	if err != nil {
		slog.Warn("I-5c: attrs_set landed on a done triage task, but re-reading task_triage failed", "task_id", taskID, "error", err)
		return
	}
	slog.Warn("I-5c: attrs_set landed on a done triage task",
		"task_id", taskID, "source_closed", orchestrator.SourceClosed(tt.Detail))
}
