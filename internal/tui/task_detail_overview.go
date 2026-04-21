package tui

import (
	"encoding/json"
	"strings"
)

// openFinding represents a single unresolved verification finding.
type openFinding struct {
	gate    string
	message string
}

// parseOpenFindings extracts all non-resolved findings from the task payload's
// verification section. The structure mirrors orchestrator.verificationSubkeys.
func parseOpenFindings(payload json.RawMessage) []openFinding {
	if len(payload) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil
	}
	raw, ok := m["verification"]
	if !ok || string(raw) == "null" {
		return nil
	}
	var sub map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sub); err != nil {
		return nil
	}

	type finding struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	type verEntry struct {
		Findings []finding `json:"findings"`
	}

	var result []openFinding
	for gate, v := range sub {
		var entry verEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			continue
		}
		for _, f := range entry.Findings {
			if f.Status != "resolved" {
				result = append(result, openFinding{gate: gate, message: f.Message})
			}
		}
	}
	return result
}

// renderSectionHeader returns a "─── Title ───" line padded to width.
func renderSectionHeader(title string, width int) string {
	s := "─── " + title + " "
	fillLen := max(width-len([]rune(s)), 0)
	return styleDim.Render(s + strings.Repeat("─", fillLen))
}

// renderOverview renders the Overview tab content (Timeline only).
func (s *TaskDetailScreen) renderOverview(width, height int) string {
	var sb strings.Builder

	groups := buildTreeTimeline(s.detail)
	sb.WriteString(renderSectionHeader("Timeline", width))
	sb.WriteByte('\n')

	timelineHeight := max(height-1, 2)
	sb.WriteString(renderTreeTimeline(groups, width, timelineHeight, s.timelineCursor, s.shared.BlinkOn))

	return sb.String()
}
