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
	"encoding/json"
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
	ID          string
	Role        string
	HandlerID   string
	DisplayName string // optional; shown instead of HandlerID when non-empty
	Status      string // one of JobStatusRunning / Completed / Failed (or other)
	ExitCode    int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Event is a single row in the unified timeline. Exactly one of Action / Job
// is populated, matching Kind.
//
// Sticky marks a synthesized reference row that surfaces a long-lived job
// under the task's CURRENT status group (in addition to the historic group
// where the job originally landed). Without this, a single hook job that
// outlives multiple ask/answer round trips (the canonical `boid task ask`
// blocking RPC pattern) would only render under its starting executing group
// while later status visits look empty even though the same agent process is
// still running there. Sticky events are non-authoritative — they share the
// underlying *JobInfo with the original row — so renderers must treat them as
// display-only (no separate progress fetch, no double-counted runtime, etc.).
type Event struct {
	Time    time.Time
	HasTime bool
	Kind    EventKind
	Label   string
	Action  *orchestrator.Action
	Job     *JobInfo
	Sticky  bool
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

// IsProgressAction reports whether an action is a non-transitioning progress note.
func IsProgressAction(a *orchestrator.Action) bool {
	return a.Type == "progress"
}

// ActionTypeAPIGatewayRequest is the orchestrator.Action.Type recorded for
// one API gateway request (docs/plans/api-gateway.md §論点3: "確定: method +
// service + path + status を timeline に。body は記録しない"). Exported so
// internal/server's apigateway.RequestRecorder adapter
// (apigateway_notify.go) can tag every Action it writes with this exact
// value — a single source of truth shared by the writer (internal/server)
// and the readers here (IsAPIGatewayRequestAction/BuildActionLabel), rather
// than two packages each hard-coding the same magic string and risking
// drift.
//
// NOTE (2026-08-07, user feedback): these rows are intentionally
// audit-only. A task that fans out into a paginated Jira search, a
// status-polling loop, or any multi-request gateway operation used to spam
// the task-detail timeline with one meaningless "api: GET ... → 200" row
// per request — signal-free noise for a human reading the timeline, even
// though the underlying row has real value as an access-audit record.
// Build (below) deliberately excludes ActionTypeAPIGatewayRequest actions
// from the rendered Event list for this reason. The row still lands in the
// actions table via internal/server's recorder and remains queryable via
// TaskRepository.ListActionsByTask for anyone building an audit view —
// only the timeline *display* dropped it.
const ActionTypeAPIGatewayRequest = "api_gateway_request"

// IsAPIGatewayRequestAction reports whether an action records one API
// gateway request. Consulted only by Build's unconditional exclusion check
// above (see the NOTE on ActionTypeAPIGatewayRequest) — there is currently
// no other reader. BuildActionLabel deliberately has no branch for this
// action type: Build never calls it for a gateway row, so a label-building
// branch there would be unreachable dead code. If a future audit-log view
// wants a rendered label for these rows, add the branch back then rather
// than speculatively now.
func IsAPIGatewayRequestAction(a *orchestrator.Action) bool {
	return a.Type == ActionTypeAPIGatewayRequest
}

// IsAnsweredAction reports whether an action records a human's accept/
// reject answer to a Web UI suggestion (J-6,
// docs/plans/ingestion-identity.md PR-3). Consulted by Build to
// deliberately ALLOW this one non-transitioning action type through, unlike
// the general non-transitioning/non-progress exclusion just below (which
// still applies to attrs_set / child_added / child_specced / noted — a
// pre-existing gap, not a new one, that this PR does not touch: see Opus
// review finding #6, 2026-08-19).
//
// The reason `answered` gets the exception where those others don't:
// `answered` is the FIRST non-transitioning action a human directly
// triggers by clicking a Web UI button (the task-detail page's Accept/
// Reject pair). Without this, clicking Reject makes the suggestion card
// disappear (applyAnsweredSideEffect strips detail.attrs.suggestion) with
// literally no human-visible trace anywhere in the UI — the very "誰がいつ
// 却下したか" question the design doc's script-facing action_list read口
// answers programmatically had no answer for a person reading the task
// detail page. attrs_set/child_added/child_specced/noted are daemon-
// internal or khi-script-originated, not something a human clicks, so they
// don't share this specific gap.
func IsAnsweredAction(a *orchestrator.Action) bool {
	return a.Type == "answered"
}

// BuildActionLabel returns the display label for a timeline action.
// State transitions: "<type> → <to_status>".
// Progress actions: "進捗: <message>" (extracted from JSON payload).
// Answered actions: "answered: <accept|reject>" (falls back to the bare
// "answered" type string if the payload is missing/malformed — Build
// itself never lets a malformed answered payload land here in practice,
// since parseAnsweredPayload validates it before CreateAction, but this
// stays defensive rather than assuming that invariant).
func BuildActionLabel(a *orchestrator.Action) string {
	if IsStateTransition(a) {
		return a.Type + " → " + string(a.ToStatus)
	}
	if IsAnsweredAction(a) {
		return buildAnsweredLabel(a)
	}
	if IsProgressAction(a) {
		return buildProgressLabel(a)
	}
	return a.Type
}

// buildProgressLabel extracts the message from a progress Action's JSON payload.
func buildProgressLabel(a *orchestrator.Action) string {
	if len(a.Payload) > 0 {
		var p struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(a.Payload, &p); err == nil && p.Message != "" {
			return "進捗: " + p.Message
		}
	}
	return "進捗"
}

// buildAnsweredLabel extracts the accept/reject decision from an answered
// Action's JSON payload (answeredPayload's wire shape, internal/api's
// workflow_triage.go: {"answer": "accept"|"reject", "verb": ..., "basis":
// ...}). Falls back to the bare action type when the payload is missing or
// doesn't parse — timeline must never fail to render over a malformed row.
func buildAnsweredLabel(a *orchestrator.Action) string {
	if len(a.Payload) > 0 {
		var p struct {
			Answer string `json:"answer"`
		}
		if err := json.Unmarshal(a.Payload, &p); err == nil && p.Answer != "" {
			return "answered: " + p.Answer
		}
	}
	return a.Type
}

// BuildJobLabel returns the display label for a job.
//   - completed: "[role] <name> ✓ <duration>" (name omitted when empty)
//   - failed:    "[role] <name> ✗ <duration>"
//   - running:   "[role] <elapsed> ago"
//   - other:     "[role] <name><status>"
//
// DisplayName is used when set; otherwise HandlerID is used as fallback.
// When role is "hook", the "[hook]" prefix is omitted — the handler name alone
// identifies the hook sufficiently.
func BuildJobLabel(j *JobInfo) string {
	role := j.Role
	if role == "" {
		role = "job"
	}
	label := j.DisplayName
	if label == "" {
		label = j.HandlerID
	}
	handler := ""
	if label != "" {
		handler = label + " "
	}
	if role == "hook" {
		switch j.Status {
		case JobStatusCompleted:
			return fmt.Sprintf("%s✓ %s", handler, JobDuration(j))
		case JobStatusFailed:
			return fmt.Sprintf("%s✗ %s", handler, JobDuration(j))
		case JobStatusRunning:
			return fmt.Sprintf("%s%s ago", handler, FormatElapsed(j.CreatedAt))
		default:
			return fmt.Sprintf("%s%s", handler, j.Status)
		}
	}
	switch j.Status {
	case JobStatusCompleted:
		return fmt.Sprintf("[%s] %s✓ %s", role, handler, JobDuration(j))
	case JobStatusFailed:
		return fmt.Sprintf("[%s] %s✗ %s", role, handler, JobDuration(j))
	case JobStatusRunning:
		return fmt.Sprintf("[%s] %s ago", role, FormatElapsed(j.CreatedAt))
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

// FormatElapsed returns a short MM:SS (or HH:MM:SS) elapsed string since t.
// Matches the TUI helper so that TUI display is unchanged after shared-timeline migration.
// Exported so the Web UI can compute the initial server-side value for a
// running job's live-ticking elapsed counter.
func FormatElapsed(t time.Time) string {
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
// Only state-transition actions and jobs are included. hook_fired actions are
// intentionally dropped because the associated job carries the same information
// (success, handler id, duration) plus output.
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
		// API gateway request actions are deliberately NOT included here —
		// see the NOTE on ActionTypeAPIGatewayRequest above. They stay in
		// the actions table (audit trail) but never become timeline Events.
		// Checked unconditionally and first — not merely left out of the
		// allow-list below — so a gateway Action can never leak through via
		// IsStateTransition/IsProgressAction regardless of what its
		// FromStatus/ToStatus happen to hold.
		if IsAPIGatewayRequestAction(a) {
			continue
		}
		// answered (J-6) is a deliberate, narrow exception to the
		// non-transitioning exclusion below — see IsAnsweredAction's own
		// doc comment for why (Opus review finding #6, 2026-08-19).
		// attrs_set/child_added/child_specced/noted stay excluded.
		if !IsStateTransition(a) && !IsProgressAction(a) && !IsAnsweredAction(a) {
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

			// Progress actions are non-transitioning: don't open a new status group.
			if toStatus := string(a.ToStatus); toStatus != "" && toStatus != fromStatus {
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

	// Sticky "currently-running" reference: when the task is in a live status
	// (executing or awaiting) and exactly one hook job is still running, append
	// a Sticky=true reference row to the latest group so the agent shows up
	// under "where the task is now," not only under the historic executing
	// group where the job was created. Skip when the running job's original
	// row is already in the latest group (no double-render on first executing
	// visit) and when the task is in a terminal status (the historic placement
	// is the truth there).
	if task != nil && len(groups) > 0 {
		cur := string(task.Status)
		if cur == string(orchestrator.TaskStatusExecuting) || cur == string(orchestrator.TaskStatusAwaiting) {
			var running *JobInfo
			for _, j := range jobs {
				if j != nil && j.Status == JobStatusRunning {
					running = j
					break
				}
			}
			if running != nil {
				last := &groups[len(groups)-1]
				already := false
				for _, ev := range last.Events {
					if ev.Kind == KindJob && ev.Job != nil && ev.Job.ID == running.ID {
						already = true
						break
					}
				}
				if !already {
					last.Events = append(last.Events, Event{
						Time:    running.CreatedAt,
						HasTime: !running.CreatedAt.IsZero(),
						Kind:    KindJob,
						Label:   BuildJobLabel(running),
						Job:     running,
						Sticky:  true,
					})
				}
			}
		}
	}

	return groups
}
