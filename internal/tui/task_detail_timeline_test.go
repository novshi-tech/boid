package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- buildTimeline tests ---

// TestTimelineFromEmpty verifies that buildTimeline handles nil and empty details gracefully.
func TestTimelineFromEmpty(t *testing.T) {
	// nil detail
	events := buildTimeline(nil)
	if len(events) != 0 {
		t.Errorf("nil detail: want 0 events, got %d", len(events))
	}

	// empty detail (no actions, no jobs, no task)
	events = buildTimeline(&api.TaskDetailView{})
	if len(events) != 0 {
		t.Errorf("empty detail: want 0 events, got %d", len(events))
	}

	// detail with task but empty payload
	events = buildTimeline(&api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:     "test",
			Status: orchestrator.TaskStatusPending,
		},
	})
	if len(events) != 0 {
		t.Errorf("detail with no actions/jobs/findings: want 0 events, got %d", len(events))
	}
}

// TestBuildTimeline_SortsEvents verifies that events are sorted by time ascending
// and no-time events (findings) appear at the bottom.
func TestBuildTimeline_SortsEvents(t *testing.T) {
	now := time.Now()
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:     "test",
			Status: orchestrator.TaskStatusVerifying,
			Payload: json.RawMessage(`{
				"verification": {
					"gate-a": {
						"findings": [{"message": "conflict", "status": "open"}]
					}
				}
			}`),
		},
		Actions: []*orchestrator.Action{
			{
				ID:        "a2",
				TaskID:    "test",
				Type:      "done",
				CreatedAt: now.Add(-1 * time.Minute),
			},
			{
				ID:        "a1",
				TaskID:    "test",
				Type:      "start",
				CreatedAt: now.Add(-3 * time.Minute),
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

	events := buildTimeline(detail)

	// Expect: a1 (start, -3m), j1 (job, -2m), a2 (done, -1m), then finding (no time)
	if len(events) < 4 {
		t.Fatalf("want at least 4 events, got %d", len(events))
	}

	// First three must be timed, last must be finding (no time)
	for i, ev := range events[:3] {
		if !ev.HasTime {
			t.Errorf("events[%d]: expected HasTime=true, got false", i)
		}
	}
	if events[len(events)-1].Kind != timelineKindFinding {
		t.Errorf("last event: expected finding, got %q", events[len(events)-1].Kind)
	}
	if events[len(events)-1].HasTime {
		t.Errorf("finding event: expected HasTime=false")
	}

	// Verify time ordering of timed events
	for i := 1; i < len(events); i++ {
		if !events[i].HasTime || !events[i-1].HasTime {
			break
		}
		if events[i].Time.Before(events[i-1].Time) {
			t.Errorf("events[%d].Time (%v) is before events[%d].Time (%v): not sorted ascending",
				i, events[i].Time, i-1, events[i-1].Time)
		}
	}

	// Verify kinds
	if events[0].Kind != timelineKindAction {
		t.Errorf("events[0].Kind: want %q, got %q", timelineKindAction, events[0].Kind)
	}
	if events[1].Kind != timelineKindJob {
		t.Errorf("events[1].Kind: want %q, got %q", timelineKindJob, events[1].Kind)
	}
	if events[2].Kind != timelineKindAction {
		t.Errorf("events[2].Kind: want %q, got %q", timelineKindAction, events[2].Kind)
	}
}

// TestBuildTimeline_JobEvents verifies job events have correct labels and Job pointers.
func TestBuildTimeline_JobEvents(t *testing.T) {
	now := time.Now()
	job := &api.Job{
		ID:        "job-1",
		Role:      "verifier",
		Status:    api.JobStatusCompleted,
		ExitCode:  0,
		CreatedAt: now.Add(-5 * time.Minute),
		UpdatedAt: now,
	}
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{ID: "t"},
		Jobs: []*api.Job{job},
	}

	events := buildTimeline(detail)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Kind != timelineKindJob {
		t.Errorf("kind: want %q, got %q", timelineKindJob, ev.Kind)
	}
	if ev.Job != job {
		t.Error("Job pointer should match the source job")
	}
	if !containsStr(ev.Label, "[verifier]") {
		t.Errorf("label: expected '[verifier]', got %q", ev.Label)
	}
	if !containsStr(ev.Label, "exit=0") {
		t.Errorf("label: expected 'exit=0', got %q", ev.Label)
	}
}

// TestBuildTimeline_FindingEvents verifies finding events are extracted correctly.
func TestBuildTimeline_FindingEvents(t *testing.T) {
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID: "t",
			Payload: json.RawMessage(`{
				"verification": {
					"my-gate": {
						"findings": [
							{"message": "err1", "status": "open"},
							{"message": "err2", "status": "resolved"}
						]
					}
				}
			}`),
		},
	}

	events := buildTimeline(detail)
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	for _, ev := range events {
		if ev.Kind != timelineKindFinding {
			t.Errorf("kind: want %q, got %q", timelineKindFinding, ev.Kind)
		}
		if ev.HasTime {
			t.Errorf("finding event should have HasTime=false")
		}
	}

	// Check resolved/open flags
	var openCount, resolvedCount int
	for _, ev := range events {
		if ev.Resolved {
			resolvedCount++
		} else {
			openCount++
		}
	}
	if openCount != 1 {
		t.Errorf("want 1 open finding, got %d", openCount)
	}
	if resolvedCount != 1 {
		t.Errorf("want 1 resolved finding, got %d", resolvedCount)
	}
}

// TestBuildTimeline_WorkspacePath verifies worktree path appears in Sub field.
func TestBuildTimeline_WorkspacePath(t *testing.T) {
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{ID: "t"},
		Jobs: []*api.Job{
			{
				ID:            "j1",
				Role:          "main",
				Status:        api.JobStatusRunning,
				WorkspacePath: "/path/to/worktree",
				CreatedAt:     time.Now(),
			},
		},
	}

	events := buildTimeline(detail)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if !containsStr(events[0].Sub, "/path/to/worktree") {
		t.Errorf("Sub: expected worktree path, got %q", events[0].Sub)
	}
}

// --- renderTimeline tests ---

// TestRenderTimeline_Empty verifies the empty state message.
func TestRenderTimeline_Empty(t *testing.T) {
	out := renderTimeline(nil, 80, 20, 0)
	if !containsStr(out, "no timeline events") {
		t.Errorf("empty: expected 'no timeline events', got %q", out)
	}
}

// TestRenderTimeline_ShowsEvents verifies events appear in rendered output.
func TestRenderTimeline_ShowsEvents(t *testing.T) {
	now := time.Now()
	events := []timelineEvent{
		{
			Time:    now.Add(-2 * time.Minute),
			Kind:    timelineKindAction,
			Label:   "start applied",
			HasTime: true,
		},
		{
			Time:    now.Add(-1 * time.Minute),
			Kind:    timelineKindJob,
			Label:   "[main] running 01:00",
			HasTime: true,
			Job:     &api.Job{ID: "j1", Status: api.JobStatusRunning},
		},
	}

	out := renderTimeline(events, 80, 20, 0)
	if !containsStr(out, "start applied") {
		t.Error("expected 'start applied' in rendered output")
	}
	if !containsStr(out, "[main] running 01:00") {
		t.Error("expected '[main] running 01:00' in rendered output")
	}
}

// TestRenderTimeline_CursorHighlight verifies the cursor indicator appears on the selected row.
func TestRenderTimeline_CursorHighlight(t *testing.T) {
	events := []timelineEvent{
		{Kind: timelineKindAction, Label: "first", HasTime: false},
		{Kind: timelineKindAction, Label: "second", HasTime: false},
	}

	// cursor on first row
	out := renderTimeline(events, 80, 20, 0)
	if !containsStr(out, "▸") {
		t.Error("cursor=0: expected cursor indicator '▸' in output")
	}
}

// TestRenderTimeline_FindingIcons verifies ! and ✓ icons for open/resolved findings.
func TestRenderTimeline_FindingIcons(t *testing.T) {
	events := []timelineEvent{
		{Kind: timelineKindFinding, Label: "[gate] err (open)", Resolved: false},
		{Kind: timelineKindFinding, Label: "[gate] ok (resolved)", Resolved: true},
	}

	out := renderTimeline(events, 80, 20, 0)
	if !containsStr(out, "!") {
		t.Error("expected '!' icon for open finding")
	}
	if !containsStr(out, "✓") {
		t.Error("expected '✓' icon for resolved finding")
	}
}

// TestRenderTimeline_NoTimeLabel verifies findings show "--:--:--" time placeholder.
func TestRenderTimeline_NoTimeLabel(t *testing.T) {
	events := []timelineEvent{
		{Kind: timelineKindFinding, Label: "[gate] err (open)", HasTime: false},
	}
	out := renderTimeline(events, 80, 20, 0)
	if !containsStr(out, "--:--:--") {
		t.Errorf("no-time event: expected '--:--:--', got %q", out)
	}
}

// TestRenderTimeline_ScrollWindow verifies that when there are more events than height,
// the visible window scrolls to keep the cursor in view.
func TestRenderTimeline_ScrollWindow(t *testing.T) {
	// Build 10 events
	events := make([]timelineEvent, 10)
	for i := range events {
		events[i] = timelineEvent{
			Kind:    timelineKindAction,
			Label:   fmt.Sprintf("event-%d", i),
			HasTime: false,
		}
	}

	height := 5

	// cursor=0 → first event visible
	out := renderTimeline(events, 80, height, 0)
	if !containsStr(out, "event-0") {
		t.Error("cursor=0: expected event-0 to be visible")
	}

	// cursor=9 (last) → last 5 events visible
	out = renderTimeline(events, 80, height, 9)
	if !containsStr(out, "event-9") {
		t.Error("cursor=9: expected event-9 to be visible")
	}
	if containsStr(out, "event-0") {
		t.Error("cursor=9: event-0 should not be visible when scrolled down")
	}
}

// --- userDrivenActionTypes tests ---

func TestUserDrivenActionTypes_KnownKeys(t *testing.T) {
	// These must all be classified as user-driven.
	known := []string{"start", "abort", "rerun", "done"}
	for _, k := range known {
		if !userDrivenActionTypes[k] {
			t.Errorf("userDrivenActionTypes[%q]: want true", k)
		}
	}
}

func TestUserDrivenActionTypes_InternalNotPresent(t *testing.T) {
	// These internal actions must NOT be user-driven.
	internal := []string{"hook_fired", "exit_gate_fired", "entry_gate_fired", "auto_advance"}
	for _, k := range internal {
		if userDrivenActionTypes[k] {
			t.Errorf("userDrivenActionTypes[%q]: want false (internal action)", k)
		}
	}
}

// --- overviewJobDuration tests ---

func TestOverviewJobDuration_Normal(t *testing.T) {
	now := time.Now()
	j := &api.Job{
		Status:    api.JobStatusCompleted,
		CreatedAt: now.Add(-2 * time.Minute),
		UpdatedAt: now,
	}
	dur := overviewJobDuration(j)
	if dur == "?" {
		t.Error("overviewJobDuration: expected non-? duration for normal job")
	}
}

func TestOverviewJobDuration_ZeroUpdatedAt(t *testing.T) {
	now := time.Now()
	j := &api.Job{
		Status:    api.JobStatusCompleted,
		CreatedAt: now.Add(-2 * time.Minute),
		// UpdatedAt zero
	}
	dur := overviewJobDuration(j)
	if dur != "?" {
		t.Errorf("overviewJobDuration with zero UpdatedAt: want '?', got %q", dur)
	}
}

func TestBuildOverviewJobLabel_Completed(t *testing.T) {
	now := time.Now()
	j := &api.Job{
		Role:      "gate",
		Status:    api.JobStatusCompleted,
		ExitCode:  0,
		CreatedAt: now.Add(-30 * time.Second),
		UpdatedAt: now,
	}
	label := buildOverviewJobLabel(j)
	if !containsStr(label, "[gate]") {
		t.Errorf("completed label: expected '[gate]', got %q", label)
	}
	if !containsStr(label, "✓") {
		t.Errorf("completed label: expected '✓', got %q", label)
	}
}

func TestBuildOverviewJobLabel_Failed(t *testing.T) {
	now := time.Now()
	j := &api.Job{
		Role:      "hook",
		Status:    api.JobStatusFailed,
		ExitCode:  1,
		CreatedAt: now.Add(-106 * time.Second),
		UpdatedAt: now,
	}
	label := buildOverviewJobLabel(j)
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
