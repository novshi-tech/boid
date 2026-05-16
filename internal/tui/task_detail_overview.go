package tui

import (
	"encoding/json"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// styleAwaitingBanner is the lipgloss border for the "Question from agent" banner.
var styleAwaitingBanner = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.Color("12")).
	Padding(0, 1)

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

// renderAwaitingBanner renders the "Question from agent" banner for awaiting tasks.
// Shows the question preview (up to 2 lines) and a hint to open the answer form.
// Only rendered for root tasks (ParentID == ""); child-task questions are answered
// by the parent supervisor, so showing the user-facing prompt would be misleading.
func renderAwaitingBanner(task *orchestrator.Task, width int) string {
	if task.ParentID != "" {
		return ""
	}
	ap := orchestrator.GetAwaitingPayload(task.Payload)
	if ap.QuestionID == "" && ap.Question == "" {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(styleBadge.Render("Question from agent"))

	if ap.PendingAnswer != "" {
		sb.WriteString(styleDim.Render("  (answered, waiting for agent)"))
	}
	sb.WriteByte('\n')

	// Show up to 2 lines of question text
	if ap.Question != "" {
		lines := strings.Split(ap.Question, "\n")
		preview := lines[0]
		if len(lines) > 1 && strings.TrimSpace(lines[1]) != "" {
			preview += "\n" + lines[1]
		}
		innerWidth := max(width-4, 10)
		sb.WriteString(styleAwaitingBanner.Width(innerWidth).Render(truncate(preview, innerWidth*2)))
		sb.WriteByte('\n')
	}

	if ap.PendingAnswer == "" {
		sb.WriteString(styleDim.Render("  press Enter or action key to answer"))
	}
	sb.WriteByte('\n')
	return sb.String()
}

// renderOverview renders the Overview tab content (Timeline only).
func (s *TaskDetailScreen) renderOverview(width, height int) string {
	var sb strings.Builder

	// Awaiting banner at top when task is in awaiting state
	bannerLines := 0
	if s.detail != nil && s.detail.Task != nil && s.detail.Task.Status == orchestrator.TaskStatusAwaiting {
		banner := renderAwaitingBanner(s.detail.Task, width)
		if banner != "" {
			sb.WriteString(banner)
			bannerLines = strings.Count(banner, "\n") + 1
		}
	}

	groups := buildTreeTimeline(s.detail)
	sb.WriteString(renderSectionHeader("Timeline", width))
	sb.WriteByte('\n')

	timelineHeight := max(height-1-bannerLines, 2)
	sb.WriteString(renderTreeTimeline(groups, width, timelineHeight, s.timelineCursor, s.shared.BlinkOn))

	return sb.String()
}
