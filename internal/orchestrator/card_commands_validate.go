package orchestrator

import (
	"fmt"
	"sort"
	"strings"
)

// ValidateCardCommands checks a project.yaml card_commands: / card_events:
// declaration at load time, mirroring ValidateTriggers for triggers[].
func ValidateCardCommands(commands map[string]CardCommand, events CardEventsConfig) error {
	keys := make([]string, 0, len(commands))
	for key := range commands {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		cmd := commands[key]
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("project.yaml: card_commands: key must not be empty")
		}
		if strings.TrimSpace(cmd.Label) == "" {
			return fmt.Errorf("project.yaml: card_commands.%s: label must not be empty", key)
		}
		if strings.TrimSpace(cmd.Run) == "" {
			return fmt.Errorf("project.yaml: card_commands.%s: run must not be empty", key)
		}
	}
	if events.Command != "" {
		if _, ok := commands[events.Command]; !ok {
			return fmt.Errorf("project.yaml: card_events.command: %q is not declared in card_commands", events.Command)
		}
	}
	return nil
}
