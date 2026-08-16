package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestDetailPrimaryAction_Working(t *testing.T) {
	task := &orchestrator.Task{Status: orchestrator.TaskStatusWorking}

	action, label := detailPrimaryAction(task, []string{"ready", "triage", "park"})
	if action != "ready" || label != "Go" {
		t.Fatalf("detailPrimaryAction(working) = (%q, %q), want (\"ready\", \"Go\")", action, label)
	}
}

func TestDetailPrimaryAction_WorkingWithoutReady(t *testing.T) {
	task := &orchestrator.Task{Status: orchestrator.TaskStatusWorking}

	action, label := detailPrimaryAction(task, []string{"park"})
	if action != "" || label != "" {
		t.Fatalf("detailPrimaryAction(working, no ready) = (%q, %q), want (\"\", \"\")", action, label)
	}
}

func TestTaskActionBar_WorkingHasTriageMenuItem(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Status: orchestrator.TaskStatusWorking}

	var buf bytes.Buffer
	if err := TaskActionBar(task, []string{"ready", "triage", "park"}, "timeline", false).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, `value="triage"`) {
		t.Error("working status action bar should contain a triage action menu item")
	}
	if !strings.Contains(html, `>triage<`) {
		t.Error("working status action bar should have a visible \"triage\" label")
	}
}

func TestTaskActionBar_WorkingWithoutTriageAction(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Status: orchestrator.TaskStatusWorking}

	var buf bytes.Buffer
	if err := TaskActionBar(task, []string{"park"}, "timeline", false).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if strings.Contains(html, `value="triage"`) {
		t.Error("triage menu item should not render when triage is not in availableActions")
	}
}

// TestTaskActionBar_CapturedTriageNotDuplicated guards against the menu's
// triage item repeating the primary "Triage" button that detailPrimaryAction
// already renders for captured (Opus review finding, 2026-08-16): the menu
// item is scoped to working only, so a captured card's single triage
// affordance stays the primary button.
func TestTaskActionBar_CapturedTriageNotDuplicated(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Status: orchestrator.TaskStatusCaptured}

	var buf bytes.Buffer
	if err := TaskActionBar(task, []string{"triage", "park"}, "timeline", false).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if strings.Count(html, `value="triage"`) != 1 {
		t.Errorf("captured status action bar should have exactly one triage action, got %d", strings.Count(html, `value="triage"`))
	}
}
