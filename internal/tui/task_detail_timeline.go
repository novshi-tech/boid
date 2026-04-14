package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

const (
	timelineKindAction  = "action"
	timelineKindFinding = "finding"
	timelineKindJob     = "job"
)

// timelineEvent is a single row in the tree timeline view.
type timelineEvent struct {
	Time     time.Time
	Kind     string
	Label    string
	Job      *api.Job // non-nil for job events
	HasTime  bool     // false = no reliable timestamp (findings)
	Resolved bool     // for finding events: true = resolved
}

// allFinding holds one verification finding regardless of status.
type allFinding struct {
	gate     string
	message  string
	resolved bool
}

// parseAllFindings extracts all verification findings (open and resolved) from the task payload.
func parseAllFindings(payload json.RawMessage) []allFinding {
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

	type findingEntry struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	type verEntry struct {
		Findings []findingEntry `json:"findings"`
	}

	var result []allFinding
	for gate, v := range sub {
		var entry verEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			continue
		}
		for _, f := range entry.Findings {
			result = append(result, allFinding{
				gate:     gate,
				message:  f.Message,
				resolved: f.Status == "resolved",
			})
		}
	}
	return result
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

// buildJobTimelineLabel returns the display label for a completed or failed job.
// Format: "[role] <handler_id> ✓|✗ <duration>". handler_id is omitted when empty.
func buildJobTimelineLabel(j *api.Job) string {
	role := j.Role
	if role == "" {
		role = "job"
	}
	handler := ""
	if j.HandlerID != "" {
		handler = j.HandlerID + " "
	}
	dur := jobDuration(j)
	switch j.Status {
	case api.JobStatusCompleted:
		return fmt.Sprintf("[%s] %s✓ %s", role, handler, dur)
	case api.JobStatusFailed:
		return fmt.Sprintf("[%s] %s✗ %s", role, handler, dur)
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

// buildTreeTimeline groups filtered events by the task status in which they
// occurred. Only state-transition actions, completed/failed jobs, and findings
// are included. hook_fired / exit_gate_fired / entry_gate_fired actions are
// dropped because the associated job carries the same information (and more).
//
// Each group's EnteredAt is the time the task entered that status:
//   - initial state: task.CreatedAt
//   - subsequent states: the CreatedAt of the transition action that moved into it
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
		if j.Status == api.JobStatusRunning {
			continue
		}
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

	// Compute per-status entry times.
	stateEntered := map[string]time.Time{}
	stateHasEntry := map[string]bool{}
	initialStatus := ""
	for _, it := range items {
		if it.action == nil {
			continue
		}
		a := it.action
		if initialStatus == "" {
			initialStatus = string(a.FromStatus)
		}
		stateEntered[string(a.ToStatus)] = a.CreatedAt
		stateHasEntry[string(a.ToStatus)] = !a.CreatedAt.IsZero()
	}
	if initialStatus == "" && detail.Task != nil {
		initialStatus = string(detail.Task.Status)
	}
	if initialStatus != "" && detail.Task != nil && !detail.Task.CreatedAt.IsZero() {
		stateEntered[initialStatus] = detail.Task.CreatedAt
		stateHasEntry[initialStatus] = true
	}

	var groups []statusGroup
	groupIdx := map[string]int{}

	addEvent := func(status string, ev timelineEvent) {
		if _, ok := groupIdx[status]; !ok {
			groupIdx[status] = len(groups)
			groups = append(groups, statusGroup{
				Status:       status,
				EnteredAt:    stateEntered[status],
				HasEnteredAt: stateHasEntry[status],
			})
		}
		idx := groupIdx[status]
		groups[idx].Events = append(groups[idx].Events, ev)
	}

	currentStatus := initialStatus

	for _, it := range items {
		if it.action != nil {
			a := it.action
			groupStatus := string(a.FromStatus)
			if groupStatus == "" {
				groupStatus = currentStatus
			}
			addEvent(groupStatus, timelineEvent{
				Time:    a.CreatedAt,
				Kind:    timelineKindAction,
				Label:   buildActionTreeLabel(a),
				HasTime: !a.CreatedAt.IsZero(),
			})
			if string(a.ToStatus) != "" {
				currentStatus = string(a.ToStatus)
			}
		} else {
			j := it.job
			addEvent(currentStatus, timelineEvent{
				Time:    j.CreatedAt,
				Kind:    timelineKindJob,
				Label:   buildJobTimelineLabel(j),
				Job:     j,
				HasTime: !j.CreatedAt.IsZero(),
			})
		}
	}

	// Findings sit in the current task status group (no reliable timestamp).
	if detail.Task != nil {
		taskStatus := string(detail.Task.Status)
		for _, f := range parseAllFindings(detail.Task.Payload) {
			status := "open"
			if f.resolved {
				status = "resolved"
			}
			addEvent(taskStatus, timelineEvent{
				Kind:     timelineKindFinding,
				Label:    fmt.Sprintf("[%s] %s (%s)", f.gate, f.message, status),
				HasTime:  false,
				Resolved: f.resolved,
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
func renderTreeTimeline(groups []statusGroup, width, height, cursor int) string {
	_ = width

	totalEvents := 0
	for _, g := range groups {
		totalEvents += len(g.Events)
	}
	if totalEvents == 0 {
		return styleDim.Render("  (no timeline events)") + "\n"
	}

	type visualRow struct {
		isHeader   bool
		headerText string
		ev         timelineEvent
		prefix     string // "├─ " or "└─ "
		evIdx      int    // index among selectable events; -1 for headers
	}

	rows := make([]visualRow, 0, totalEvents+len(groups))
	evIdx := 0
	for _, grp := range groups {
		headerText := statusHeaderStyle(grp.Status).Render(grp.Status)
		if grp.HasEnteredAt {
			headerText += "  " + styleDim.Render(grp.EnteredAt.Local().Format("15:04:05"))
		}
		rows = append(rows, visualRow{isHeader: true, headerText: headerText, evIdx: -1})
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
			sb.WriteString(row.headerText)
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
					icon = styleRunning.Render("●")
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
		case timelineKindFinding:
			if ev.Resolved {
				icon = styleTaskDim.Render("✓")
			} else {
				icon = styleWarn.Render("!")
			}
		default:
			icon = styleDim.Render("→")
		}

		line := fmt.Sprintf("%s%s%s  %s  %s",
			cursorStr,
			row.prefix,
			styleDim.Render(timeStr),
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
