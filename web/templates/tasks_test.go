package templates

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// card machine v2 (docs/plans/suggestion-as-state-transition.md §3.2):
// working's primary bottom-bar action is "done" (the forward edge once work
// is underway) — parked's is "go" (see TestApplyAction_CardTransitions_
// HumanCanApplyEveryEdge_NoSuggestion, internal/api, for the machine-level
// pin of the same edges these render buttons for).
func TestDetailPrimaryAction_Working(t *testing.T) {
	task := &orchestrator.Task{Status: orchestrator.TaskStatusWorking}

	action, label := detailPrimaryAction(task, []string{"park", "done"})
	if action != "done" || label != "Done" {
		t.Fatalf("detailPrimaryAction(working) = (%q, %q), want (\"done\", \"Done\")", action, label)
	}
}

func TestDetailPrimaryAction_WorkingWithoutDone(t *testing.T) {
	task := &orchestrator.Task{Status: orchestrator.TaskStatusWorking}

	action, label := detailPrimaryAction(task, []string{"park"})
	if action != "" || label != "" {
		t.Fatalf("detailPrimaryAction(working, no done) = (%q, %q), want (\"\", \"\")", action, label)
	}
}

// TestTaskActionBar_ParkedHasWorkingMenuItem: "working" (parked→working,
// card machine v2's lighter alternative to Go) only ever appears in
// availableActions for a PARKED task — see machine_card.go's rule table —
// so, unlike v1's "triage" item (which was gated on task.Status==Working),
// this menu item's gate is hasAction alone.
func TestTaskActionBar_ParkedHasWorkingMenuItem(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Status: orchestrator.TaskStatusParked}

	var buf bytes.Buffer
	if err := TaskActionBar(task, []string{"go", "working", "drop"}, "timeline", false).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, `value="working"`) {
		t.Error("parked status action bar should contain a working action menu item")
	}
	if !strings.Contains(html, `>working<`) {
		t.Error("parked status action bar should have a visible \"working\" label")
	}
}

func TestTaskActionBar_ParkedWithoutWorkingAction(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Status: orchestrator.TaskStatusParked}

	var buf bytes.Buffer
	if err := TaskActionBar(task, []string{"go", "drop"}, "timeline", false).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if strings.Contains(html, `value="working"`) {
		t.Error("working menu item should not render when working is not in availableActions")
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
		Verb:   "park",
		Action: "set aside for later",
		Reason: "source event fired",
		Basis:  "issue #42 reopened",
	}

	var buf bytes.Buffer
	if err := TaskDetailSuggestionSection("task-1", orchestrator.TaskStatusWorking, suggestion).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	for _, want := range []string{"badge-verb-park", "park", "set aside for later", "source event fired", "issue #42 reopened"} {
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
	if err := TaskDetailSuggestionSection("task-7", orchestrator.TaskStatusParked, suggestion).Render(context.Background(), &buf); err != nil {
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

// TestTaskDetailSuggestionSection_DoneAndDroppedStatus_ShowsAcceptRejectButtons
// pins PR #987 review's BLOCKER 3 fix: card machine v2's "answered" rule now
// reaches done/dropped (NewCardMachine's own doc comment) specifically so a
// suggestion khi legitimately places there — e.g. "reopen" — can actually be
// accepted or rejected. This INVERTS what used to be
// TestTaskDetailSuggestionSection_DoneStatus_HidesAcceptRejectButtons (Opus
// review finding #3, 2026-08-19 revisit of PR-3, back when v1's "answered"
// FromStatus set was {captured,triaged,parked,ready,working} and a
// done/dropped card's suggestion — reachable via the SAME I-5b service-layer
// guard that still lets attrs_set land there — could be displayed but never
// answered at all). This template only renders what CanApplyManualAction
// says; the machine-rule assertion itself is pinned in internal/orchestrator's
// TestCardMachineV2_CanApplyManualAction_Answered.
func TestTaskDetailSuggestionSection_DoneAndDroppedStatus_ShowsAcceptRejectButtons(t *testing.T) {
	suggestion := orchestrator.Suggestion{Verb: "reopen", Reason: "issue reopened", Basis: "issue #42"}

	for _, status := range []orchestrator.TaskStatus{orchestrator.TaskStatusDone, orchestrator.TaskStatusDropped} {
		var buf bytes.Buffer
		if err := TaskDetailSuggestionSection("task-9", status, suggestion).Render(context.Background(), &buf); err != nil {
			t.Fatalf("render (%s): %v", status, err)
		}
		html := buf.String()

		if !strings.Contains(html, "<form") {
			t.Errorf("%s task must render the answer form; got: %s", status, html)
		}
		if !strings.Contains(html, ">Accept<") || !strings.Contains(html, ">Reject<") {
			t.Errorf("%s task must render Accept/Reject buttons; got: %s", status, html)
		}
		for _, want := range []string{"reopen", "issue reopened", "issue #42"} {
			if !strings.Contains(html, want) {
				t.Errorf("%s task should show suggestion content %q; got: %s", status, want, html)
			}
		}
	}
}

// TestTaskDetailSuggestionSection_AbortedStatus_HidesAcceptRejectButtons pins
// that the gate still correctly hides the buttons for a status genuinely
// outside "answered"'s FromStatus set. aborted is not a status a real card
// ever reaches (card machine v2 has no rule targeting it at all — a card's
// only terminal statuses are done/dropped), so this is defense-in-depth
// coverage for "the gate still says no for something," not a realistic
// production scenario.
func TestTaskDetailSuggestionSection_AbortedStatus_HidesAcceptRejectButtons(t *testing.T) {
	suggestion := orchestrator.Suggestion{Verb: "go", Reason: "issue reopened", Basis: "issue #42"}

	var buf bytes.Buffer
	if err := TaskDetailSuggestionSection("task-9", orchestrator.TaskStatusAborted, suggestion).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if strings.Contains(html, "<form") {
		t.Errorf("aborted task must not render the answer form at all; got: %s", html)
	}
	if strings.Contains(html, ">Accept<") || strings.Contains(html, ">Reject<") {
		t.Errorf("aborted task must not render Accept/Reject buttons; got: %s", html)
	}
	// The suggestion's own content is still shown — only the buttons hide.
	for _, want := range []string{"go", "issue reopened", "issue #42"} {
		if !strings.Contains(html, want) {
			t.Errorf("aborted task should still show suggestion content %q; got: %s", want, html)
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

// awaitingTask builds a task parked on a question, optionally as someone's
// child.
func awaitingTask(t *testing.T, id, parentID, qid string) *orchestrator.Task {
	t.Helper()
	ap, err := json.Marshal(orchestrator.AwaitingPayload{Question: "send this?", QuestionID: qid})
	if err != nil {
		t.Fatalf("marshal awaiting: %v", err)
	}
	payload, err := json.Marshal(map[string]json.RawMessage{string(orchestrator.TraitAwaiting): ap})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return &orchestrator.Task{ID: id, ParentID: parentID, Status: orchestrator.TaskStatusAwaiting, Payload: payload}
}

// The banner is the only in-page route to the answer form, and it used to be
// suppressed for every child task (`ap.QuestionID != "" && task.ParentID ==
// ""`). That left a triage card's child answerable only by hand-assembling
// /tasks/<id>/questions/<qid> — which is what happened four times on
// 2026-08-24. The Q&A page and POST /tasks/{id}/answer never had the
// restriction, so only the affordance was missing.
func TestTaskDetailAwaitingBanner_ChildTaskLinksToItsQuestion(t *testing.T) {
	task := awaitingTask(t, "child-1", "parent-1", "q-1")

	var buf bytes.Buffer
	if err := TaskDetailAwaitingBanner(task).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, `href="/tasks/child-1/questions/q-1"`) {
		t.Errorf("child task banner missing its answer link, got:\n%s", got)
	}
}

func TestTaskDetailAwaitingBanner_RootTaskLinksToItsQuestion(t *testing.T) {
	task := awaitingTask(t, "root-1", "", "q-9")

	var buf bytes.Buffer
	if err := TaskDetailAwaitingBanner(task).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, `href="/tasks/root-1/questions/q-9"`) {
		t.Errorf("root task banner missing its answer link, got:\n%s", got)
	}
}

// No question id means there is no answer page to link to.
func TestTaskDetailAwaitingBanner_NoQuestionID_RendersNothing(t *testing.T) {
	task := &orchestrator.Task{ID: "child-1", ParentID: "parent-1", Status: orchestrator.TaskStatusAwaiting}

	var buf bytes.Buffer
	if err := TaskDetailAwaitingBanner(task).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "" {
		t.Errorf("banner without a question id should render nothing, got: %s", got)
	}
}
