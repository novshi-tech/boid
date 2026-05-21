package api

import "github.com/novshi-tech/boid/internal/orchestrator"

type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return e.Message
}

type ActionApplication struct {
	Task         *orchestrator.Task   `json:"task"`
	Action       *orchestrator.Action `json:"action"`
	MatchedHooks []string             `json:"matched_hooks,omitempty"`
}

type ProjectReloadResult struct {
	Status string   `json:"status"`
	Errors []string `json:"errors,omitempty"`
}

type TaskDetailView struct {
	Task              *orchestrator.Task
	Actions           []*orchestrator.Action
	Jobs              []*Job
	AvailableActions  []string             `json:"available_actions"`
	Dependents        []*orchestrator.Task `json:"dependents,omitempty"`
	DependsOnResolved []*orchestrator.Task `json:"depends_on_resolved,omitempty"`
}
