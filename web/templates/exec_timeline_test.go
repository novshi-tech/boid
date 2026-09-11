package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/timeline"
)

func TestExecTimelineRowsInterleavesChildren(t *testing.T) {
	base := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	rows := execTimelineRows([]timeline.StatusGroup{{Status: "executing", EnteredAt: base, HasEnteredAt: true, Events: []timeline.Event{
		{Label: "progress", Time: base.Add(2 * time.Minute), HasTime: true},
		{Label: "running", Sticky: true},
	}}}, []ChildTreeNode{{Task: &orchestrator.Task{ID: "child", Status: orchestrator.TaskStatusExecuting}, CreatedAt: base.Add(time.Minute), HasCreatedAt: true, TerminalEvents: []ChildTerminalEvent{
		{Status: orchestrator.TaskStatusDone, Time: base.Add(3 * time.Minute), HasTime: true},
	}}, {Task: &orchestrator.Task{ID: "legacy"}}})
	if len(rows) != 6 {
		t.Fatalf("rows = %d, want 6", len(rows))
	}
	if !rows[0].Sticky || rows[1].Child == nil || rows[1].Child.Status != orchestrator.TaskStatusDone || rows[2].Event == nil || rows[2].Event.Label != "progress" || rows[3].Child == nil || rows[3].Child.Kind != "Created" || rows[4].Group == nil || rows[5].HasTime {
		t.Fatalf("unexpected merged timeline: %+v", rows)
	}
}

// Regression: completed children must not also appear in a Current subtasks list.
func TestExecTimelineCompletedChildrenHaveNoSeparateList(t *testing.T) {
	now := time.Now()
	children := []ChildTreeNode{}
	for _, id := range []string{"child-one", "child-two", "child-three"} {
		children = append(children, ChildTreeNode{Task: &orchestrator.Task{ID: id, Title: id, Status: orchestrator.TaskStatusDone}, CreatedAt: now.Add(-time.Hour), HasCreatedAt: true, TerminalEvents: []ChildTerminalEvent{{Status: orchestrator.TaskStatusDone, Time: now, HasTime: true}}})
	}
	var buf bytes.Buffer
	if err := TaskDetailTimelineSection(&orchestrator.Task{}, nil, children).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	if strings.Contains(html, "Current subtasks") || strings.Contains(html, "detail-child-tree") {
		t.Fatal("separate child list remains")
	}
	if strings.Count(html, "timeline-row-child") != 6 {
		t.Fatal("want only the three creation and three completion entries")
	}
	for _, child := range children {
		for _, kind := range []string{"Created", "Finished"} {
			if strings.Count(html, kind+": "+child.Task.Title) != 1 {
				t.Fatalf("child %s must have one %s entry", child.Task.ID, kind)
			}
		}
	}
}
