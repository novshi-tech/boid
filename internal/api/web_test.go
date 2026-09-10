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
	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// stubWebService is a full implementation of WebService for testing.
type stubWebService struct {
	tasks                 []*orchestrator.Task
	taskDetail            *TaskDetailView
	taskDetails           map[string]*TaskDetailView
	taskDetailErrs        map[string]error
	jobDetail             *JobWithContext
	projects              []*orchestrator.Project
	behaviors             []string
	workspaces            []*orchestrator.WorkspaceSummary
	capturedFilter        orchestrator.TaskFilter
	applyActionErr        error
	applyActionCalls      []applyActionCall
	duplicateTaskNewID    string
	duplicateTaskErr      error
	createTaskResult      *orchestrator.Task
	createTaskErr         error
	createTaskCalls       []CreateTaskRequest
	updateTaskErr         error
	updateTaskCalls       []UpdateTaskRequest
	projectByID           *orchestrator.Project
	projectByIDErr        error
	answerSuggestionErr   error
	answerSuggestionCalls []answerSuggestionCall
}

type answerSuggestionCall struct {
	taskID string
	req    AnswerSuggestionRequest
}

type applyActionCall struct {
	taskID     string
	actionType string
}

func (s *stubWebService) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	s.capturedFilter = filter
	if filter.ParentID == nil {
		return s.tasks, nil
	}
	// PR-2 (docs/plans/webui-detail-list-redesign.md §7): WebHandler.
	// cardChildrenFromTriage/execChildTree both filter by ParentID, and
	// tests exercising them (e.g. a multi-level exec child tree) need this
	// stub to behave like the real store.ListTasks — otherwise every level
	// of a recursive walk would see the whole unfiltered fixture and loop
	// forever, or a card's children query would pick up unrelated fixture
	// rows. PR-4 (§3.5) added a second caller: TaskList itself now ALWAYS
	// sets ParentID to the root scope ("") — see
	// TestWebTaskList_RootOnly/TestWebTaskList_ChildTaskNeverAppears
	// (web_task_list_v2_test.go) — so most fixtures in this file (which
	// default ParentID to "") pass through this same filtering unchanged;
	// only a fixture that deliberately sets a non-empty ParentID is affected.
	var out []*orchestrator.Task
	for _, t := range s.tasks {
		if t.ParentID == *filter.ParentID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *stubWebService) GetTaskDetail(id string) (*TaskDetailView, error) {
	if err := s.taskDetailErrs[id]; err != nil {
		return nil, err
	}
	if detail := s.taskDetails[id]; detail != nil {
		return detail, nil
	}
	if s.taskDetail == nil {
		return nil, fmt.Errorf("%w: %s", orchestrator.ErrTaskNotFound, id)
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

func (s *stubWebService) DuplicateTask(_ context.Context, id string) (string, error) {
	return s.duplicateTaskNewID, s.duplicateTaskErr
}

func (s *stubWebService) DeleteTask(id string, force bool) error {
	return nil
}

func (s *stubWebService) ListJobs(status string) ([]JobWithContext, error) {
	return nil, nil
}

func (s *stubWebService) ListSessions() ([]JobWithContext, error) {
	return nil, nil
}

func (s *stubWebService) GetJob(id string) (*JobWithContext, error) {
	if s.jobDetail == nil {
		return nil, fmt.Errorf("job not found: %s", id)
	}
	return s.jobDetail, nil
}

func (s *stubWebService) CreateTask(_ context.Context, req CreateTaskRequest) (*orchestrator.Task, error) {
	s.createTaskCalls = append(s.createTaskCalls, req)
	return s.createTaskResult, s.createTaskErr
}

func (s *stubWebService) UpdateTask(_ context.Context, id string, req UpdateTaskRequest) error {
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

func (s *stubWebService) AnswerSuggestion(taskID string, req AnswerSuggestionRequest) error {
	s.answerSuggestionCalls = append(s.answerSuggestionCalls, answerSuggestionCall{taskID: taskID, req: req})
	return s.answerSuggestionErr
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

// RunCardCommandAsHuman / CardCommandOptionsForProject: stubWebService
// itself is used by every non-card-command test in this package (task
// list/detail/answer/etc.), which never exercise the command UI — a fixed
// nil result matches CardHandler's own "not configured" posture and keeps
// every existing fixture from needing to know about card commands at all.
// Tests that DO exercise the command UI use their own dedicated fake (see
// web_card_command_test.go's cardCommandWebService).
func (s *stubWebService) RunCardCommandAsHuman(ctx context.Context, cardID, commandKey, instruction string) (*RunCardCommandResult, error) {
	return nil, &StatusError{Code: http.StatusNotImplemented, Message: "card commands not configured"}
}

func (s *stubWebService) CardCommandOptionsForProject(ctx context.Context, projectID string) []CardCommandOption {
	return nil
}

// stubWorkflowService implements WorkflowService for WebAppService tests.
type stubWorkflowService struct {
	applyActionErr error
	appliedTaskID  string
	appliedType    string
	appliedPayload json.RawMessage

	completedJobs        []completedJobCall
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

func (s *stubWorkflowService) GetCard(taskID string) (*CardView, error) {
	return &CardView{TaskID: taskID}, nil
}

func (s *stubWorkflowService) ListCards(orchestrator.TaskFilter) ([]*CardView, error) {
	return nil, nil
}

func (s *stubWorkflowService) CompleteJob(ctx context.Context, jobID string, req JobDoneRequest) (*Job, error) {
	s.completedJobs = append(s.completedJobs, completedJobCall{JobID: jobID, ExitCode: req.ExitCode})
	return &Job{ID: jobID, Status: JobStatusCompleted, ExitCode: req.ExitCode}, nil
}

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
	r.Post("/tasks/{id}/suggestion", h.PostAnswerSuggestion)
	r.Post("/tasks/{id}/duplicate", h.PostDuplicate)
	return r
}

// stubSessionDispatcher is a minimal SessionDispatcher for testing
// PostStartShapingSession (and could back PostStartSession tests too, but
// none exist yet).
type stubSessionDispatcher struct {
	result   *StartSessionResult
	err      error
	lastReq  StartSessionRequest
	callable bool
}

func (s *stubSessionDispatcher) StartSession(ctx context.Context, req StartSessionRequest) (*StartSessionResult, error) {
	s.callable = true
	s.lastReq = req
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func newTestWebHandlerWithSessionStart(dispatcher SessionDispatcher) *chi.Mux {
	h := &WebHandler{SessionDispatcher: dispatcher}
	r := chi.NewRouter()
	r.Post("/projects/{id}/sessions/start", h.PostStartSession)
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

// TestWebHandlerTaskList_SavesFilterToCookie covers the browser-side
// persistence request: after narrowing the list, the filter query should be
// remembered so a later plain "/" visit (no query at all) reapplies it
// without the user re-selecting anything.
func TestWebHandlerTaskList_SavesFilterToCookie(t *testing.T) {
	svc := &stubWebService{
		projects: []*orchestrator.Project{{ID: "proj-1", WorkspaceID: "ws-1"}},
	}
	r := newTestWebHandlerWithTaskList(svc)

	req := httptest.NewRequest(http.MethodGet, "/?status=executing&project=proj-1&workspace=ws-1&active=1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	res := w.Result()
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == taskFilterCookieName {
			cookie = c
			break
		}
	}
	if cookie == nil {
		t.Fatalf("expected Set-Cookie %q, got none: %v", taskFilterCookieName, res.Cookies())
	}
	got, err := url.ParseQuery(cookie.Value)
	if err != nil {
		t.Fatalf("cookie value not a valid query: %v", err)
	}
	if got.Get("status") != "executing" || got.Get("project") != "proj-1" || got.Get("workspace") != "ws-1" || got.Get("active") != "1" {
		t.Errorf("cookie value = %q, missing expected filter params", cookie.Value)
	}
	if cookie.MaxAge <= 0 {
		t.Errorf("MaxAge = %d, want a positive persistence window", cookie.MaxAge)
	}
}

// TestWebHandlerTaskList_RestoresFilterFromCookie covers a bare "/" browser
// navigation (no query string at all) with a previously saved cookie: the
// handler should redirect to the saved filter query rather than showing an
// unfiltered list.
func TestWebHandlerTaskList_RestoresFilterFromCookie(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerWithTaskList(svc)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: taskFilterCookieName, Value: "status=executing&active=1"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 redirect", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "/?status=executing&active=1" {
		t.Errorf("Location = %q, want restored filter query", loc)
	}
}

// TestWebHandlerTaskList_NoCookieShowsUnfilteredList covers the true
// first-ever visit: no cookie saved yet, bare "/" should render normally
// (no redirect loop, no crash).
func TestWebHandlerTaskList_NoCookieShowsUnfilteredList(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerWithTaskList(svc)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// TestWebHandlerTaskList_ClearedParamDeletesCookieAndRedirects covers the
// "Clear filters" empty-state link (?cleared=1): it must both wipe the saved
// cookie and land on a bare "/", or a saved filter would immediately
// reassert itself and clearing would look like a no-op.
func TestWebHandlerTaskList_ClearedParamDeletesCookieAndRedirects(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerWithTaskList(svc)

	req := httptest.NewRequest(http.MethodGet, "/?cleared=1", nil)
	req.AddCookie(&http.Cookie{Name: taskFilterCookieName, Value: "status=executing"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 redirect", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
	res := w.Result()
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == taskFilterCookieName {
			cookie = c
			break
		}
	}
	if cookie == nil {
		t.Fatalf("expected a Set-Cookie clearing %q, got none", taskFilterCookieName)
	}
	if cookie.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want negative (delete)", cookie.MaxAge)
	}
}

// TestWebHandlerTaskList_HXPollingDoesNotRedirect covers the list's 5s
// polling refresh (hx-get={currentURL}, HX-Request: true), which legitimately
// requests a bare "/" when no filter is active. It must render inline, never
// redirect — a redirect here would break htmx's polling loop.
func TestWebHandlerTaskList_HXPollingDoesNotRedirect(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerWithTaskList(svc)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(&http.Cookie{Name: taskFilterCookieName, Value: "status=executing"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no redirect during htmx polling)", w.Code)
	}
}

func TestWebHandlerTaskList_HXRequestReturnsFragment(t *testing.T) {
	svc := &stubWebService{
		tasks: []*orchestrator.Task{
			{ID: "t-1", Type: orchestrator.TaskTypeExecution, Title: "hello", Status: "executing", Exec: &orchestrator.ExecAttrs{}},
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

func TestWebHandlerPostStartSession_ForwardsInstruction(t *testing.T) {
	dispatcher := &stubSessionDispatcher{result: &StartSessionResult{JobID: "job-1"}}
	r := newTestWebHandlerWithSessionStart(dispatcher)

	body := url.Values{
		"harness_type": {"claude"},
		"instruction":  {"  このプロジェクトで◯◯をやって  "},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/sessions/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if dispatcher.lastReq.Instruction != "このプロジェクトで◯◯をやって" {
		t.Errorf("Instruction = %q, want trimmed instruction text", dispatcher.lastReq.Instruction)
	}
}

func TestWebHandlerPostStartSession_EmptyInstructionOK(t *testing.T) {
	dispatcher := &stubSessionDispatcher{result: &StartSessionResult{JobID: "job-1"}}
	r := newTestWebHandlerWithSessionStart(dispatcher)

	body := url.Values{"harness_type": {"claude"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/sessions/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if dispatcher.lastReq.Instruction != "" {
		t.Errorf("Instruction = %q, want empty", dispatcher.lastReq.Instruction)
	}
}

// TestWebHandler_TaskDetail_ShowsTriageChildren moved to
// web_card_timeline_test.go: a card's children now render from the
// timeline read model (CardTimeline), which needs a real card_requests/
// actions-backed DB fixture, not just a stub TaskTriage row.

// TestWebHandler_TaskDetail_NoTriageChildren_NoSection ensures the vast
// majority of non-triage tasks render with no children section (nil
// TaskTriage / missing sidecar row / no children key), not an empty box.
func TestWebHandler_TaskDetail_NoTriageChildren_NoSection(t *testing.T) {
	svc := &stubWebService{taskDetail: &TaskDetailView{Task: &orchestrator.Task{
		ID:     "task-1",
		Type:   orchestrator.TaskTypeExecution,
		Title:  "regular task",
		Status: orchestrator.TaskStatusExecuting,
		Exec:   &orchestrator.ExecAttrs{},
	}}}
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if strings.Contains(w.Body.String(), "detail-children") {
		t.Error("body contains detail-children section, want none for a task with no triage children")
	}
}

// TestWebHandler_TaskDetail_Card_IdentityRow_ShowsProjectAndKind pins the
// meta 2-段 identity row for a card (docs/plans/webui-detail-list-redesign.md
// §3.1 部品A): "<project> / <kind>", no ラベル, no "card" filler.
func TestWebHandler_TaskDetail_Card_IdentityRow_ShowsProjectAndKind(t *testing.T) {
	svc := &stubWebService{taskDetail: &TaskDetailView{Task: &orchestrator.Task{
		ID:        "task-1",
		Type:      orchestrator.TaskTypeCard,
		ProjectID: "proj-a",
		Title:     "card title",
		Status:    orchestrator.TaskStatusParked,
		Card:      &orchestrator.CardAttrs{Kind: "issue"},
	}}}
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<div class="detail-identity">proj-a / issue</div>`) {
		t.Errorf("expected card identity row \"proj-a / issue\", got: %s", body)
	}
	// N4 (Opus review, PR #996): data-task-id is what TaskDetailLiveScript
	// reads the task id from (§7 罠2) — a regression here would silently
	// break live updates on every card detail page, with no visible error.
	if !strings.Contains(body, `id="task-status" data-task-id="task-1"`) {
		t.Errorf("card status section should carry data-task-id for TaskDetailLiveScript, got: %s", body)
	}
}

// TestWebHandler_TaskDetail_Exec_IdentityRow_ShowsProjectAndBehavior is the
// execution-task counterpart: "<project> / <behavior>".
func TestWebHandler_TaskDetail_Exec_IdentityRow_ShowsProjectAndBehavior(t *testing.T) {
	svc := &stubWebService{taskDetail: &TaskDetailView{Task: &orchestrator.Task{
		ID:        "task-1",
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "proj-b",
		Title:     "exec title",
		Status:    orchestrator.TaskStatusExecuting,
		Exec:      &orchestrator.ExecAttrs{Behavior: "implement"},
	}}}
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<div class="detail-identity">proj-b / implement</div>`) {
		t.Errorf("expected exec identity row \"proj-b / implement\", got: %s", body)
	}
}

func TestTaskDetail_IdentityResourcesRenderInFullPageAndStatusFragment(t *testing.T) {
	detail := makeTaskDetailView()
	detail.Identities = []apiwire.TaskIdentity{
		{Identity: "github:pr:42", URL: "https://github.example/acme/repo/pull/42", DisplayName: "PR 42"},
		{Identity: "jira:X-1", URL: "https://jira.example/browse/X-1"},
		{Identity: "slack:thread:123"},
	}
	svc := &stubWebService{taskDetail: detail}
	r := newTestWebHandler(svc)

	for _, path := range []string{"/tasks/task-1", "/tasks/task-1/fragment?kind=status"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, w.Code)
		}
		body := w.Body.String()
		for _, want := range []string{
			`href="https://github.example/acme/repo/pull/42" target="_blank" rel="noopener noreferrer">PR 42</a>`,
			`href="https://jira.example/browse/X-1" target="_blank" rel="noopener noreferrer">jira:X-1</a>`,
			`<span class="task-identity-key">slack:thread:123</span>`,
			`<span class="task-identity-unset">reference unavailable</span>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s missing %q: %s", path, want, body)
			}
		}
	}
}

// TestWebHandler_TaskDetail_Card_MovementRow_ShowsTransitionEdgeWithVerb was
// the old movement row contract ("parked —go→ working" inline in the status
// strip). The live suggestion now renders as a pinned timeline item instead
// — see TestCardDetail_PinnedSuggestion_RendersAcceptRejectAndNoTransitionEdge
// in web_card_timeline_test.go for the replacement, which also pins that the
// transition-edge text is now deliberately gone.

// TestWebHandler_TaskDetail_Card_DescriptionShownInBody_NoTabNeeded pins
// §3.3 item 3 (decided, not left to a later PR): a card's Description is
// shown directly in the page body, not hidden behind a tab click — the
// plain GET (no ?tab= at all) must already contain it.
func TestWebHandler_TaskDetail_Card_DescriptionShownInBody_NoTabNeeded(t *testing.T) {
	svc := &stubWebService{taskDetail: &TaskDetailView{Task: &orchestrator.Task{
		ID:          "task-1",
		Type:        orchestrator.TaskTypeCard,
		Title:       "card title",
		Description: "captured content the card is about",
		Status:      orchestrator.TaskStatusParked,
		Card:        &orchestrator.CardAttrs{},
	}}}
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "captured content the card is about") {
		t.Errorf("card body should show Description without navigating to a tab, got: %s", w.Body.String())
	}
}

// TestWebHandler_TaskDetail_Exec_DescriptionNotShownByDefault is the
// execution-task counterpart: Description stays behind the existing
// Description tab (unchanged for PR-1 — §3.4), so the default (Timeline)
// view must NOT show it.
func TestWebHandler_TaskDetail_Exec_DescriptionNotShownByDefault(t *testing.T) {
	svc := &stubWebService{taskDetail: &TaskDetailView{Task: &orchestrator.Task{
		ID:          "task-1",
		Type:        orchestrator.TaskTypeExecution,
		Title:       "exec title",
		Description: "exec task description text",
		Status:      orchestrator.TaskStatusExecuting,
		Exec:        &orchestrator.ExecAttrs{},
	}}}
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if strings.Contains(w.Body.String(), "exec task description text") {
		t.Errorf("exec detail's default (Timeline) view should not show Description, got: %s", w.Body.String())
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

func (s *dupTaskSvcStub) CreateTask(_ context.Context, req CreateTaskRequest) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *dupTaskSvcStub) GetTask(id string) (*orchestrator.Task, error) { return nil, nil }
func (s *dupTaskSvcStub) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *dupTaskSvcStub) UpdateTask(_ context.Context, id string, req UpdateTaskRequest) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *dupTaskSvcStub) DeleteTask(id string, force bool) error { return nil }
func (s *dupTaskSvcStub) GetTaskDetail(id string) (*TaskDetailView, error) {
	return nil, nil
}
func (s *dupTaskSvcStub) GetTaskField(id, path string) (string, error) { return "", nil }
func (s *dupTaskSvcStub) ImportTasks(_ context.Context, reqs []CreateTaskRequest) (*ImportResult, error) {
	return nil, nil
}
func (s *dupTaskSvcStub) DuplicateTask(_ context.Context, id string, autoStart bool) (*orchestrator.Task, error) {
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

	newID, err := svc.DuplicateTask(context.Background(), "orig-id")
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
	_, err := svc.DuplicateTask(context.Background(), "any-id")
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

	_, err := svc.DuplicateTask(context.Background(), "missing-id")
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
			ID:     "task-1",
			Type:   orchestrator.TaskTypeExecution,
			Title:  "Test Task",
			Status: "executing",
			Exec:   &orchestrator.ExecAttrs{Behavior: "dev"},
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

// makeCardTaskDetailView is makeTaskDetailView's card-typed counterpart,
// used by tests that exercise the card-only rendering path (suggestion /
// children / movement edge). status defaults every card-machine verb's
// FromStatus can plausibly apply to; individual tests override where a
// specific verb's FromStatus matters (e.g. "reopen" only fires from
// done/dropped).
func makeCardTaskDetailView(id string, status orchestrator.TaskStatus) *TaskDetailView {
	return &TaskDetailView{
		Task: &orchestrator.Task{
			ID:     id,
			Type:   orchestrator.TaskTypeCard,
			Title:  "Test Card",
			Status: status,
			Card:   &orchestrator.CardAttrs{},
		},
	}
}

// TestTaskDetailFragment_Status_RendersSuggestion and
// TestTaskDetail_RendersSuggestion moved to web_card_timeline_test.go: the
// pinned suggestion item now comes from the CardTimeline read model (which
// derives it from the actions log, not just the live task_triage detail
// blob a stub TaskTriage row can provide), so the fixture needs a real DB.
// See TestCardDetail_PinnedSuggestion_RendersAcceptRejectAndNoTransitionEdge
// and its fragment-path sibling there.

// TestTaskDetailFragment_JobsKindRemoved pins the death of fragment
// kind=jobs (docs/plans/webui-detail-list-redesign.md §7 PR-1 死骸掃除):
// TaskDetailJobsSection was never reachable from the page (the `jobs`
// argument flowed TaskDetail → TabsSection → TabPanel and was never read
// there), and the fragment endpoint was its only caller. Same response as
// any other unrecognized kind now — see TestTaskDetailFragment_UnknownKind.
func TestTaskDetailFragment_JobsKindRemoved(t *testing.T) {
	svc := &stubWebService{taskDetail: makeTaskDetailView()}
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/fragment?kind=jobs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (kind=jobs no longer exists)", w.Code)
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
			testCardProject("proj-1", "default"),
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
	if strings.Contains(body, `name="behavior"`) {
		t.Error("Card form must not contain behavior field")
	}
	if !strings.Contains(body, `name="description"`) {
		t.Error("form should contain description field")
	}
	if strings.Contains(body, `name="auto_start"`) {
		t.Error("Card form must not contain auto_start field")
	}
}

func TestWebHandler_PostTaskCreate_Success(t *testing.T) {
	newTask := &orchestrator.Task{ID: "new-task-id", Title: "My Task"}
	svc := &stubWebService{createTaskResult: newTask, projects: []*orchestrator.Project{testCardProject("proj-1", "default")}}
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
			testCardProject("proj-1", "default"),
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
	if !strings.Contains(respBody, `data-default="false" selected`) {
		t.Errorf("response should mark project_id selected, got: %s", respBody)
	}
	if strings.Contains(respBody, `name="auto_start"`) {
		t.Error("Card form must not restore execution-only fields")
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
			Type:   orchestrator.TaskTypeExecution,
			Title:  "My Task",
			Status: orchestrator.TaskStatusPending,
			Exec:   &orchestrator.ExecAttrs{},
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
			Type:   orchestrator.TaskTypeExecution,
			Status: orchestrator.TaskStatusExecuting,
			Exec:   &orchestrator.ExecAttrs{},
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
			Type:   orchestrator.TaskTypeExecution,
			Status: orchestrator.TaskStatusPending,
			Exec: &orchestrator.ExecAttrs{
				Instructions: orchestrator.Instructions{{
					Message: "old message",
					Model:   "sonnet",
				}},
			},
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

func TestWebHandler_PostTaskCreate_IgnoresExecutionFields(t *testing.T) {
	svc := &stubWebService{
		createTaskResult: &orchestrator.Task{ID: "new-task-id"},
		projects:         []*orchestrator.Project{testCardProject("proj-1", "default")},
	}
	body := url.Values{
		"title": {"My Card"}, "project_id": {"proj-1"}, "description": {"A goal"},
		"behavior": {"executor"}, "auto_start": {"on"}, "agent": {"claude-code"},
		"model": {"sonnet"}, "parent_id": {"other"}, "remote_id": {"OLD-1"},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	newTestWebHandlerWithTaskCreate(svc).ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	got := svc.createTaskCalls[0]
	if got.InitialStatus != "parked" || got.AutoStart || got.Behavior != "" || len(got.Instructions) != 0 || got.ParentID != "" || got.RemoteID != "" {
		t.Fatalf("Card request carries execution fields: %+v", got)
	}
	if got.Title != "My Card" || got.Description != "A goal" {
		t.Fatalf("lost Card input: %+v", got)
	}
}

func TestWebHandler_TaskNew_OnlyCardFields(t *testing.T) {
	svc := &stubWebService{projects: []*orchestrator.Project{testCardProject("proj-1", "default")}}
	w := httptest.NewRecorder()
	newTestWebHandlerWithTaskCreate(svc).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/new", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	for _, field := range []string{"title", "description", "workspace", "project_id"} {
		if !strings.Contains(w.Body.String(), `name="`+field+`"`) {
			t.Errorf("missing %s", field)
		}
	}
	for _, field := range []string{"agent", "model", "behavior", "auto_start", "remote_id"} {
		if strings.Contains(w.Body.String(), `name="`+field+`"`) {
			t.Errorf("unexpected %s", field)
		}
	}
}

func TestWebHandler_PostEdit_RemoteID(t *testing.T) {
	detail := &TaskDetailView{
		Task: &orchestrator.Task{
			ID:       "task-1",
			Type:     orchestrator.TaskTypeExecution,
			Status:   orchestrator.TaskStatusPending,
			RemoteID: "OLD-1",
			Exec: &orchestrator.ExecAttrs{
				Instructions: orchestrator.Instructions{{
					Message: "old message",
				}},
			},
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

// liveScriptMarker is a substring unique to TaskDetailLiveScript's <script>
// body (the module-level re-entry guard variable name). Used by the B1
// regression tests below as a cheap, robust way to detect the script's
// presence/absence without depending on exact whitespace/formatting.
//
// Appears exactly TWICE per script instance — once in the guard check
// (`if (window.__boidTimelineSubscriber) return`) and once in the
// assignment (`window.__boidTimelineSubscriber = true`) — so
// liveScriptInstanceCount below divides the raw occurrence count by this.
const liveScriptMarker = "__boidTimelineSubscriber"

// occurrencesPerLiveScript is how many times liveScriptMarker appears
// within a single rendering of TaskDetailLiveScript (see its doc comment).
const occurrencesPerLiveScript = 2

// liveScriptInstanceCount returns how many times TaskDetailLiveScript was
// rendered into body.
func liveScriptInstanceCount(body string) int {
	return strings.Count(body, liveScriptMarker) / occurrencesPerLiveScript
}

// TestTaskDetail_Exec_LiveScriptRendersOnceOutsideTabs pins Opus review
// finding B1 (PR #996, blocking): TaskDetailLiveScript must render exactly
// once per full page load, and — critically — NOT as part of the #tabs
// fragment a tab-link click swaps. It used to live inside #tabs (rendered
// by TaskDetailExecTabPanel), paired with an htmx:beforeSwap teardown that
// closed the EventSource whenever #tabs was about to be swapped; the first
// tab click tore the connection down, and the freshly-swapped-in copy of
// the script never reopened it (its own top-of-IIFE re-entry guard
// short-circuited before ever calling openES() again) — SSE updates for
// #task-status/#task-timeline died permanently after exactly one tab
// switch. This (and TestTaskDetail_Exec_TabSwapFragment_HasNoLiveScript
// below) can't drive real HTMX/EventSource behavior from a Go test, but
// together they pin the structural invariant the fix depends on: the
// script is emitted exactly once, and never inside anything #tabs-swappable.
func TestTaskDetail_Exec_LiveScriptRendersOnceOutsideTabs(t *testing.T) {
	svc := &stubWebService{taskDetail: makeTaskDetailView()}
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()

	if got := liveScriptInstanceCount(body); got != 1 {
		t.Errorf("live script rendered %d times on the full exec detail page, want exactly 1: %s", got, body)
	}

	// "Outside #tabs" is pinned by comparing against the #tabs fragment
	// itself (TestTaskDetail_Exec_TabSwapFragment_HasNoLiveScript below):
	// that fragment is rendered by the exact same TaskDetailExecTabsSection
	// call the full page embeds for its #tabs subtree, and it never
	// contains the script — so if the full page DOES contain the script
	// (asserted above) and the #tabs subtree provably does not, the script
	// must live outside #tabs.
}

// TestTaskDetail_Exec_TabSwapFragment_HasNoLiveScript is
// TestTaskDetail_Exec_LiveScriptRendersOnceOutsideTabs's other half: the
// HX-Request fragment a tab-link click actually receives (what
// TaskDetailExecTabsSection renders — the exact #tabs subtree HTMX swaps)
// must never contain the live script at all, on ANY tab — including
// "timeline", where the script used to live before this fix (inside
// TaskDetailExecTabPanel's activeTab=="timeline" branch). If it did, every
// tab click would re-insert a copy that (per the script's own re-entry
// guard) can never actually resubscribe — the B1 bug this test guards
// against.
func TestTaskDetail_Exec_TabSwapFragment_HasNoLiveScript(t *testing.T) {
	for _, tab := range []string{"timeline", "description", "payload", "instructions"} {
		t.Run(tab, func(t *testing.T) {
			svc := &stubWebService{taskDetail: makeTaskDetailView()}
			r := newTestWebHandler(svc)

			req := httptest.NewRequest(http.MethodGet, "/tasks/task-1?tab="+tab, nil)
			req.Header.Set("HX-Request", "true")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			body := w.Body.String()
			if strings.Contains(body, liveScriptMarker) {
				t.Errorf("tab-swap fragment (#tabs, tab=%s) must not contain the live-update script, got: %s", tab, body)
			}
		})
	}
}

// TestTaskDetail_Card_HXRequest_FullPageRender pins 罠1's resolution
// (docs/plans/webui-detail-list-redesign.md §7 PR-1): a card detail page
// has no tabs, so an HX-Request GET with a `tab` query param — the shape a
// tab-link click sends on the execution layout — must fall through to a
// full page render for a card instead of trying (and failing) to render a
// #tabs fragment that does not exist on the card layout. Twin of
// TestTaskDetail_Tab_HXRequest_Timeline above, which pins the execution
// side of the same branch.
func TestTaskDetail_Card_HXRequest_FullPageRender(t *testing.T) {
	svc := &stubWebService{taskDetail: makeCardTaskDetailView("task-1", orchestrator.TaskStatusParked)}
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1?tab=timeline", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<html") {
		t.Errorf("HX-Request on a card should still get the full page (no #tabs to fragment-swap into), got: %s", body)
	}
	if strings.Contains(body, `id="tabs"`) {
		t.Errorf("card detail page should render no #tabs element at all, got: %s", body)
	}
}

// TestTaskDetail_CardVsExec_RenderDifferently pins the entity split itself
// (§7 PR-1's core requirement): the two layouts must diverge in ways a
// reader (and a test) can observe from the same handler, for the same
// generic task detail route.
func TestTaskDetail_CardVsExec_RenderDifferently(t *testing.T) {
	execSvc := &stubWebService{taskDetail: makeTaskDetailView()}
	execR := newTestWebHandler(execSvc)
	execReq := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	execW := httptest.NewRecorder()
	execR.ServeHTTP(execW, execReq)
	execBody := execW.Body.String()

	cardSvc := &stubWebService{taskDetail: makeCardTaskDetailView("task-1", orchestrator.TaskStatusParked)}
	cardR := newTestWebHandler(cardSvc)
	cardReq := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	cardW := httptest.NewRecorder()
	cardR.ServeHTTP(cardW, cardReq)
	cardBody := cardW.Body.String()

	if execW.Code != http.StatusOK || cardW.Code != http.StatusOK {
		t.Fatalf("status = (exec %d, card %d), want (200, 200)", execW.Code, cardW.Code)
	}

	// An execution task's detail page has tabs; a card's does not.
	if !strings.Contains(execBody, `id="tabs"`) {
		t.Error("execution task detail should render #tabs")
	}
	if strings.Contains(cardBody, `id="tabs"`) {
		t.Error("card detail should NOT render #tabs (§7 罠1 — no tabs on the card layout)")
	}

	// Neither layout ever prints the literal filler behavior label "card"
	// that the old shared status strip used for a triage task (§3.1: "埋め草
	// 'card' ラベルは廃止"). The card fixture has no Kind set, so its
	// identity row renders as just the project id with a trailing " / ".
	if strings.Contains(cardBody, `>card<`) {
		t.Errorf("card identity row should not render the literal filler \"card\", got: %s", cardBody)
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
			ID: "task-1", Type: orchestrator.TaskTypeExecution, Title: "Test Task", Status: "executing",
			CreatedAt: now.Add(-1 * time.Minute), Exec: &orchestrator.ExecAttrs{},
		},
		Jobs: []*Job{
			{
				ID: "job-123", Role: "hook", HandlerID: "go-dev/pr-verify",
				Status:    JobStatusCompleted,
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
// /jobs/{id}/terminal is now a redirect to /jobs/{id}; the terminal widget is
// embedded in the job detail page for interactive running jobs.

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
	r := newTestWebHandlerWithJobDetail(svc)

	req := httptest.NewRequest(http.MethodGet, "/jobs/job-term-1", nil)
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
	r := newTestWebHandlerWithJobDetail(svc)

	req := httptest.NewRequest(http.MethodGet, "/jobs/job-done-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "boid-terminal-xterm") {
		t.Error("job detail should not render xterm widget for non-running interactive job")
	}
	if !strings.Contains(body, "interactive") && !strings.Contains(body, "Live output") {
		t.Errorf("page should mention interactive/live-output note: %s", body[:min(300, len(body))])
	}
}

func TestTerminalPage_RequiresAuth(t *testing.T) {
	// Verify /jobs/{id}/terminal is still registered and redirects (not chi 404).
	svc := &stubWebService{}
	h := &WebHandler{Service: svc}
	r := h.Routes()

	req := httptest.NewRequest(http.MethodGet, "/jobs/some-id/terminal", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Route is registered as a redirect; must not return chi's 404.
	if strings.Contains(w.Body.String(), "404 page not found") {
		t.Error("/jobs/{id}/terminal route should be registered in WebHandler.Routes()")
	}
	if w.Code != http.StatusFound {
		t.Errorf("/jobs/{id}/terminal should redirect (302), got %d", w.Code)
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

func TestJobDetail_NoTask_BackToSessions(t *testing.T) {
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
	want := `/sessions`
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
