package api

import "github.com/novshi-tech/boid/internal/orchestrator"

// checkDependencies is a no-op retained for workflow_action.go compatibility
// after the depends_on feature was removed from api/task layers.
func checkDependencies(_ *orchestrator.Task, _ func(string) (*orchestrator.Task, error)) error {
	return nil
}
