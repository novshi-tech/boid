package api

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// machineFor selects the orchestrator.StateMachine that governs task, based
// on task.Type: a card uses orchestrator.NewCardMachine, everything else
// orchestrator.NewExecutionMachine. Pure and total — cannot fail.
func machineFor(task *orchestrator.Task) *orchestrator.StateMachine {
	if task.Type == orchestrator.TaskTypeCard {
		return orchestrator.NewCardMachine()
	}
	return orchestrator.NewExecutionMachine()
}
