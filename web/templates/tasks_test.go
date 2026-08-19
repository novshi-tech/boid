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

// TestTaskListFragment_EmptyOpenTab_ShowsCreateFirstTaskCopy pins the
// unchanged default (BD-8 残件2 must not touch the Open tab's behavior): an
// empty install with no filter still gets the "create your first task"
// invitation, not "nothing in this view".
func TestTaskListFragment_EmptyOpenTab_ShowsCreateFirstTaskCopy(t *testing.T) {
	filter := orchestrator.TaskFilter{Status: "open"}

	var buf bytes.Buffer
	if err := TaskListFragment(nil, filter, "/?status=open").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, "No tasks yet") {
		t.Errorf("expected the Open tab's empty-install copy, got: %s", html)
	}
	if !strings.Contains(html, `href="/tasks/new"`) {
		t.Errorf("expected the Create CTA on the empty Open tab, got: %s", html)
	}
}

// TestTaskListFragment_EmptyNonOpenTabs_ShowsViewSpecificCopy is the BD-8
// 残件2 regression: Closed / Queue(queue_next) / Parked are each their own
// status predicate, not "Open minus something" — an empty result on one of
// them means the view has nothing right now, not that the product has zero
// tasks. Before this fix, taskFilterActive (which never looks at
// filter.Status) alone picked the copy, so all three tabs rendered the same
// misleading "No tasks yet / Create your first task to get started" as a
// truly-empty install, complete with a Create CTA that has nothing to do
// with why the tab is empty.
func TestTaskListFragment_EmptyNonOpenTabs_ShowsViewSpecificCopy(t *testing.T) {
	for _, status := range []string{"closed", "queue_next", "parked"} {
		t.Run(status, func(t *testing.T) {
			filter := orchestrator.TaskFilter{Status: status}

			var buf bytes.Buffer
			if err := TaskListFragment(nil, filter, "/?status="+status).Render(context.Background(), &buf); err != nil {
				t.Fatalf("render: %v", err)
			}
			html := buf.String()

			if strings.Contains(html, "No tasks yet") {
				t.Errorf("status=%q must not show the empty-install copy, got: %s", status, html)
			}
			if strings.Contains(html, `href="/tasks/new"`) {
				t.Errorf("status=%q must not show the Create CTA (misleading — creating a task won't populate this view), got: %s", status, html)
			}
			if !strings.Contains(html, "Nothing in this view") {
				t.Errorf("status=%q should show view-specific empty copy, got: %s", status, html)
			}
		})
	}
}

// TestTaskListFragment_EmptyNonOpenTabWithFilter_ShowsFilterCopy pins the
// priority order when both conditions hold: an additional filter (e.g.
// search text) narrows a non-Open tab to zero results. The
// filter-narrowed-to-nothing copy (with its "clear filters" CTA) wins over
// the view-specific empty copy, since clearing the filter is the more
// specific/actionable next step.
func TestTaskListFragment_EmptyNonOpenTabWithFilter_ShowsFilterCopy(t *testing.T) {
	filter := orchestrator.TaskFilter{Status: "parked", Title: "no such task"}

	var buf bytes.Buffer
	if err := TaskListFragment(nil, filter, "/?status=parked&q=no+such+task").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, "No tasks match the current filters") {
		t.Errorf("expected the filter-narrowed-to-nothing copy to win, got: %s", html)
	}
	if strings.Contains(html, "Nothing in this view") {
		t.Errorf("view-specific copy should not show when an additional filter is also active, got: %s", html)
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
	if err := TaskDetailSuggestionSection("task-1", orchestrator.TaskStatusTriaged, suggestion).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	for _, want := range []string{"badge-verb-wake", "wake", "re-triage now", "source event fired", "issue #42 reopened"} {
		if !strings.Contains(html, want) {
			t.Errorf("suggestion section missing %q; got: %s", want, html)
		}
	}
}

// TestTaskDetailSuggestionSection_RendersAcceptRejectButtons pins the PR-3
// (B-3+B-4, J-6) accept/reject buttons: both POST to
// /tasks/<id>/suggestion, and both carry the suggestion's own verb/basis as
// hidden fields.
func TestTaskDetailSuggestionSection_RendersAcceptRejectButtons(t *testing.T) {
	suggestion := orchestrator.Suggestion{Verb: "go", Basis: "issue #42"}

	var buf bytes.Buffer
	if err := TaskDetailSuggestionSection("task-7", orchestrator.TaskStatusTriaged, suggestion).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, `action="/tasks/task-7/suggestion"`) {
		t.Errorf("suggestion answer form should POST to /tasks/task-7/suggestion; got: %s", html)
	}
	if !strings.Contains(html, `name="answer" value="accept"`) {
		t.Errorf("missing accept button's hidden answer field; got: %s", html)
	}
	if !strings.Contains(html, `name="answer" value="reject"`) {
		t.Errorf("missing reject button's hidden answer field; got: %s", html)
	}
	if !strings.Contains(html, `name="verb" value="go"`) {
		t.Errorf("missing hidden verb field carrying the suggestion's own verb; got: %s", html)
	}
	if !strings.Contains(html, `name="basis" value="issue #42"`) {
		t.Errorf("missing hidden basis field carrying the suggestion's own basis; got: %s", html)
	}
	if !strings.Contains(html, ">Accept<") || !strings.Contains(html, ">Reject<") {
		t.Errorf("missing Accept/Reject button labels; got: %s", html)
	}
}

// TestTaskDetailSuggestionSection_DoneStatus_HidesAcceptRejectButtons pins
// Opus review finding #3 (2026-08-19 revisit of PR-3): a triage task that
// carries a stale, never-answered suggestion into done (auto-done, or
// simply nobody clicked before it advanced) must NOT render clickable
// Accept/Reject buttons — the state machine rejects `answered` from done
// (TestDefaultMachine_TriageVocabulary_FromStatusEnumerated_NotWildcard),
// so clicking used to redirect to an opaque
// `no transition for action "answered" from status "done"` error. The
// suggestion's own text (verb/reason/basis) still renders — only the
// buttons are gated — so the historical record stays visible.
func TestTaskDetailSuggestionSection_DoneStatus_HidesAcceptRejectButtons(t *testing.T) {
	suggestion := orchestrator.Suggestion{Verb: "go", Reason: "issue reopened", Basis: "issue #42"}

	var buf bytes.Buffer
	if err := TaskDetailSuggestionSection("task-9", orchestrator.TaskStatusDone, suggestion).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if strings.Contains(html, "<form") {
		t.Errorf("done task must not render the answer form at all; got: %s", html)
	}
	if strings.Contains(html, ">Accept<") || strings.Contains(html, ">Reject<") {
		t.Errorf("done task must not render Accept/Reject buttons; got: %s", html)
	}
	// The suggestion's own content is still shown — only the buttons hide.
	for _, want := range []string{"go", "issue reopened", "issue #42"} {
		if !strings.Contains(html, want) {
			t.Errorf("done task should still show suggestion content %q; got: %s", want, html)
		}
	}
}

// TestTaskDetailSuggestionSection_EmptyRendersNothing pins the "no
// suggestion" case (the overwhelming majority of tasks): no card at all,
// not an empty one.
func TestTaskDetailSuggestionSection_EmptyRendersNothing(t *testing.T) {
	var buf bytes.Buffer
	if err := TaskDetailSuggestionSection("task-1", orchestrator.TaskStatusTriaged, orchestrator.Suggestion{}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if html := strings.TrimSpace(buf.String()); html != "" {
		t.Errorf("expected no output for an empty suggestion, got: %s", html)
	}
}

// TestTaskDetailSuggestionSection_UnknownVerb_StillRendersTextWithNeutralClass
// is the task-detail-page half of BD-8 残件4's regression (the queue/Parked
// row half is TestTaskTreeRow_UnknownVerb_StillRendersTextWithNeutralClass,
// web/templates/components/task_tree_test.go): an unrecognized verb must
// still render its literal text (rule 5, 隠さない) via
// components.VerbBadgeClass's shared fallback, not a "badge-verb-<word>"
// class with no matching CSS rule.
func TestTaskDetailSuggestionSection_UnknownVerb_StillRendersTextWithNeutralClass(t *testing.T) {
	suggestion := orchestrator.Suggestion{Verb: "mystery", Reason: "unclear"}

	var buf bytes.Buffer
	if err := TaskDetailSuggestionSection("task-1", orchestrator.TaskStatusTriaged, suggestion).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, "badge-verb-unknown") {
		t.Errorf("expected neutral badge-verb-unknown class, got: %s", html)
	}
	if !strings.Contains(html, ">mystery<") {
		t.Errorf("expected the unknown verb's literal text to still render, got: %s", html)
	}
}
