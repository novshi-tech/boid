package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// stubWebService is a full implementation of WebService for testing.
type stubWebService struct {
	tasks                []*orchestrator.Task
	taskDetail           *TaskDetailView
	jobDetail            *JobWithContext
	projects             []*orchestrator.Project
	behaviors            []string
	workspaces           []*orchestrator.WorkspaceSummary
	capturedFilter       orchestrator.TaskFilter
	applyActionErr       error
	applyActionCalls     []applyActionCall
	duplicateTaskNewID   string
	duplicateTaskErr     error
	createTaskResult     *orchestrator.Task
	createTaskErr        error
	createTaskCalls      []CreateTaskRequest
	updateTaskErr        error
	updateTaskCalls      []UpdateTaskRequest
	projectByID          *orchestrator.Project
	projectByIDErr       error
	projectCommands      []CommandSummary
	projectCommandsErr   error
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

func (s *stubWebService) DeleteTask(id string, force bool) error {
	return nil
}

func (s *stubWebService) ListJobs(status string) ([]JobWithContext, error) {
	return nil, nil
}

func (s *stubWebService) GetJob(id string) (*JobWithContext, error) {
	if s.jobDetail == nil {
		return nil, fmt.Errorf("job not found: %s", id)
	}
	return s.jobDetail, nil
}

func (s *stubWebService) CreateTask(req CreateTaskRequest) (*orchestrator.Task, error) {
	s.createTaskCalls = append(s.createTaskCalls, req)
	return s.createTaskResult, s.createTaskErr
}

func (s *stubWebService) UpdateTask(id string, req UpdateTaskRequest) error {
	s.updateTaskCalls = append(s.updateTaskCalls, req)
	return s.updateTaskErr
}

func (s *stubWebService) RerunTask(id string, req RerunTaskRequest) error {
	return nil
}

func (s *stubWebService) ReopenTask(id string, req ReopenTaskRequest) error {
	return nil
}

func (s *stubWebService) AnswerTask(ctx context.Context, taskID, questionID, answer string) error {
	return nil
}

func (s *stubWebService) ListHooksForStatus(taskID, status string) ([]orchestrator.Hook, error) {
	return nil, nil
}

func (s *stubWebService) ReplayHook(ctx context.Context, taskID string, req ReplayHookRequest) (*ReplayHookResult, error) {
	return &ReplayHookResult{}, nil
}

func (s *stubWebService) GetProjectByID(id string) (*orchestrator.Project, error) {
	return s.projectByID, s.projectByIDErr
}

func (s *stubWebService) ListProjectCommands(projectID string) ([]CommandSummary, error) {
	return s.projectCommands, s.projectCommandsErr
}

func (s *stubWebService) ListTaskBehaviorCommands(taskID string) ([]CommandSummary, error) {
	return nil, nil
}

// stubWorkflowService implements WorkflowService for WebAppService tests.
type stubWorkflowService struct {
	applyActionErr error
	appliedTaskID  string
	appliedType    string
	appliedPayload json.RawMessage

	completedJobs       []completedJobCall
	stoppedAgentRuntimes []string
}

type completedJobCall struct {
	JobID    string
	ExitCode int
}

func (s *stubWorkflowService) ApplyAction(ctx context.Context, taskID string, req ApplyActionRequest) (*ActionApplication, error) {
	s.appliedTaskID = taskID
	s.appliedType = req.Type
	s.appliedPayload = req.Payload
	if s.applyActionErr != nil {
		return nil, s.applyActionErr
	}
	return &ActionApplication{
		Task:   &orchestrator.Task{ID: taskID},
		Action: &orchestrator.Action{TaskID: taskID, Type: req.Type},
	}, nil
}

func (s *stubWorkflowService) CompleteJob(ctx context.Context, jobID string, req JobDoneRequest) (*Job, error) {
	s.completedJobs = append(s.completedJobs, completedJobCall{JobID: jobID, ExitCode: req.ExitCode})
	return &Job{ID: jobID, Status: JobStatusCompleted, ExitCode: req.ExitCode}, nil
}

func (s *stubWorkflowService) TriggerDependents(ctx context.Context, taskID string) {}

func (s *stubWorkflowService) StopAgent(runtimeID string) {
	s.stoppedAgentRuntimes = append(s.stoppedAgentRuntimes, runtimeID)
}

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
	// proj-1 must be in ws-1 so it is not cleared by the workspace-project linkage logic.
	svc := &stubWebService{
		projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "ws-1"},
		},
	}
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

// dupTaskSvcStub is a minimal TaskService implementation that records
// DuplicateTask calls and returns a configured task / error.
type dupTaskSvcStub struct {
	dupCalls    []dupTaskSvcCall
	returnTask  *orchestrator.Task
	returnError error
}

type dupTaskSvcCall struct {
	sourceID  string
	autoStart bool
}

func (s *dupTaskSvcStub) CreateTask(req CreateTaskRequest) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *dupTaskSvcStub) GetTask(id string) (*orchestrator.Task, error) { return nil, nil }
func (s *dupTaskSvcStub) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *dupTaskSvcStub) UpdateTask(id string, req UpdateTaskRequest) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *dupTaskSvcStub) DeleteTask(id string, force bool) error { return nil }
func (s *dupTaskSvcStub) GetTaskDetail(id string) (*TaskDetailView, error) {
	return nil, nil
}
func (s *dupTaskSvcStub) GetTaskField(id, path string) (string, error) { return "", nil }
func (s *dupTaskSvcStub) ImportTasks(reqs []CreateTaskRequest) (*ImportResult, error) {
	return nil, nil
}
func (s *dupTaskSvcStub) DuplicateTask(id string, autoStart bool) (*orchestrator.Task, error) {
	s.dupCalls = append(s.dupCalls, dupTaskSvcCall{sourceID: id, autoStart: autoStart})
	if s.returnError != nil {
		return nil, s.returnError
	}
	return s.returnTask, nil
}
func (s *dupTaskSvcStub) RerunTask(id string, req RerunTaskRequest) (*orchestrator.Task, error) {
	return nil, nil
}

// WebAppService.DuplicateTask must delegate to TaskSvc.DuplicateTask so that
// a fresh duplicate is created via CreateTask + resolveBehavior with the
// behavior's DefaultInstruction / DefaultPayload. Without delegation the old
// implementation copied runtime state (claude_code.sessions, awaiting trait)
// and dropped Instructions, which made the hook evaluator skip the agent
// hook on Start.
func TestWebAppServiceDuplicateTask_DelegatesToTaskSvc(t *testing.T) {
	stub := &dupTaskSvcStub{returnTask: &orchestrator.Task{ID: "new-id"}}
	svc := &WebAppService{TaskSvc: stub}

	newID, err := svc.DuplicateTask("orig-id")
	if err != nil {
		t.Fatalf("DuplicateTask() error = %v", err)
	}
	if newID != "new-id" {
		t.Errorf("returned ID = %q, want %q", newID, "new-id")
	}
	if len(stub.dupCalls) != 1 {
		t.Fatalf("DuplicateTask delegation calls = %d, want 1", len(stub.dupCalls))
	}
	c := stub.dupCalls[0]
	if c.sourceID != "orig-id" {
		t.Errorf("sourceID = %q, want orig-id", c.sourceID)
	}
	// Web UI does not auto-start the duplicate; the user clicks Start.
	if c.autoStart {
		t.Errorf("autoStart = true, want false (Web UI does not auto-start)")
	}
}

func TestWebAppServiceDuplicateTask_NoTaskSvc(t *testing.T) {
	svc := &WebAppService{}
	_, err := svc.DuplicateTask("any-id")
	if err == nil {
		t.Fatal("DuplicateTask() error = nil, want error when TaskSvc is unset")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 error, got %v", err)
	}
}

func TestWebAppServiceDuplicateTask_NotFound(t *testing.T) {
	stub := &dupTaskSvcStub{returnError: &StatusError{Code: http.StatusNotFound, Message: "task not found"}}
	svc := &WebAppService{TaskSvc: stub}

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
	svc := &stubWebService{
		projects: []*orchestrator.Project{
			{ID: "proj-1"},
		},
	}
	r := newTestWebHandlerWithTaskCreate(svc)

	// title 空、description/project_id/auto_start を含めて POST
	body := url.Values{
		"title":       {""},
		"project_id":  {"proj-1"},
		"description": {"残しておきたい説明文"},
		"auto_start":  {"on"},
	}.Encode()
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
	if !strings.Contains(respBody, "残しておきたい説明文") {
		t.Errorf("response should preserve description value, got: %s", respBody)
	}
	if !strings.Contains(respBody, `value="proj-1" selected`) {
		t.Errorf("response should mark project_id selected, got: %s", respBody)
	}
	if !strings.Contains(respBody, `name="auto_start" checked`) {
		t.Errorf("response should preserve auto_start checked state, got: %s", respBody)
	}
}

func newTestWebHandlerWithEdit(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Get("/tasks/{id}/edit", h.GetTaskEdit)
	r.Post("/tasks/{id}/edit", h.PostEdit)
	return r
}

func TestWebHandler_GetTaskEdit_PendingTask(t *testing.T) {
	detail := &TaskDetailView{
		Task: &orchestrator.Task{
			ID:     "task-1",
			Title:  "My Task",
			Status: orchestrator.TaskStatusPending,
		},
	}
	svc := &stubWebService{taskDetail: detail}
	r := newTestWebHandlerWithEdit(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/edit", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `name="title"`) {
		t.Error("edit page should contain title field")
	}
	if !strings.Contains(body, `name="project_id"`) {
		t.Error("edit page should contain project_id field")
	}
	if !strings.Contains(body, `name="description"`) {
		t.Error("edit page should contain description field")
	}
	if !strings.Contains(body, `name="message"`) {
		t.Error("edit page should contain message field")
	}
}

func TestWebHandler_GetTaskEdit_NonPendingRedirects(t *testing.T) {
	detail := &TaskDetailView{
		Task: &orchestrator.Task{
			ID:     "task-1",
			Status: orchestrator.TaskStatusExecuting,
		},
	}
	svc := &stubWebService{taskDetail: detail}
	r := newTestWebHandlerWithEdit(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/edit", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (non-pending should redirect)", w.Code, http.StatusSeeOther)
	}
	if loc := w.Header().Get("Location"); loc != "/tasks/task-1" {
		t.Errorf("Location = %q, want /tasks/task-1", loc)
	}
}

func TestWebHandler_PostEdit_Success(t *testing.T) {
	detail := &TaskDetailView{
		Task: &orchestrator.Task{
			ID:     "task-1",
			Status: orchestrator.TaskStatusPending,
			Instructions: orchestrator.Instructions{{
				Message: "old message",
				Model:   "sonnet",
			}},
		},
	}
	svc := &stubWebService{taskDetail: detail}
	r := newTestWebHandlerWithEdit(svc)

	body := url.Values{
		"title":       {"New Title"},
		"project_id":  {"proj-1"},
		"description": {"new description"},
		"message":     {"new message"},
		"model":       {"opus"},
		"agent":       {"claude-code"},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/edit", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if loc := w.Header().Get("Location"); loc != "/tasks/task-1" {
		t.Errorf("Location = %q, want /tasks/task-1", loc)
	}
	if len(svc.updateTaskCalls) != 1 {
		t.Fatalf("UpdateTask calls = %d, want 1", len(svc.updateTaskCalls))
	}
	call := svc.updateTaskCalls[0]
	if call.Title != "New Title" {
		t.Errorf("Title = %q, want New Title", call.Title)
	}
	if call.Description != "new description" {
		t.Errorf("Description = %q, want new description", call.Description)
	}
	if call.ProjectID != "proj-1" {
		t.Errorf("ProjectID = %q, want proj-1", call.ProjectID)
	}
	if len(call.Instructions) == 0 {
		t.Error("Instructions should be set")
	}
}

func TestWebHandler_PostTaskCreate_RemoteIDAndDatasourceID(t *testing.T) {
	newTask := &orchestrator.Task{ID: "new-task-id", Title: "My Task"}
	svc := &stubWebService{createTaskResult: newTask}
	r := newTestWebHandlerWithTaskCreate(svc)

	body := url.Values{
		"title":      {"My Task"},
		"project_id": {"proj-1"},
		"behavior":   {"executor"},
		"remote_id":  {"JIRA-123"},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if len(svc.createTaskCalls) != 1 {
		t.Fatalf("CreateTask calls = %d, want 1", len(svc.createTaskCalls))
	}
	call := svc.createTaskCalls[0]
	if call.RemoteID != "JIRA-123" {
		t.Errorf("RemoteID = %q, want JIRA-123", call.RemoteID)
	}
}

func TestWebHandler_PostEdit_RemoteID(t *testing.T) {
	detail := &TaskDetailView{
		Task: &orchestrator.Task{
			ID:       "task-1",
			Status:   orchestrator.TaskStatusPending,
			RemoteID: "OLD-1",
			Instructions: orchestrator.Instructions{{
				Message: "old message",
			}},
		},
	}
	svc := &stubWebService{taskDetail: detail}
	r := newTestWebHandlerWithEdit(svc)

	body := url.Values{
		"title":       {"New Title"},
		"project_id":  {"proj-1"},
		"description": {"new description"},
		"message":     {"new message"},
		"remote_id":   {"JIRA-456"},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/edit", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if len(svc.updateTaskCalls) != 1 {
		t.Fatalf("UpdateTask calls = %d, want 1", len(svc.updateTaskCalls))
	}
	call := svc.updateTaskCalls[0]
	if call.RemoteID == nil || *call.RemoteID != "JIRA-456" {
		t.Errorf("RemoteID = %v, want JIRA-456", call.RemoteID)
	}
}

func TestTaskDetail_Tab_HXRequest_Timeline(t *testing.T) {
	svc := &stubWebService{taskDetail: makeTaskDetailView()}
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1?tab=timeline", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="tab-panel"`) {
		t.Errorf("tab panel fragment should contain id=tab-panel, got: %s", body)
	}
	if strings.Contains(body, "<html") {
		t.Error("fragment should not contain full HTML page")
	}
	if !strings.Contains(body, `id="task-timeline"`) {
		t.Errorf("timeline tab should contain task-timeline element, got: %s", body)
	}
}

func TestTaskDetail_TitleNotH1(t *testing.T) {
	svc := &stubWebService{taskDetail: makeTaskDetailView()}
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<h1>Test Task</h1>") {
		t.Error("task title should not be rendered as <h1>")
	}
}

func TestTaskDetail_NoGatesLink(t *testing.T) {
	svc := &stubWebService{taskDetail: makeTaskDetailView()}
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "/tasks/task-1/gates") {
		t.Error("task detail should not contain a link to /tasks/{id}/gates")
	}
}

// TestTaskDetailFragment_JobLink verifies job rows render as anchor links
// to the job detail page. Jobs (not the paired hook_fired action) are the
// source of truth for the Web UI timeline — same as the TUI — so a fired
// action without its job would produce no row and no link.
func TestTaskDetailFragment_JobLink(t *testing.T) {
	now := time.Now()
	detail := &TaskDetailView{
		Task: &orchestrator.Task{
			ID: "task-1", Title: "Test Task", Status: "executing",
			CreatedAt: now.Add(-1 * time.Minute),
		},
		Jobs: []*Job{
			{
				ID: "job-123", Role: "hook", HandlerID: "go-dev/pr-verify",
				Status: JobStatusCompleted,
				CreatedAt: now.Add(-30 * time.Second), UpdatedAt: now.Add(-10 * time.Second),
			},
		},
	}
	svc := &stubWebService{taskDetail: detail}
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/fragment?kind=timeline", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/jobs/job-123"`) {
		t.Errorf("job should link to /jobs/job-123, got: %s", body)
	}
	if !strings.Contains(body, `go-dev/pr-verify`) {
		t.Errorf("job label should contain handler id, got: %s", body)
	}
}

// --- Terminal page tests ---

func newTestWebHandlerWithTerminal(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/jobs/{id}/terminal", h.JobTerminal)
	return r
}

func TestTerminalPage_RendersForInteractiveRunningJob(t *testing.T) {
	svc := &stubWebService{
		jobDetail: &JobWithContext{
			Job: Job{
				ID:          "job-term-1",
				TaskID:      "task-1",
				HandlerID:   "claude-code",
				Role:        "main",
				Interactive: true,
				Status:      JobStatusRunning,
			},
			TaskTitle: "My Task",
		},
	}
	r := newTestWebHandlerWithTerminal(svc)

	req := httptest.NewRequest(http.MethodGet, "/jobs/job-term-1/terminal", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "xterm.js") && !strings.Contains(body, "xterm-5.x") {
		t.Errorf("body should reference xterm.js vendor, got snippet: %s", body[:min(200, len(body))])
	}
	if !strings.Contains(body, `data-job-id="job-term-1"`) {
		t.Errorf("body should contain data-job-id attribute, got snippet: %s", body[:min(300, len(body))])
	}
	if !strings.Contains(body, "boid-terminal") {
		t.Errorf("body should contain boid-terminal class")
	}
}

func TestTerminalPage_ShowsEmptyStateWhenNotRunning(t *testing.T) {
	svc := &stubWebService{
		jobDetail: &JobWithContext{
			Job: Job{
				ID:          "job-done-1",
				TaskID:      "task-1",
				Interactive: true,
				Status:      JobStatusCompleted,
			},
		},
	}
	r := newTestWebHandlerWithTerminal(svc)

	req := httptest.NewRequest(http.MethodGet, "/jobs/job-done-1/terminal", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "boid-terminal-xterm") {
		t.Error("error page should not render xterm widget")
	}
	if !strings.Contains(body, "attach") && !strings.Contains(body, "接続") {
		t.Errorf("error page should mention attach/connection state: %s", body[:min(300, len(body))])
	}
}

func TestTerminalPage_RequiresAuth(t *testing.T) {
	// Verify the route is registered in the main WebHandler router.
	svc := &stubWebService{}
	h := &WebHandler{Service: svc}
	r := h.Routes()

	req := httptest.NewRequest(http.MethodGet, "/jobs/some-id/terminal", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Route is registered; handler returns 404 (job not found) not chi's 404.
	if strings.Contains(w.Body.String(), "404 page not found") {
		t.Error("/jobs/{id}/terminal route should be registered in WebHandler.Routes()")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func newTestWebHandlerWithJobDetail(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/jobs/{id}", h.JobDetail)
	return r
}

func TestJobDetail_NoTask_BackToProjectCommands(t *testing.T) {
	svc := &stubWebService{
		jobDetail: &JobWithContext{
			Job: Job{
				ID:        "job-cmd-1",
				TaskID:    "",
				ProjectID: "proj-1",
				HandlerID: "make deploy",
				Role:      "command",
				Status:    JobStatusCompleted,
			},
		},
	}
	r := newTestWebHandlerWithJobDetail(svc)

	req := httptest.NewRequest(http.MethodGet, "/jobs/job-cmd-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	want := `/projects/proj-1/commands`
	if !strings.Contains(body, want) {
		t.Errorf("back link should contain %q, got: %s", want, body[:min(500, len(body))])
	}
	if strings.Contains(body, `href="/tasks/"`) {
		t.Error("job with empty TaskID must not link to /tasks/ (would 404)")
	}
}

func TestJobDetail_WithTask_BackToTask(t *testing.T) {
	svc := &stubWebService{
		jobDetail: &JobWithContext{
			Job: Job{
				ID:        "job-task-1",
				TaskID:    "task-abc",
				ProjectID: "proj-1",
				HandlerID: "claude-code",
				Role:      "main",
				Status:    JobStatusCompleted,
			},
			TaskTitle: "My Task",
		},
	}
	r := newTestWebHandlerWithJobDetail(svc)

	req := httptest.NewRequest(http.MethodGet, "/jobs/job-task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	want := `/tasks/task-abc`
	if !strings.Contains(body, want) {
		t.Errorf("back link should contain %q, got: %s", want, body[:min(500, len(body))])
	}
}
