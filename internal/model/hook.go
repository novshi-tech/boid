package model

type Hook struct {
	ID             string      `yaml:"id" json:"id"`
	On             string      `yaml:"on" json:"on"`
	RequiresTraits []TraitType `yaml:"requires_traits" json:"requires_traits"`
	Requires       []string    `yaml:"requires" json:"requires"`
	ScriptPath     string      `yaml:"-" json:"-"`
}

type HookFireEvent struct {
	EventID   string
	TaskID    string
	ProjectID string
	Hook      Hook
}

type TraitType string

const (
	TraitAgentPrompt TraitType = "agent_prompt"
	TraitPR          TraitType = "pr"
	TraitPipeline    TraitType = "pipeline"
	TraitTasks       TraitType = "tasks"
	TraitReview      TraitType = "review"
)
