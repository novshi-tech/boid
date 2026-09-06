package orchestrator

import "fmt"

// ValidateCardCommands checks a project.yaml `card_commands:` /
// `card_events:` declaration at LOAD time (called from
// parseProjectMetaBytes, spec_loader.go), mirroring ValidateTriggers for
// `triggers[]`. A malformed declaration must fail `boid project add`/
// `fetch` loudly rather than surface later as a silently-broken command
// button or a card_events reference that never fires.
func ValidateCardCommands(commands map[string]CardCommand, events CardEvents) error {
	for key, cmd := range commands {
		if key == "" {
			return fmt.Errorf("project.yaml: card_commands: key must not be empty")
		}
		if cmd.Label == "" {
			return fmt.Errorf("project.yaml: card_commands.%s: label must not be empty", key)
		}
		if cmd.Run == "" {
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
