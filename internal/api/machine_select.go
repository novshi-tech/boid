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

// machineFor selects the orchestrator.StateMachine that governs task: PR-B
// (docs/plans/suggestion-as-state-transition-impl.md §2) splits the single
// unified orchestrator.NewMachine into orchestrator.NewCardMachine (a card —
// a task_triage sidecar row exists) and orchestrator.NewExecutionMachine (an
// ordinary, session-bearing task — no sidecar row). store may be nil (not
// every caller wires TaskTriage — TaskAppService/TaskWorkflowService/
// WebAppService all tolerate it being unset, matching every other optional
// dependency's convention in this package): a nil store cannot look anything
// up, so it falls back to NewExecutionMachine — the overwhelmingly common
// case (an ordinary dev task) and the one every pre-PR-B caller effectively
// assumed by construction (the unified machine had every rule regardless of
// whether TaskTriage happened to be wired). This is deliberately a SOFTER
// fallback than resolveReopenVariant's own nil-store handling (which 503s):
// resolveReopenVariant's wrong-guess branch would dispatch a card as a job,
// while machineFor's wrong-guess branch only costs a card action a spurious
// 400 ("action not available") — not dangerous enough to warrant failing the
// entire request closed.
func machineFor(store TaskTriageStore, task *orchestrator.Task) (*orchestrator.StateMachine, error) {
	if store == nil {
		return orchestrator.NewExecutionMachine(), nil
	}
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
	return orchestrator.NewExecutionMachine(), nil
}
