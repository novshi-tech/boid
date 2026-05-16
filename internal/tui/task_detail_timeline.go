package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/timeline"
)

// Aliases to the shared timeline types so the TUI keeps its terse local names.
type (
	statusGroup   = timeline.StatusGroup
	timelineEvent = timeline.Event
)

const (
	timelineKindAction = timeline.KindAction
	timelineKindJob    = timeline.KindJob
)

// jobsToInfo converts api.Job pointers into the neutral timeline.JobInfo DTO
// so the timeline package doesn't need to import internal/api (which would
// create an import cycle via web/templates).
func jobsToInfo(jobs []*api.Job) []*timeline.JobInfo {
	out := make([]*timeline.JobInfo, 0, len(jobs))
	for _, j := range jobs {
		if j == nil {
			continue
		}
		out = append(out, &timeline.JobInfo{
			ID:          j.ID,
			Role:        j.Role,
			HandlerID:   j.HandlerID,
			DisplayName: j.DisplayName,
			Status:      string(j.Status),
			ExitCode:    j.ExitCode,
			CreatedAt:   j.CreatedAt,
			UpdatedAt:   j.UpdatedAt,
		})
	}
	return out
}

// buildTreeTimeline is a thin wrapper so existing call sites / tests remain
// unchanged. Building the groups is delegated to the shared package so the
// TUI and Web UI stay consistent by construction.
func buildTreeTimeline(detail *api.TaskDetailView) []statusGroup {
	if detail == nil {
		return nil
	}
	return timeline.Build(detail.Task, detail.Actions, jobsToInfo(detail.Jobs))
}

// Helpers kept for test compatibility; delegate to shared package.
func buildJobTimelineLabel(j *api.Job) string {
	info := jobsToInfo([]*api.Job{j})
	if len(info) == 0 {
		return ""
	}
	return timeline.BuildJobLabel(info[0])
}

func jobDuration(j *api.Job) string {
	info := jobsToInfo([]*api.Job{j})
	if len(info) == 0 {
		return ""
	}
	return timeline.JobDuration(info[0])
}

func selectableEventsInGroups(groups []statusGroup) []timelineEvent {
	return timeline.SelectableEvents(groups)
}

// statusHeaderStyle returns the style to apply to a state group header.
func statusHeaderStyle(status string) lipgloss.Style {
	switch orchestrator.TaskStatus(status) {
	case orchestrator.TaskStatusPending:
		return stylePending.Bold(true)
	case orchestrator.TaskStatusExecuting:
		return styleExecuting.Bold(true)
	case orchestrator.TaskStatusAborted:
		return styleAborted.Bold(true)
	}
	return styleHeader
}

// renderTreeTimeline renders status groups as a tree. State headers at column 0
// show the state name and the time it was entered; events hang below with
// ├─ / └─ tree connectors. The cursor highlights event rows only.
// blinkOn controls whether running-job dots render at full color (true) or dim (false).
func renderTreeTimeline(groups []statusGroup, width, height, cursor int, blinkOn bool) string {
	_ = width

	if len(groups) == 0 {
		return styleDim.Render("  (no timeline events)") + "\n"
	}
	totalEvents := 0
	for _, g := range groups {
		totalEvents += len(g.Events)
	}

	type visualRow struct {
		isHeader     bool
		status       string
		enteredAt    time.Time
		hasEnteredAt bool
		ev           timelineEvent
		prefix       string // "├─ " or "└─ "
		evIdx        int    // index among selectable events; -1 for headers
	}

	rows := make([]visualRow, 0, totalEvents+len(groups))
	evIdx := 0
	for _, grp := range groups {
		rows = append(rows, visualRow{
			isHeader:     true,
			status:       grp.Status,
			enteredAt:    grp.EnteredAt,
			hasEnteredAt: grp.HasEnteredAt,
			evIdx:        -1,
		})
		for i, ev := range grp.Events {
			prefix := "├─ "
			if i == len(grp.Events)-1 {
				prefix = "└─ "
			}
			rows = append(rows, visualRow{
				isHeader: false,
				ev:       ev,
				prefix:   prefix,
				evIdx:    evIdx,
			})
			evIdx++
		}
	}

	cursorVisualRow := 0
	for i, row := range rows {
		if !row.isHeader && row.evIdx == cursor {
			cursorVisualRow = i
			break
		}
	}

	scroll := 0
	if cursorVisualRow >= height {
		scroll = cursorVisualRow - height + 1
	}
	if maxScroll := max(len(rows)-height, 0); scroll > maxScroll {
		scroll = maxScroll
	}

	var sb strings.Builder
	end := min(scroll+height, len(rows))
	for i := scroll; i < end; i++ {
		row := rows[i]

		if row.isHeader {
			var headerTimeStr string
			if row.hasEnteredAt {
				headerTimeStr = row.enteredAt.Local().Format("15:04:05")
			} else {
				headerTimeStr = "--:--:--"
			}
			sb.WriteString(fmt.Sprintf("  %s  %s",
				styleDim.Render(headerTimeStr),
				statusHeaderStyle(row.status).Render(row.status),
			))
			sb.WriteByte('\n')
			continue
		}

		ev := row.ev
		selected := row.evIdx == cursor

		cursorStr := "  "
		if selected {
			cursorStr = styleCursor.Render("▸ ")
		}

		var timeStr string
		if ev.HasTime {
			timeStr = ev.Time.Local().Format("15:04:05")
		} else {
			timeStr = "--:--:--"
		}

		var icon string
		switch ev.Kind {
		case timelineKindJob:
			if ev.Job != nil {
				switch ev.Job.Status {
				case timeline.JobStatusRunning:
					if blinkOn {
						icon = styleRunning.Render("●")
					} else {
						icon = styleTaskDim.Render("●")
					}
				case timeline.JobStatusCompleted:
					icon = styleCompleted.Render("●")
				case timeline.JobStatusFailed:
					icon = styleFailed.Render("●")
				default:
					icon = stylePending.Render("●")
				}
			} else {
				icon = "●"
			}
		case timelineKindAction:
			if ev.Action != nil && ev.Action.Type == "progress" {
				icon = styleDim.Render("◇")
			} else {
				icon = styleVerifying.Render("◆")
			}
		default:
			icon = styleDim.Render("→")
		}

		line := fmt.Sprintf("  %s%s  %s%s  %s",
			cursorStr,
			styleDim.Render(timeStr),
			row.prefix,
			icon,
			ev.Label,
		)
		sb.WriteString(line)
		sb.WriteByte('\n')
	}

	if end < len(rows) {
		sb.WriteString(styleDim.Render(fmt.Sprintf("  ... %d more", len(rows)-end)))
		sb.WriteByte('\n')
	}

	return sb.String()
}
