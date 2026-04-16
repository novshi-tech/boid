package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

const (
	timelineKindAction = "action"
	timelineKindJob    = "job"
)

// timelineEvent is a single row in the tree timeline view.
type timelineEvent struct {
	Time    time.Time
	Kind    string
	Label   string
	Job     *api.Job // non-nil for job events
	HasTime bool
}

// statusGroup groups timeline events under a single task status node in the tree view.
type statusGroup struct {
	Status      string
	EnteredAt   time.Time
	HasEnteredAt bool
	Events      []timelineEvent
}

// isStateTransition reports whether an action moves the task to a different status.
func isStateTransition(a *orchestrator.Action) bool {
	return a.FromStatus != "" && a.ToStatus != "" && a.FromStatus != a.ToStatus
}

// buildActionTreeLabel returns the display label for a state-transition action.
// Format: "<type> → <to_status>".
func buildActionTreeLabel(a *orchestrator.Action) string {
	if isStateTransition(a) {
		return a.Type + " → " + string(a.ToStatus)
	}
	return a.Type
}

// buildJobTimelineLabel returns the display label for a job.
// Format for completed/failed: "[role] <handler_id> ✓|✗ <duration>". handler_id is omitted when empty.
// Format for running: "[role] <elapsed> ago".
func buildJobTimelineLabel(j *api.Job) string {
	role := j.Role
	if role == "" {
		role = "job"
	}
	handler := ""
	if j.HandlerID != "" {
		handler = j.HandlerID + " "
	}
	switch j.Status {
	case api.JobStatusCompleted:
		return fmt.Sprintf("[%s] %s✓ %s", role, handler, jobDuration(j))
	case api.JobStatusFailed:
		return fmt.Sprintf("[%s] %s✗ %s", role, handler, jobDuration(j))
	case api.JobStatusRunning:
		return fmt.Sprintf("[%s] %s ago", role, formatElapsed(j.CreatedAt))
	default:
		return fmt.Sprintf("[%s] %s%s", role, handler, string(j.Status))
	}
}

// jobDuration returns a human-readable duration for a completed/failed job.
func jobDuration(j *api.Job) string {
	if j.UpdatedAt.IsZero() || !j.UpdatedAt.After(j.CreatedAt) {
		return "?"
	}
	d := j.UpdatedAt.Sub(j.CreatedAt).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm%ds", m, s)
}

// buildTreeTimeline groups filtered events by the task status visit in which
// they occurred. Only state-transition actions and jobs are included.
// hook_fired / exit_gate_fired / entry_gate_fired actions are dropped because
// the associated job carries the same information (and more).
//
// Each visit to a status creates a new group, so repeated visits (e.g.
// executing → aborted → pending → executing) produce distinct groups in
// chronological order rather than collapsing all events for the same status
// into one group.
//
// Each group's EnteredAt is the time the task entered that visit:
//   - initial group: task.CreatedAt
//   - subsequent groups: the CreatedAt of the transition action that moved into it
func buildTreeTimeline(detail *api.TaskDetailView) []statusGroup {
	if detail == nil {
		return nil
	}

	type rawItem struct {
		t       time.Time
		hasTime bool
		action  *orchestrator.Action
		job     *api.Job
	}

	var items []rawItem
	for _, a := range detail.Actions {
		if !isStateTransition(a) {
			continue
		}
		items = append(items, rawItem{t: a.CreatedAt, hasTime: !a.CreatedAt.IsZero(), action: a})
	}
	for _, j := range detail.Jobs {
		items = append(items, rawItem{t: j.CreatedAt, hasTime: !j.CreatedAt.IsZero(), job: j})
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].hasTime != items[j].hasTime {
			return items[i].hasTime
		}
		if !items[i].hasTime {
			return false
		}
		return items[i].t.Before(items[j].t)
	})

	// Determine the initial status from the first transition action's FromStatus,
	// falling back to the task's current status.
	initialStatus := ""
	for _, it := range items {
		if it.action != nil {
			initialStatus = string(it.action.FromStatus)
			break
		}
	}
	if initialStatus == "" && detail.Task != nil {
		initialStatus = string(detail.Task.Status)
	}

	var groups []statusGroup
	currentGroupIdx := -1

	// Create the initial status group (EnteredAt = task.CreatedAt).
	if initialStatus != "" {
		enteredAt := time.Time{}
		hasEnteredAt := false
		if detail.Task != nil && !detail.Task.CreatedAt.IsZero() {
			enteredAt = detail.Task.CreatedAt
			hasEnteredAt = true
		}
		groups = append(groups, statusGroup{
			Status:       initialStatus,
			EnteredAt:    enteredAt,
			HasEnteredAt: hasEnteredAt,
		})
		currentGroupIdx = 0
	}

	for _, it := range items {
		if it.action != nil {
			a := it.action
			fromStatus := string(a.FromStatus)

			// If the current group's status doesn't match a.FromStatus, create a new
			// group for fromStatus (handles missing intermediate transitions).
			if currentGroupIdx < 0 || groups[currentGroupIdx].Status != fromStatus {
				groups = append(groups, statusGroup{Status: fromStatus})
				currentGroupIdx = len(groups) - 1
			}

			// Append the action event to the current group.
			groups[currentGroupIdx].Events = append(groups[currentGroupIdx].Events, timelineEvent{
				Time:    a.CreatedAt,
				Kind:    timelineKindAction,
				Label:   buildActionTreeLabel(a),
				HasTime: !a.CreatedAt.IsZero(),
			})

			// Each transition creates a new group for the destination status.
			if toStatus := string(a.ToStatus); toStatus != "" {
				groups = append(groups, statusGroup{
					Status:       toStatus,
					EnteredAt:    a.CreatedAt,
					HasEnteredAt: !a.CreatedAt.IsZero(),
				})
				currentGroupIdx = len(groups) - 1
			}
		} else {
			j := it.job
			// If no group exists yet, create the initial one (safety guard).
			if currentGroupIdx < 0 && initialStatus != "" {
				enteredAt := time.Time{}
				hasEnteredAt := false
				if detail.Task != nil && !detail.Task.CreatedAt.IsZero() {
					enteredAt = detail.Task.CreatedAt
					hasEnteredAt = true
				}
				groups = append(groups, statusGroup{
					Status:       initialStatus,
					EnteredAt:    enteredAt,
					HasEnteredAt: hasEnteredAt,
				})
				currentGroupIdx = 0
			}
			if currentGroupIdx >= 0 {
				groups[currentGroupIdx].Events = append(groups[currentGroupIdx].Events, timelineEvent{
					Time:    j.CreatedAt,
					Kind:    timelineKindJob,
					Label:   buildJobTimelineLabel(j),
					Job:     j,
					HasTime: !j.CreatedAt.IsZero(),
				})
			}
		}
	}

	// Ensure the current task status is visible as a group even if no events
	// sit under it yet (e.g. the task just entered the state).
	if detail.Task != nil {
		cur := string(detail.Task.Status)
		if cur != "" && (currentGroupIdx < 0 || groups[currentGroupIdx].Status != cur) {
			// Find EnteredAt from the last transition into this status.
			var enteredAt time.Time
			var hasEnteredAt bool
			for _, it := range items {
				if it.action != nil && string(it.action.ToStatus) == cur {
					enteredAt = it.action.CreatedAt
					hasEnteredAt = !it.action.CreatedAt.IsZero()
				}
			}
			groups = append(groups, statusGroup{
				Status:       cur,
				EnteredAt:    enteredAt,
				HasEnteredAt: hasEnteredAt,
			})
		}
	}

	return groups
}

// selectableEventsInGroups returns the flat event list from all groups in order.
// Used for cursor-count clamping and enter-key drilldown.
func selectableEventsInGroups(groups []statusGroup) []timelineEvent {
	var events []timelineEvent
	for _, g := range groups {
		events = append(events, g.Events...)
	}
	return events
}

// statusHeaderStyle returns the style to apply to a state group header.
func statusHeaderStyle(status string) lipgloss.Style {
	switch orchestrator.TaskStatus(status) {
	case orchestrator.TaskStatusPending:
		return stylePending.Bold(true)
	case orchestrator.TaskStatusExecuting:
		return styleExecuting.Bold(true)
	case orchestrator.TaskStatusVerifying:
		return styleVerifying.Bold(true)
	case orchestrator.TaskStatusReworking:
		return styleWarn.Bold(true)
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
				case api.JobStatusRunning:
					if blinkOn {
						icon = styleRunning.Render("●")
					} else {
						icon = styleTaskDim.Render("●")
					}
				case api.JobStatusCompleted:
					icon = styleCompleted.Render("●")
				case api.JobStatusFailed:
					icon = styleFailed.Render("●")
				default:
					icon = stylePending.Render("●")
				}
			} else {
				icon = "●"
			}
		case timelineKindAction:
			icon = styleVerifying.Render("◆")
		default:
			icon = styleDim.Render("→")
		}

		line := fmt.Sprintf("%s%s  %s%s  %s",
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
