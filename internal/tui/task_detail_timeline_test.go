package tui

import (
	"encoding/json"
	"fmt"
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
