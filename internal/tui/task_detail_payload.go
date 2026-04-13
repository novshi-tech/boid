package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/api"
	"gopkg.in/yaml.v3"
)

// payloadSection represents one top-level key in the task payload.
type payloadSection struct {
	key  string
	data json.RawMessage
}

// knownSectionOrder defines the preferred display order for well-known payload keys.
var knownSectionOrder = []string{"instructions", "artifacts", "verification", "tasks"}

// extractPayloadSections parses the task payload and returns sections in display order.
// Known sections appear first in the predefined order; unknown keys are appended alphabetically.
func extractPayloadSections(payload json.RawMessage) []payloadSection {
	if len(payload) == 0 || string(payload) == "null" {
		return nil
	}
	raw := make(map[string]json.RawMessage)
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil
	}
	if len(raw) == 0 {
		return nil
	}

	var sections []payloadSection
	seen := make(map[string]bool)

	// known keys first
	for _, k := range knownSectionOrder {
		if v, ok := raw[k]; ok {
			sections = append(sections, payloadSection{key: k, data: v})
			seen[k] = true
		}
	}
	// remaining keys, sorted alphabetically
	var other []string
	for k := range raw {
		if !seen[k] {
			other = append(other, k)
		}
	}
	sort.Strings(other)
	for _, k := range other {
		sections = append(sections, payloadSection{key: k, data: raw[k]})
	}
	return sections
}

// jsonToYAML converts JSON data to a YAML string for display.
func jsonToYAML(data json.RawMessage) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}
	out, err := yaml.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("yaml marshal: %w", err)
	}
	return string(out), nil
}

// yamlToJSON converts a YAML string back to a JSON RawMessage.
func yamlToJSON(yamlStr string) (json.RawMessage, error) {
	var v any
	if err := yaml.Unmarshal([]byte(yamlStr), &v); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("json marshal: %w", err)
	}
	return out, nil
}

// renderPayload renders the Payload tab content: section list (top) + YAML preview (bottom).
func renderPayload(detail *api.TaskDetailView, cursor, previewScroll, width, height int) string {
	if detail == nil || detail.Task == nil {
		return styleDim.Render("  (no task data)") + "\n"
	}

	sections := extractPayloadSections(detail.Task.Payload)
	if len(sections) == 0 {
		return styleDim.Render("  (payload is empty)") + "\n"
	}

	var sb strings.Builder

	// --- section list ---
	sb.WriteString(styleTitle.Render("Sections:"))
	sb.WriteByte('\n')

	maxListLines := min(max(height/3, 3), len(sections))

	for i, sec := range sections {
		if i >= maxListLines {
			break
		}
		var line string
		if i == cursor {
			line = styleCursor.Render("  ▸ ") + styleTitle.Render(sec.key) + styleDim.Render("   (edit: e)")
		} else {
			line = "    " + styleDim.Render(sec.key)
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}

	// --- preview separator ---
	var selectedSection *payloadSection
	if cursor >= 0 && cursor < len(sections) {
		selectedSection = &sections[cursor]
	}

	previewLabel := ""
	if selectedSection != nil {
		previewLabel = "Preview (" + selectedSection.key + ") "
	}
	fillLen := max(width-4-len([]rune(previewLabel))-4, 0)
	sep := styleDim.Render("─── " + previewLabel + strings.Repeat("─", fillLen))
	sb.WriteString(sep)
	sb.WriteByte('\n')

	// preview height budget: total - header(1) - listLines - separator(1)
	previewHeight := max(height-1-maxListLines-1, 2)

	if selectedSection == nil {
		sb.WriteString(styleDim.Render("  (select a section with j/k)"))
		sb.WriteByte('\n')
		return sb.String()
	}

	yamlStr, err := jsonToYAML(selectedSection.data)
	if err != nil {
		sb.WriteString(styleError.Render("  error: "+err.Error()))
		sb.WriteByte('\n')
		return sb.String()
	}

	lines := strings.Split(strings.TrimRight(yamlStr, "\n"), "\n")
	start := min(previewScroll, max(len(lines)-1, 0))
	end := min(start+previewHeight, len(lines))

	for _, line := range lines[start:end] {
		sb.WriteString("  " + line)
		sb.WriteByte('\n')
	}

	return sb.String()
}
