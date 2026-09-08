package templates

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/timeline"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func renderItem(t *testing.T, item timeline.CardItem, cardID string, status orchestrator.TaskStatus, pinned bool, awaitingQuestionID string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := CardTimelineItem(item, cardID, status, pinned, awaitingQuestionID).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// --- date separators (pure function, no rendering) ---

func TestCardHistoryDateSeparators_FirstItemAlwaysGetsOne(t *testing.T) {
	t1 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.Local)
	items := []timeline.CardItem{{ID: "a", HasTime: true, Time: t1}}
	seps := cardHistoryDateSeparators(items, "")
	if seps[0] == "" {
		t.Fatal("first item with no prior date key should get a separator")
	}
	if want := "Sep 5, 2026"; seps[0] != want {
		t.Errorf("separator = %q, want %q", seps[0], want)
	}
}

func TestCardHistoryDateSeparators_SameDayNoRepeat(t *testing.T) {
	d := time.Date(2026, 9, 5, 0, 0, 0, 0, time.Local)
	items := []timeline.CardItem{
		{ID: "a", HasTime: true, Time: d.Add(10 * time.Hour)},
		{ID: "b", HasTime: true, Time: d.Add(9 * time.Hour)},
		{ID: "c", HasTime: true, Time: d.Add(8 * time.Hour)},
	}
	seps := cardHistoryDateSeparators(items, "")
	if seps[0] == "" {
		t.Fatal("first item should get a separator")
	}
	if seps[1] != "" || seps[2] != "" {
		t.Errorf("same-day items must not repeat the separator, got %v", seps)
	}
}

func TestCardHistoryDateSeparators_DayChangeGetsNewSeparator(t *testing.T) {
	items := []timeline.CardItem{
		{ID: "a", HasTime: true, Time: time.Date(2026, 9, 5, 12, 0, 0, 0, time.Local)},
		{ID: "b", HasTime: true, Time: time.Date(2026, 9, 4, 12, 0, 0, 0, time.Local)},
	}
	seps := cardHistoryDateSeparators(items, "")
	if seps[0] != "Sep 5, 2026" {
		t.Errorf("seps[0] = %q, want Sep 5, 2026", seps[0])
	}
	if seps[1] != "Sep 4, 2026" {
		t.Errorf("seps[1] = %q, want Sep 4, 2026 (day changed)", seps[1])
	}
}

// TestCardHistoryDateSeparators_PriorDateKeyContinuity is the "Load older"
// continuity contract: the first item of a NEW page must not repeat a
// separator for a day the previous page already showed.
func TestCardHistoryDateSeparators_PriorDateKeyContinuity(t *testing.T) {
	items := []timeline.CardItem{
		{ID: "a", HasTime: true, Time: time.Date(2026, 9, 5, 1, 0, 0, 0, time.Local)},
		{ID: "b", HasTime: true, Time: time.Date(2026, 9, 4, 23, 0, 0, 0, time.Local)},
	}
	seps := cardHistoryDateSeparators(items, "2026-09-05")
	if seps[0] != "" {
		t.Errorf("seps[0] = %q, want empty — continues the day the prior page already showed", seps[0])
	}
	if seps[1] != "Sep 4, 2026" {
		t.Errorf("seps[1] = %q, want Sep 4, 2026", seps[1])
	}
}

func TestCardHistoryDateSeparators_NoTimeItemNeitherStartsNorBreaksRun(t *testing.T) {
	items := []timeline.CardItem{
		{ID: "a", HasTime: true, Time: time.Date(2026, 9, 5, 1, 0, 0, 0, time.Local)},
		{ID: "b", HasTime: false},
		{ID: "c", HasTime: true, Time: time.Date(2026, 9, 5, 0, 0, 0, 0, time.Local)},
	}
	seps := cardHistoryDateSeparators(items, "")
	if seps[0] == "" {
		t.Fatal("first item should get a separator")
	}
	if seps[1] != "" {
		t.Errorf("no-time item must not get a separator, got %q", seps[1])
	}
	if seps[2] != "" {
		t.Errorf("same-day item after a no-time item must not repeat the separator, got %q", seps[2])
	}
}

// --- 5 raw Action kinds: label + escaping ---

func TestCardTimelineItem_Suggestion_Historical_RendersReadOnlyNoAcceptReject(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemSuggestion, ID: "a1", HasTime: true, Time: time.Now(),
		Action: &orchestrator.Action{ID: "a1", Payload: mustJSON(t, map[string]any{
			"suggestion": map[string]any{"verb": "park", "reason": "waiting on ci"},
		})},
	}
	html := renderItem(t, item, "card-1", orchestrator.TaskStatusWorking, false, "")
	if !strings.Contains(html, ">park<") || !strings.Contains(html, "waiting on ci") {
		t.Errorf("missing verb/reason; got:\n%s", html)
	}
	if strings.Contains(html, "detail-suggestion-answer-form") {
		t.Errorf("a historical (non-pinned) suggestion must not render Accept/Reject forms; got:\n%s", html)
	}
}

func TestCardTimelineItem_Suggestion_Pinned_RendersAcceptReject(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemSuggestion, ID: "a1", Pinned: true, HasTime: true, Time: time.Now(),
		Action: &orchestrator.Action{ID: "a1", Payload: mustJSON(t, map[string]any{
			"suggestion": map[string]any{"verb": "go", "reason": "specced"},
		})},
	}
	html := renderItem(t, item, "card-1", orchestrator.TaskStatusParked, true, "")
	if !strings.Contains(html, "detail-suggestion-answer-form") {
		t.Errorf("a pinned suggestion must render the Accept/Reject forms; got:\n%s", html)
	}
	if !strings.Contains(html, `value="go"`) {
		t.Errorf("missing verb=go hidden field; got:\n%s", html)
	}
}

func TestCardTimelineItem_Answered_RendersAcceptedOrRejectedAndEscapesBasis(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemAnswered, ID: "a2", HasTime: true, Time: time.Now(),
		Action: &orchestrator.Action{ID: "a2", Payload: mustJSON(t, map[string]string{
			"answer": "accept", "verb": "go", "basis": "<script>alert(1)</script>",
		})},
	}
	html := renderItem(t, item, "card-1", "", false, "")
	if !strings.Contains(html, ">Accepted<") {
		t.Errorf("missing Accepted label; got:\n%s", html)
	}
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Errorf("basis was not escaped; got:\n%s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Errorf("expected the basis to be HTML-escaped; got:\n%s", html)
	}
}

func TestCardTimelineItem_Answered_Reject(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemAnswered, ID: "a3", HasTime: true, Time: time.Now(),
		Action: &orchestrator.Action{ID: "a3", Payload: mustJSON(t, map[string]string{"answer": "reject", "verb": "drop"})},
	}
	html := renderItem(t, item, "card-1", "", false, "")
	if !strings.Contains(html, ">Rejected<") {
		t.Errorf("missing Rejected label; got:\n%s", html)
	}
}

func TestCardTimelineItem_Summary_RendersTextAndEscapes(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemSummary, ID: "a4", HasTime: true, Time: time.Now(),
		Action: &orchestrator.Action{ID: "a4", Payload: mustJSON(t, map[string]string{
			"summary": "found the root cause <img src=x onerror=alert(1)>",
		})},
	}
	html := renderItem(t, item, "card-1", "", false, "")
	if !strings.Contains(html, "found the root cause") {
		t.Errorf("missing summary text; got:\n%s", html)
	}
	if strings.Contains(html, "<img src=x onerror=alert(1)>") {
		t.Errorf("summary was not escaped; got:\n%s", html)
	}
}

func TestCardTimelineItem_Note_RendersArbitraryJSONAndEscapes(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemNote, ID: "a5", HasTime: true, Time: time.Now(),
		Action: &orchestrator.Action{ID: "a5", Payload: mustJSON(t, map[string]any{
			"link": "https://example.com/<script>", "kind": "external",
		})},
	}
	html := renderItem(t, item, "card-1", "", false, "")
	if !strings.Contains(html, "link") || !strings.Contains(html, "external") {
		t.Errorf("missing note body content; got:\n%s", html)
	}
	if strings.Contains(html, "<script>") {
		t.Errorf("note body was not escaped; got:\n%s", html)
	}
}

// TestCardTimelineItem_Note_MalformedPayload_DoesNotPanicOrBreak: a noted
// action carries no structure validation, so a non-object JSON value (here:
// a bare string) must still render without panicking.
func TestCardTimelineItem_Note_MalformedPayload_DoesNotPanicOrBreak(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemNote, ID: "a6", HasTime: true, Time: time.Now(),
		Action: &orchestrator.Action{ID: "a6", Payload: json.RawMessage(`"just a plain string"`)},
	}
	html := renderItem(t, item, "card-1", "", false, "")
	if !strings.Contains(html, "just a plain string") {
		t.Errorf("expected the raw string value to still render; got:\n%s", html)
	}
}

func TestCardTimelineItem_WakeDue_RendersLabel(t *testing.T) {
	item := timeline.CardItem{Kind: timeline.CardItemWakeDue, ID: "a7", HasTime: true, Time: time.Now()}
	html := renderItem(t, item, "card-1", "", false, "")
	if !strings.Contains(html, ">Wake condition due<") {
		t.Errorf("missing wake_due label; got:\n%s", html)
	}
}

// --- child / child_finished ---

func TestCardTimelineItem_Child_TaskExistsFalse_NoLink(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemChild, ID: "child:c1", CorrelationID: "c1",
		Child: &timeline.CardChildDetail{ChildID: "c1", Title: "do it", Status: "dispatched", TaskRef: "task-x", TaskExists: false},
	}
	html := renderItem(t, item, "card-1", "", true, "")
	if strings.Contains(html, `href="/tasks/task-x"`) {
		t.Errorf("TaskExists=false must drop the task link; got:\n%s", html)
	}
}

func TestCardTimelineItem_Child_TaskExistsTrue_RendersLink(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemChild, ID: "child:c1", CorrelationID: "c1",
		Child: &timeline.CardChildDetail{ChildID: "c1", Title: "do it", Status: "dispatched", TaskRef: "task-x", TaskExists: true},
	}
	html := renderItem(t, item, "card-1", "", true, "")
	if !strings.Contains(html, `href="/tasks/task-x"`) {
		t.Errorf("TaskExists=true should render the task link; got:\n%s", html)
	}
}

// TestCardTimelineItem_Child_SpecShowsDescriptionOnlyNotInstruction: the
// spec collapse shows description, never instruction.
func TestCardTimelineItem_Child_SpecShowsDescriptionOnlyNotInstruction(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemChild, ID: "child:c1", CorrelationID: "c1",
		Child: &timeline.CardChildDetail{
			ChildID: "c1", Title: "do it", Status: "specced",
			Spec: &orchestrator.TaskTriageChildSpec{
				Project: "rook-server", Behavior: "research",
				Description: "investigate the conflict handling in PR #1063",
				Instruction: "never conclude from a guess",
			},
		},
	}
	html := renderItem(t, item, "card-1", "", true, "")
	for _, want := range []string{"research", "rook-server", "investigate the conflict handling in PR #1063"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q; got:\n%s", want, html)
		}
	}
	if strings.Contains(html, "never conclude from a guess") {
		t.Error("spec collapse must not include instruction text")
	}
}

// TestCardTimelineItem_Child_DispatchedChild_ChipPrefersLiveStatusOverLedger
// pins the pre-PR-6a ChildRow.DisplayStatus behavior: once a live status
// resolves, it wins over the ledger's bare "dispatched".
func TestCardTimelineItem_Child_DispatchedChild_ChipPrefersLiveStatusOverLedger(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemChild, ID: "child:c1", CorrelationID: "c1",
		Child: &timeline.CardChildDetail{ChildID: "c1", Title: "do it", Status: "dispatched", LiveStatus: "executing"},
	}
	html := renderItem(t, item, "card-1", "", true, "")
	if !strings.Contains(html, `class="badge badge-executing"`) {
		t.Errorf("chip should show the live status badge-executing; got:\n%s", html)
	}
	if strings.Contains(html, `class="badge badge-dispatched"`) {
		t.Errorf("chip should NOT show the bare ledger badge-dispatched once a live status resolved; got:\n%s", html)
	}
}

func TestCardTimelineItem_Child_DispatchedChild_NoLiveStatus_ChipShowsLedger(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemChild, ID: "child:c1", CorrelationID: "c1",
		Child: &timeline.CardChildDetail{ChildID: "c1", Title: "do it", Status: "dispatched"},
	}
	html := renderItem(t, item, "card-1", "", true, "")
	if !strings.Contains(html, `class="badge badge-dispatched"`) {
		t.Errorf("chip should fall back to the ledger status badge-dispatched when no live status resolved; got:\n%s", html)
	}
}

func TestCardTimelineItem_Child_AwaitingQuestion_RendersWarningAndLink(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemChild, ID: "child:c1", CorrelationID: "c1",
		Child: &timeline.CardChildDetail{ChildID: "c1", Title: "ask", Status: "dispatched", TaskRef: "task-x", TaskExists: true},
	}
	html := renderItem(t, item, "card-1", "", true, "q-9")
	if !strings.Contains(html, `href="/tasks/task-x/questions/q-9"`) {
		t.Errorf("missing direct question link; got:\n%s", html)
	}
	if !strings.Contains(html, "⚠") {
		t.Errorf("missing warning marker; got:\n%s", html)
	}
}

func TestCardTimelineItem_ChildFinished_LinksBackToAnchorByCorrelationID(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemChildFinished, ID: "action-1", CorrelationID: "c1",
		Child: &timeline.CardChildDetail{ChildID: "c1", Title: "do it", ClosingActionType: "child_closed"},
	}
	html := renderItem(t, item, "card-1", "", false, "")
	if !strings.Contains(html, `href="#card-child-c1"`) {
		t.Errorf("finished item should link back to its anchor's DOM id; got:\n%s", html)
	}
	if !strings.Contains(html, ">closed<") {
		t.Errorf("missing closed label; got:\n%s", html)
	}
}

func TestCardTimelineItem_ChildFinished_Dropped(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemChildFinished, ID: "action-1", CorrelationID: "c1",
		Child: &timeline.CardChildDetail{ChildID: "c1", ClosingActionType: "child_dropped"},
	}
	html := renderItem(t, item, "card-1", "", false, "")
	if !strings.Contains(html, ">dropped<") {
		t.Errorf("missing dropped label; got:\n%s", html)
	}
}

// --- command ---

func TestCardTimelineItem_Command_Pinned_UsesLabelFallsBackToCommandKey(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemCommand, ID: "pending-command:r1", Pinned: true,
		Command: &timeline.CardCommandDetail{RequestID: "r1", CommandKey: "discuss", Status: orchestrator.CardRequestStatusQueued},
	}
	html := renderItem(t, item, "card-1", "", true, "")
	if !strings.Contains(html, ">discuss<") {
		t.Errorf("queued command with no label should fall back to command_key; got:\n%s", html)
	}
	if !strings.Contains(html, ">queued<") {
		t.Errorf("missing status badge text; got:\n%s", html)
	}
}

func TestCardTimelineItem_Command_Pinned_PrefersLabelOverCommandKey(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemCommand, ID: "pending-command:r1", Pinned: true,
		Command: &timeline.CardCommandDetail{RequestID: "r1", CommandKey: "review", Label: "Run", Status: orchestrator.CardRequestStatusAttached},
	}
	html := renderItem(t, item, "card-1", "", true, "")
	if !strings.Contains(html, ">Run<") {
		t.Errorf("should use the launched label, not the raw command_key; got:\n%s", html)
	}
}

func TestCardTimelineItem_Command_TargetExistsFalse_NoLink(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemCommand, ID: "cmd-1",
		Command: &timeline.CardCommandDetail{
			RequestID: "r1", CommandKey: "discuss", Outcome: "finished",
			TargetKind: "task", TargetID: "task-y", TargetExists: false,
		},
	}
	html := renderItem(t, item, "card-1", "", false, "")
	if strings.Contains(html, `href="/tasks/task-y"`) {
		t.Errorf("TargetExists=false must drop the target link; got:\n%s", html)
	}
}

func TestCardTimelineItem_Command_TargetExistsTrue_RendersLink(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemCommand, ID: "cmd-1",
		Command: &timeline.CardCommandDetail{
			RequestID: "r1", CommandKey: "discuss", Outcome: "finished",
			TargetKind: "task", TargetID: "task-y", TargetExists: true,
		},
	}
	html := renderItem(t, item, "card-1", "", false, "")
	if !strings.Contains(html, `href="/tasks/task-y"`) {
		t.Errorf("TargetExists=true should render the target link; got:\n%s", html)
	}
}

func TestCardTimelineItem_Command_HistoricalOutcome_RendersResultErrorReason(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemCommand, ID: "cmd-1",
		Command: &timeline.CardCommandDetail{
			RequestID: "r1", CommandKey: "review", Outcome: "failed",
			Result: "wrote 2 findings", Error: "boom <b>bold</b>", Reason: "timeout",
		},
	}
	html := renderItem(t, item, "card-1", "", false, "")
	for _, want := range []string{"wrote 2 findings", "timeout"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q; got:\n%s", want, html)
		}
	}
	if strings.Contains(html, "<b>bold</b>") {
		t.Errorf("command error text was not escaped; got:\n%s", html)
	}
}

// --- pinned vs. non-pinned timestamp presentation ---

func TestCardTimelineItem_Pinned_ShowsDateAndTime(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemWakeDue, ID: "a1", Pinned: true, HasTime: true,
		Time: time.Date(2026, 9, 5, 14, 30, 0, 0, time.Local),
	}
	html := renderItem(t, item, "card-1", "", true, "")
	// Both halves: a pinned item sits above the history list, where no date
	// separator reaches it, so it must carry the date AND the clock.
	for _, want := range []string{"Sep 5, 2026", "14:30"} {
		if !strings.Contains(html, want) {
			t.Errorf("pinned item should spell out %q (date and time); got:\n%s", want, html)
		}
	}
}

func TestCardTimelineItem_NonPinned_ShowsTimeOnlyNotDate(t *testing.T) {
	item := timeline.CardItem{
		Kind: timeline.CardItemWakeDue, ID: "a1", HasTime: true,
		Time: time.Date(2026, 9, 5, 14, 30, 0, 0, time.Local),
	}
	html := renderItem(t, item, "card-1", "", false, "")
	if strings.Contains(html, "Sep 5, 2026") {
		t.Errorf("a non-pinned (history) item must show time only, no date (the date separator carries it); got:\n%s", html)
	}
	// "time only" still means a time is shown — asserting only the date's
	// absence passes just as happily when the clock is dropped entirely.
	if !strings.Contains(html, "14:30") {
		t.Errorf("a history item must still show its clock time; got:\n%s", html)
	}
}

// The page must say which timezone its timestamps are in (§5.4's "表示
// タイムゾーンを画面で確認可能にする"), or a reader cannot tell whether a
// time is theirs or the server's.
func TestCardHistorySection_NamesTheDisplayTimezone(t *testing.T) {
	tl := &CardTimelineView{
		History: []timeline.CardItem{{Kind: timeline.CardItemWakeDue, ID: "a1", HasTime: true, Time: time.Now()}},
	}
	var buf bytes.Buffer
	if err := CardHistorySection(tl, "card-1").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	zone, _ := time.Now().Zone()
	if html := buf.String(); !strings.Contains(html, zone) || !strings.Contains(html, "UTC") {
		t.Errorf("expected the display timezone (%q, UTC offset) to be named on the page, got: %s", zone, html)
	}
}

// --- CardHistorySection / CardHistoryOlderFragment: pinned/history dedup and the item cap live in internal/timeline; here we only check the view wiring renders what it's given. ---

func TestCardPinnedSection_NilView_RendersNothing(t *testing.T) {
	var buf bytes.Buffer
	if err := CardPinnedSection(nil, "card-1", orchestrator.TaskStatusParked).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "" {
		t.Errorf("nil view should render nothing, got: %s", got)
	}
}

func TestCardHistorySection_EmptyHistory_RendersEmptyState(t *testing.T) {
	var buf bytes.Buffer
	if err := CardHistorySection(&CardTimelineView{}, "card-1").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "No history yet.") {
		t.Errorf("expected the empty-history copy, got: %s", buf.String())
	}
}

func TestCardHistorySection_HasMore_RendersLoadOlderButton(t *testing.T) {
	tl := &CardTimelineView{
		History:    []timeline.CardItem{{Kind: timeline.CardItemWakeDue, ID: "a1", HasTime: true, Time: time.Now()}},
		HasMore:    true,
		NextCursor: "2026-09-05T00:00:00Z|a1",
	}
	var buf bytes.Buffer
	if err := CardHistorySection(tl, "card-1").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, "Load older") {
		t.Errorf("expected a Load older control, got: %s", html)
	}
	if !strings.Contains(html, "/tasks/card-1/card-timeline?cursor=") {
		t.Errorf("Load older button should hx-get the card-timeline endpoint with the next cursor, got: %s", html)
	}
	// The control must sit INSIDE the list and swap its own <li>. With
	// hx-target="this" the loaded page lands inside the button's <li>, one
	// nesting level deeper per click and outside the list's own styling.
	if !strings.Contains(html, `hx-target="closest li"`) {
		t.Errorf("Load older button must target its own li, got: %s", html)
	}
	listEnd := strings.Index(html, "</ul>")
	control := strings.Index(html, "card-timeline-load-older")
	if listEnd < 0 || control < 0 || control > listEnd {
		t.Errorf("Load older control must render inside ul.card-timeline-list (control=%d, </ul>=%d), got: %s",
			control, listEnd, html)
	}
}

func TestCardHistorySection_NoMore_RendersNoLoadOlderButton(t *testing.T) {
	tl := &CardTimelineView{
		History: []timeline.CardItem{{Kind: timeline.CardItemWakeDue, ID: "a1", HasTime: true, Time: time.Now()}},
		HasMore: false,
	}
	var buf bytes.Buffer
	if err := CardHistorySection(tl, "card-1").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), "Load older") {
		t.Errorf("no more history should not render a Load older control, got: %s", buf.String())
	}
}

// --- date separators, asserted against rendered output (not just the pure
// cardHistoryDateSeparators function) ---

func TestCardHistoryItems_RendersDateSeparatorAtDayBoundary(t *testing.T) {
	items := []timeline.CardItem{
		{Kind: timeline.CardItemWakeDue, ID: "a", HasTime: true, Time: time.Date(2026, 9, 5, 10, 0, 0, 0, time.Local)},
		{Kind: timeline.CardItemWakeDue, ID: "b", HasTime: true, Time: time.Date(2026, 9, 5, 9, 0, 0, 0, time.Local)},
		{Kind: timeline.CardItemWakeDue, ID: "c", HasTime: true, Time: time.Date(2026, 9, 4, 23, 0, 0, 0, time.Local)},
	}
	var buf bytes.Buffer
	if err := cardHistoryItems(items, "card-1", "").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if got := strings.Count(html, "card-timeline-date-sep"); got != 2 {
		t.Fatalf("rendered date separators = %d, want 2 (one per distinct day, including the leading one); html:\n%s", got, html)
	}
	if !strings.Contains(html, ">Sep 5, 2026<") || !strings.Contains(html, ">Sep 4, 2026<") {
		t.Errorf("missing one of the expected separator labels; html:\n%s", html)
	}
	// The day-change separator must sit strictly between item b and item c.
	sepPos := strings.LastIndex(html, "card-timeline-date-sep")
	bPos := strings.Index(html, `id="card-item-wake_due-b"`)
	cPos := strings.Index(html, `id="card-item-wake_due-c"`)
	if sepPos < 0 || bPos < 0 || cPos < 0 || !(bPos < sepPos && sepPos < cPos) {
		t.Fatalf("day-change separator must render strictly between item b and item c; html:\n%s", html)
	}
}

func TestCardHistoryItems_SameDay_RendersExactlyOneLeadingSeparator(t *testing.T) {
	items := []timeline.CardItem{
		{Kind: timeline.CardItemWakeDue, ID: "a", HasTime: true, Time: time.Date(2026, 9, 5, 10, 0, 0, 0, time.Local)},
		{Kind: timeline.CardItemWakeDue, ID: "b", HasTime: true, Time: time.Date(2026, 9, 5, 9, 0, 0, 0, time.Local)},
	}
	var buf bytes.Buffer
	if err := cardHistoryItems(items, "card-1", "").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := strings.Count(buf.String(), "card-timeline-date-sep"); got != 1 {
		t.Errorf("rendered date separators = %d, want exactly 1 (no repeat within the same day); html:\n%s", got, buf.String())
	}
}

// TestCardHistoryOlderFragment_PriorDateKey_DoesNotRepeatTheContinuedDay is
// the "Load older" continuity contract rendered end to end: a fragment
// whose first item continues the day the previous page already showed
// (priorDateKey) must not repeat that separator, but a genuine day change
// within the same fragment still gets one.
func TestCardHistoryOlderFragment_PriorDateKey_DoesNotRepeatTheContinuedDay(t *testing.T) {
	items := []timeline.CardItem{
		{Kind: timeline.CardItemWakeDue, ID: "d", HasTime: true, Time: time.Date(2026, 9, 5, 1, 0, 0, 0, time.Local)},
		{Kind: timeline.CardItemWakeDue, ID: "e", HasTime: true, Time: time.Date(2026, 9, 4, 23, 0, 0, 0, time.Local)},
	}
	var buf bytes.Buffer
	if err := CardHistoryOlderFragment("card-1", items, false, "", "2026-09-05").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if got := strings.Count(html, "card-timeline-date-sep"); got != 1 {
		t.Fatalf("separators = %d, want exactly 1 (only the genuine day change, not the continued day); html:\n%s", got, html)
	}
	if !strings.Contains(html, ">Sep 4, 2026<") {
		t.Errorf("missing the day-change separator label; html:\n%s", html)
	}
}

func TestTaskDetailCardSummary_Empty_RendersNothing(t *testing.T) {
	var buf bytes.Buffer
	if err := TaskDetailCardSummary("").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "" {
		t.Errorf("empty summary should render nothing, got: %s", got)
	}
}

func TestTaskDetailCardSummary_RendersAndEscapes(t *testing.T) {
	var buf bytes.Buffer
	if err := TaskDetailCardSummary("found a fix <script>alert(1)</script>").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, "found a fix") {
		t.Errorf("missing summary text, got: %s", html)
	}
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Errorf("summary was not escaped, got: %s", html)
	}
}
