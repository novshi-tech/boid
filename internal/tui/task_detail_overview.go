package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/novshi-tech/boid/internal/api"
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

// renderOverview renders the Overview tab content.
//
// Layout (top to bottom):
//  1. ─── Active ─── section: currently running jobs.
//  2. ─── Timeline ─── section: completed/failed jobs + user-driven actions + findings
//     (filtered via buildOverviewTimeline; running jobs excluded).
//  3. ─── Findings (open) ─── section: only when open findings exist.
func (s *TaskDetailScreen) renderOverview(width, height int) string {
	var sb strings.Builder
	used := 0

	// ─── Active ───────────────────────────────────────────────
	sb.WriteString(renderSectionHeader("Active", width))
	sb.WriteByte('\n')
	used++

	var runningJobs []*api.Job
	if s.detail != nil {
		for _, j := range s.detail.Jobs {
			if j.Status == api.JobStatusRunning {
				runningJobs = append(runningJobs, j)
			}
		}
	}

	if len(runningJobs) == 0 {
		sb.WriteString(styleDim.Render("  no active job"))
		sb.WriteByte('\n')
		used++
	} else {
		for _, j := range runningJobs {
			line := fmt.Sprintf("  %s running job: [%s] %s",
				styleRunning.Render("●"),
				j.Role,
				styleDim.Render(formatElapsed(j.CreatedAt)+" ago"),
			)
			sb.WriteString(line)
			sb.WriteByte('\n')
			used++
		}
	}

	// ─── Findings (open) ──────────────────────────────────────
	var findings []openFinding
	if s.detail != nil && s.detail.Task != nil {
		findings = parseOpenFindings(s.detail.Task.Payload)
	}
	if len(findings) > 0 {
		sb.WriteString(renderSectionHeader("Findings (open)", width))
		sb.WriteByte('\n')
		used++
		for _, f := range findings {
			line := fmt.Sprintf("  %s [%s] %s",
				styleWarn.Render("!"),
				f.gate,
				f.message,
			)
			sb.WriteString(line)
			sb.WriteByte('\n')
			used++
		}
	}

	// ─── Timeline ─────────────────────────────────────────────
	events := buildOverviewTimeline(s.detail)
	sb.WriteString(renderSectionHeader("Timeline", width))
	sb.WriteByte('\n')
	used++

	timelineHeight := max(height-used, 2)
	sb.WriteString(renderTimeline(events, width, timelineHeight, s.timelineCursor))

	return sb.String()
}
