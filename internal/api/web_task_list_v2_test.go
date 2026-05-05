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

// TestWebTaskList_DefaultStatusIsOpen verifies that empty ?status= defaults to "open" filter.
func TestWebTaskList_DefaultStatusIsOpen(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if svc.capturedFilter.Status != "open" {
		t.Errorf("default Status = %q, want \"open\"", svc.capturedFilter.Status)
	}
}

// TestWebTaskList_OpenStatus verifies that ?status=open passes "open" to ListTasks.
func TestWebTaskList_OpenStatus(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/?status=open", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if svc.capturedFilter.Status != "open" {
		t.Errorf("Status = %q, want \"open\"", svc.capturedFilter.Status)
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

// TestWebTaskList_ClosedRendersFlat verifies that closed tasks are rendered as flat list
// (no parent-child nesting in HTML output — each task-tree-row has no margin-left indentation).
func TestWebTaskList_ClosedRendersFlat(t *testing.T) {
	parent := &orchestrator.Task{
		ID: "p1", Title: "Parent", Status: orchestrator.TaskStatusDone,
		ParentID: "", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	child := &orchestrator.Task{
		ID: "c1", Title: "Child", Status: orchestrator.TaskStatusDone,
		ParentID: "p1", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	svc := &stubWebService{tasks: []*orchestrator.Task{parent, child}}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/?status=closed", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	body := w.Body.String()
	// flat list: no margin-left: 16px (depth > 0)
	if strings.Contains(body, "margin-left: 16px") {
		t.Errorf("closed list should be flat (no indentation), but got margin-left: 16px in:\n%s", body)
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
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	svc := &stubWebService{tasks: []*orchestrator.Task{task}}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/?status=open", nil)
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

// TestWebTaskList_OpenClosedToggleInHTML verifies the open/closed toggle is present in HTML.
func TestWebTaskList_OpenClosedToggleInHTML(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerFull(svc)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	body := w.Body.String()
	// Hidden status input must be present (fix for status drop bug).
	if !strings.Contains(body, `name="status"`) {
		t.Errorf("body should contain hidden status input, got: %s", body)
	}
	// Open and Closed tab buttons must be present.
	if !strings.Contains(body, ">Open<") {
		t.Errorf("body should contain Open tab button, got: %s", body)
	}
	if !strings.Contains(body, ">Closed<") {
		t.Errorf("body should contain Closed tab button, got: %s", body)
	}
	// Old 7-button style should be gone.
	if strings.Contains(body, `value="pending"`) {
		t.Errorf("body should NOT contain pending button (old 7-button style), got: %s", body)
	}
	if strings.Contains(body, `value="executing"`) {
		t.Errorf("body should NOT contain executing button (old 7-button style), got: %s", body)
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

// TestBuildFlatItems verifies that BuildFlatItems returns items with Depth=0, HasChildren=false.
func TestBuildFlatItems(t *testing.T) {
	now := time.Now()
	parent := &orchestrator.Task{ID: "p", Title: "Parent", ParentID: "", CreatedAt: now, UpdatedAt: now}
	child := &orchestrator.Task{ID: "c", Title: "Child", ParentID: "p", CreatedAt: now, UpdatedAt: now}

	items := BuildFlatItems([]*orchestrator.Task{parent, child}, nil)

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	for i, item := range items {
		if item.Depth != 0 {
			t.Errorf("items[%d].Depth = %d, want 0", i, item.Depth)
		}
		if item.HasChildren {
			t.Errorf("items[%d].HasChildren = true, want false", i)
		}
		if item.ParentID != "" {
			t.Errorf("items[%d].ParentID = %q, want empty", i, item.ParentID)
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
