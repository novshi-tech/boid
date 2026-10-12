package orchestrator

// WorkspaceDefaults supplies automatic judgment for repository-free cards.
func WorkspaceDefaults() *ProjectMeta {
	readonly := true
	return &ProjectMeta{
		DefaultTaskBehavior: "judge",
		TaskBehaviors: map[string]TaskBehavior{
			"judge": {
				Readonly:           &readonly,
				DefaultInstruction: &Instruction{Message: "Use /boid-card-judge to advance the card supplied by boid card context. Follow /boid-task for task lifecycle and use boid task ask when clarification is needed."},
			},
		},
		CardCommands: map[string]CardCommand{
			"judge": {Label: "Judge", CardWrite: true, Run: "printf '%s\\n' 'title: \"[judge]\"' 'behavior: judge' 'auto_start: true' | boid task create"},
		},
		CardCommandsOrder: []string{"judge"},
		CardEvents:        CardEventsConfig{Command: "judge"},
	}
}
