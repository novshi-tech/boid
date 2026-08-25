package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// newTestWebHandlerFull registers / endpoint for task list tests.
func newTestWebHandlerFull(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/", h.TaskList)
	return r
}

// TestWebTaskList_DefaultStatusIsEmpty pins docs/plans/
// webui-detail-list-redesign.md PR-4's §3.5 default: no ?status= means NO
// status narrowing at all (every status, newest-updated first) — the old
// default-to-"open" tab is gone.
func TestWebTaskList_DefaultStatusIsEmpty(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if svc.capturedFilter.Status != "" {
		t.Errorf("default Status = %q, want \"\" (全状態表示, no default-to-open)", svc.capturedFilter.Status)
	}
	if svc.capturedFilter.ActiveOnly {
		t.Error("default ActiveOnly = true, want false (no filter applied by default)")
	}
}

// TestWebTaskList_ActiveOnlyQueryParam verifies ?active=1 sets
// filter.ActiveOnly — the replacement for the old status=open tab.
func TestWebTaskList_ActiveOnlyQueryParam(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/?active=1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if !svc.capturedFilter.ActiveOnly {
		t.Error("?active=1 should set filter.ActiveOnly = true")
	}
}

// TestWebTaskList_ClosedStatus verifies that ?status=closed passes "closed" to ListTasks.
func TestWebTaskList_ClosedStatus(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/?status=closed", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if svc.capturedFilter.Status != "closed" {
		t.Errorf("Status = %q, want \"closed\"", svc.capturedFilter.Status)
	}
}

// TestWebTaskList_RootOnly pins §3.5's "1本のフラットリスト、トップレベル
// (parent_id無し) のみ" — TaskList always filters ParentID to the root scope
// ("") regardless of any other query param, so a non-root task never reaches
// the list (its children are read from the parent's own detail page, PR-2).
func TestWebTaskList_RootOnly(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if svc.capturedFilter.ParentID == nil {
		t.Fatal("ParentID filter must be set (root-only listing)")
	}
	if *svc.capturedFilter.ParentID != "" {
		t.Errorf("ParentID = %q, want \"\" (root tasks only)", *svc.capturedFilter.ParentID)
	}
}

// TestWebTaskList_ChildTaskNeverAppears is the render-level twin of
// TestWebTaskList_RootOnly: a non-root task in the underlying store (the
// stub honors ParentID like the real store.ListTasks does) must not show up
// in the rendered list at all.
func TestWebTaskList_ChildTaskNeverAppears(t *testing.T) {
	parent := &orchestrator.Task{
		ID: "p1", Title: "Parent Task", Status: orchestrator.TaskStatusDone,
		ParentID: "", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	child := &orchestrator.Task{
		ID: "c1", Title: "Child Task", Status: orchestrator.TaskStatusDone,
		ParentID: "p1", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	svc := &stubWebService{tasks: []*orchestrator.Task{parent, child}}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/?status=closed", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "Parent Task") {
		t.Errorf("expected the root task to render, got: %s", body)
	}
	if strings.Contains(body, "Child Task") {
		t.Errorf("a non-root task must never appear in the list (PR-4 root-only), got: %s", body)
	}
}

// TestWebTaskList_WorkspaceFiltersProjects verifies that workspace filter limits project list.
func TestWebTaskList_WorkspaceFiltersProjects(t *testing.T) {
	proj1 := &orchestrator.Project{ID: "proj-a", WorkspaceID: "ws-1"}
	proj2 := &orchestrator.Project{ID: "proj-b", WorkspaceID: "ws-2"}
	proj3 := &orchestrator.Project{ID: "proj-c", WorkspaceID: "ws-1"}
	svc := &stubWebService{projects: []*orchestrator.Project{proj1, proj2, proj3}}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/?workspace=ws-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	// proj-a and proj-c belong to ws-1, proj-b does not
	if !strings.Contains(body, "proj-a") {
		t.Errorf("body should contain proj-a (in ws-1), got: %s", body)
	}
	if !strings.Contains(body, "proj-c") {
		t.Errorf("body should contain proj-c (in ws-1), got: %s", body)
	}
	if strings.Contains(body, "proj-b") {
		t.Errorf("body should NOT contain proj-b (in ws-2), got: %s", body)
	}
}

// TestWebTaskList_ProjectClearedWhenNotInWorkspace verifies that project filter is cleared
// when selected project does not belong to the selected workspace.
func TestWebTaskList_ProjectClearedWhenNotInWorkspace(t *testing.T) {
	proj1 := &orchestrator.Project{ID: "proj-a", WorkspaceID: "ws-1"}
	proj2 := &orchestrator.Project{ID: "proj-b", WorkspaceID: "ws-2"}
	svc := &stubWebService{projects: []*orchestrator.Project{proj1, proj2}}
	r := newTestWebHandlerFull(svc)

	// workspace=ws-1 but project=proj-b (which is in ws-2) — should be cleared
	req := httptest.NewRequest(http.MethodGet, "/?workspace=ws-1&project=proj-b", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// The captured filter for ListTasks should have ProjectID cleared
	if svc.capturedFilter.ProjectID != "" {
		t.Errorf("ProjectID should be cleared when project not in workspace, got: %q", svc.capturedFilter.ProjectID)
	}
}

// TestWebTaskList_TaskRowIsAnchorTag verifies that task rows are wrapped in <a> elements.
func TestWebTaskList_TaskRowIsAnchorTag(t *testing.T) {
	task := &orchestrator.Task{
		ID: "t-abc", Title: "My Task", Status: orchestrator.TaskStatusExecuting,
		CreatedAt: time.Now(), UpdatedAt: time.Now(), Exec: &orchestrator.ExecAttrs{},
	}
	svc := &stubWebService{tasks: []*orchestrator.Task{task}}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	body := w.Body.String()
	// The task row should be an <a> element pointing to /tasks/{id}
	if !strings.Contains(body, `<a `) {
		t.Errorf("body should contain anchor tag for task row, got: %s", body)
	}
	if !strings.Contains(body, "/tasks/t-abc") {
		t.Errorf("body should contain link to /tasks/t-abc, got: %s", body)
	}
	// task-row class should be on the <a> tag (alongside status-specific classes)
	if !strings.Contains(body, `class="task-row `) {
		t.Errorf("body should contain task-row class on anchor, got: %s", body)
	}
}

// TestWebTaskList_ActiveOnlyToggleInHTML verifies the active-only toggle
// (the PR-4 replacement for the old 4-tab status switcher) renders, and the
// old tab markup does not.
func TestWebTaskList_ActiveOnlyToggleInHTML(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `name="active"`) {
		t.Errorf("body should contain the active-only toggle checkbox, got: %s", body)
	}
	// Old tab UI must be fully gone.
	if strings.Contains(body, "status-tab") {
		t.Errorf("body should NOT contain the old status-tab markup, got: %s", body)
	}
	if strings.Contains(body, `value="pending"`) || strings.Contains(body, `value="executing"`) {
		t.Errorf("body should NOT contain the old per-status tab buttons, got: %s", body)
	}
}

// TestWebTaskList_ClosedStatusWithQuery verifies that ?status=closed&q=foo retains status=closed.
func TestWebTaskList_ClosedStatusWithQuery(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/?status=closed&q=foo", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if svc.capturedFilter.Status != "closed" {
		t.Errorf("Status = %q, want \"closed\" (status must not be dropped when q is set)", svc.capturedFilter.Status)
	}
	if svc.capturedFilter.Title != "foo" {
		t.Errorf("Title = %q, want \"foo\"", svc.capturedFilter.Title)
	}
}

// TestWebTaskList_ClosedStatusWithWorkspace verifies that ?status=closed&workspace=ws retains status=closed.
func TestWebTaskList_ClosedStatusWithWorkspace(t *testing.T) {
	proj := &orchestrator.Project{ID: "p1", WorkspaceID: "ws-1"}
	svc := &stubWebService{projects: []*orchestrator.Project{proj}}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/?status=closed&workspace=ws-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if svc.capturedFilter.Status != "closed" {
		t.Errorf("Status = %q, want \"closed\" (status must not be dropped when workspace is set)", svc.capturedFilter.Status)
	}
	if svc.capturedFilter.WorkspaceID != "ws-1" {
		t.Errorf("WorkspaceID = %q, want \"ws-1\"", svc.capturedFilter.WorkspaceID)
	}
}

// TestWebTaskList_PageParam verifies ?page=2 translates into the right
// Limit/Offset (§3.5, §5 論点4): page size 50, so page 2 is offset 50.
func TestWebTaskList_PageParam(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/?page=2", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if svc.capturedFilter.Offset != 50 {
		t.Errorf("Offset = %d, want 50 (page 2, page size 50)", svc.capturedFilter.Offset)
	}
	if svc.capturedFilter.Limit != 51 {
		t.Errorf("Limit = %d, want 51 (page size + 1, to detect hasMore)", svc.capturedFilter.Limit)
	}
}

// makeExecTasks builds n distinct root execution tasks — enough fixture rows
// to exercise WebHandler.TaskList's hasMore trim (taskListPageSize=50).
func makeExecTasks(n int) []*orchestrator.Task {
	tasks := make([]*orchestrator.Task, n)
	for i := 0; i < n; i++ {
		tasks[i] = &orchestrator.Task{
			ID:     "t-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Title:  "task",
			Type:   orchestrator.TaskTypeExecution,
			Status: orchestrator.TaskStatusExecuting,
			Exec:   &orchestrator.ExecAttrs{},
		}
	}
	return tasks
}

// TestWebTaskList_HasMore_TrimsToPageSizeAndRendersNextLink pins the
// hasMore trim itself (WebHandler.TaskList: `hasMore := len(tasks) >
// taskListPageSize`) — previously untested at the HTTP level even though
// TestWebTaskList_PageParam already covered the Limit=51 request side.
// A mutation that hardcodes hasMore=false passed `go test ./internal/api/...`
// green (memory: [[next-session-webui-detail-list-impl]] follow-up 1, N1).
// The stub returns exactly what capturedFilter.Limit asked for is NOT
// enforced by the stub — so a 51-row fixture directly exercises the trim.
func TestWebTaskList_HasMore_TrimsToPageSizeAndRendersNextLink(t *testing.T) {
	svc := &stubWebService{tasks: makeExecTasks(taskListPageSize + 1)}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Next →") {
		t.Errorf("expected a Next link when the store returns more than %d rows, got: %s", taskListPageSize, body)
	}
	// The 51st row must be trimmed before rendering — exactly
	// taskListPageSize rows render, not taskListPageSize+1.
	if got := strings.Count(body, `data-task-id="t-`); got != taskListPageSize {
		t.Errorf("rendered %d task rows, want exactly %d (the +1 lookahead row must be trimmed)", got, taskListPageSize)
	}
}

// TestWebTaskList_NoHasMore_NoNextLink is the negative twin: exactly
// taskListPageSize rows (no lookahead row) must not show a Next link.
func TestWebTaskList_NoHasMore_NoNextLink(t *testing.T) {
	svc := &stubWebService{tasks: makeExecTasks(taskListPageSize)}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "Next →") {
		t.Errorf("did not expect a Next link when the store returns exactly %d rows, got: %s", taskListPageSize, body)
	}
}

// TestWebTaskList_PageParam_InvalidFallsBackToPage1 pins the defensive
// parse: a non-numeric or non-positive ?page= must not 500 or produce a
// negative OFFSET — it degrades to page 1.
func TestWebTaskList_PageParam_InvalidFallsBackToPage1(t *testing.T) {
	for _, raw := range []string{"abc", "-1", "0", ""} {
		svc := &stubWebService{}
		r := newTestWebHandlerFull(svc)

		req := httptest.NewRequest(http.MethodGet, "/?page="+raw, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("page=%q: status = %d, want 200", raw, w.Code)
		}
		if svc.capturedFilter.Offset != 0 {
			t.Errorf("page=%q: Offset = %d, want 0 (falls back to page 1)", raw, svc.capturedFilter.Offset)
		}
	}
}

// TestFilterProjectsByWorkspace_EmptyWorkspace returns all projects when workspace is empty.
func TestFilterProjectsByWorkspace_EmptyWorkspace(t *testing.T) {
	projects := []*orchestrator.Project{
		{ID: "a", WorkspaceID: "ws-1"},
		{ID: "b", WorkspaceID: "ws-2"},
	}
	got := filterProjectsByWorkspace(projects, "")
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}

// TestFilterProjectsByWorkspace_MatchesWorkspace filters to correct workspace.
func TestFilterProjectsByWorkspace_MatchesWorkspace(t *testing.T) {
	projects := []*orchestrator.Project{
		{ID: "a", WorkspaceID: "ws-1"},
		{ID: "b", WorkspaceID: "ws-2"},
		{ID: "c", WorkspaceID: "ws-1"},
	}
	got := filterProjectsByWorkspace(projects, "ws-1")
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "c" {
		t.Errorf("got IDs [%s, %s], want [a, c]", got[0].ID, got[1].ID)
	}
}

// TestProjectInList_EmptyProjectID always returns true.
func TestProjectInList_EmptyProjectID(t *testing.T) {
	projects := []*orchestrator.Project{{ID: "a"}}
	if !projectInList(projects, "") {
		t.Error("empty projectID should always return true")
	}
}

// TestProjectInList_Found returns true when project is in list.
func TestProjectInList_Found(t *testing.T) {
	projects := []*orchestrator.Project{{ID: "a"}, {ID: "b"}}
	if !projectInList(projects, "a") {
		t.Error("project a should be found")
	}
}

// TestProjectInList_NotFound returns false when project is not in list.
func TestProjectInList_NotFound(t *testing.T) {
	projects := []*orchestrator.Project{{ID: "a"}}
	if projectInList(projects, "x") {
		t.Error("project x should not be found")
	}
}
