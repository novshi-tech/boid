package orchestrator

import (
	"fmt"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/db"
	"gopkg.in/yaml.v3"
)

// workspaceTriggers validates explicit and derived triggers without storing derived entries.
func workspaceTriggers(ws *WorkspaceMeta) ([]Trigger, error) {
	triggers := append([]Trigger(nil), ws.Triggers...)
	derived, err := deriveSignalTriggers(ws.Signals.Sources)
	if err != nil {
		return nil, err
	}
	triggers = append(triggers, derived...)
	return triggers, ValidateTriggers(triggers)
}

func validateWorkspaceAutomation(ws *WorkspaceMeta) error {
	if strings.Contains(ws.BaseBranch, "${current_branch}") || strings.Contains(ws.BaseBranch, "$current_branch") {
		return fmt.Errorf("base_branch: current_branch expansion is no longer supported; use a literal branch or omit base_branch to let the agent select it")
	}
	if _, err := workspaceTriggers(ws); err != nil {
		return err
	}
	return ValidateCardCommands(ws.CardCommands, ws.CardEvents)
}

func mappingOrder(n *yaml.Node) []string {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	var order []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		order = append(order, n.Content[i].Value)
	}
	return order
}

// orderedWorkspaceNode retains card command declaration order during YAML export.
func orderedWorkspaceNode(value any, order []string) (*yaml.Node, error) {
	n, err := encodeRoundTrippable(value)
	if err != nil {
		return nil, err
	}
	reorderCardCommands(n, order)
	return n, nil
}

func reorderCardCommands(n *yaml.Node, order []string) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value != "card_commands" {
			continue
		}
		commands := n.Content[i+1]
		if commands.Kind != yaml.MappingNode {
			continue
		}
		entries := make(map[string][]*yaml.Node)
		for j := 0; j+1 < len(commands.Content); j += 2 {
			entries[commands.Content[j].Value] = commands.Content[j : j+2]
		}
		var content []*yaml.Node
		for _, key := range order {
			if pair, ok := entries[key]; ok {
				content = append(content, pair...)
				delete(entries, key)
			}
		}
		for j := 0; j+1 < len(commands.Content); j += 2 {
			key := commands.Content[j].Value
			if pair, ok := entries[key]; ok {
				content = append(content, pair...)
				delete(entries, key)
			}
		}
		commands.Content = content
	}
}

func (ws WorkspaceMeta) MarshalYAML() (any, error) {
	type plain WorkspaceMeta
	return orderedWorkspaceNode(plain(ws), ws.CardCommandsOrder)
}

func (ws *WorkspaceMeta) UnmarshalYAML(n *yaml.Node) error {
	type plain WorkspaceMeta
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*ws = WorkspaceMeta(p)
	data, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	return hydrateWorkspaceAutomation(ws, data)
}

// mergeWorkspaceAutomation applies workspace automation over built-in defaults.
func (s *ProjectStore) mergeWorkspaceAutomation(out *ProjectMeta, ws *WorkspaceMeta, workspaceID string) error {
	triggers, err := workspaceTriggers(ws)
	if err != nil {
		return err
	}
	builtinOrder := append([]string(nil), out.CardCommandsOrder...)
	accepted := make(map[string]bool)
	out.Triggers = triggers
	out.Signals = ws.Signals
	order := append([]string(nil), ws.CardCommandsOrder...)
	seen := make(map[string]bool, len(order))
	for _, name := range order {
		seen[name] = true
	}
	var missing []string
	for name := range ws.CardCommands {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	order = append(order, missing...)
	for _, name := range order {
		if out.CardCommands == nil {
			out.CardCommands = make(map[string]CardCommand)
		}
		accepted[name] = true
		out.CardCommands[name] = ws.CardCommands[name]
	}
	if ws.CardEvents.Command != "" {
		out.CardEvents = ws.CardEvents
	}
	out.CardCommandsOrder = nil
	for _, name := range builtinOrder {
		if !accepted[name] {
			out.CardCommandsOrder = append(out.CardCommandsOrder, name)
		}
	}
	for _, name := range order {
		if accepted[name] {
			out.CardCommandsOrder = append(out.CardCommandsOrder, name)
			delete(accepted, name)
		}
	}
	if err := ValidateCardCommands(out.CardCommands, out.CardEvents); err != nil {
		return fmt.Errorf("workspace %q: %w", workspaceID, err)
	}
	return nil
}

func hydrateWorkspaceAutomation(ws *WorkspaceMeta, data []byte) error {
	meta := ProjectMeta{CardCommands: ws.CardCommands, CardEvents: ws.CardEvents}
	if err := hydrateCardCommandsOrderAndEvents(&meta, data); err != nil {
		return err
	}
	ws.CardCommandsOrder = meta.CardCommandsOrder
	ws.CardEvents = meta.CardEvents
	return validateWorkspaceAutomation(ws)
}

// WorkspaceCardEventCommandInTx reads events inside the caller's transaction.
func (s *ProjectStore) WorkspaceCardEventCommandInTx(dbtx db.DBTX, workspaceID string) (string, bool, error) {
	var ws *WorkspaceMeta
	var err error
	if s.workspaceStore == nil {
		return "", false, fmt.Errorf("workspace store not configured")
	}
	if s.workspaceStore.repo != nil {
		ws, err = loadWorkspaceMeta(dbtx, workspaceID)
	} else {
		ws, err = s.workspaceStore.Load(workspaceID)
	}
	if err != nil {
		return "", false, err
	}
	out := WorkspaceDefaults()
	if err := s.mergeWorkspaceAutomation(out, ws, workspaceID); err != nil {
		return "", false, err
	}
	return out.CardEvents.Command, out.CardEvents.Command != "", nil
}

// CardEventCommandInTx resolves a repository card's workspace without leaving the transaction.
func (s *ProjectStore) CardEventCommandInTx(dbtx db.DBTX, projectID string) (string, bool, error) {
	project, err := GetProject(dbtx, projectID)
	if err != nil {
		return "", false, err
	}
	if project.WorkspaceID == "" {
		return "", false, nil
	}
	return s.WorkspaceCardEventCommandInTx(dbtx, project.WorkspaceID)
}
