package api

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// machineFor selects the orchestrator.StateMachine that governs task:
// PR-B (docs/plans/suggestion-as-state-transition-impl.md §2) split the
// single unified orchestrator.NewMachine into orchestrator.NewCardMachine (a
// card) and orchestrator.NewExecutionMachine (an ordinary, session-bearing
// task); card-model-cleanup PR-2 (docs/plans/card-model-cleanup.md §3.6)
// then replaced the sidecar-row lookup this function used to make (a DB
// round trip through CardStore, with a status-based fallback for a rowless
// card and a fail-open/fail-closed split between write and read call sites —
// see git history for the full reasoning that used to live here) with a
// direct switch on task.Type, now that Type is loaded as part of the task
// itself.
//
// This makes machineFor a pure, total function: it cannot fail, cannot
// disagree with the task it was just handed, and has nothing left to fall
// back on — task.Type already IS the discriminator. The former
// machineForDisplay (a fail-open sibling for read-only call sites, needed
// because a sidecar lookup could transiently fail) is gone for the same
// reason: there is no longer a lookup that can fail. Every call site
// (workflow_action.go, workflow_replay.go, task_service.go, web_service.go)
// now calls this directly.
func machineFor(task *orchestrator.Task) *orchestrator.StateMachine {
	if task.Type == orchestrator.TaskTypeCard {
		return orchestrator.NewCardMachine()
	}
	return orchestrator.NewExecutionMachine()
}
