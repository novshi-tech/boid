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
// project): attrs_set's OWN payload contract (parseAttrsSetPayload) rejects
// an empty object, so every row this guard can ever see was seeded by a
// call that wrote REAL content (urgency/kind/opaque attrs) — there is no
// path that creates a truly empty "phantom" task_triage row via attrs_set
// (contrast with `answered`, whose miss-case fix in PR-3 exists precisely
// because ITS only effect is negative — stripping a key that can't be
// present without a row already). A task that reaches "done" carrying such
// a row got there via triage_done (machine.go), which itself only fires
// from "working" — and nothing in the machine ever moves a task from
// captured/triaged/parked/ready/working into executing/awaiting (the ONLY
// statuses the ordinary Manual "done" rule accepts) — so a task created with
// initial_status=pending (CreateTask's default, and what every ordinary dev
// task actually uses) can never receive attrs_set at all, at any point in
// its life: this guard is simply never reached for it. See the PR-5 report
// for the full trace of why the one remaining theoretical path (someone
// deliberately using initial_status=triaged/captured OUTSIDE a real triage
// workflow) is accepted rather than closed.
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
func logAttrsSetOnDoneTriage(taskTriage TaskTriageStore, taskID string) {
	if taskTriage == nil {
		return
	}
	tt, err := taskTriage.GetTaskTriage(taskID)
	if err != nil {
		slog.Warn("I-5c: attrs_set landed on a done triage task, but re-reading task_triage failed", "task_id", taskID, "error", err)
		return
	}
	slog.Info("I-5c: attrs_set landed on a done triage task",
		"task_id", taskID, "source_closed", orchestrator.SourceClosed(tt.Detail))
}
