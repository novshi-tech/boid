// Package timeline builds the unified task-detail timeline consumed by both
// the TUI and the Web UI. It imports orchestrator only — no api dependency —
// so web/templates can import it without creating an import cycle with
// internal/api (which pulls in web/templates for rendering).
//
// The builder takes fully-resolved inputs: the task, its actions, and a list
// of JobInfo records. Each caller adapts from its own Job shape (api.Job /
// dispatcher job model) via ConvertAPIJob-style helpers.
package timeline

import (
	"fmt"
	"sort"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// JobStatus mirrors the string values used by api.JobStatus so timeline
// renderers don't need to import api to discriminate running / completed /
// failed states. Callers pass these strings through.
const (
	JobStatusRunning   = "running"
	JobStatusCompleted = "completed"
	JobStatusFailed    = "failed"
)

// EventKind distinguishes action rows from job rows.
type EventKind string

const (
	KindAction EventKind = "action"
	KindJob    EventKind = "job"
)

// JobInfo is the minimum job data needed to place a job on the timeline and
// render its label / status icon / link target. Callers convert from their
// native job type (api.Job for TUI / Web).
type JobInfo struct {
	ID        string
	Role      string
	HandlerID string
	Status    string // one of JobStatusRunning / Completed / Failed (or other)
	ExitCode  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Event is a single row in the unified timeline. Exactly one of Action / Job
// is populated, matching Kind.
type Event struct {
	Time    time.Time
	HasTime bool
	Kind    EventKind
	Label   string
	Action  *orchestrator.Action
	Job     *JobInfo
}

// StatusGroup groups events under a single task-status visit.
// Repeated visits to the same status produce distinct groups in order.
type StatusGroup struct {
	Status       string
	EnteredAt    time.Time
	HasEnteredAt bool
	Events       []Event
}

// IsStateTransition reports whether an action moves the task to a different status.
func IsStateTransition(a *orchestrator.Action) bool {
	return a.FromStatus != "" && a.ToStatus != "" && a.FromStatus != a.ToStatus
}

// BuildActionLabel returns the display label for a state-transition action.
// Format: "<type> → <to_status>".
func BuildActionLabel(a *orchestrator.Action) string {
	if IsStateTransition(a) {
		return a.Type + " → " + string(a.ToStatus)
	}
	return a.Type
}

// BuildJobLabel returns the display label for a job.
//   - completed: "[role] <handler> ✓ <duration>" (handler omitted when empty)
//   - failed:    "[role] <handler> ✗ <duration>"
//   - running:   "[role] <elapsed> ago"
//   - other:     "[role] <handler><status>"
func BuildJobLabel(j *JobInfo) string {
	role := j.Role
	if role == "" {
		role = "job"
	}
	handler := ""
	if j.HandlerID != "" {
		handler = j.HandlerID + " "
	}
	switch j.Status {
	case JobStatusCompleted:
		return fmt.Sprintf("[%s] %s✓ %s", role, handler, JobDuration(j))
	case JobStatusFailed:
		return fmt.Sprintf("[%s] %s✗ %s", role, handler, JobDuration(j))
	case JobStatusRunning:
		return fmt.Sprintf("[%s] %s ago", role, formatElapsed(j.CreatedAt))
	default:
		return fmt.Sprintf("[%s] %s%s", role, handler, j.Status)
	}
}

// JobDuration returns a human-readable duration for a completed/failed job.
// Returns "?" when the job has no UpdatedAt or UpdatedAt is not after CreatedAt.
func JobDuration(j *JobInfo) string {
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

// formatElapsed returns a short MM:SS (or HH:MM:SS) elapsed string since t.
// Matches the TUI helper so that TUI display is unchanged after shared-timeline migration.
func formatElapsed(t time.Time) string {
	d := max(time.Since(t), 0)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// Build groups filtered events by the task-status visit in which they occurred.
// Only state-transition actions and jobs are included. hook_fired /
// exit_gate_fired / entry_gate_fired actions are intentionally dropped because
// the associated job carries the same information (success, handler id,
// duration) plus output.
//
// Each visit to a status creates a new group so repeated visits
// (e.g. executing → aborted → pending → executing) produce distinct groups
// in chronological order instead of collapsing same-status events into one.
//
// Each group's EnteredAt records when the task entered that visit:
//   - initial group: task.CreatedAt
//   - subsequent groups: the CreatedAt of the transition action that moved into it
func Build(task *orchestrator.Task, actions []*orchestrator.Action, jobs []*JobInfo) []StatusGroup {
	type rawItem struct {
		t       time.Time
		hasTime bool
		action  *orchestrator.Action
		job     *JobInfo
	}

	var items []rawItem
	for _, a := range actions {
		if !IsStateTransition(a) {
			continue
		}
		items = append(items, rawItem{t: a.CreatedAt, hasTime: !a.CreatedAt.IsZero(), action: a})
	}
	for _, j := range jobs {
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

	initialStatus := ""
	for _, it := range items {
		if it.action != nil {
			initialStatus = string(it.action.FromStatus)
			break
		}
	}
	if initialStatus == "" && task != nil {
		initialStatus = string(task.Status)
	}

	var groups []StatusGroup
	currentGroupIdx := -1

	if initialStatus != "" {
		enteredAt := time.Time{}
		hasEnteredAt := false
		if task != nil && !task.CreatedAt.IsZero() {
			enteredAt = task.CreatedAt
			hasEnteredAt = true
		}
		groups = append(groups, StatusGroup{
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

			if currentGroupIdx < 0 || groups[currentGroupIdx].Status != fromStatus {
				groups = append(groups, StatusGroup{Status: fromStatus})
				currentGroupIdx = len(groups) - 1
			}

			groups[currentGroupIdx].Events = append(groups[currentGroupIdx].Events, Event{
				Time:    a.CreatedAt,
				HasTime: !a.CreatedAt.IsZero(),
				Kind:    KindAction,
				Label:   BuildActionLabel(a),
				Action:  a,
			})

			if toStatus := string(a.ToStatus); toStatus != "" {
				groups = append(groups, StatusGroup{
					Status:       toStatus,
					EnteredAt:    a.CreatedAt,
					HasEnteredAt: !a.CreatedAt.IsZero(),
				})
				currentGroupIdx = len(groups) - 1
			}
		} else {
			j := it.job
			if currentGroupIdx < 0 && initialStatus != "" {
				enteredAt := time.Time{}
				hasEnteredAt := false
				if task != nil && !task.CreatedAt.IsZero() {
					enteredAt = task.CreatedAt
					hasEnteredAt = true
				}
				groups = append(groups, StatusGroup{
					Status:       initialStatus,
					EnteredAt:    enteredAt,
					HasEnteredAt: hasEnteredAt,
				})
				currentGroupIdx = 0
			}
			if currentGroupIdx >= 0 {
				groups[currentGroupIdx].Events = append(groups[currentGroupIdx].Events, Event{
					Time:    j.CreatedAt,
					HasTime: !j.CreatedAt.IsZero(),
					Kind:    KindJob,
					Label:   BuildJobLabel(j),
					Job:     j,
				})
			}
		}
	}

	if task != nil {
		cur := string(task.Status)
		if cur != "" && (currentGroupIdx < 0 || groups[currentGroupIdx].Status != cur) {
			var enteredAt time.Time
			var hasEnteredAt bool
			for _, it := range items {
				if it.action != nil && string(it.action.ToStatus) == cur {
					enteredAt = it.action.CreatedAt
					hasEnteredAt = !it.action.CreatedAt.IsZero()
				}
			}
			groups = append(groups, StatusGroup{
				Status:       cur,
				EnteredAt:    enteredAt,
				HasEnteredAt: hasEnteredAt,
			})
		}
	}

	return groups
}

// SelectableEvents returns the flat event list from all groups in order.
// Used by TUI for cursor clamping and enter-key drilldown.
func SelectableEvents(groups []StatusGroup) []Event {
	var events []Event
	for _, g := range groups {
		events = append(events, g.Events...)
	}
	return events
}
