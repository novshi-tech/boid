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

// TestBuildTreeTimeline_NonTransitionActionLabel verifies that non-transition actions
// do NOT have "→ <to_status>" in their label.
func TestBuildTreeTimeline_NonTransitionActionLabel(t *testing.T) {
	now := time.Now()
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{ID: "t", Status: orchestrator.TaskStatusExecuting},
		Actions: []*orchestrator.Action{
			{
				ID:         "a1",
				Type:       "start",
				FromStatus: orchestrator.TaskStatusPending,
				ToStatus:   orchestrator.TaskStatusExecuting,
				CreatedAt:  now.Add(-time.Minute),
			},
			{
				// Self-loop: no state transition.
				ID:         "a2",
				Type:       "hook_fired",
				FromStatus: orchestrator.TaskStatusExecuting,
				ToStatus:   orchestrator.TaskStatusExecuting,
				Payload: json.RawMessage(`{
					"kit_id":"go-dev","hook_id":"go-dev/pr-verify",
					"source_state":"executing","success":true
				}`),
				CreatedAt: now,
			},
		},
	}

	groups := buildTreeTimeline(detail)
	// Find "executing" group.
	var execGroup *statusGroup
	for i := range groups {
		if groups[i].Status == "executing" {
			execGroup = &groups[i]
			break
		}
	}
	if execGroup == nil {
		t.Fatal("executing group not found")
	}
	// Find hook_fired event.
	for _, ev := range execGroup.Events {
		if containsStr(ev.Label, "hook_fired") {
			if containsStr(ev.Label, "→") {
				t.Errorf("non-transition action should not contain '→': got %q", ev.Label)
			}
			return
		}
	}
	t.Error("hook_fired event not found in executing group")
}

// TestBuildTreeTimeline_HookFiredSourceState verifies that hook_fired uses source_state
// from the payload to determine its status group.
func TestBuildTreeTimeline_HookFiredSourceState(t *testing.T) {
	now := time.Now()
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{ID: "t", Status: orchestrator.TaskStatusExecuting},
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
				Type:       "hook_fired",
				FromStatus: orchestrator.TaskStatusExecuting,
				ToStatus:   orchestrator.TaskStatusExecuting,
				Payload: json.RawMessage(`{
					"kit_id":"go-dev","hook_id":"go-dev/pr-verify",
					"source_state":"executing","success":true
				}`),
				CreatedAt: now.Add(-1 * time.Minute),
			},
		},
	}

	groups := buildTreeTimeline(detail)

	// hook_fired should be in the "executing" group, not "pending".
	var execGroup *statusGroup
	for i := range groups {
		if groups[i].Status == "executing" {
			execGroup = &groups[i]
			break
		}
	}
	if execGroup == nil {
		t.Fatal("executing group not found")
	}

	found := false
	for _, ev := range execGroup.Events {
		if containsStr(ev.Label, "hook_fired") {
			found = true
			// Label should include hook_id and "ok".
			if !containsStr(ev.Label, "go-dev/pr-verify") {
				t.Errorf("hook_fired label: want 'go-dev/pr-verify', got %q", ev.Label)
			}
			if !containsStr(ev.Label, "ok") {
				t.Errorf("hook_fired label: want 'ok', got %q", ev.Label)
			}
		}
	}
	if !found {
		t.Error("hook_fired event not found in executing group")
	}
}

// TestBuildTreeTimeline_FindingsGoToCurrentStatus verifies findings are placed
// in the current task status group.
func TestBuildTreeTimeline_FindingsGoToCurrentStatus(t *testing.T) {
	now := time.Now()
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:     "t",
			Status: orchestrator.TaskStatusVerifying,
			Payload: json.RawMessage(`{
				"verification": {
					"mergeable-check": {
						"findings": [{"message": "conflict", "status": "open"}]
					}
				}
			}`),
		},
		Actions: []*orchestrator.Action{
			{
				ID:         "a1",
				Type:       "start",
				FromStatus: orchestrator.TaskStatusPending,
				ToStatus:   orchestrator.TaskStatusExecuting,
				CreatedAt:  now.Add(-2 * time.Minute),
			},
			{
				ID:         "a2",
				Type:       "done",
				FromStatus: orchestrator.TaskStatusExecuting,
				ToStatus:   orchestrator.TaskStatusVerifying,
				CreatedAt:  now.Add(-1 * time.Minute),
			},
		},
	}

	groups := buildTreeTimeline(detail)

	// Finding should be in "verifying" group (current task status).
	var verGroup *statusGroup
	for i := range groups {
		if groups[i].Status == "verifying" {
			verGroup = &groups[i]
			break
		}
	}
	if verGroup == nil {
		t.Fatal("verifying group not found")
	}

	found := false
	for _, ev := range verGroup.Events {
		if ev.Kind == timelineKindFinding {
			found = true
			break
		}
	}
	if !found {
		t.Error("finding event not found in verifying group")
	}
}

// --- renderTreeTimeline tests ---

// TestRenderTreeTimeline_Empty verifies the empty state message when no groups.
func TestRenderTreeTimeline_Empty(t *testing.T) {
	out := renderTreeTimeline(nil, 80, 20, 0)
	if !containsStr(out, "no timeline events") {
		t.Errorf("empty: expected 'no timeline events', got %q", out)
	}

	// Also test with groups that have no events.
	out = renderTreeTimeline([]statusGroup{{Status: "pending", Events: nil}}, 80, 20, 0)
	if !containsStr(out, "no timeline events") {
		t.Errorf("no-events: expected 'no timeline events', got %q", out)
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

	out := renderTreeTimeline(groups, 80, 30, 0)

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
	out0 := renderTreeTimeline(groups, 80, 20, 0)
	for _, line := range strings.Split(out0, "\n") {
		if strings.Contains(line, "▸") {
			if !strings.Contains(line, "event-in-pending") {
				t.Errorf("cursor=0: cursor on wrong line: %q", line)
			}
		}
	}

	// cursor=1 → cursor indicator must be on the line containing "event-in-executing".
	out1 := renderTreeTimeline(groups, 80, 20, 1)
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

	out := renderTreeTimeline(groups, 80, 20, 0)

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
