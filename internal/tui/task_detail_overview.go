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
			worktree := ""
			if j.WorkspacePath != "" {
				worktree = "  worktree=" + j.WorkspacePath
			}
			line := fmt.Sprintf("  %s running job: [%s] %s%s",
				styleRunning.Render("●"),
				j.Role,
				styleDim.Render(formatElapsed(j.CreatedAt)+" ago"),
				styleDim.Render(worktree),
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

	// ─── Deps summary ─────────────────────────────────────────
	if s.detail != nil {
		hasDeps := len(s.detail.DependsOnResolved) > 0 || len(s.detail.Dependents) > 0
		if hasDeps {
			sb.WriteString(renderSectionHeader("Deps summary", width))
			sb.WriteByte('\n')
			used++
			if len(s.detail.DependsOnResolved) > 0 {
				var parts []string
				for _, dep := range s.detail.DependsOnResolved {
					parts = append(parts, dep.Title+" ("+string(dep.Status)+")")
				}
				sb.WriteString("  depends_on: " + strings.Join(parts, ", "))
				sb.WriteByte('\n')
				used++
			}
			if len(s.detail.Dependents) > 0 {
				sb.WriteString(fmt.Sprintf("  dependents: %d waiting", len(s.detail.Dependents)))
				sb.WriteByte('\n')
				used++
			}
		}
	}

	// ─── Description ──────────────────────────────────────────
	sb.WriteString(renderSectionHeader("Description", width))
	sb.WriteByte('\n')
	used++

	var descLines []string
	if s.detail != nil && s.detail.Task != nil && s.detail.Task.Description != "" {
		descLines = strings.Split(s.detail.Task.Description, "\n")
	}

	if len(descLines) == 0 {
		sb.WriteString(styleDim.Render("  (no description)"))
		sb.WriteByte('\n')
	} else {
		// Reserve 1 line for "... more" hint if needed
		descHeight := max(height-used-1, 1)

		start := s.descScroll
		if start >= len(descLines) {
			start = 0
		}
		end := min(start+descHeight, len(descLines))
		for _, line := range descLines[start:end] {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
		if end < len(descLines) {
			sb.WriteString(styleDim.Render(fmt.Sprintf("  ... %d more lines", len(descLines)-end)))
			sb.WriteByte('\n')
		}
	}

	return sb.String()
}
