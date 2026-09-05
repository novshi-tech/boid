package templates

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// card machine v2: working's primary bottom-bar action is "complete" (the
// forward edge once work is underway) — parked's is "go" (see
// TestApplyAction_CardTransitions_HumanCanApplyEveryEdge_NoSuggestion,
// internal/api, for the machine-level pin of the same edges these render
// buttons for).
func TestDetailPrimaryAction_Working(t *testing.T) {
	task := &orchestrator.Task{Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}

	action, label := detailPrimaryAction(task, []string{"park", "complete"})
	if action != "complete" || label != "Complete" {
		t.Fatalf("detailPrimaryAction(working) = (%q, %q), want (\"complete\", \"Complete\")", action, label)
	}
}

func TestDetailPrimaryAction_WorkingWithoutComplete(t *testing.T) {
	task := &orchestrator.Task{Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}

	action, label := detailPrimaryAction(task, []string{"park"})
	if action != "" || label != "" {
		t.Fatalf("detailPrimaryAction(working, no complete) = (%q, %q), want (\"\", \"\")", action, label)
	}
}

// TestTaskCardActionBar_ParkedHasStartMenuItem: "start" (parked→working,
// card machine v2's lighter alternative to Go) only ever appears in
// availableActions for a PARKED task — see machine_card.go's rule table —
// so this menu item's gate is hasAction alone.
func TestTaskCardActionBar_ParkedHasStartMenuItem(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}

	var buf bytes.Buffer
	if err := TaskCardActionBar(task, []string{"go", "start", "drop"}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, `value="start"`) {
		t.Error("parked status action bar should contain a start action menu item")
	}
	if !strings.Contains(html, `>start<`) {
		t.Error("parked status action bar should have a visible \"start\" label")
	}
}

func TestTaskCardActionBar_ParkedWithoutStartAction(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}

	var buf bytes.Buffer
	if err := TaskCardActionBar(task, []string{"go", "drop"}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if strings.Contains(html, `value="start"`) {
		t.Error("start menu item should not render when start is not in availableActions")
	}
}

// TestTaskCardActionBar_ParkedHasCompleteMenuItem pins the human half of
// card machine v2's eighth edge (complete: parked→done). The machine rule
// alone is not enough to satisfy the design's escape hatch ("a human's
// direct action must always be able to reach every edge"): this kebab menu
// enumerates its items verb by verb rather than looping over
// availableActions, so a rule with no matching item here is reachable from
// the CLI only. parked's PRIMARY button stays Go — closing a card is never
// the default move — so complete lives in the menu.
func TestTaskCardActionBar_ParkedHasCompleteMenuItem(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}

	var buf bytes.Buffer
	if err := TaskCardActionBar(task, []string{"go", "start", "drop", "complete"}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, `value="complete"`) {
		t.Error("parked status action bar should contain a complete action menu item")
	}
	if !strings.Contains(html, `>complete<`) {
		t.Error("parked status action bar should have a visible \"complete\" label")
	}
	// The primary slot is unchanged: Go, not Complete.
	if !strings.Contains(html, `>Go<`) {
		t.Errorf("parked's primary button must still be Go; got: %s", html)
	}
}

func TestTaskCardActionBar_ParkedWithoutCompleteAction(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}

	var buf bytes.Buffer
	if err := TaskCardActionBar(task, []string{"go", "drop"}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), `value="complete"`) {
		t.Error("complete menu item should not render when complete is not in availableActions")
	}
}

// TestTaskCardActionBar_WorkingDoesNotDuplicateComplete is the negative twin
// of TestTaskCardActionBar_ParkedHasCompleteMenuItem: on a WORKING card,
// complete is already the primary bottom-bar button (detailPrimaryAction),
// so the menu item must stay out of the way — exactly one complete form in
// the whole bar, not two.
func TestTaskCardActionBar_WorkingDoesNotDuplicateComplete(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}

	var buf bytes.Buffer
	if err := TaskCardActionBar(task, []string{"park", "complete"}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := strings.Count(buf.String(), `value="complete"`); got != 1 {
		t.Errorf("working card renders %d complete forms, want exactly 1 (the primary button)", got)
	}
}

// TestTaskCardActionBar_WorkingHasGoMenuItem pins the working→working
// self-loop's own UI affordance: unlike every other kebab item in this bar,
// "go" here is gated on status alone (AvailableActions excludes self-loops,
// so hasAction(availableActions, "go") would never be true for working).
func TestTaskCardActionBar_WorkingHasGoMenuItem(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}

	var buf bytes.Buffer
	if err := TaskCardActionBar(task, []string{"park", "complete"}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, `value="go"`) {
		t.Error("working status action bar should contain a go action menu item")
	}
}

// TestTaskCardActionBar_ParkedDoesNotDuplicateGo confirms the working-only
// go menu item does not also render on parked, where Go is already the
// primary bottom-bar button.
func TestTaskCardActionBar_ParkedDoesNotDuplicateGo(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}

	var buf bytes.Buffer
	if err := TaskCardActionBar(task, []string{"go", "start", "drop"}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := strings.Count(buf.String(), `value="go"`); got != 1 {
		t.Errorf("parked card renders %d go forms, want exactly 1 (the primary button)", got)
	}
}

// TestTaskFilters_NoStatusTabsInList mirrors components' own
// TestTaskFilters_NoStatusTabs at the page-content level (docs/plans/
// webui-detail-list-redesign.md PR-4, §3.5): the old Open/Closed/Queue/
// Parked status tabs (components.statusTab/statusTabActive, filters.templ)
// are gone — TaskListContent renders the active-only toggle instead, no
// tabs at all regardless of filter.Status.
func TestTaskFilters_NoStatusTabsInList(t *testing.T) {
	filter := orchestrator.TaskFilter{Status: "parked"}

	var buf bytes.Buffer
	if err := TaskListContent(nil, filter, 1, false, nil, nil, "/?status=parked").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if strings.Contains(html, "status-tab") {
		t.Errorf("expected no status-tab markup (removed by PR-4), got: %s", html)
	}
	if !strings.Contains(html, `name="active"`) {
		t.Errorf("expected the active-only toggle to render, got: %s", html)
	}
}

// TestTaskListFragment_EmptyDefaultView_ShowsCreateFirstTaskCopy pins the
// unchanged default-empty-install experience: no filter at all (the new
// default — 全状態表示, no more "open" tab) on page 1 still gets the "create
// your first task" invitation.
func TestTaskListFragment_EmptyDefaultView_ShowsCreateFirstTaskCopy(t *testing.T) {
	var buf bytes.Buffer
	if err := TaskListFragment(nil, orchestrator.TaskFilter{}, 1, false, "/").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, "No tasks yet") {
		t.Errorf("expected the empty-install copy, got: %s", html)
	}
	if !strings.Contains(html, `href="/tasks/new"`) {
		t.Errorf("expected the Create CTA on the empty default view, got: %s", html)
	}
}

// TestTaskListFragment_EmptyWithFilter_ShowsFilterCopy pins the PR-4
// generalization of the old per-tab empty copy: ANY narrowing filter
// (status, active-only, search, project, ...) that returns zero rows shows
// the "no tasks match the current filters" copy with a Clear filters CTA —
// there is no more per-tab "nothing in this view" branch, because there are
// no more tabs. taskFilterActive (tasks.templ) now folds Status/ActiveOnly
// into the same check as Title/ProjectID/etc.
func TestTaskListFragment_EmptyWithFilter_ShowsFilterCopy(t *testing.T) {
	cases := []struct {
		name   string
		filter orchestrator.TaskFilter
	}{
		{"status", orchestrator.TaskFilter{Status: "parked"}},
		{"active_only", orchestrator.TaskFilter{ActiveOnly: true}},
		{"title_search", orchestrator.TaskFilter{Title: "no such task"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TaskListFragment(nil, c.filter, 1, false, "/?q=no+such+task").Render(context.Background(), &buf); err != nil {
				t.Fatalf("render: %v", err)
			}
			html := buf.String()

			if !strings.Contains(html, "No tasks match the current filters") {
				t.Errorf("expected the filter-narrowed-to-nothing copy, got: %s", html)
			}
			if strings.Contains(html, "No tasks yet") {
				t.Errorf("must not show the empty-install copy when a filter is active, got: %s", html)
			}
		})
	}
}

// TestTaskListFragment_EmptyPastPage1_ShowsGoBackCopy pins the pagination
// boundary's empty state (§3.5, §5 論点4): landing on page 2+ with zero rows
// (the dataset shrank, or the caller typo'd ?page=) shows a "go back" hint,
// not the misleading "create your first task" install copy.
func TestTaskListFragment_EmptyPastPage1_ShowsGoBackCopy(t *testing.T) {
	var buf bytes.Buffer
	if err := TaskListFragment(nil, orchestrator.TaskFilter{}, 2, false, "/?page=2").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, "Nothing on this page") {
		t.Errorf("expected the past-page-1 empty copy, got: %s", html)
	}
	if strings.Contains(html, "No tasks yet") || strings.Contains(html, "No tasks match the current filters") {
		t.Errorf("must not show the page-1 empty copies when page > 1, got: %s", html)
	}
}

// TestTaskListPagination_RendersPrevAndNext pins the Prev/Next nav's
// visibility rules: Prev only past page 1, Next only when hasMore.
func TestTaskListPagination_RendersPrevAndNext(t *testing.T) {
	rows := []ListRow{{Task: &orchestrator.Task{ID: "t1", Title: "t1", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}}}}

	var buf bytes.Buffer
	if err := TaskListFragment(rows, orchestrator.TaskFilter{}, 2, true, "/?page=2").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, "← Prev") {
		t.Errorf("expected a Prev link on page 2, got: %s", html)
	}
	if !strings.Contains(html, "Next →") {
		t.Errorf("expected a Next link when hasMore is true, got: %s", html)
	}
	// page 1 is the canonical/bookmarkable URL with no "page" param at all
	// (pageURL's own canonicalization — TestPageURL_SetsAndRemovesPageParam
	// pins this directly), so the Prev link (page 2 - 1 = page 1) targets a
	// bare "/", not "?page=1".
	if !strings.Contains(html, `hx-get="/"`) {
		t.Errorf("Prev link should point at / (page 1, no page param), got: %s", html)
	}
	if !strings.Contains(html, "page=3") {
		t.Errorf("Next link should point at page=3, got: %s", html)
	}
}

// TestTaskListPagination_Page1NoMore_RendersNeitherLink pins the common
// case: a single page of results shows no pagination nav at all.
func TestTaskListPagination_Page1NoMore_RendersNeitherLink(t *testing.T) {
	rows := []ListRow{{Task: &orchestrator.Task{ID: "t1", Title: "t1", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}}}}

	var buf bytes.Buffer
	if err := TaskListFragment(rows, orchestrator.TaskFilter{}, 1, false, "/").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if strings.Contains(html, "list-pagination") {
		t.Errorf("expected no pagination nav on a single-page result, got: %s", html)
	}
}

// TestPageURL_SetsAndRemovesPageParam pins pageURL's canonicalization: page
// 1 removes the "page" param entirely (a clean, bookmarkable URL), any other
// page sets it.
func TestPageURL_SetsAndRemovesPageParam(t *testing.T) {
	if got := pageURL("/?status=open&page=3", 1); strings.Contains(got, "page=") {
		t.Errorf("pageURL(..., 1) = %q, want no page param", got)
	}
	if got := pageURL("/?status=open", 2); !strings.Contains(got, "page=2") {
		t.Errorf("pageURL(..., 2) = %q, want page=2", got)
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
	if !strings.Contains(html, ">Accept: go<") || !strings.Contains(html, ">Reject<") {
		t.Errorf("missing Accept/Reject button labels; got: %s", html)
	}
}

// TestTaskDetailSuggestionSection_GoVerb_AcceptButtonMatchesPrimaryGoWeight
// pins PR-2's UI requirement (docs/plans/suggestion-as-state-transition-impl.md
// §4.3, design doc §3.2): accept(go) actually dispatches specced children and
// starts real work, the same consequence as clicking the bottom action bar's
// own primary "Go" button (actionPrimaryClass(action)'s "btn btn-primary" for
// action=="go", tasks.templ) — so accepting a "go" suggestion must look as
// weighty as that button, not like the compact "read and dismiss" Accept
// used for every other verb. Asserted as "carries btn-primary but NOT
// btn-sm" rather than an exact string match, so this stays robust to any
// future class reordering.
func TestTaskDetailSuggestionSection_GoVerb_AcceptButtonMatchesPrimaryGoWeight(t *testing.T) {
	suggestion := orchestrator.Suggestion{Verb: "go", Reason: "children are specced"}

	var buf bytes.Buffer
	if err := TaskDetailSuggestionSection("task-1", orchestrator.TaskStatusParked, suggestion).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	acceptButton := extractAcceptButtonTag(t, html, "go")
	if !strings.Contains(acceptButton, "btn-primary") {
		t.Errorf("go accept button must carry btn-primary; got: %s", acceptButton)
	}
	if strings.Contains(acceptButton, "btn-sm") {
		t.Errorf("go accept button must NOT be the compact btn-sm variant (must match the primary Go button's weight); got: %s", acceptButton)
	}
}

// verbApplicableStatus names ONE valid FromStatus per verb (machine_card.go's
// rule table — go/start/drop only from parked, park only from working,
// complete from parked OR working, reopen from done or dropped). PR-3 added
// components.SuggestionInapplicable, which hides the Accept button entirely
// when a suggestion's verb doesn't match the task's CURRENT status — so
// every render test below that wants to see a LIVE Accept button (as opposed
// to the dedicated inapplicable-message tests, further down) must pass a
// status the verb actually applies from, not an arbitrary card status.
// Verbs with more than one valid FromStatus (go, complete, reopen) just
// pick one here; cardVerbApplicableStatuses (further down) is the
// exhaustive twin.
var verbApplicableStatus = map[string]orchestrator.TaskStatus{
	"go":       orchestrator.TaskStatusParked,
	"start":    orchestrator.TaskStatusParked,
	"drop":     orchestrator.TaskStatusParked,
	"park":     orchestrator.TaskStatusWorking,
	"complete": orchestrator.TaskStatusWorking,
	"reopen":   orchestrator.TaskStatusDone,
}

// TestTaskDetailSuggestionSection_NonGoVerb_AcceptButtonStaysCompact is the
// negative twin: every non-go, non-drop verb's accept keeps the original
// compact styling — only "go" carries the "this dispatches real work"
// weight and "drop" carries the danger weight (its own dedicated test,
// TestTaskDetailSuggestionSection_DropVerb_AcceptButtonIsDangerAndConfirms,
// PR #988 review MEDIUM 1) — start/park/complete/reopen suggestions never
// task-ify or release identities on accept.
func TestTaskDetailSuggestionSection_NonGoVerb_AcceptButtonStaysCompact(t *testing.T) {
	for _, verb := range []string{"start", "park", "complete", "reopen"} {
		suggestion := orchestrator.Suggestion{Verb: verb}
		var buf bytes.Buffer
		if err := TaskDetailSuggestionSection("task-1", verbApplicableStatus[verb], suggestion).Render(context.Background(), &buf); err != nil {
			t.Fatalf("render (%s): %v", verb, err)
		}
		html := buf.String()
		acceptButton := extractAcceptButtonTag(t, html, verb)
		if !strings.Contains(acceptButton, "btn-sm") {
			t.Errorf("verb=%s: accept button should stay compact (btn-sm); got: %s", verb, acceptButton)
		}
		if strings.Contains(acceptButton, "btn-danger") {
			t.Errorf("verb=%s: accept button must not be danger-colored; got: %s", verb, acceptButton)
		}
	}
}

// TestTaskDetailSuggestionSection_DropVerb_AcceptButtonIsDangerAndConfirms
// pins PR #988 review's MEDIUM 1 fix: accept("drop") is a second entry
// point to the SAME identity-releasing transition (tx.UnlinkAllForTask) the
// kebab menu's direct "drop" item reaches — that direct item is
// danger-colored (action-menu-item-danger) and gated behind
// confirm('Drop this task? This discards it — a dropped task does not
// resume.') (TaskActionBar, this file). Before this fix, accept("drop") was
// the SAME compact, confirm-less "btn btn-primary btn-sm" every other verb
// used — the most destructive of the six verbs rendered with the least
// friction of all of them.
func TestTaskDetailSuggestionSection_DropVerb_AcceptButtonIsDangerAndConfirms(t *testing.T) {
	suggestion := orchestrator.Suggestion{Verb: "drop", Reason: "duplicate of #41"}

	var buf bytes.Buffer
	if err := TaskDetailSuggestionSection("task-1", orchestrator.TaskStatusParked, suggestion).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	acceptButton := extractAcceptButtonTag(t, html, "drop")
	if !strings.Contains(acceptButton, "btn-danger") {
		t.Errorf("drop accept button must be danger-colored; got: %s", acceptButton)
	}

	acceptForm := extractAcceptFormTag(t, html, "drop")
	wantConfirm := `confirm('Drop this task? This discards it — a dropped task does not resume.')`
	if !strings.Contains(acceptForm, wantConfirm) {
		t.Errorf("drop accept form must confirm with the SAME text as the direct kebab drop item; got form: %s", acceptForm)
	}
}

// TestTaskDetailSuggestionSection_ParkVerb_AcceptButtonConfirms is drop's
// twin for "park": the kebab menu's direct "park" item is gated behind
// confirm('Park this task (set aside for later)?') (TaskActionBar, this
// file) but is NOT danger-colored (park is reversible — reopen/wake bring
// the card back). accept("park") must match both halves: same confirm text,
// still non-danger styling.
func TestTaskDetailSuggestionSection_ParkVerb_AcceptButtonConfirms(t *testing.T) {
	suggestion := orchestrator.Suggestion{Verb: "park", Reason: "waiting on upstream"}

	var buf bytes.Buffer
	if err := TaskDetailSuggestionSection("task-1", orchestrator.TaskStatusWorking, suggestion).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	acceptButton := extractAcceptButtonTag(t, html, "park")
	if strings.Contains(acceptButton, "btn-danger") {
		t.Errorf("park accept button must NOT be danger-colored (park is reversible); got: %s", acceptButton)
	}

	acceptForm := extractAcceptFormTag(t, html, "park")
	wantConfirm := `confirm('Park this task (set aside for later)?')`
	if !strings.Contains(acceptForm, wantConfirm) {
		t.Errorf("park accept form must confirm with the SAME text as the direct kebab park item; got form: %s", acceptForm)
	}
}

// TestTaskDetailSuggestionSection_NonConfirmingVerbs_NoConfirmDialog is the
// negative twin of the two tests above: go/start/complete/reopen's direct
// kebab/action-bar counterparts have no confirm() dialog either (only
// abort/rerun/drop/park/delete do, none of which apply here), so their
// accept forms must not gain one just because they arrived via a
// suggestion.
func TestTaskDetailSuggestionSection_NonConfirmingVerbs_NoConfirmDialog(t *testing.T) {
	for _, verb := range []string{"go", "start", "complete", "reopen"} {
		suggestion := orchestrator.Suggestion{Verb: verb}
		var buf bytes.Buffer
		if err := TaskDetailSuggestionSection("task-1", verbApplicableStatus[verb], suggestion).Render(context.Background(), &buf); err != nil {
			t.Fatalf("render (%s): %v", verb, err)
		}
		html := buf.String()
		acceptForm := extractAcceptFormTag(t, html, verb)
		if strings.Contains(acceptForm, "confirm(") {
			t.Errorf("verb=%s: accept form must not confirm; got form: %s", verb, acceptForm)
		}
	}
}

// extractAcceptButtonTag pulls out the <button ...>Accept: <verb></button>
// tag from rendered HTML, for asserting on its class attribute specifically
// (as opposed to the Reject button, which sits right next to it in the same
// markup).
func extractAcceptButtonTag(t *testing.T, html, verb string) string {
	t.Helper()
	label := ">Accept: " + verb + "<"
	idx := strings.Index(html, label)
	if idx == -1 {
		t.Fatalf("no Accept button found for verb %q in: %s", verb, html)
	}
	start := strings.LastIndex(html[:idx], "<button")
	if start == -1 {
		t.Fatalf("no opening <button tag before Accept in: %s", html)
	}
	return html[start : idx+len(label)]
}

// extractAcceptFormTag pulls out the <form ...>...</form> block that wraps
// the accept button for the given verb, for asserting on the FORM's
// onsubmit attribute (the confirm() dialog lives there, not on the button).
func extractAcceptFormTag(t *testing.T, html, verb string) string {
	t.Helper()
	label := ">Accept: " + verb + "<"
	labelIdx := strings.Index(html, label)
	if labelIdx == -1 {
		t.Fatalf("no Accept button found for verb %q in: %s", verb, html)
	}
	start := strings.LastIndex(html[:labelIdx], "<form")
	if start == -1 {
		t.Fatalf("no opening <form tag before Accept button in: %s", html)
	}
	end := strings.Index(html[labelIdx:], "</form>")
	if end == -1 {
		t.Fatalf("no closing </form> tag after Accept button in: %s", html)
	}
	return html[start : labelIdx+end+len("</form>")]
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
		if !strings.Contains(html, ">Accept: reopen<") || !strings.Contains(html, ">Reject<") {
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
	if strings.Contains(html, ">Accept: go<") || strings.Contains(html, ">Reject<") {
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
	// card-model-cleanup PR-2: TaskStatusTriaged no longer exists (design doc
	// §3.3, folded into TaskStatusParked well before this PR) — an empty
	// suggestion renders nothing regardless of status, so any valid status
	// pins the same behavior.
	var buf bytes.Buffer
	if err := TaskDetailSuggestionSection("task-1", orchestrator.TaskStatusParked, orchestrator.Suggestion{}).Render(context.Background(), &buf); err != nil {
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

	// card-model-cleanup PR-2: TaskStatusTriaged no longer exists (design doc
	// §3.3) — TaskStatusParked is the current card status this scenario maps
	// to (the verb badge itself renders unconditionally, before the
	// CanApplyManualAction gate, so the exact status doesn't change what this
	// test asserts).
	var buf bytes.Buffer
	if err := TaskDetailSuggestionSection("task-1", orchestrator.TaskStatusParked, suggestion).Render(context.Background(), &buf); err != nil {
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

// ---- PR-3 (suggestion 状態遷移化 follow-up): 適用不能な suggestion の防御 ----
//
// cardVerbApplicableStatuses is every status a verb's own card-machine rule
// (machine_card.go) actually fires from — the exhaustive twin of
// verbApplicableStatus above (which only names ONE status per verb; "go"
// and "complete" each have two, and "reopen" has two, done AND dropped).
var cardVerbApplicableStatuses = map[string]map[orchestrator.TaskStatus]bool{
	"go":       {orchestrator.TaskStatusParked: true, orchestrator.TaskStatusWorking: true},
	"start":    {orchestrator.TaskStatusParked: true},
	"drop":     {orchestrator.TaskStatusParked: true},
	"park":     {orchestrator.TaskStatusWorking: true},
	"complete": {orchestrator.TaskStatusParked: true, orchestrator.TaskStatusWorking: true},
	"reopen":   {orchestrator.TaskStatusDone: true, orchestrator.TaskStatusDropped: true},
}

// TestTaskDetailSuggestionSection_InapplicableVerb_HidesAcceptShowsMessageAndReject
// is the fix's core render pin: accept("reopen") on a card that is currently
// "parked" (reopen only fires from done/dropped — machine_card.go) used to
// render a live Accept button that 409'd on click with an opaque `no
// transition for action "reopen" from status "parked"`. Now: no Accept
// button, a plain-language explanation naming both the verb and the status,
// and Reject still present (an inapplicable suggestion must remain
// dismissible — this PR's own description covers why the queue predicate
// itself keeps showing it).
//
// The verb was "done" until 2026-08-25, when card machine v2 gained its
// eighth edge (done: parked→done) and that combination became APPLICABLE —
// see NewCardMachine's own doc comment. "reopen" on parked is the
// replacement because verb and status stay textually distinct there (unlike
// "park" on "parked", where a Contains check for the verb would pass on the
// status string alone and prove nothing).
func TestTaskDetailSuggestionSection_InapplicableVerb_HidesAcceptShowsMessageAndReject(t *testing.T) {
	suggestion := orchestrator.Suggestion{Verb: "reopen", Reason: "the source came back"}

	var buf bytes.Buffer
	if err := TaskDetailSuggestionSection("task-1", orchestrator.TaskStatusParked, suggestion).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if strings.Contains(html, ">Accept: reopen<") {
		t.Errorf("Accept button must not render for an inapplicable verb/status combo; got: %s", html)
	}
	if !strings.Contains(html, "reopen") || !strings.Contains(html, "parked") {
		t.Errorf("inapplicable message should name the verb and the status; got: %s", html)
	}
	// Review LOW 4: a bare "cannot be applied" is not enough — the message
	// must also say what CAN be applied instead, same as the API's 409
	// (both now pull from orchestrator.StateMachine.AvailableActionsHint).
	for _, want := range []string{"go", "start", "drop", "complete"} {
		if !strings.Contains(html, want) {
			t.Errorf("inapplicable message should name every action available from parked (%q missing); got: %s", want, html)
		}
	}
	if !strings.Contains(html, `name="answer" value="reject"`) || !strings.Contains(html, ">Reject<") {
		t.Errorf("Reject must still render for an inapplicable suggestion (it must remain dismissible); got: %s", html)
	}
	// The suggestion's own content (badge/reason) still renders regardless —
	// only the Accept affordance is what's gated.
	if !strings.Contains(html, "badge-verb-reopen") || !strings.Contains(html, "the source came back") {
		t.Errorf("suggestion content should still render even when inapplicable; got: %s", html)
	}
}

// TestTaskDetailSuggestionSection_AllVerbStatusCombinations_AcceptOnlyWhenApplicable
// is the exhaustive 6-verb × 4-status (24 combination) render-level twin of
// orchestrator.TestCardMachineV2_CanApplyTransitionAction_PinsExactlyEightEdges:
// Accept renders for exactly the 8 applicable combinations,
// never for the other 16 — and Reject renders for ALL 24, since an
// inapplicable suggestion must still be dismissible.
func TestTaskDetailSuggestionSection_AllVerbStatusCombinations_AcceptOnlyWhenApplicable(t *testing.T) {
	verbs := []string{"go", "start", "park", "drop", "complete", "reopen"}
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusWorking,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusDropped,
	}

	for _, verb := range verbs {
		for _, status := range statuses {
			suggestion := orchestrator.Suggestion{Verb: verb}
			var buf bytes.Buffer
			if err := TaskDetailSuggestionSection("task-1", status, suggestion).Render(context.Background(), &buf); err != nil {
				t.Fatalf("render (verb=%s status=%s): %v", verb, status, err)
			}
			html := buf.String()

			wantAccept := cardVerbApplicableStatuses[verb][status]
			hasAccept := strings.Contains(html, ">Accept: "+verb+"<")
			if hasAccept != wantAccept {
				t.Errorf("verb=%s status=%s: Accept button present=%v, want %v", verb, status, hasAccept, wantAccept)
			}
			if !strings.Contains(html, `name="answer" value="reject"`) {
				t.Errorf("verb=%s status=%s: Reject must always render; got: %s", verb, status, html)
			}
			if !wantAccept && !strings.Contains(html, "detail-suggestion-inapplicable") {
				t.Errorf("verb=%s status=%s: expected an inapplicable message in place of Accept; got: %s", verb, status, html)
			}
		}
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
	// card-model-cleanup PR-2: Payload moved to Exec.Payload, and "awaiting"
	// is an Execution-only status (design doc §3.3).
	return &orchestrator.Task{
		ID:       id,
		Type:     orchestrator.TaskTypeExecution,
		ParentID: parentID,
		Status:   orchestrator.TaskStatusAwaiting,
		Exec:     &orchestrator.ExecAttrs{Payload: payload},
	}
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
	// Exec left nil deliberately: this exercises taskExecPayload's nil-Exec
	// path (card-model-cleanup PR-2), matching the pre-refactor fixture's
	// unset Payload.
	task := &orchestrator.Task{ID: "child-1", Type: orchestrator.TaskTypeExecution, ParentID: "parent-1", Status: orchestrator.TaskStatusAwaiting}

	var buf bytes.Buffer
	if err := TaskDetailAwaitingBanner(task).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "" {
		t.Errorf("banner without a question id should render nothing, got: %s", got)
	}
}

// TestCardDescriptionOpenByDefault_ByteLengthGuard pins Opus review finding
// N2 (PR #996, non-blocking but folded into the B1 fix commit): the
// original implementation collapsed only on line count
// (cardDescriptionCollapseThresholdLines), which missed exactly the case
// its own doc comment calls out — an ingested description can run up to
// 64KiB with NO newlines at all (a single huge line, or pasted text with no
// line breaks), and would have rendered fully expanded regardless of size.
func TestCardDescriptionOpenByDefault_ByteLengthGuard(t *testing.T) {
	longSingleLine := strings.Repeat("x", cardDescriptionCollapseThresholdBytes+1)
	if strings.Contains(longSingleLine, "\n") {
		t.Fatal("test fixture must have zero newlines to actually exercise the byte-length guard")
	}
	if got := cardDescriptionOpenByDefault(longSingleLine); got {
		t.Errorf("a %d-byte single-line description should collapse by default (byte guard), got open=%v", len(longSingleLine), got)
	}
}

func TestCardDescriptionOpenByDefault_ShortSingleLine_StaysOpen(t *testing.T) {
	short := "a short single-line description"
	if got := cardDescriptionOpenByDefault(short); !got {
		t.Errorf("a short single-line description should stay open by default, got open=%v", got)
	}
}

func TestCardDescriptionOpenByDefault_ManyShortLines_Collapses(t *testing.T) {
	manyLines := strings.Repeat("line\n", cardDescriptionCollapseThresholdLines+1)
	if len(manyLines) >= cardDescriptionCollapseThresholdBytes {
		t.Fatal("test fixture must stay under the byte threshold to actually exercise the line-count guard")
	}
	if got := cardDescriptionOpenByDefault(manyLines); got {
		t.Errorf("a description with more than %d lines should collapse by default (line guard), got open=%v", cardDescriptionCollapseThresholdLines, got)
	}
}

func TestCardDescriptionOpenByDefault_Empty_ReturnsFalse(t *testing.T) {
	if got := cardDescriptionOpenByDefault(""); got {
		t.Errorf("empty description should return open=false (nothing to collapse or show), got open=%v", got)
	}
}

// --- PR-2 (docs/plans/webui-detail-list-redesign.md §3.3 item 2 / §7 PR-2):
// the integrated child list (ChildRow) and the exec root child tree
// (ChildTreeNode). WebHandler builds these; these tests pin what the
// template does with them once built. ---

func TestChildRow_DisplayStatus_PrefersLiveStatusOverLedger(t *testing.T) {
	row := ChildRow{
		Child:      orchestrator.TaskTriageChild{ID: "c1", Status: orchestrator.TaskTriageChildStatusDispatched},
		LiveStatus: string(orchestrator.TaskStatusExecuting),
	}
	if got := row.DisplayStatus(); got != "executing" {
		t.Errorf("DisplayStatus() = %q, want executing", got)
	}
}

func TestChildRow_DisplayStatus_FallsBackToLedgerWhenNoLiveStatus(t *testing.T) {
	row := ChildRow{Child: orchestrator.TaskTriageChild{ID: "c1", Status: orchestrator.TaskTriageChildStatusSpecced}}
	if got := row.DisplayStatus(); got != "specced" {
		t.Errorf("DisplayStatus() = %q, want specced", got)
	}
}

func TestTaskDetailChildrenSection_DispatchedChild_ChipShowsLiveStatusNotLedger(t *testing.T) {
	rows := []ChildRow{{
		Child:      orchestrator.TaskTriageChild{ID: "c1", Title: "do it", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "task-x"},
		LiveStatus: string(orchestrator.TaskStatusExecuting),
	}}

	var buf bytes.Buffer
	if err := TaskDetailChildrenSection(rows).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, "badge-executing") {
		t.Errorf("chip should show the live status badge-executing; got:\n%s", html)
	}
	if strings.Contains(html, "badge-dispatched") {
		t.Errorf("chip should NOT show the bare ledger badge-dispatched once a live status resolved; got:\n%s", html)
	}
}

func TestTaskDetailChildrenSection_NonDispatchedChild_ChipShowsLedgerStatus(t *testing.T) {
	rows := []ChildRow{{Child: orchestrator.TaskTriageChild{ID: "c1", Title: "not yet", Status: orchestrator.TaskTriageChildStatusSpecced}}}

	var buf bytes.Buffer
	if err := TaskDetailChildrenSection(rows).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if html := buf.String(); !strings.Contains(html, "badge-specced") {
		t.Errorf("chip should show the ledger status badge-specced; got:\n%s", html)
	}
}

// This is the template-level half of the doc's own acceptance condition
// ("awaiting の子を持つ card のフィクスチャで、⚠バッジと質問への直リンクが実際に
// レンダされることをテストで確認する") — internal/api's
// TestWebHandler_TaskDetail_Card_AwaitingChild_RendersWarningBadgeAndQuestionLink
// is the HTTP-handler half.
func TestTaskDetailChildrenSection_AwaitingChild_RendersWarningAndQuestionLink(t *testing.T) {
	rows := []ChildRow{{
		Child:              orchestrator.TaskTriageChild{ID: "c1", Title: "ask something", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "task-x"},
		LiveStatus:         string(orchestrator.TaskStatusAwaiting),
		AwaitingQuestionID: "q-9",
	}}

	var buf bytes.Buffer
	if err := TaskDetailChildrenSection(rows).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, `href="/tasks/task-x/questions/q-9"`) {
		t.Errorf("missing direct link to the child's question; got:\n%s", html)
	}
	if !strings.Contains(html, "⚠") {
		t.Errorf("missing the warning marker; got:\n%s", html)
	}
}

func TestTaskDetailChildrenSection_NonAwaitingChild_NoQuestionLink(t *testing.T) {
	rows := []ChildRow{{
		Child:      orchestrator.TaskTriageChild{ID: "c1", Title: "running", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "task-x"},
		LiveStatus: string(orchestrator.TaskStatusExecuting),
	}}

	var buf bytes.Buffer
	if err := TaskDetailChildrenSection(rows).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if html := buf.String(); strings.Contains(html, "questions/") {
		t.Errorf("a non-awaiting child must not get a question link; got:\n%s", html)
	}
}

func TestTaskDetailChildrenSection_EmptyRendersNothing(t *testing.T) {
	var buf bytes.Buffer
	if err := TaskDetailChildrenSection(nil).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "" {
		t.Errorf("no children should render nothing, got: %s", got)
	}
}

func TestTaskDetailExecChildTreeSection_RendersNestedRowsIndentedByDepth(t *testing.T) {
	nodes := []ChildTreeNode{
		{Task: &orchestrator.Task{ID: "c1", Title: "child A", Status: orchestrator.TaskStatusExecuting}, Depth: 1},
		{Task: &orchestrator.Task{ID: "c1-1", Title: "grandchild A1", Status: orchestrator.TaskStatusDone}, Depth: 2},
	}

	var buf bytes.Buffer
	if err := TaskDetailExecChildTreeSection(nodes).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	for _, want := range []string{"child A", "grandchild A1", `href="/tasks/c1"`, `href="/tasks/c1-1"`, "--depth: 1", "--depth: 2"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q; got:\n%s", want, html)
		}
	}
}

func TestTaskDetailExecChildTreeSection_EmptyRendersNothing(t *testing.T) {
	var buf bytes.Buffer
	if err := TaskDetailExecChildTreeSection(nil).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "" {
		t.Errorf("no child tree should render nothing, got: %s", got)
	}
}
