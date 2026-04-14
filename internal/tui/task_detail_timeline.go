package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/api"
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
