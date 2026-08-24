package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// hasTaskTriageRow reports whether taskID carries a task_triage sidecar row —
// the shared discriminator PR-B's machineFor and 決定17's resolveReopenVariant
// (triage_done.go) both key off to tell a card apart from an ordinary task.
// This is the SAME judgment both functions made independently before PR-B
// (docs/plans/suggestion-as-state-transition-impl.md §2 asks for this
// consolidation); factoring it out here is a pure refactor of that shared
// logic, not a behavior change to either caller.
//
// Callers must check `store != nil` themselves BEFORE calling this — what a
// nil store means differs per caller: resolveReopenVariant fails closed on
// its own dangerous branch (misrouting a done card's reopen into RUNNING it
// as a job), while machineFor falls back to the ordinary-task machine (see
// machineFor's own doc comment for why that fallback is safe there).
//
// Only sql.ErrNoRows means "ordinary task, not a card". Any other error is
// indeterminate and returned as such — guessing on a transient failure risks
// exactly the dangerous branch machineFor/resolveReopenVariant each exist to
// avoid.
func hasTaskTriageRow(store TaskTriageStore, taskID string) (bool, error) {
	_, err := store.GetTaskTriage(taskID)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, err
	}
}

// isCardLifecycleStatus reports whether status is one of the SIX statuses
// that are unambiguously card-only — captured/triaged/parked/ready/working
// (machine_card.go's own preExecutionStatuses set, unexported there) plus
// dropped. machineFor uses this as a STATUS-based fallback for a task with
// no CONFIRMED sidecar row (PR #986 review, Blocker 1 — see machineFor's own
// doc comment for the full reasoning; this comment covers only which
// statuses belong in the set and why).
//
// The dividing line is NOT "pre-execution" — it is "unambiguous": can this
// status ever belong to an ordinary (non-card) task? done/aborted are the
// ONLY two that can, which is exactly why machineFor excludes just those two
// from this fallback (see its own doc comment) rather than excluding
// anything wider. dropped belongs on the "unambiguous" side of that line
// despite being a terminal status like done/aborted: grepping the full rule
// table confirms every rule with ToStatus "dropped" lives in NewCardMachine
// (the four `drop` rules) — NewExecutionMachine has none — and
// task_create.go's allowedCreateInitialStatuses cannot create a task
// directly into "dropped" either. So a dropped task is never an ordinary
// one, the same way captured/triaged/parked/ready/working never are; leaving
// it out here (as an earlier version of this fix did) reopened Blocker 1's
// exact gap for exactly one status; see the regression test this fix adds
// (a rowless dropped card's `reopen` used to 409 against
// NewExecutionMachine, which has no dropped→anything rule, instead of
// reaching NewCardMachine's `reopen: dropped→triaged` "recovery from a
// mistaken drop" rule).
//
// Deliberately NOT orchestrator.IsPreExecutionStatus, which excludes
// "working" (and doesn't cover "dropped" at all) on purpose — but for an
// UNRELATED reason (model.go's own doc comment: working is closer in kind to
// executing/awaiting for open-list/queue filtering purposes, since a working
// card has already been through Go). That exclusion does not apply here: a
// card reaches "working" via Dispatch (workflow_triage.go) without ever
// needing a task_triage row to exist first (Dispatch tolerates
// sql.ErrNoRows as "no children" — see its own doc comment), so a working
// card missing its row is exactly as plausible as a captured/triaged one,
// and excluding "working" here would silently reopen Blocker 1's gap for it.
func isCardLifecycleStatus(status orchestrator.TaskStatus) bool {
	switch status {
	case orchestrator.TaskStatusCaptured, orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusParked, orchestrator.TaskStatusReady, orchestrator.TaskStatusWorking,
		orchestrator.TaskStatusDropped:
		return true
	default:
		return false
	}
}

// machineFor selects the orchestrator.StateMachine that governs task: PR-B
// (docs/plans/suggestion-as-state-transition-impl.md §2) splits the single
// unified orchestrator.NewMachine into orchestrator.NewCardMachine (a card)
// and orchestrator.NewExecutionMachine (an ordinary, session-bearing task).
//
// A CONFIRMED task_triage sidecar row (store non-nil and GetTaskTriage finds
// a row) always means NewCardMachine — this is the one case where the
// answer is definite. Everything else is decided by isCardLifecycleStatus,
// NOT collapsed straight to NewExecutionMachine as an earlier version of
// this function did (PR #986 review, Blocker 1):
//
//   - store nil (not every caller wires TaskTriage — TaskAppService/
//     TaskWorkflowService/WebAppService all tolerate it being unset,
//     matching every other optional dependency's convention in this
//     package): there is nothing to look up, so the task's OWN status is
//     the only signal available.
//   - store non-nil but GetTaskTriage reports sql.ErrNoRows: task_create.go's
//     SeedTaskTriage is deliberately BEST-EFFORT at task-creation time — a
//     transient failure there (its own doc comment: "the row is re-created
//     by the first side-effect action anyway ... a lazily-seeded row is a
//     strictly better outcome than a lost card") leaves a real card
//     genuinely rowless. Before this fallback, such a task was routed to
//     NewExecutionMachine, which has no rule for ANY card verb (attrs_set/
//     child_added/child_specced/child_dropped/noted/answered/triage/ready/
//     park/drop/wake_*) NOR for any of its six statuses (isCardLifecycleStatus
//     — captured/triaged/parked/ready/working/dropped) — every one of
//     those verbs 400s at the IsManualAction gate before ever reaching the
//     applyAttrsSetSideEffect/applyParkSideEffect ErrNoRows-creates-row path
//     task_create.go's comment promises as the recovery. The card became
//     reachable only via delete — a permanently stuck task. Falling back to
//     NewCardMachine by STATUS here (isCardLifecycleStatus) restores that
//     recovery path exactly.
//
// done/aborted are DELIBERATELY EXCLUDED from isCardLifecycleStatus, so a
// rowless task in one of those two statuses still falls through to
// NewExecutionMachine below: a done card and a done ordinary task are
// indistinguishable by status alone (the same fact resolveReopenVariant's
// own doc comment states for the identical reason) — guessing by status
// there would misapply one machine's rules to the other side's task, the
// dangerous direction this whole function exists to avoid. For those two
// statuses only a CONFIRMED sidecar row may decide.
//
// A genuine (non-ErrNoRows) lookup failure is indeterminate and returned as
// an error rather than guessed at either way — same posture as
// resolveReopenVariant's own equivalent branch. See machineForDisplay below
// for the read-only call sites that intentionally soften this into a
// status-based guess instead of failing the whole read closed.
func machineFor(store TaskTriageStore, task *orchestrator.Task) (*orchestrator.StateMachine, error) {
	if store != nil {
		isCard, err := hasTaskTriageRow(store, task.ID)
		if err != nil {
			return nil, &StatusError{
				Code:    http.StatusServiceUnavailable,
				Message: fmt.Sprintf("cannot determine state machine for task %s: %v", task.ID, err),
			}
		}
		if isCard {
			return orchestrator.NewCardMachine(), nil
		}
	}
	if isCardLifecycleStatus(task.Status) {
		return orchestrator.NewCardMachine(), nil
	}
	return orchestrator.NewExecutionMachine(), nil
}

// machineForDisplay is machineFor's fail-OPEN sibling for READ-ONLY call
// sites (TaskAppService/WebAppService.GetTaskDetail) whose only use of the
// resolved machine is computing AvailableActions for display — never a
// state mutation (PR #986 review, Blocker 2).
//
// Before this, a genuine transient task_triage lookup failure (SQLITE_BUSY
// etc — hasTaskTriageRow's non-ErrNoRows branch) turned machineFor's 503
// into GetTaskDetail failing outright, which WebHandler.TaskDetail
// (web.go) then rendered as an opaque "Task not found" 404 — contradicting
// the posture every OTHER best-effort task_triage read in this package
// already takes (WebHandler.loadTriage / triageChildrenFor tolerate a
// lookup failure by rendering the page without the triage extras, never by
// failing the whole view). A transient DB blip should cost this one render
// a possibly-wrong AvailableActions list (the only thing machineFor's
// choice affects here), not the entire task detail page/API response.
//
// Every WRITE path (ApplyAction, ReplayHook) keeps machineFor's fail-CLOSED
// 503 unchanged — only this read-only sibling downgrades a lookup failure
// to "guess by status and keep rendering".
func machineForDisplay(store TaskTriageStore, task *orchestrator.Task) *orchestrator.StateMachine {
	sm, err := machineFor(store, task)
	if err != nil {
		if isCardLifecycleStatus(task.Status) {
			return orchestrator.NewCardMachine()
		}
		return orchestrator.NewExecutionMachine()
	}
	return sm
}
