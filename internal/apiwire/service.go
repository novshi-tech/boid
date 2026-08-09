package apiwire

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type ActionApplication struct {
	Task         *orchestrator.Task   `json:"task"`
	Action       *orchestrator.Action `json:"action"`
	MatchedHooks []string             `json:"matched_hooks,omitempty"`
}

type TaskDetailView struct {
	Task             *orchestrator.Task
	Actions          []*orchestrator.Action
	Jobs             []*Job
	AvailableActions []string `json:"available_actions"`
}
