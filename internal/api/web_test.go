package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// stubWebService is a full implementation of WebService for testing.
type stubWebService struct {
	tasks              []*orchestrator.Task
	taskDetail         *TaskDetailView
	projects           []*orchestrator.Project
	behaviors          []string
	workspaces         []*orchestrator.WorkspaceSummary
	capturedFilter     orchestrator.TaskFilter
	applyActionErr     error
	applyActionCalls   []applyActionCall
	duplicateTaskNewID string
	duplicateTaskErr   error
	createTaskResult   *orchestrator.Task
	createTaskErr      error
	updateTaskErr      error
	updateTaskCalls    []UpdateTaskRequest
}

type applyActionCall struct {
	taskID     string
	actionType string
}

func (s *stubWebService) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	s.capturedFilter = filter
	return s.tasks, nil
}

func (s *stubWebService) GetTaskDetail(id string) (*TaskDetailView, error) {
	if s.taskDetail == nil {
		return nil, fmt.Errorf("task not found: %s", id)
	}
	return s.taskDetail, nil
}

func (s *stubWebService) ListProjects() ([]*orchestrator.Project, error) {
	return s.projects, nil
}

func (s *stubWebService) ListBehaviors() ([]string, error) {
	return s.behaviors, nil
}

func (s *stubWebService) ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error) {
	return s.workspaces, nil
}

func (s *stubWebService) ApplyAction(taskID string, actionType string) error {
	s.applyActionCalls = append(s.applyActionCalls, applyActionCall{taskID: taskID, actionType: actionType})
	return s.applyActionErr
}

func (s *stubWebService) DuplicateTask(id string) (string, error) {
	return s.duplicateTaskNewID, s.duplicateTaskErr
}

func (s *stubWebService) ListJobs(status string) ([]JobWithContext, error) {
	return nil, nil
}

func (s *stubWebService) GetJob(id string) (*JobWithContext, error) {
	return nil, fmt.Errorf("job not found: %s", id)
}

func (s *stubWebService) CreateTask(req CreateTaskRequest) (*orchestrator.Task, error) {
	return s.createTaskResult, s.createTaskErr
}

func (s *stubWebService) UpdateTask(id string, req UpdateTaskRequest) error {
	s.updateTaskCalls = append(s.updateTaskCalls, req)
	return s.updateTaskErr
}

func (s *stubWebService) RerunTask(id string, req RerunTaskRequest) error {
	return nil
}

func (s *stubWebService) ListGatesForStatus(taskID, status string) ([]orchestrator.Gate, error) {
	return nil, nil
}

func (s *stubWebService) ReplayGate(ctx context.Context, taskID string, req ReplayGateRequest) (*ReplayGateResult, error) {
	return &ReplayGateResult{}, nil
}

// stubWorkflowService implements WorkflowService for WebAppService tests.
type stubWorkflowService struct {
	applyActionErr error
	appliedTaskID  string
	appliedType    string
}

func (s *stubWorkflowService) ApplyAction(ctx context.Context, taskID string, req ApplyActionRequest) (*ActionApplication, error) {
	s.appliedTaskID = taskID
	s.appliedType = req.Type
	if s.applyActionErr != nil {
		return nil, s.applyActionErr
	}
	return &ActionApplication{
		Task:   &orchestrator.Task{ID: taskID},
		Action: &orchestrator.Action{TaskID: taskID, Type: req.Type},
	}, nil
}

func (s *stubWorkflowService) CompleteJob(ctx context.Context, jobID string, req JobDoneRequest) (*Job, error) {
	return nil, nil
}

func (s *stubWorkflowService) TriggerDependents(ctx context.Context, taskID string) {}

func TestWebAppServiceApplyAction_Success(t *testing.T) {
	workflow := &stubWorkflowService{}
	svc := &WebAppService{
		Tasks:    &stubTaskStore{},
		Workflow: workflow,
	}

	if err := svc.ApplyAction("task-1", "start"); err != nil {
		t.Fatalf("ApplyAction() error = %v", err)
	}
	if workflow.appliedTaskID != "task-1" {
		t.Errorf("appliedTaskID = %q, want %q", workflow.appliedTaskID, "task-1")
	}
	if workflow.appliedType != "start" {
		t.Errorf("appliedType = %q, want %q", workflow.appliedType, "start")
	}
}

func TestWebAppServiceApplyAction_NoWorkflow(t *testing.T) {
	svc := &WebAppService{}

	err := svc.ApplyAction("task-1", "start")
	if err == nil {
		t.Fatal("ApplyAction() error = nil, want error")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusInternalServerError {
		t.Fatalf("expected StatusInternalServerError, got %v", err)
	}
}

func TestWebAppServiceApplyAction_WorkflowError(t *testing.T) {
	workflow := &stubWorkflowService{applyActionErr: fmt.Errorf("invalid transition")}
	svc := &WebAppService{
		Tasks:    &stubTaskStore{},
		Workflow: workflow,
	}

	err := svc.ApplyAction("task-1", "start")
	if err == nil {
		t.Fatal("ApplyAction() error = nil, want error")
	}
}

func newTestWebHandler(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Get("/tasks/{id}/fragment", h.TaskDetailFragment)
	r.Post("/tasks/{id}/action", h.PostAction)
	r.Post("/tasks/{id}/duplicate", h.PostDuplicate)
	return r
}

func newTestWebHandlerWithTaskList(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/", h.TaskList)
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Post("/tasks/{id}/action", h.PostAction)
	r.Post("/tasks/{id}/duplicate", h.PostDuplicate)
	return r
}

func TestWebHandlerTaskList_FiltersMappedToTaskFilter(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerWithTaskList(svc)

	req := httptest.NewRequest(http.MethodGet, "/?status=executing&project=proj-1&behavior=dev&workspace=ws-1&q=myquery", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := svc.capturedFilter
	if got.Status != "executing" {
		t.Errorf("Status = %q, want executing", got.Status)
	}
	if got.ProjectID != "proj-1" {
		t.Errorf("ProjectID = %q, want proj-1", got.ProjectID)
	}
	if got.Behavior != "dev" {
		t.Errorf("Behavior = %q, want dev", got.Behavior)
	}
	if got.WorkspaceID != "ws-1" {
		t.Errorf("WorkspaceID = %q, want ws-1", got.WorkspaceID)
	}
	if got.Title != "myquery" {
		t.Errorf("Title = %q, want myquery", got.Title)
	}
}

func TestWebHandlerTaskList_HXRequestReturnsFragment(t *testing.T) {
	svc := &stubWebService{
		tasks: []*orchestrator.Task{
			{ID: "t-1", Title: "hello", Status: "executing"},
		},
	}
	r := newTestWebHandlerWithTaskList(svc)

	req := httptest.NewRequest(http.MethodGet, "/?status=executing", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="task-list"`) {
		t.Errorf("fragment should contain task-list div, got: %s", body)
	}
	if strings.Contains(body, "<html") {
		t.Errorf("fragment should not contain full HTML page")
	}
}

func TestWebHandlerTaskList_FullPageWithoutHXRequest(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerWithTaskList(svc)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<html") {
		t.Errorf("full page should contain html element")
	}
	if !strings.Contains(body, `id="task-list"`) {
		t.Errorf("full page should contain task-list div")
	}
	if !strings.Contains(body, `id="filter-form"`) {
		t.Errorf("full page should contain filter-form")
	}
}

func TestWebHandlerPostAction_Success(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandler(svc)

	body := url.Values{"type": {"start"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/action", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if loc != "/tasks/task-1" {
		t.Errorf("Location = %q, want /tasks/task-1", loc)
	}
	if len(svc.applyActionCalls) != 1 {
		t.Fatalf("ApplyAction calls = %d, want 1", len(svc.applyActionCalls))
	}
	if svc.applyActionCalls[0].taskID != "task-1" || svc.applyActionCalls[0].actionType != "start" {
		t.Errorf("ApplyAction call = %+v", svc.applyActionCalls[0])
	}
}

func TestWebHandlerPostAction_MissingType(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandler(svc)

	body := url.Values{}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/action", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error param", loc)
	}
	if len(svc.applyActionCalls) != 0 {
		t.Errorf("ApplyAction should not be called when type is missing")
	}
}

func TestWebHandlerPostAction_ServiceError(t *testing.T) {
	svc := &stubWebService{applyActionErr: fmt.Errorf("cannot apply: wrong status")}
	r := newTestWebHandler(svc)

	body := url.Values{"type": {"abort"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/action", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error param", loc)
	}
	if !strings.Contains(loc, "/tasks/task-1") {
		t.Errorf("Location = %q, want redirect to task detail", loc)
	}
}

func TestWebHandlerPostDuplicate_Success(t *testing.T) {
	svc := &stubWebService{duplicateTaskNewID: "new-task-id"}
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/duplicate", nil)
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if loc != "/tasks/new-task-id" {
		t.Errorf("Location = %q, want /tasks/new-task-id", loc)
	}
}

func TestWebHandler_RemovedRoutes_Return404(t *testing.T) {
	svc := &stubWebService{}
	h := &WebHandler{Service: svc}
	r := h.Routes()

	for _, path := range []string{"/jobs", "/projects"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", path, w.Code)
		}
	}
}

func TestWebHandler_JobDetail_RouteStillRegistered(t *testing.T) {
	svc := &stubWebService{}
	h := &WebHandler{Service: svc}
	r := h.Routes()

	req := httptest.NewRequest(http.MethodGet, "/jobs/some-id", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// ルートは登録されている (handler の 404 であり chi の 404 page not found ではない)
	if strings.Contains(w.Body.String(), "404 page not found") {
		t.Error("/jobs/{id} route should be registered; got chi 404 instead of handler response")
	}
}

func TestWebHandlerPostDuplicate_Error(t *testing.T) {
	svc := &stubWebService{duplicateTaskErr: fmt.Errorf("task not found")}
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/duplicate", nil)
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/tasks/task-1") {
		t.Errorf("Location = %q, want redirect to original task", loc)
	}
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error param", loc)
	}
}

func TestWebAppServiceDuplicateTask_Success(t *testing.T) {
	original := &orchestrator.Task{
		ID:           "orig-id",
		ProjectID:    "proj-1",
		Title:        "My Task",
		Description:  "desc",
		Behavior: "dev",
		Traits:   []string{"trait1"},
		Readonly:     false,
		Worktree:     true,
		BranchPrefix: "feature/",
		BaseBranch:   "main",
	}
	store := &stubTaskStore{task: original}
	svc := &WebAppService{Tasks: store}

	newID, err := svc.DuplicateTask("orig-id")
	if err != nil {
		t.Fatalf("DuplicateTask() error = %v", err)
	}
	if newID == "" {
		t.Error("DuplicateTask() returned empty ID")
	}
}

func TestWebAppServiceDuplicateTask_NotFound(t *testing.T) {
	store := &stubTaskStore{err: fmt.Errorf("task not found")}
	svc := &WebAppService{Tasks: store}

	_, err := svc.DuplicateTask("missing-id")
	if err == nil {
		t.Fatal("DuplicateTask() error = nil, want error")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected StatusNotFound, got %v", err)
	}
}

func makeTaskDetailView() *TaskDetailView {
	return &TaskDetailView{
		Task: &orchestrator.Task{
			ID:       "task-1",
			Title:    "Test Task",
			Status:   "executing",
			Behavior: "dev",
		},
		Actions:          []*orchestrator.Action{{Type: "start", FromStatus: "pending", ToStatus: "executing"}},
		Jobs:             []*Job{{ID: "job-1", Role: "main", Status: JobStatusRunning}},
		AvailableActions: []string{"abort"},
	}
}

func TestTaskDetailFragment_Timeline(t *testing.T) {
	svc := &stubWebService{taskDetail: makeTaskDetailView()}
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/fragment?kind=timeline", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="task-timeline"`) {
		t.Errorf("timeline fragment should contain task-timeline element, got: %s", body)
	}
	if strings.Contains(body, "<html") {
		t.Error("fragment should not contain full HTML page")
	}
}

func TestTaskDetailFragment_Status(t *testing.T) {
	svc := &stubWebService{taskDetail: makeTaskDetailView()}
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/fragment?kind=status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="task-status"`) {
		t.Errorf("status fragment should contain task-status element, got: %s", body)
	}
	if strings.Contains(body, "<html") {
		t.Error("fragment should not contain full HTML page")
	}
	if !strings.Contains(body, "executing") {
		t.Errorf("status fragment should contain current status badge, got: %s", body)
	}
}

func TestTaskDetailFragment_Jobs(t *testing.T) {
	svc := &stubWebService{taskDetail: makeTaskDetailView()}
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/fragment?kind=jobs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="task-jobs"`) {
		t.Errorf("jobs fragment should contain task-jobs element, got: %s", body)
	}
	if strings.Contains(body, "<html") {
		t.Error("fragment should not contain full HTML page")
	}
}

func TestTaskDetailFragment_UnknownKind(t *testing.T) {
	svc := &stubWebService{taskDetail: makeTaskDetailView()}
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/fragment?kind=unknown", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestTaskDetailFragment_TaskNotFound(t *testing.T) {
	svc := &stubWebService{} // taskDetail is nil → GetTaskDetail returns error
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/missing/fragment?kind=timeline", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func newTestWebHandlerWithTaskCreate(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/new", h.TaskNew)
	r.Post("/tasks", h.PostTaskCreate)
	r.Get("/tasks/{id}", h.TaskDetail)
	return r
}

func TestWebHandler_TaskNew_Renders(t *testing.T) {
	svc := &stubWebService{
		projects: []*orchestrator.Project{
			{ID: "proj-1"},
		},
	}
	r := newTestWebHandlerWithTaskCreate(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/new", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<html") {
		t.Error("should return full HTML page")
	}
	if !strings.Contains(body, `name="title"`) {
		t.Error("form should contain title field")
	}
	if !strings.Contains(body, `name="project_id"`) {
		t.Error("form should contain project_id field")
	}
	if !strings.Contains(body, `name="behavior"`) {
		t.Error("form should contain behavior field")
	}
	if !strings.Contains(body, `name="description"`) {
		t.Error("form should contain description field")
	}
	if !strings.Contains(body, `name="auto_start"`) {
		t.Error("form should contain auto_start field")
	}
}

func TestWebHandler_PostTaskCreate_Success(t *testing.T) {
	newTask := &orchestrator.Task{ID: "new-task-id", Title: "My Task"}
	svc := &stubWebService{createTaskResult: newTask}
	r := newTestWebHandlerWithTaskCreate(svc)

	body := url.Values{"title": {"My Task"}, "project_id": {"proj-1"}, "behavior": {"dev"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if loc != "/tasks/new-task-id" {
		t.Errorf("Location = %q, want /tasks/new-task-id", loc)
	}
}

func TestWebHandler_PostTaskCreate_ValidationError(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerWithTaskCreate(svc)

	// title 空
	body := url.Values{"title": {""}, "project_id": {"proj-1"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, "タイトルは必須") {
		t.Errorf("response should contain error message, got: %s", respBody)
	}
}

func newTestWebHandlerWithEditDescription(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Get("/tasks/{id}/edit/description", h.EditDescription)
	r.Post("/tasks/{id}/edit/description", h.PostEditDescription)
	return r
}

func TestWebHandler_EditDescription_Renders(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{
			Task: &orchestrator.Task{
				ID:          "task-1",
				Title:       "My Task",
				Description: "current description text",
				Status:      "pending",
			},
		},
	}
	r := newTestWebHandlerWithEditDescription(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/edit/description", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<html") {
		t.Error("should return full HTML page")
	}
	if !strings.Contains(body, `name="description"`) {
		t.Error("form should contain description textarea")
	}
	if !strings.Contains(body, "current description text") {
		t.Error("textarea should contain current description value")
	}
}

func TestWebHandler_PostEditDescription_Success(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerWithEditDescription(svc)

	body := url.Values{"description": {"updated description"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/edit/description", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if loc != "/tasks/task-1" {
		t.Errorf("Location = %q, want /tasks/task-1", loc)
	}
	if len(svc.updateTaskCalls) != 1 {
		t.Fatalf("UpdateTask calls = %d, want 1", len(svc.updateTaskCalls))
	}
	if svc.updateTaskCalls[0].Description != "updated description" {
		t.Errorf("UpdateTask description = %q, want %q", svc.updateTaskCalls[0].Description, "updated description")
	}
}

func TestWebHandler_PostEditDescription_Empty(t *testing.T) {
	// 空文字 POST は UpdateTask を呼び出すが、サービス内で空文字は無視されるため成功扱い
	svc := &stubWebService{}
	r := newTestWebHandlerWithEditDescription(svc)

	body := url.Values{"description": {""}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/edit/description", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (empty description should redirect)", w.Code, http.StatusSeeOther)
	}
	if len(svc.updateTaskCalls) != 1 {
		t.Fatalf("UpdateTask calls = %d, want 1", len(svc.updateTaskCalls))
	}
	if svc.updateTaskCalls[0].Description != "" {
		t.Errorf("UpdateTask description = %q, want empty string", svc.updateTaskCalls[0].Description)
	}
}
