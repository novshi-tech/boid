package api

// The card detail page's timeline UI, wired against a real (in-memory
// SQLite) DB rather than stubs, since the read model (internal/timeline)
// derives pinned/history status from the actions log, not just a stub
// TaskTriage row's live detail blob.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// dbBackedWebService overrides stubWebService.GetTaskDetail with a real,
// per-id DB lookup — stubWebService always returns the same fixed
// taskDetail regardless of id, which cannot support looking up a card AND
// (separately) one of its dispatched children's own task row in the same
// test, as the pinned-child awaiting-question lookup does.
type dbBackedWebService struct {
	*stubWebService
	repo *orchestrator.TaskRepository
}

func (s dbBackedWebService) GetTaskDetail(id string) (*TaskDetailView, error) {
	task, err := s.repo.GetTask(id)
	if err != nil {
		return nil, err
	}
	return &TaskDetailView{Task: task}, nil
}

func newCardTimelineTestHandler(t *testing.T) (*WebHandler, db.DBTX, string) {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	projectID := "proj-1"
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: projectID, WorkDir: "/tmp/" + projectID}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	repo := orchestrator.NewTaskRepository(d.Conn)
	h := &WebHandler{
		Service:      dbBackedWebService{stubWebService: &stubWebService{}, repo: repo},
		TaskTriage:   repo,
		CardTimeline: DBCardTimelineStore{DB: d.Conn},
	}
	return h, d.Conn, projectID
}

func newCardTimelineTestCard(t *testing.T, conn db.DBTX, projectID, cardID string) {
	t.Helper()
	card := &orchestrator.Task{ID: cardID, ProjectID: projectID, Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	if err := orchestrator.CreateTask(conn, card); err != nil {
		t.Fatalf("create card: %v", err)
	}
}

func createCardTimelineAction(t *testing.T, conn db.DBTX, taskID, actionType string, payload any) *orchestrator.Action {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	a := &orchestrator.Action{TaskID: taskID, Type: actionType, Payload: raw, Actor: orchestrator.ActorDaemon}
	if err := orchestrator.CreateAction(context.Background(), conn, a, nil, nil); err != nil {
		t.Fatalf("create action %s: %v", actionType, err)
	}
	return a
}

// writeLiveSuggestion mirrors what applyAttrsSetSideEffect actually does
// (workflow_card.go) closely enough for a read-model fixture: an attrs_set
// action AND the promoted task_triage.detail.suggestion/SuggestionVerb, so
// DetailSuggestion(detail) resolves and CardPinnedItems pins it.
func writeLiveSuggestion(t *testing.T, conn db.DBTX, cardID, verb, reason string) {
	t.Helper()
	createCardTimelineAction(t, conn, cardID, "attrs_set", map[string]any{
		"suggestion": map[string]string{"verb": verb, "reason": reason},
	})
	tt, err := orchestrator.GetTaskTriage(conn, cardID)
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	newDetail, err := orchestrator.FoldDetailAttrs(tt.Detail, map[string]json.RawMessage{
		"suggestion": json.RawMessage(`{"verb":"` + verb + `","reason":"` + reason + `"}`),
	})
	if err != nil {
		t.Fatalf("fold detail attrs: %v", err)
	}
	tt.Detail = newDetail
	tt.SuggestionVerb = verb
	if err := orchestrator.UpsertTaskTriage(conn, tt); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}
}

func addOpenChild(t *testing.T, conn db.DBTX, cardID, childID, title string) {
	t.Helper()
	createCardTimelineAction(t, conn, cardID, "child_added", map[string]string{"id": childID, "title": title})
	tt, err := orchestrator.GetTaskTriage(conn, cardID)
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	newDetail, err := orchestrator.AddDetailChild(tt.Detail, orchestrator.TaskTriageChild{ID: childID, Title: title})
	if err != nil {
		t.Fatalf("AddDetailChild: %v", err)
	}
	tt.Detail = newDetail
	if err := orchestrator.UpsertTaskTriage(conn, tt); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}
}

func getHTML(t *testing.T, h *WebHandler, path string) (int, string) {
	t.Helper()
	r := chi.NewRouter()
	r.Mount("/", h.Routes())
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// --- pinned suggestion replaces the old movement-row transition edge ---

func TestCardDetail_PinnedSuggestion_RendersAcceptRejectAndNoTransitionEdge(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")
	writeLiveSuggestion(t, repo, "card-1", "go", "children specced")

	code, body := getHTML(t, h, "/tasks/card-1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", code, body)
	}
	if !strings.Contains(body, "detail-suggestion-answer-form") {
		t.Errorf("pinned suggestion should render Accept/Reject forms; got:\n%s", body)
	}
	if !strings.Contains(body, `value="go"`) {
		t.Errorf("missing verb=go hidden field; got:\n%s", body)
	}
	if strings.Contains(body, "—go→") {
		t.Errorf("the movement-row transition edge is gone (the pinned suggestion carries it now); got:\n%s", body)
	}
}

func TestCardDetail_Fragment_Status_IncludesPinnedSuggestion(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")
	writeLiveSuggestion(t, repo, "card-1", "reopen", "source event fired")

	code, body := getHTML(t, h, "/tasks/card-1/fragment?kind=status")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", code, body)
	}
	for _, want := range []string{"badge-verb-reopen", "source event fired"} {
		if !strings.Contains(body, want) {
			t.Errorf("status fragment missing %q; got:\n%s", want, body)
		}
	}
}

// --- pinned/history dedup, the 10-item cap, and awaiting-child link ---

// TestCardDetail_OpenChild_PinnedNotInHistory pins the non-duplication
// contract end to end: an open (unresolved) child renders as a pinned item
// and must not ALSO appear in the newest-first history page.
func TestCardDetail_OpenChild_PinnedNotInHistory(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")
	addOpenChild(t, repo, "card-1", "c1", "investigate the flaky test")

	_, body := getHTML(t, h, "/tasks/card-1")
	if strings.Count(body, "investigate the flaky test") != 1 {
		t.Errorf("open child should render exactly once (pinned only), got %d occurrences; body:\n%s", strings.Count(body, "investigate the flaky test"), body)
	}
	if !strings.Contains(body, "(no spec yet)") {
		t.Errorf("open child should show the no-spec-yet hint; got:\n%s", body)
	}
}

// TestCardDetail_TenItemCap_HistoryPagingLeavesLoadOlder builds 12
// historical items (well past DefaultCardTimelineLimit=10) plus one pinned
// suggestion, and checks: the pinned item renders once (not part of the
// capped page), the first page shows exactly 10 history rows, and a Load
// older control is present for the rest.
func TestCardDetail_TenItemCap_HistoryPagingLeavesLoadOlder(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")
	for i := 0; i < 12; i++ {
		createCardTimelineAction(t, repo, "card-1", "attrs_set", map[string]string{"summary": "note number " + fmt.Sprintf("%02d", i)})
	}
	writeLiveSuggestion(t, repo, "card-1", "park", "waiting on ci")

	code, body := getHTML(t, h, "/tasks/card-1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := strings.Count(body, "note number"); got != 10 {
		t.Errorf("history page should render exactly 10 items (default limit), got %d; body:\n%s", got, body)
	}
	if !strings.Contains(body, "Load older") {
		t.Errorf("expected a Load older control with 12 history items past the 10-cap; body:\n%s", body)
	}
	if got := strings.Count(body, "waiting on ci"); got != 1 {
		t.Errorf("pinned suggestion should render exactly once (pinned section only, not also duplicated into history), got %d; body:\n%s", got, body)
	}
}

// TestCardDetail_LoadOlder_PagesWithoutDuplicationOrLoss follows the exact
// cursor the first page's Load older button carries and checks the second
// page picks up exactly where the first left off — no repeated or skipped
// item.
func TestCardDetail_LoadOlder_PagesWithoutDuplicationOrLoss(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")
	for i := 0; i < 15; i++ {
		createCardTimelineAction(t, repo, "card-1", "attrs_set", map[string]string{"summary": "note number " + fmt.Sprintf("%02d", i)})
	}

	_, page1 := getHTML(t, h, "/tasks/card-1")
	cursor, lastDate := extractLoadOlderParams(t, page1)

	code, page2 := getHTML(t, h, "/tasks/card-1/card-timeline?"+url.Values{"cursor": {cursor}, "last_date": {lastDate}}.Encode())
	if code != http.StatusOK {
		t.Fatalf("card-timeline status = %d, want 200; body:\n%s", code, page2)
	}

	// 15 items total, page1 has 10 (newest), so page2 must carry exactly the
	// remaining 5, each exactly once across both pages.
	seen := map[string]int{}
	for i := 0; i < 15; i++ {
		want := "note number " + fmt.Sprintf("%02d", i)
		seen[want] = strings.Count(page1, want) + strings.Count(page2, want)
	}
	for note, count := range seen {
		if count != 1 {
			t.Errorf("%q appeared %d times across page1+page2, want exactly 1", note, count)
		}
	}
	if strings.Contains(page2, "Load older") {
		t.Errorf("page2 should be exhausted (15 items, 10+5), want no further Load older; body:\n%s", page2)
	}
}

// extractLoadOlderParams pulls the cursor and last_date query params out of
// the rendered Load older button's hx-get URL.
func extractLoadOlderParams(t *testing.T, html string) (cursor, lastDate string) {
	t.Helper()
	idx := strings.Index(html, "/card-timeline?")
	if idx < 0 {
		t.Fatalf("no Load older hx-get URL found in:\n%s", html)
	}
	rest := html[idx+len("/card-timeline?"):]
	end := strings.IndexAny(rest, `"`)
	if end < 0 {
		t.Fatalf("could not find end of hx-get URL in:\n%s", html)
	}
	// The templ-rendered attribute value is HTML-attribute-escaped (&amp;
	// between params) — undo that before parsing as a query string.
	raw := strings.ReplaceAll(rest[:end], "&amp;", "&")
	values, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parse query %q: %v", raw, err)
	}
	return values.Get("cursor"), values.Get("last_date")
}

// TestCardDetail_AwaitingChild_RendersQuestionLink: a dispatched child
// currently awaiting an answer gets a ⚠ marker and a direct link to its
// open question.
func TestCardDetail_AwaitingChild_RendersQuestionLink(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")
	addOpenChild(t, repo, "card-1", "c1", "child task")

	// Spec + dispatch the child to a real, live, awaiting task row.
	tt, err := orchestrator.GetTaskTriage(repo, "card-1")
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	newDetail, err := orchestrator.SpecDetailChild(tt.Detail, "c1", orchestrator.TaskTriageChildSpec{Project: projectID}, "")
	if err != nil {
		t.Fatalf("SpecDetailChild: %v", err)
	}
	tt.Detail = newDetail
	if err := orchestrator.UpsertTaskTriage(repo, tt); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}

	ap, err := json.Marshal(orchestrator.AwaitingPayload{QuestionID: "q-9"})
	if err != nil {
		t.Fatalf("marshal awaiting payload: %v", err)
	}
	execPayload, err := json.Marshal(map[string]json.RawMessage{string(orchestrator.TraitAwaiting): ap})
	if err != nil {
		t.Fatalf("marshal exec payload: %v", err)
	}
	childTask := &orchestrator.Task{
		ID: "task-x", ProjectID: projectID, ParentID: "card-1", Type: orchestrator.TaskTypeExecution,
		Status: orchestrator.TaskStatusPending, Exec: &orchestrator.ExecAttrs{},
	}
	if err := orchestrator.CreateTask(repo, childTask); err != nil {
		t.Fatalf("create child task: %v", err)
	}
	childTask.Status = orchestrator.TaskStatusAwaiting
	childTask.Exec.Payload = execPayload
	if err := orchestrator.UpdateTask(repo, childTask); err != nil {
		t.Fatalf("update child task: %v", err)
	}

	tt, err = orchestrator.GetTaskTriage(repo, "card-1")
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	children, err := orchestrator.DetailChildren(tt.Detail)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	for i := range children {
		if children[i].ID == "c1" {
			children[i].TaskRef = "task-x"
			children[i].Status = orchestrator.TaskTriageChildStatusDispatched
		}
	}
	newDetail, err = orchestrator.SetDetailChildren(tt.Detail, children)
	if err != nil {
		t.Fatalf("SetDetailChildren: %v", err)
	}
	tt.Detail = newDetail
	if err := orchestrator.UpsertTaskTriage(repo, tt); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}

	code, body := getHTML(t, h, "/tasks/card-1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", code, body)
	}
	if !strings.Contains(body, `href="/tasks/task-x/questions/q-9"`) {
		t.Errorf("body missing the child's direct question link; got:\n%s", body)
	}
	if !strings.Contains(body, "⚠") {
		t.Errorf("body missing the warning marker for an awaiting child; got:\n%s", body)
	}
}

// TestCardDetail_ClosedChildTaskGCd_TaskExistsFalse_NoLink pins the
// GC-survival contract at the HTTP layer: once a closed child's own task
// row is gone, the page must still render (result read from the
// child_closed action's own payload) with no dangling task link.
func TestCardDetail_ClosedChildTaskGCd_TaskExistsFalse_NoLink(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")
	addOpenChild(t, repo, "card-1", "c1", "child task")

	tt, err := orchestrator.GetTaskTriage(repo, "card-1")
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	newDetail, err := orchestrator.SpecDetailChild(tt.Detail, "c1", orchestrator.TaskTriageChildSpec{Project: projectID}, "")
	if err != nil {
		t.Fatalf("SpecDetailChild: %v", err)
	}
	tt.Detail = newDetail
	if err := orchestrator.UpsertTaskTriage(repo, tt); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}

	childTask := &orchestrator.Task{
		ID: "task-x", ProjectID: projectID, ParentID: "card-1", Type: orchestrator.TaskTypeExecution,
		Status: orchestrator.TaskStatusPending, Exec: &orchestrator.ExecAttrs{},
	}
	if err := orchestrator.CreateTask(repo, childTask); err != nil {
		t.Fatalf("create child task: %v", err)
	}

	tt, err = orchestrator.GetTaskTriage(repo, "card-1")
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	children, err := orchestrator.DetailChildren(tt.Detail)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	for i := range children {
		children[i].TaskRef = "task-x"
		children[i].Status = orchestrator.TaskTriageChildStatusDispatched
	}
	newDetail, err = orchestrator.SetDetailChildren(tt.Detail, children)
	if err != nil {
		t.Fatalf("SetDetailChildren: %v", err)
	}
	tt.Detail = newDetail
	newDetail, changed, err := orchestrator.MarkDetailChildClosed(tt.Detail, "task-x")
	if err != nil {
		t.Fatalf("MarkDetailChildClosed: %v", err)
	}
	if !changed {
		t.Fatalf("expected MarkDetailChildClosed to report changed=true")
	}
	tt.Detail = newDetail
	if err := orchestrator.UpsertTaskTriage(repo, tt); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}
	createCardTimelineAction(t, repo, "card-1", "child_closed", map[string]string{
		"child_id": "task-x", "child_status": "done", "summary": "closed the gap",
	})

	if err := orchestrator.DeleteTask(repo, "task-x"); err != nil {
		t.Fatalf("delete child task (simulating GC): %v", err)
	}

	code, body := getHTML(t, h, "/tasks/card-1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", code, body)
	}
	if !strings.Contains(body, "closed the gap") {
		t.Errorf("closed child's result should survive its own task row's GC; body:\n%s", body)
	}
	if strings.Contains(body, `href="/tasks/task-x"`) {
		t.Errorf("a GC'd child task must not render a dangling link; body:\n%s", body)
	}
}

// TestCardDetail_MaliciousChildTitle_Escaped: a sandbox-controlled child
// title must render HTML-escaped.
func TestCardDetail_MaliciousChildTitle_Escaped(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")
	addOpenChild(t, repo, "card-1", "c1", `<img src=x onerror=alert(1)>`)

	_, body := getHTML(t, h, "/tasks/card-1")
	if strings.Contains(body, "<img src=x onerror=alert(1)>") {
		t.Errorf("child title was not escaped; body:\n%s", body)
	}
	if !strings.Contains(body, "&lt;img") {
		t.Errorf("expected the child title to render HTML-escaped; body:\n%s", body)
	}
}

// --- command items (pinned attached + terminal history) ---

func TestCardDetail_PinnedCommand_RendersLabelAndTargetLink(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")

	target := &orchestrator.Task{ID: "task-y", ProjectID: projectID, ParentID: "", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}}
	if err := orchestrator.CreateTask(repo, target); err != nil {
		t.Fatalf("create target task: %v", err)
	}
	req := &orchestrator.CardRequest{
		CardID: "card-1", CommandKey: "review", Status: orchestrator.CardRequestStatusLaunching,
		LauncherJobID: "launcher-1", Launched: orchestrator.CardRequestDefinition{CommandKey: "review", Label: "Run"},
	}
	if err := orchestrator.CreateCardRequest(repo, req); err != nil {
		t.Fatalf("create card request: %v", err)
	}
	if err := orchestrator.AttachCardRequestOwned(repo, req.ID, "launcher-1", orchestrator.CardRequestTargetKindTask, "task-y"); err != nil {
		t.Fatalf("attach card request: %v", err)
	}

	code, body := getHTML(t, h, "/tasks/card-1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", code, body)
	}
	if !strings.Contains(body, ">Run<") {
		t.Errorf("pinned command should show its launched label; body:\n%s", body)
	}
	if !strings.Contains(body, `href="/tasks/task-y"`) {
		t.Errorf("pinned command with an existing task target should link to it; body:\n%s", body)
	}
}

// TestCardDetail_HistoricalCommand_TargetTaskGCd_NoLink is the command-side
// counterpart of the child GC-survival test: once a finished command's
// target task row is gone, its result must still render with no dangling
// link.
func TestCardDetail_HistoricalCommand_TargetTaskGCd_NoLink(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-1")

	target := &orchestrator.Task{ID: "task-z", ProjectID: projectID, Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{}}
	if err := orchestrator.CreateTask(repo, target); err != nil {
		t.Fatalf("create target task: %v", err)
	}
	createCardTimelineAction(t, repo, "card-1", orchestrator.ActionTypeCommandFinished, map[string]string{
		"request_id": "r1", "command_key": "review", "launched_label": "Run",
		"target_kind": orchestrator.CardRequestTargetKindTask, "target_id": "task-z",
		"result": "reviewed and merged",
	})
	if err := orchestrator.DeleteTask(repo, "task-z"); err != nil {
		t.Fatalf("delete target task (simulating GC): %v", err)
	}

	code, body := getHTML(t, h, "/tasks/card-1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", code, body)
	}
	if !strings.Contains(body, "reviewed and merged") {
		t.Errorf("command result should survive its target task's GC; body:\n%s", body)
	}
	if strings.Contains(body, `href="/tasks/task-z"`) {
		t.Errorf("a GC'd command target must not render a dangling link; body:\n%s", body)
	}
}
