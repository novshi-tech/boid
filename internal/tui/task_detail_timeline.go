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

// timelineEvent is a single row in the tree timeline view.
type timelineEvent struct {
	Time     time.Time
	Kind     string
	Label    string
	Sub      string   // optional secondary info (e.g. worktree path)
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
	Status string
	Events []timelineEvent
}

// isHookOrGateFired reports whether an action type represents a hook or gate firing.
func isHookOrGateFired(t string) bool {
	return t == "hook_fired" || t == "exit_gate_fired" || t == "entry_gate_fired"
}

// isStateTransition reports whether an action represents a meaningful state transition.
func isStateTransition(a *orchestrator.Action) bool {
	return a.FromStatus != "" && a.ToStatus != "" && a.FromStatus != a.ToStatus
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
// - hook/gate-fired: "<type>: <hook_id> ok|fail"
// - state transition: "<type> → <to_status>"
func buildActionTreeLabel(a *orchestrator.Action) string {
	if isHookOrGateFired(a.Type) {
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
	if isStateTransition(a) {
		return a.Type + " → " + string(a.ToStatus)
	}
	return a.Type
}

// buildJobTimelineLabel returns the display label for a completed or failed job.
// Format: "[role] ✓ 2m" / "[role] ✗ 2m".
func buildJobTimelineLabel(j *api.Job) string {
	role := j.Role
	if role == "" {
		role = "job"
	}
	dur := jobDuration(j)
	switch j.Status {
	case api.JobStatusCompleted:
		return fmt.Sprintf("[%s] ✓ %s", role, dur)
	case api.JobStatusFailed:
		return fmt.Sprintf("[%s] ✗ %s", role, dur)
	default:
		return fmt.Sprintf("[%s] %s", role, string(j.Status))
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

// buildTreeTimeline groups filtered timeline events by the task status in which
// they occurred. Only state-transition actions, hook/gate-fired actions,
// completed/failed jobs, and findings are included — ephemeral running jobs and
// non-transition bookkeeping actions are omitted to keep the view signal-heavy.
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
		if !isStateTransition(a) && !isHookOrGateFired(a.Type) {
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
			// hook/gate fired: honour source_state from payload when present.
			if isHookOrGateFired(a.Type) {
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

			if string(a.ToStatus) != "" {
				currentStatus = string(a.ToStatus)
			} else if groupStatus != "" {
				currentStatus = groupStatus
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
