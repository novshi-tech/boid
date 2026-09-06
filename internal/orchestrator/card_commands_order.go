package orchestrator

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// hydrateCardCommandsOrderAndEvents fills meta.CardCommandsOrder from
// card_commands:'s raw mapping node order (a plain Go map cannot carry
// that), and re-decodes card_events: alone with KnownFields enforcement so
// a typo'd field there fails load instead of silently reading as unset.
func hydrateCardCommandsOrderAndEvents(meta *ProjectMeta, data []byte) error {
	var doc struct {
		CardCommands yaml.Node `yaml:"card_commands"`
		CardEvents   yaml.Node `yaml:"card_events"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("card_commands/card_events: %w", err)
	}

	if doc.CardCommands.Kind == yaml.MappingNode {
		order := make([]string, 0, len(doc.CardCommands.Content)/2)
		for i := 0; i+1 < len(doc.CardCommands.Content); i += 2 {
			order = append(order, doc.CardCommands.Content[i].Value)
		}
		meta.CardCommandsOrder = order
	}

	if doc.CardEvents.Kind != 0 {
		var strict CardEventsConfig
		if err := decodeStrictNode(doc.CardEvents, &strict); err != nil {
			return fmt.Errorf("card_events: %w", err)
		}
		meta.CardEvents = strict
	}
	meta.CardEvents.Command = strings.TrimSpace(meta.CardEvents.Command)

	return nil
}
