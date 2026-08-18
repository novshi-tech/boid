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

// TestTaskListContent_ParkedTabActive guards BD-8's Web UI fix: ?status=parked
// used to fall through TaskFilters' old else-branch and render Open as the
// active tab (there was no dedicated parked case at all — see
// components.statusTab / statusTabActive, filters.templ). Renders through
// TaskListContent (not components.TaskFilters directly) because that's the
// actual call path a browser hits (tasks.templ wraps components.TaskFilters).
func TestTaskListContent_ParkedTabActive(t *testing.T) {
	filter := orchestrator.TaskFilter{Status: "parked"}

	var buf bytes.Buffer
	if err := TaskListContent(nil, filter, nil, nil, "/?status=parked").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, `class="status-tab active" role="tab" aria-selected="true">Parked</button>`) {
		t.Error("Parked tab should be active for ?status=parked")
	}
	if strings.Contains(html, `class="status-tab active" role="tab" aria-selected="true">Open</button>`) {
		t.Error("Open tab should NOT be active for ?status=parked (this was the reported bug)")
	}
}

// TestTaskListContent_OpenTabActiveByDefault pins the unchanged default:
// no status (as web.go's TaskList normalizes an empty query param to
// "open" before this template ever sees it) still shows Open as active.
func TestTaskListContent_OpenTabActiveByDefault(t *testing.T) {
	filter := orchestrator.TaskFilter{Status: "open"}

	var buf bytes.Buffer
	if err := TaskListContent(nil, filter, nil, nil, "/?status=open").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, `class="status-tab active" role="tab" aria-selected="true">Open</button>`) {
		t.Error("Open tab should be active for ?status=open")
	}
	if strings.Contains(html, `class="status-tab active" role="tab" aria-selected="true">Parked</button>`) {
		t.Error("Parked tab should NOT be active for ?status=open")
	}
}

// TestTaskDetailSuggestionSection_RendersVerbActionReasonBasis pins the
// task detail page's suggestion card (BD-8 作るもの (C) 3): verb badge +
// action, then reason and basis on their own lines.
func TestTaskDetailSuggestionSection_RendersVerbActionReasonBasis(t *testing.T) {
	suggestion := orchestrator.Suggestion{
		Verb:   "wake",
		Action: "re-triage now",
		Reason: "source event fired",
		Basis:  "issue #42 reopened",
	}

	var buf bytes.Buffer
	if err := TaskDetailSuggestionSection(suggestion).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	for _, want := range []string{"badge-verb-wake", "wake", "re-triage now", "source event fired", "issue #42 reopened"} {
		if !strings.Contains(html, want) {
			t.Errorf("suggestion section missing %q; got: %s", want, html)
		}
	}
}

// TestTaskDetailSuggestionSection_EmptyRendersNothing pins the "no
// suggestion" case (the overwhelming majority of tasks): no card at all,
// not an empty one.
func TestTaskDetailSuggestionSection_EmptyRendersNothing(t *testing.T) {
	var buf bytes.Buffer
	if err := TaskDetailSuggestionSection(orchestrator.Suggestion{}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if html := strings.TrimSpace(buf.String()); html != "" {
		t.Errorf("expected no output for an empty suggestion, got: %s", html)
	}
}
