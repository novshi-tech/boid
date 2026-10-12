package orchestrator

import (
	"context"
	"fmt"
	"github.com/novshi-tech/boid/internal/db"
	"log/slog"
	"sort"

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

// mergeWorkspaceAutomation anchors workspace definitions to its receiver.
func (s *ProjectStore) mergeWorkspaceAutomation(out *ProjectMeta, ws *WorkspaceMeta, workspaceID string) error {
	triggers, err := workspaceTriggers(ws)
	if err != nil {
		return err
	}
	occupiedTriggers := make(map[string]string)
	occupiedCommands := make(map[string]string)
	builtinJudge := out.CardCommands["judge"]
	builtinEvents := out.CardEvents
	builtinOrder := append([]string(nil), out.CardCommandsOrder...)
	accepted := make(map[string]bool)
	s.mu.RLock()
	for id, meta := range s.metas {
		if s.workspaceIDs[id] != workspaceID || id == DefaultMetaprojectID(workspaceID) {
			continue
		}
		for _, trig := range meta.Triggers {
			occupiedTriggers[trig.Name] = id
		}
		for name := range meta.CardCommands {
			occupiedCommands[name] = id
		}
	}
	s.mu.RUnlock()
	for _, trig := range triggers {
		if id, ok := occupiedTriggers[trig.Name]; ok {
			slog.Warn("workspace trigger shadowed by project.yaml", "workspace_id", workspaceID, "project_id", id, "name", trig.Name)
			continue
		}
		out.Triggers = append(out.Triggers, trig)
	}
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
		if id, ok := occupiedCommands[name]; ok {
			slog.Warn("workspace card command shadowed by project.yaml", "workspace_id", workspaceID, "project_id", id, "name", name)
			continue
		}
		if out.CardCommands == nil {
			out.CardCommands = make(map[string]CardCommand)
		}
		accepted[name] = true
		out.CardCommands[name] = ws.CardCommands[name]
	}
	if ws.CardEvents.Command != "" {
		if _, ok := occupiedCommands[ws.CardEvents.Command]; ok {
			delete(accepted, "judge")
			out.CardCommands["judge"] = builtinJudge
			out.CardEvents = builtinEvents
			slog.Warn("workspace card events command shadowed by project.yaml; keeping built-in judge", "workspace_id", workspaceID, "name", ws.CardEvents.Command)
		} else if _, ok := out.CardCommands[ws.CardEvents.Command]; !ok {
			delete(accepted, "judge")
			out.CardCommands["judge"] = builtinJudge
			out.CardEvents = builtinEvents
			slog.Warn("workspace card events command unavailable; keeping built-in judge", "workspace_id", workspaceID, "name", ws.CardEvents.Command)
		} else {
			out.CardEvents = ws.CardEvents
		}
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
		return fmt.Errorf("workspace %q receiver: %w", workspaceID, err)
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

// CardEventCommandInTx resolves receiver events using the caller's database transaction.
func (s *ProjectStore) CardEventCommandInTx(dbtx db.DBTX, projectID string) (string, bool, error) {
	s.mu.RLock()
	meta := s.metas[projectID]
	workspaceID := s.workspaceIDs[projectID]
	s.mu.RUnlock()
	if meta == nil {
		return "", false, nil
	}
	if workspaceID == "" || projectID != DefaultMetaprojectID(workspaceID) {
		return meta.CardEvents.Command, meta.CardEvents.Command != "", nil
	}
	if s.workspaceStore == nil || s.workspaceStore.repo == nil {
		hydrated, err := s.GetWithWorkspace(context.Background(), projectID)
		if err != nil {
			return "", false, err
		}
		return hydrated.CardEvents.Command, hydrated.CardEvents.Command != "", nil
	}
	ws, err := loadWorkspaceMeta(dbtx, workspaceID)
	if err != nil {
		return "", false, err
	}
	out := cloneProjectMeta(meta)
	if err := s.mergeWorkspaceAutomation(out, ws, workspaceID); err != nil {
		return "", false, err
	}
	return out.CardEvents.Command, out.CardEvents.Command != "", nil
}
