package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- jobDuration / buildJobTimelineLabel tests ---

func TestJobDuration_Normal(t *testing.T) {
	now := time.Now()
	j := &api.Job{
		Status:    api.JobStatusCompleted,
		CreatedAt: now.Add(-2 * time.Minute),
		UpdatedAt: now,
	}
	if dur := jobDuration(j); dur == "?" {
		t.Error("jobDuration: expected non-? for normal job")
	}
}

func TestJobDuration_ZeroUpdatedAt(t *testing.T) {
	j := &api.Job{
		Status:    api.JobStatusCompleted,
		CreatedAt: time.Now().Add(-2 * time.Minute),
	}
	if dur := jobDuration(j); dur != "?" {
		t.Errorf("jobDuration with zero UpdatedAt: want '?', got %q", dur)
	}
}

func TestBuildJobTimelineLabel_Completed(t *testing.T) {
	now := time.Now()
	j := &api.Job{
		Role: "gate", Status: api.JobStatusCompleted, ExitCode: 0,
		CreatedAt: now.Add(-30 * time.Second), UpdatedAt: now,
	}
	label := buildJobTimelineLabel(j)
	if !containsStr(label, "[gate]") {
		t.Errorf("completed label: expected '[gate]', got %q", label)
	}
	if !containsStr(label, "✓") {
		t.Errorf("completed label: expected '✓', got %q", label)
	}
}

func TestBuildJobTimelineLabel_Failed(t *testing.T) {
	now := time.Now()
	j := &api.Job{
		Role: "hook", Status: api.JobStatusFailed, ExitCode: 1,
		CreatedAt: now.Add(-106 * time.Second), UpdatedAt: now,
	}
	label := buildJobTimelineLabel(j)
	if !containsStr(label, "[hook]") {
		t.Errorf("failed label: expected '[hook]', got %q", label)
	}
	if !containsStr(label, "✗") {
		t.Errorf("failed label: expected '✗', got %q", label)
	}
}

// --- buildTreeTimeline tests ---

// TestBuildTreeTimeline_GroupsByStatus verifies events are grouped by the source status.
func TestBuildTreeTimeline_GroupsByStatus(t *testing.T) {
	now := time.Now()
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{ID: "t", Status: orchestrator.TaskStatusVerifying},
		Actions: []*orchestrator.Action{
			{
				ID:         "a1",
				Type:       "start",
				FromStatus: orchestrator.TaskStatusPending,
				ToStatus:   orchestrator.TaskStatusExecuting,
				CreatedAt:  now.Add(-3 * time.Minute),
			},
			{
				ID:         "a2",
				Type:       "done",
				FromStatus: orchestrator.TaskStatusExecuting,
				ToStatus:   orchestrator.TaskStatusVerifying,
				CreatedAt:  now.Add(-1 * time.Minute),
			},
		},
		Jobs: []*api.Job{
			{
				ID:        "j1",
				Role:      "main",
				Status:    api.JobStatusCompleted,
				CreatedAt: now.Add(-2 * time.Minute),
				UpdatedAt: now.Add(-1 * time.Minute),
			},
		},
	}

	groups := buildTreeTimeline(detail)

	// Expect at least 2 groups: pending and executing.
	if len(groups) < 2 {
		t.Fatalf("want >= 2 groups, got %d", len(groups))
	}

	// First group: pending (where "start" action occurred).
	if groups[0].Status != "pending" {
		t.Errorf("groups[0].Status: want %q, got %q", "pending", groups[0].Status)
	}
	if len(groups[0].Events) != 1 {
		t.Errorf("pending group: want 1 event, got %d", len(groups[0].Events))
	}

	// Second group: executing (job + "done" action).
	if groups[1].Status != "executing" {
		t.Errorf("groups[1].Status: want %q, got %q", "executing", groups[1].Status)
	}
	if len(groups[1].Events) != 2 {
		t.Errorf("executing group: want 2 events, got %d", len(groups[1].Events))
	}
}

// TestBuildTreeTimeline_ActionLabel_Transition verifies that state-transition actions
// have "→ <to_status>" appended to their label.
func TestBuildTreeTimeline_ActionLabel_Transition(t *testing.T) {
	now := time.Now()
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{ID: "t", Status: orchestrator.TaskStatusExecuting},
		Actions: []*orchestrator.Action{
			{
				ID:         "a1",
				Type:       "start",
				FromStatus: orchestrator.TaskStatusPending,
				ToStatus:   orchestrator.TaskStatusExecuting,
				CreatedAt:  now,
			},
		},
	}

	groups := buildTreeTimeline(detail)
	if len(groups) == 0 || len(groups[0].Events) == 0 {
		t.Fatal("want at least 1 group with 1 event")
	}

	label := groups[0].Events[0].Label
	if !containsStr(label, "→") {
		t.Errorf("transition action label: want '→', got %q", label)
	}
	if !containsStr(label, "executing") {
		t.Errorf("transition action label: want 'executing', got %q", label)
	}
}

// TestBuildTreeTimeline_DropsHookAndGateFiredActions verifies that hook_fired /
// exit_gate_fired / entry_gate_fired action events are excluded — the
// corresponding job event already conveys hook id, success, and duration.
func TestBuildTreeTimeline_DropsHookAndGateFiredActions(t *testing.T) {
	now := time.Now()
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{ID: "t", Status: orchestrator.TaskStatusExecuting, CreatedAt: now.Add(-5 * time.Minute)},
		Actions: []*orchestrator.Action{
			{
				ID: "a1", Type: "start",
				FromStatus: orchestrator.TaskStatusPending,
				ToStatus:   orchestrator.TaskStatusExecuting,
				CreatedAt:  now.Add(-4 * time.Minute),
			},
			{
				ID: "a2", Type: "hook_fired",
				FromStatus: orchestrator.TaskStatusExecuting,
				ToStatus:   orchestrator.TaskStatusExecuting,
				Payload:    json.RawMessage(`{"hook_id":"x/y","success":true}`),
				CreatedAt:  now.Add(-3 * time.Minute),
			},
			{
				ID: "a3", Type: "exit_gate_fired",
				FromStatus: orchestrator.TaskStatusExecuting,
				ToStatus:   orchestrator.TaskStatusExecuting,
				Payload:    json.RawMessage(`{"hook_id":"x/z","success":true}`),
				CreatedAt:  now.Add(-2 * time.Minute),
			},
		},
	}
	groups := buildTreeTimeline(detail)
	for _, g := range groups {
		for _, ev := range g.Events {
			if containsStr(ev.Label, "hook_fired") ||
				containsStr(ev.Label, "exit_gate_fired") ||
				containsStr(ev.Label, "entry_gate_fired") {
				t.Errorf("hook/gate fired action should be dropped, got label %q", ev.Label)
			}
		}
	}
}

// TestBuildJobTimelineLabel_IncludesHandlerID verifies hook/gate handler IDs
// appear in the label so users don't need the removed hook_fired action rows.
func TestBuildJobTimelineLabel_IncludesHandlerID(t *testing.T) {
	now := time.Now()
	j := &api.Job{
		Role:      "hook",
		HandlerID: "claude-code/run-agent",
		Status:    api.JobStatusCompleted,
		CreatedAt: now.Add(-2 * time.Minute),
		UpdatedAt: now,
	}
	label := buildJobTimelineLabel(j)
	if !containsStr(label, "claude-code/run-agent") {
		t.Errorf("job label: want handler id, got %q", label)
	}
	if !containsStr(label, "[hook]") {
		t.Errorf("job label: want role prefix, got %q", label)
	}
}

// TestBuildTreeTimeline_StateEntryTime verifies that each group records the
// task.CreatedAt for the initial state and transition timestamps for others.
func TestBuildTreeTimeline_StateEntryTime(t *testing.T) {
	created := time.Unix(1_700_000_000, 0)
	now := created.Add(10 * time.Minute)
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID: "t", Status: orchestrator.TaskStatusVerifying,
			CreatedAt: created,
			Payload: json.RawMessage(`{"verification":{"g":{"findings":[{"message":"x","status":"open"}]}}}`),
		},
		Actions: []*orchestrator.Action{
			{
				ID: "a1", Type: "start",
				FromStatus: orchestrator.TaskStatusPending,
				ToStatus:   orchestrator.TaskStatusExecuting,
				CreatedAt:  now.Add(-5 * time.Minute),
			},
			{
				ID: "a2", Type: "done",
				FromStatus: orchestrator.TaskStatusExecuting,
				ToStatus:   orchestrator.TaskStatusVerifying,
				CreatedAt:  now.Add(-1 * time.Minute),
			},
		},
	}
	groups := buildTreeTimeline(detail)
	got := map[string]time.Time{}
	for _, g := range groups {
		if g.HasEnteredAt {
			got[g.Status] = g.EnteredAt
		}
	}
	if !got["pending"].Equal(created) {
		t.Errorf("pending entry time: want %v, got %v", created, got["pending"])
	}
	if !got["executing"].Equal(now.Add(-5 * time.Minute)) {
		t.Errorf("executing entry time: want %v, got %v", now.Add(-5*time.Minute), got["executing"])
	}
	if !got["verifying"].Equal(now.Add(-1 * time.Minute)) {
		t.Errorf("verifying entry time: want %v, got %v", now.Add(-1*time.Minute), got["verifying"])
	}
}

// TestBuildTreeTimeline_ExcludesFindings verifies that verification findings
// are NOT surfaced in the timeline. Findings are shown in the dedicated
// "Findings (open)" section and via the Payload tab; mixing raw finding
// message text into the timeline would expose payload content inconsistently.
func TestBuildTreeTimeline_ExcludesFindings(t *testing.T) {
	now := time.Now()
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:     "t",
			Status: orchestrator.TaskStatusVerifying,
			Payload: json.RawMessage(`{
				"verification": {
					"mergeable-check": {
						"findings": [
							{"message": "conflict detail", "status": "open"},
							{"message": "other", "status": "resolved"}
						]
					}
				}
			}`),
		},
		Actions: []*orchestrator.Action{
			{
				ID: "a1", Type: "start",
				FromStatus: orchestrator.TaskStatusPending,
				ToStatus:   orchestrator.TaskStatusExecuting,
				CreatedAt:  now.Add(-2 * time.Minute),
			},
			{
				ID: "a2", Type: "done",
				FromStatus: orchestrator.TaskStatusExecuting,
				ToStatus:   orchestrator.TaskStatusVerifying,
				CreatedAt:  now.Add(-1 * time.Minute),
			},
		},
	}
	groups := buildTreeTimeline(detail)
	for _, g := range groups {
		for _, ev := range g.Events {
			if containsStr(ev.Label, "conflict detail") || containsStr(ev.Label, "mergeable-check") {
				t.Errorf("finding content should not appear in timeline, got %q", ev.Label)
			}
		}
	}
}

// --- renderTreeTimeline tests ---

// TestRenderTreeTimeline_Empty verifies the empty state message when no groups.
func TestRenderTreeTimeline_Empty(t *testing.T) {
	out := renderTreeTimeline(nil, 80, 20, 0, false)
	if !containsStr(out, "no timeline events") {
		t.Errorf("empty: expected 'no timeline events', got %q", out)
	}
}

// TestRenderTreeTimeline_EmptyGroupStillShowsHeader verifies that a group with
// no events (e.g. the task just entered a new state) still renders its header
// so the user can see where the task currently is.
func TestRenderTreeTimeline_EmptyGroupStillShowsHeader(t *testing.T) {
	out := renderTreeTimeline([]statusGroup{{Status: "pending", Events: nil}}, 80, 20, 0, false)
	if !containsStr(out, "pending") {
		t.Errorf("expected 'pending' header in output, got %q", out)
	}
}

// TestRenderTreeTimeline_BoxChars verifies ├─ and └─ appear in the rendered output.
func TestRenderTreeTimeline_BoxChars(t *testing.T) {
	now := time.Now()
	groups := []statusGroup{
		{
			Status: "pending",
			Events: []timelineEvent{
				{Time: now, Kind: timelineKindAction, Label: "start → executing", HasTime: true},
			},
		},
		{
			Status: "executing",
			Events: []timelineEvent{
				{Time: now.Add(time.Minute), Kind: timelineKindJob, Label: "[main] exit=0 done",
					HasTime: true, Job: &api.Job{Status: api.JobStatusCompleted}},
				{Time: now.Add(2 * time.Minute), Kind: timelineKindAction, Label: "done → verifying", HasTime: true},
			},
		},
	}

	out := renderTreeTimeline(groups, 80, 30, 0, false)

	// Single-event group uses └─; multi-event group uses both ├─ and └─.
	if !containsStr(out, "└─") {
		t.Error("expected '└─' in output")
	}
	if !containsStr(out, "├─") {
		t.Error("expected '├─' in output")
	}
	// Status headers must be present.
	if !containsStr(out, "pending") {
		t.Error("expected 'pending' header in output")
	}
	if !containsStr(out, "executing") {
		t.Error("expected 'executing' header in output")
	}
}

// TestRenderTreeTimeline_CursorOnEvent verifies that the cursor indicator appears
// on the correct event line and state headers are skipped.
func TestRenderTreeTimeline_CursorOnEvent(t *testing.T) {
	groups := []statusGroup{
		{
			Status: "pending",
			Events: []timelineEvent{
				{Kind: timelineKindAction, Label: "event-in-pending", HasTime: false},
			},
		},
		{
			Status: "executing",
			Events: []timelineEvent{
				{Kind: timelineKindAction, Label: "event-in-executing", HasTime: false},
			},
		},
	}

	// cursor=0 → cursor indicator must be on the line containing "event-in-pending".
	out0 := renderTreeTimeline(groups, 80, 20, 0, false)
	for _, line := range strings.Split(out0, "\n") {
		if strings.Contains(line, "▸") {
			if !strings.Contains(line, "event-in-pending") {
				t.Errorf("cursor=0: cursor on wrong line: %q", line)
			}
		}
	}

	// cursor=1 → cursor indicator must be on the line containing "event-in-executing".
	out1 := renderTreeTimeline(groups, 80, 20, 1, false)
	for _, line := range strings.Split(out1, "\n") {
		if strings.Contains(line, "▸") {
			if !strings.Contains(line, "event-in-executing") {
				t.Errorf("cursor=1: cursor on wrong line: %q", line)
			}
		}
	}

	// Verify that at least one "▸" appears in each output.
	if !containsStr(out0, "▸") {
		t.Error("cursor=0: no cursor indicator found")
	}
	if !containsStr(out1, "▸") {
		t.Error("cursor=1: no cursor indicator found")
	}
}

// TestRenderTreeTimeline_HeaderNoCursor verifies that state headers never receive
// the cursor indicator, even when the cursor value equals zero.
func TestRenderTreeTimeline_HeaderNoCursor(t *testing.T) {
	groups := []statusGroup{
		{
			Status: "pending",
			Events: []timelineEvent{
				{Kind: timelineKindAction, Label: "start", HasTime: false},
			},
		},
	}

	out := renderTreeTimeline(groups, 80, 20, 0, false)

	for _, line := range strings.Split(out, "\n") {
		// A header line contains the status name but not a box-drawing char.
		if strings.Contains(line, "pending") && !strings.Contains(line, "─") {
			// This is the header line — it must not contain "▸".
			if strings.Contains(line, "▸") {
				t.Errorf("header line must not contain cursor indicator: %q", line)
			}
		}
	}
}

// TestSelectableEventsInGroups verifies the flat event list preserves group order.
func TestSelectableEventsInGroups(t *testing.T) {
	groups := []statusGroup{
		{
			Status: "pending",
			Events: []timelineEvent{
				{Kind: timelineKindAction, Label: "start"},
			},
		},
		{
			Status: "executing",
			Events: []timelineEvent{
				{Kind: timelineKindJob, Label: "job"},
				{Kind: timelineKindAction, Label: "done"},
			},
		},
	}

	events := selectableEventsInGroups(groups)
	if len(events) != 3 {
		t.Fatalf("want 3 events, got %d", len(events))
	}
	if events[0].Label != "start" {
		t.Errorf("events[0]: want 'start', got %q", events[0].Label)
	}
	if events[1].Label != "job" {
		t.Errorf("events[1]: want 'job', got %q", events[1].Label)
	}
	if events[2].Label != "done" {
		t.Errorf("events[2]: want 'done', got %q", events[2].Label)
	}
}

// TestBuildTreeTimeline_RunningJobAtTail verifies that a running job (created later)
// appears after an earlier completed job in the timeline.
func TestBuildTreeTimeline_RunningJobAtTail(t *testing.T) {
	now := time.Now()
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{ID: "t", Status: orchestrator.TaskStatusExecuting, CreatedAt: now.Add(-10 * time.Minute)},
		Jobs: []*api.Job{
			{ID: "j1", Role: "main", Status: api.JobStatusRunning, CreatedAt: now.Add(-2 * time.Minute)},
			{ID: "j2", Role: "hook", Status: api.JobStatusCompleted,
				CreatedAt: now.Add(-5 * time.Minute), UpdatedAt: now.Add(-3 * time.Minute)},
		},
	}
	groups := buildTreeTimeline(detail)
	events := selectableEventsInGroups(groups)
	if len(events) < 2 {
		t.Fatalf("want >= 2 events, got %d", len(events))
	}
	// Completed job (CreatedAt -5m) should appear before running job (CreatedAt -2m).
	last := events[len(events)-1]
	if last.Job == nil || last.Job.Status != api.JobStatusRunning {
		t.Errorf("last timeline event should be the running job, got job=%v", last.Job)
	}
}

// TestBuildTreeTimeline_MultiVisit_ChronologicalGroups verifies that when a task
// visits the same status multiple times, each visit creates a separate group in
// chronological order.
func TestBuildTreeTimeline_MultiVisit_ChronologicalGroups(t *testing.T) {
	now := time.Now()
	t1 := now.Add(-30 * time.Minute)
	t2 := now.Add(-20 * time.Minute)
	t3 := now.Add(-10 * time.Minute)

	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{ID: "t", Status: orchestrator.TaskStatusExecuting},
		Actions: []*orchestrator.Action{
			// 1st cycle
			{ID: "a1", Type: "start",
				FromStatus: orchestrator.TaskStatusPending, ToStatus: orchestrator.TaskStatusExecuting,
				CreatedAt: t1},
			{ID: "a2", Type: "abort",
				FromStatus: orchestrator.TaskStatusExecuting, ToStatus: orchestrator.TaskStatusAborted,
				CreatedAt: t2},
			// 2nd cycle (aborted→pending transition omitted — tests graceful handling)
			{ID: "a3", Type: "start",
				FromStatus: orchestrator.TaskStatusPending, ToStatus: orchestrator.TaskStatusExecuting,
				CreatedAt: t3},
		},
		Jobs: []*api.Job{
			{ID: "j1", Role: "main", Status: api.JobStatusFailed,
				CreatedAt: t1.Add(30 * time.Second), UpdatedAt: t2},
		},
	}
	groups := buildTreeTimeline(detail)

	// Expect at least 5 groups: pending₁, executing₁, aborted₁, pending₂, executing₂
	if len(groups) < 5 {
		t.Fatalf("multi-visit: want >= 5 groups, got %d", len(groups))
	}

	// First group: pending₁
	if groups[0].Status != "pending" {
		t.Errorf("groups[0].Status: want %q, got %q", "pending", groups[0].Status)
	}

	// Second group: executing₁ must contain the job and the abort action.
	if groups[1].Status != "executing" {
		t.Errorf("groups[1].Status: want %q, got %q", "executing", groups[1].Status)
	}
	if len(groups[1].Events) < 2 {
		t.Errorf("executing₁ group: want >= 2 events (job + abort action), got %d", len(groups[1].Events))
	}

	// A second "pending" group must appear after the first executing group.
	foundPending2 := false
	for _, g := range groups[2:] {
		if g.Status == "pending" {
			foundPending2 = true
			break
		}
	}
	if !foundPending2 {
		t.Error("want a second 'pending' group after the first cycle, not found")
	}
}

// TestBuildJobTimelineLabel_Running verifies the running job label format.
func TestBuildJobTimelineLabel_Running(t *testing.T) {
	j := &api.Job{
		Role:      "main",
		Status:    api.JobStatusRunning,
		CreatedAt: time.Now().Add(-2 * time.Minute),
	}
	label := buildJobTimelineLabel(j)
	if !containsStr(label, "[main]") {
		t.Errorf("running label: expected '[main]', got %q", label)
	}
	if !containsStr(label, "ago") {
		t.Errorf("running label: expected 'ago', got %q", label)
	}
}

// TestRenderTreeTimeline_RunningJobDimOnBlinkOff verifies that a running job's dot
// renders as dim when blinkOn=false and as running-color when blinkOn=true.
func TestRenderTreeTimeline_RunningJobDimOnBlinkOff(t *testing.T) {
	groups := []statusGroup{
		{
			Status: "executing",
			Events: []timelineEvent{
				{Kind: timelineKindJob, Label: "[main] 2m ago", HasTime: false,
					Job: &api.Job{Status: api.JobStatusRunning}},
			},
		},
	}

	outOff := renderTreeTimeline(groups, 80, 20, 0, false)
	outOn := renderTreeTimeline(groups, 80, 20, 0, true)

	dimDot := styleTaskDim.Render("●")
	runDot := styleRunning.Render("●")

	if !strings.Contains(outOff, dimDot) {
		t.Errorf("blinkOff: running dot should be dim, outOff=%q", outOff)
	}
	if !strings.Contains(outOn, runDot) {
		t.Errorf("blinkOn: running dot should be running-color, outOn=%q", outOn)
	}
}
