package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

const (
	timelineKindAction  = "action"
	timelineKindFinding = "finding"
	timelineKindJob     = "job"
)

// userDrivenActionTypes is the allowlist of action types that are considered
// user-driven. These are shown in the Overview timeline; internal engine
// actions (hook_fired, exit_gate_fired, auto_advance, etc.) are excluded.
var userDrivenActionTypes = map[string]bool{
	"start":            true,
	"abort":            true,
	"rerun":            true,
	"done":             true,
	"collect_feedback": true,
	"pause":            true,
	"resume":           true,
	"reject":           true,
	"accept":           true,
}

// timelineEvent is a single row in the unified timeline view.
type timelineEvent struct {
	Time     time.Time
	Kind     string
	Label    string
	Sub      string   // optional secondary info (e.g. worktree path)
	Job      *api.Job // non-nil for job events
	HasTime  bool     // false = no reliable timestamp; sorted to bottom
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

// buildTimeline constructs the sorted unified timeline from all event sources in detail.
func buildTimeline(detail *api.TaskDetailView) []timelineEvent {
	if detail == nil {
		return nil
	}
	var events []timelineEvent

	// Actions → action events (◆)
	for _, a := range detail.Actions {
		events = append(events, timelineEvent{
			Time:    a.CreatedAt,
			Kind:    timelineKindAction,
			Label:   a.Type + " applied",
			HasTime: !a.CreatedAt.IsZero(),
		})
	}

	// Jobs → job events (●)
	for _, j := range detail.Jobs {
		sub := ""
		if j.WorkspacePath != "" {
			sub = "worktree=" + j.WorkspacePath
		}
		events = append(events, timelineEvent{
			Time:    j.CreatedAt,
			Kind:    timelineKindJob,
			Label:   buildJobTimelineLabel(j),
			Sub:     sub,
			Job:     j,
			HasTime: !j.CreatedAt.IsZero(),
		})
	}

	// Findings → finding events (! / ✓)
	// Findings have no reliable timestamp; placed at the bottom of the timeline.
	if detail.Task != nil {
		for _, f := range parseAllFindings(detail.Task.Payload) {
			status := "open"
			if f.resolved {
				status = "resolved"
			}
			events = append(events, timelineEvent{
				Kind:     timelineKindFinding,
				Label:    fmt.Sprintf("[%s] %s (%s)", f.gate, f.message, status),
				HasTime:  false,
				Resolved: f.resolved,
			})
		}
	}

	// Sort: timed events first (ascending), no-time events at bottom.
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].HasTime != events[j].HasTime {
			return events[i].HasTime
		}
		if !events[i].HasTime {
			return false
		}
		return events[i].Time.Before(events[j].Time)
	})

	return events
}

// buildJobTimelineLabel returns the display label for a job timeline event.
func buildJobTimelineLabel(j *api.Job) string {
	role := j.Role
	if role == "" {
		role = "job"
	}
	switch j.Status {
	case api.JobStatusRunning:
		return fmt.Sprintf("[%s] running %s", role, formatElapsed(j.CreatedAt))
	case api.JobStatusCompleted:
		return fmt.Sprintf("[%s] exit=%d done", role, j.ExitCode)
	case api.JobStatusFailed:
		return fmt.Sprintf("[%s] exit=%d failed", role, j.ExitCode)
	default:
		return fmt.Sprintf("[%s] %s", role, string(j.Status))
	}
}

// buildOverviewTimeline constructs a filtered timeline for the Overview tab.
// Included:
//   - User-driven actions only (filtered by userDrivenActionTypes allowlist)
//   - Completed or failed jobs only (running jobs are shown in the Active section)
//   - All verification findings (resolved and open)
//
// Excluded: internal engine actions (hook_fired, auto_advance, etc.) and running jobs.
// Worktree paths are not included in Sub; use JobDetailScreen for those details.
func buildOverviewTimeline(detail *api.TaskDetailView) []timelineEvent {
	if detail == nil {
		return nil
	}
	var events []timelineEvent

	// User-driven actions only.
	for _, a := range detail.Actions {
		if !userDrivenActionTypes[a.Type] {
			continue
		}
		events = append(events, timelineEvent{
			Time:    a.CreatedAt,
			Kind:    timelineKindAction,
			Label:   a.Type + " applied",
			HasTime: !a.CreatedAt.IsZero(),
		})
	}

	// Completed/failed jobs only (running jobs appear in Active section).
	for _, j := range detail.Jobs {
		if j.Status == api.JobStatusRunning {
			continue
		}
		events = append(events, timelineEvent{
			Time:    j.CreatedAt,
			Kind:    timelineKindJob,
			Label:   buildOverviewJobLabel(j),
			Job:     j,
			HasTime: !j.CreatedAt.IsZero(),
		})
	}

	// Findings (all — open and resolved).
	if detail.Task != nil {
		for _, f := range parseAllFindings(detail.Task.Payload) {
			status := "open"
			if f.resolved {
				status = "resolved"
			}
			events = append(events, timelineEvent{
				Kind:     timelineKindFinding,
				Label:    fmt.Sprintf("[%s] %s (%s)", f.gate, f.message, status),
				HasTime:  false,
				Resolved: f.resolved,
			})
		}
	}

	// Sort: timed events ascending, no-time events at bottom.
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].HasTime != events[j].HasTime {
			return events[i].HasTime
		}
		if !events[i].HasTime {
			return false
		}
		return events[i].Time.Before(events[j].Time)
	})

	return events
}

// buildOverviewJobLabel returns a compact summary for a completed/failed job
// in the Overview timeline: "role  ✓ duration" or "role  ✗ duration".
func buildOverviewJobLabel(j *api.Job) string {
	role := j.Role
	if role == "" {
		role = "job"
	}
	dur := overviewJobDuration(j)
	switch j.Status {
	case api.JobStatusCompleted:
		return fmt.Sprintf("[%s]  ✓ %s", role, dur)
	case api.JobStatusFailed:
		return fmt.Sprintf("[%s]  ✗ %s", role, dur)
	default:
		return fmt.Sprintf("[%s] %s", role, string(j.Status))
	}
}

// overviewJobDuration returns a human-readable duration for a completed job.
func overviewJobDuration(j *api.Job) string {
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

// statusGroup groups timeline events under a single task status node in the tree view.
type statusGroup struct {
	Status string
	Events []timelineEvent
}

// extractSourceState reads the source_state field from an action payload.
// Used for hook_fired, exit_gate_fired, and entry_gate_fired action types.
func extractSourceState(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var m struct {
		SourceState string `json:"source_state"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	return m.SourceState
}

// buildActionTreeLabel returns the display label for an action in the tree timeline.
// For hook/gate-fired actions it extracts the handler ID and success flag from payload.
// For state-transition actions it appends " → <to_status>" to the type.
func buildActionTreeLabel(a *orchestrator.Action) string {
	switch a.Type {
	case "hook_fired", "exit_gate_fired", "entry_gate_fired":
		var p struct {
			HookID  string `json:"hook_id"`
			Success bool   `json:"success"`
		}
		if len(a.Payload) > 0 && json.Unmarshal(a.Payload, &p) == nil && p.HookID != "" {
			result := "ok"
			if !p.Success {
				result = "fail"
			}
			return a.Type + ": " + p.HookID + " " + result
		}
		return a.Type
	}
	label := a.Type
	if a.ToStatus != "" && a.FromStatus != a.ToStatus {
		label += " → " + string(a.ToStatus)
	}
	return label
}

// buildTreeTimeline groups all timeline events (actions, jobs, findings) by the
// task status in which they occurred. Groups are ordered by first occurrence.
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

	var groups []statusGroup
	groupIdx := map[string]int{}

	addEvent := func(status string, ev timelineEvent) {
		if _, ok := groupIdx[status]; !ok {
			groupIdx[status] = len(groups)
			groups = append(groups, statusGroup{Status: status})
		}
		idx := groupIdx[status]
		groups[idx].Events = append(groups[idx].Events, ev)
	}

	currentStatus := ""

	for _, it := range items {
		if it.action != nil {
			a := it.action

			groupStatus := string(a.FromStatus)
			if groupStatus == "" {
				groupStatus = currentStatus
			}
			// hook/gate fired: honour source_state from payload (same as FromStatus in practice,
			// but use the payload field as specified in the requirements).
			switch a.Type {
			case "hook_fired", "exit_gate_fired", "entry_gate_fired":
				if ss := extractSourceState(a.Payload); ss != "" {
					groupStatus = ss
				}
			}

			addEvent(groupStatus, timelineEvent{
				Time:    a.CreatedAt,
				Kind:    timelineKindAction,
				Label:   buildActionTreeLabel(a),
				HasTime: !a.CreatedAt.IsZero(),
			})

			// Advance current status on state transitions.
			if string(a.ToStatus) != "" {
				currentStatus = string(a.ToStatus)
			} else if groupStatus != "" {
				currentStatus = groupStatus
			}
		} else {
			j := it.job
			sub := ""
			if j.WorkspacePath != "" {
				sub = "worktree=" + j.WorkspacePath
			}
			addEvent(currentStatus, timelineEvent{
				Time:    j.CreatedAt,
				Kind:    timelineKindJob,
				Label:   buildJobTimelineLabel(j),
				Sub:     sub,
				Job:     j,
				HasTime: !j.CreatedAt.IsZero(),
			})
		}
	}

	// Findings are placed in the current task status group (no reliable timestamp).
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
// Used for cursor-count clamping and enter-key drilldown in the tree timeline.
func selectableEventsInGroups(groups []statusGroup) []timelineEvent {
	var events []timelineEvent
	for _, g := range groups {
		events = append(events, g.Events...)
	}
	return events
}

// renderTreeTimeline renders status groups as a tree with box-drawing characters.
// cursor is an index into the flat sequence of selectable events (state headers are not selectable).
func renderTreeTimeline(groups []statusGroup, width, height, cursor int) string {
	_ = width

	totalEvents := 0
	for _, g := range groups {
		totalEvents += len(g.Events)
	}
	if totalEvents == 0 {
		return styleDim.Render("  (no timeline events)") + "\n"
	}

	// Build a flat slice of visual rows (headers + events interleaved).
	type visualRow struct {
		isHeader bool
		header   string
		ev       timelineEvent
		prefix   string // "├─ " or "└─ "
		evIdx    int    // index among selectable events; -1 for headers
	}

	rows := make([]visualRow, 0, totalEvents+len(groups))
	evIdx := 0
	for _, grp := range groups {
		rows = append(rows, visualRow{isHeader: true, header: grp.Status, evIdx: -1})
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

	// Find the visual row position of the cursor event.
	cursorVisualRow := 0
	for i, row := range rows {
		if !row.isHeader && row.evIdx == cursor {
			cursorVisualRow = i
			break
		}
	}

	// Scroll window: keep cursor row visible.
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
			sb.WriteString(styleDim.Render("  " + row.header))
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

		labelPart := row.prefix + ev.Label
		if ev.Sub != "" {
			labelPart += "  " + styleDim.Render(ev.Sub)
		}

		line := fmt.Sprintf("%s%s  %s  %s",
			cursorStr,
			styleDim.Render(timeStr),
			icon,
			labelPart,
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

// renderTimeline renders the timeline events as a string.
// cursor selects the highlighted row; the view window follows the cursor.
func renderTimeline(events []timelineEvent, width, height, cursor int) string {
	_ = width
	if len(events) == 0 {
		return styleDim.Render("  (no timeline events)") + "\n"
	}

	// Compute scroll offset to keep cursor in view.
	scroll := 0
	if cursor >= height {
		scroll = cursor - height + 1
	}
	if maxScroll := max(len(events)-height, 0); scroll > maxScroll {
		scroll = maxScroll
	}

	var sb strings.Builder
	end := min(scroll+height, len(events))
	for i := scroll; i < end; i++ {
		ev := events[i]
		selected := i == cursor

		// Cursor indicator
		cursorStr := "  "
		if selected {
			cursorStr = styleCursor.Render("▸ ")
		}

		// Time column
		var timeStr string
		if ev.HasTime {
			timeStr = ev.Time.Local().Format("15:04:05")
		} else {
			timeStr = "--:--:--"
		}

		// Icon
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

		// Label with optional sub
		labelPart := ev.Label
		if ev.Sub != "" {
			labelPart += "  " + styleDim.Render(ev.Sub)
		}

		line := fmt.Sprintf("%s%s  %s  %s",
			cursorStr,
			styleDim.Render(timeStr),
			icon,
			labelPart,
		)
		sb.WriteString(line)
		sb.WriteByte('\n')
	}

	if end < len(events) {
		sb.WriteString(styleDim.Render(fmt.Sprintf("  ... %d more", len(events)-end)))
		sb.WriteByte('\n')
	}

	return sb.String()
}
