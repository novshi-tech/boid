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
	tasks                 []*orchestrator.Task
	taskDetail            *TaskDetailView
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

func (s *stubWebService) ListSessions() ([]JobWithContext, error) {
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

// stubTaskTriageStore backs h.TaskTriage in shaping-session tests. Only
// GetTaskTriage is exercised; the rest satisfy the interface.
type stubTaskTriageStore struct {
	triage *orchestrator.CardAttrs
	err    error
}

func (s *stubTaskTriageStore) UpsertTaskTriage(tt *orchestrator.CardAttrs) error { return nil }
func (s *stubTaskTriageStore) GetTaskTriage(taskID string) (*orchestrator.CardAttrs, error) {
	return s.triage, s.err
}
func (s *stubTaskTriageStore) ListTaskTriageByTaskIDs(taskIDs []string) (map[string]*orchestrator.CardAttrs, error) {
	out := map[string]*orchestrator.CardAttrs{}
	if s.triage != nil {
		for _, id := range taskIDs {
			if id == s.triage.TaskID {
				out[id] = s.triage
			}
		}
	}
	return out, s.err
}
func (s *stubTaskTriageStore) DeleteTaskTriage(taskID string) error { return nil }
func (s *stubTaskTriageStore) ParkedFrom(taskID string) (orchestrator.TaskStatus, error) {
	return "", nil
}

func newTestWebHandlerWithShaping(svc WebService, dispatcher SessionDispatcher, triage CardStore) *chi.Mux {
	h := &WebHandler{Service: svc, SessionDispatcher: dispatcher, TaskTriage: triage}
	r := chi.NewRouter()
	r.Post("/tasks/{id}/shape", h.PostStartShapingSession)
	return r
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

// TestWebHandlerPostStartShapingSession_Success pins the parked case — card
// machine v2's main resting/undecided status (docs/plans/
// suggestion-as-state-transition-impl.md §3.5 folds captured/triaged into
// parked). PR #987 review, BLOCKER 1: this fixture used to sit in the now
// entirely unreachable "triaged" status, which meant Shape from a card's own
// initial state had zero test coverage — and, worse, was actually broken in
// production (the handler's gate hadn't been updated to match).
func TestWebHandlerPostStartShapingSession_Success(t *testing.T) {
	svc := &stubWebService{taskDetail: &TaskDetailView{Task: &orchestrator.Task{
		ID:          "task-1",
		Type:        orchestrator.TaskTypeCard,
		ProjectID:   "meta-proj",
		Title:       "運用が回っていない気配",
		Description: "詳細不明のsummaryのみ",
		Status:      orchestrator.TaskStatusParked,
		Card:        &orchestrator.CardAttrs{},
	}}}
	dispatcher := &stubSessionDispatcher{result: &StartSessionResult{JobID: "job-9"}}
	triage := &stubTaskTriageStore{triage: &orchestrator.CardAttrs{TaskID: "task-1", Kind: "issue", Urgency: "week"}}
	r := newTestWebHandlerWithShaping(svc, dispatcher, triage)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/shape", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if loc := w.Header().Get("Location"); loc != "/jobs/job-9" {
		t.Errorf("Location = %q, want /jobs/job-9", loc)
	}
	if !dispatcher.callable {
		t.Fatal("StartSession was not called")
	}
	if dispatcher.lastReq.ProjectID != "meta-proj" {
		t.Errorf("ProjectID = %q, want meta-proj", dispatcher.lastReq.ProjectID)
	}
	if dispatcher.lastReq.HarnessType != "claude" {
		t.Errorf("HarnessType = %q, want claude", dispatcher.lastReq.HarnessType)
	}
	for _, want := range []string{"task-1", "運用が回っていない気配", "issue", "week", "詳細不明のsummaryのみ", "状態遷移は行いません"} {
		if !strings.Contains(dispatcher.lastReq.Instruction, want) {
			t.Errorf("Instruction missing %q; got:\n%s", want, dispatcher.lastReq.Instruction)
		}
	}
	// BLOCKER 2 (PR #987 review): card machine v2 has no "ready" status/verb
	// at all — the instruction must never tell the agent to move the card
	// there. Advancing the card is a human's accept or khi's own suggest,
	// never the shaping session's job.
	if strings.Contains(dispatcher.lastReq.Instruction, "card を ready に更新してください") {
		t.Errorf("parked-task instruction should not tell the agent to move the card to ready (that status no longer exists); got:\n%s", dispatcher.lastReq.Instruction)
	}
}

func TestWebHandlerPostStartShapingSession_WorkingWithOpenChild(t *testing.T) {
	svc := &stubWebService{taskDetail: &TaskDetailView{Task: &orchestrator.Task{
		ID:          "task-1",
		Type:        orchestrator.TaskTypeCard,
		ProjectID:   "meta-proj",
		Title:       "運用が回っていない気配",
		Description: "詳細不明のsummaryのみ",
		Status:      orchestrator.TaskStatusWorking,
		Card:        &orchestrator.CardAttrs{},
	}}}
	dispatcher := &stubSessionDispatcher{result: &StartSessionResult{JobID: "job-9"}}
	triage := &stubTaskTriageStore{triage: &orchestrator.CardAttrs{
		TaskID: "task-1", Kind: "issue", Urgency: "week",
		Detail: json.RawMessage(`{"children":[{"id":"ch_00","title":"サブ課題A","status":"open"}]}`),
	}}
	r := newTestWebHandlerWithShaping(svc, dispatcher, triage)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/shape", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if !dispatcher.callable {
		t.Fatal("StartSession was not called for a working task with an open child")
	}
	for _, want := range []string{"ch_00", "サブ課題A", "specced", "状態遷移は行いません"} {
		if !strings.Contains(dispatcher.lastReq.Instruction, want) {
			t.Errorf("Instruction missing %q; got:\n%s", want, dispatcher.lastReq.Instruction)
		}
	}
	if strings.Contains(dispatcher.lastReq.Instruction, "card を ready に更新してください") {
		t.Errorf("working-task instruction should not tell the agent to move the card back to ready; got:\n%s", dispatcher.lastReq.Instruction)
	}
}

func TestWebHandlerPostStartShapingSession_WorkingWithoutTriageRow(t *testing.T) {
	svc := &stubWebService{taskDetail: &TaskDetailView{Task: &orchestrator.Task{
		ID: "task-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{},
	}}}
	dispatcher := &stubSessionDispatcher{}
	// No triage row at all: an ordinary (non-triage) working task, the
	// overwhelming majority case — must not be offered Shape, since it has
	// no children list to add to or shape.
	r := newTestWebHandlerWithShaping(svc, dispatcher, nil)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/shape", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error param", loc)
	}
	if dispatcher.callable {
		t.Error("StartSession should not be called for a working task with no task_triage sidecar row")
	}
}

func TestWebHandlerPostStartShapingSession_WorkingWithTriageRowNoChildrenYet(t *testing.T) {
	svc := &stubWebService{taskDetail: &TaskDetailView{Task: &orchestrator.Task{
		ID: "task-1", Type: orchestrator.TaskTypeCard, ProjectID: "meta-proj", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{},
	}}}
	dispatcher := &stubSessionDispatcher{result: &StartSessionResult{JobID: "job-9"}}
	// A triage row exists but has no children yet — the "add a brand-new
	// child while working" use case must still be reachable, not just
	// "shape an existing open child".
	triage := &stubTaskTriageStore{triage: &orchestrator.CardAttrs{TaskID: "task-1", Kind: "issue"}}
	r := newTestWebHandlerWithShaping(svc, dispatcher, triage)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/shape", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if !dispatcher.callable {
		t.Fatal("StartSession should be callable for a working task with a triage row even with no children yet")
	}
	if !strings.Contains(dispatcher.lastReq.Instruction, "追加") {
		t.Errorf("Instruction should mention adding a new child when none exist yet; got:\n%s", dispatcher.lastReq.Instruction)
	}
}

func TestWebHandlerPostStartShapingSession_UsesSessionBehaviorsDefaults(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{Task: &orchestrator.Task{
			ID:        "task-1",
			Type:      orchestrator.TaskTypeCard,
			ProjectID: "meta-proj",
			Title:     "運用が回っていない気配",
			Status:    orchestrator.TaskStatusParked,
			Card:      &orchestrator.CardAttrs{},
		}},
		projectByID: &orchestrator.Project{
			ID: "meta-proj",
			Meta: orchestrator.ProjectMeta{
				SessionBehaviors: map[string]orchestrator.SessionBehavior{
					"shape": {HarnessType: "codex", Model: "o3-mini"},
				},
			},
		},
	}
	dispatcher := &stubSessionDispatcher{result: &StartSessionResult{JobID: "job-9"}}
	r := newTestWebHandlerWithShaping(svc, dispatcher, nil)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/shape", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if dispatcher.lastReq.HarnessType != "codex" {
		t.Errorf("HarnessType = %q, want codex", dispatcher.lastReq.HarnessType)
	}
	if dispatcher.lastReq.Model != "o3-mini" {
		t.Errorf("Model = %q, want o3-mini", dispatcher.lastReq.Model)
	}
}

func TestWebHandlerPostStartShapingSession_FallsBackWhenNoSessionBehaviors(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{Task: &orchestrator.Task{
			ID:        "task-1",
			Type:      orchestrator.TaskTypeCard,
			ProjectID: "meta-proj",
			Title:     "運用が回っていない気配",
			Status:    orchestrator.TaskStatusParked,
			Card:      &orchestrator.CardAttrs{},
		}},
		projectByID: &orchestrator.Project{ID: "meta-proj"},
	}
	dispatcher := &stubSessionDispatcher{result: &StartSessionResult{JobID: "job-9"}}
	r := newTestWebHandlerWithShaping(svc, dispatcher, nil)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/shape", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if dispatcher.lastReq.HarnessType != "claude" {
		t.Errorf("HarnessType = %q, want claude (fallback)", dispatcher.lastReq.HarnessType)
	}
	if dispatcher.lastReq.Model != "" {
		t.Errorf("Model = %q, want empty (fallback)", dispatcher.lastReq.Model)
	}
}

func TestWebHandlerPostStartShapingSession_FallsBackOnInvalidHarnessType(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{Task: &orchestrator.Task{
			ID:        "task-1",
			Type:      orchestrator.TaskTypeCard,
			ProjectID: "meta-proj",
			Title:     "運用が回っていない気配",
			Status:    orchestrator.TaskStatusParked,
			Card:      &orchestrator.CardAttrs{},
		}},
		projectByID: &orchestrator.Project{
			ID: "meta-proj",
			Meta: orchestrator.ProjectMeta{
				SessionBehaviors: map[string]orchestrator.SessionBehavior{
					"shape": {HarnessType: "bogus", Model: "o3-mini"},
				},
			},
		},
	}
	dispatcher := &stubSessionDispatcher{result: &StartSessionResult{JobID: "job-9"}}
	r := newTestWebHandlerWithShaping(svc, dispatcher, nil)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/shape", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if dispatcher.lastReq.HarnessType != "claude" {
		t.Errorf("HarnessType = %q, want claude (fallback on invalid harness_type)", dispatcher.lastReq.HarnessType)
	}
	// The fallback must be all-or-nothing: an invalid harness_type must not
	// leak the project's chosen model onto the claude fallback (that would
	// launch claude with a model value nobody actually asked claude to use).
	if dispatcher.lastReq.Model != "" {
		t.Errorf("Model = %q, want empty (fallback must not leak model when harness_type is invalid)", dispatcher.lastReq.Model)
	}
}

func TestWebHandlerPostStartShapingSession_FallsBackOnEmptyHarnessType(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{Task: &orchestrator.Task{
			ID:        "task-1",
			Type:      orchestrator.TaskTypeCard,
			ProjectID: "meta-proj",
			Title:     "運用が回っていない気配",
			Status:    orchestrator.TaskStatusParked,
			Card:      &orchestrator.CardAttrs{},
		}},
		projectByID: &orchestrator.Project{
			ID: "meta-proj",
			Meta: orchestrator.ProjectMeta{
				// harness_type omitted, model set: the whole entry falls
				// back rather than launching claude with someone else's model.
				SessionBehaviors: map[string]orchestrator.SessionBehavior{
					"shape": {Model: "o3-mini"},
				},
			},
		},
	}
	dispatcher := &stubSessionDispatcher{result: &StartSessionResult{JobID: "job-9"}}
	r := newTestWebHandlerWithShaping(svc, dispatcher, nil)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/shape", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if dispatcher.lastReq.HarnessType != "claude" {
		t.Errorf("HarnessType = %q, want claude (fallback on empty harness_type)", dispatcher.lastReq.HarnessType)
	}
	if dispatcher.lastReq.Model != "" {
		t.Errorf("Model = %q, want empty (fallback must not leak model when harness_type is empty)", dispatcher.lastReq.Model)
	}
}

// TestWebHandlerPostStartShapingSession_NotParkedOrWorking (renamed from
// v1's _NotTriaged, PR #987 review LOW 12: card machine v2 has no "triaged"
// status to name a rejection test after). done is a status Shape has never
// been offered from in either version — a finished card has nothing left to
// shape.
func TestWebHandlerPostStartShapingSession_NotParkedOrWorking(t *testing.T) {
	// A done card, per this test's own doc comment above ("a finished card
	// has nothing left to shape") — Type/Card set to match (done is shared
	// with execution tasks per card-model-cleanup PR-2's status vocabulary,
	// so this disambiguates explicitly rather than leaving Type unset).
	svc := &stubWebService{taskDetail: &TaskDetailView{Task: &orchestrator.Task{
		ID: "task-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusDone, Card: &orchestrator.CardAttrs{},
	}}}
	dispatcher := &stubSessionDispatcher{}
	r := newTestWebHandlerWithShaping(svc, dispatcher, nil)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/shape", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error param", loc)
	}
	if dispatcher.callable {
		t.Error("StartSession should not be called for a done task")
	}
}

func TestWebHandlerPostStartShapingSession_DispatcherError(t *testing.T) {
	svc := &stubWebService{taskDetail: &TaskDetailView{Task: &orchestrator.Task{
		ID: "task-1", Type: orchestrator.TaskTypeCard, ProjectID: "meta-proj", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{},
	}}}
	dispatcher := &stubSessionDispatcher{err: fmt.Errorf("no sandbox capacity")}
	r := newTestWebHandlerWithShaping(svc, dispatcher, nil)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/shape", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/tasks/task-1") || !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want /tasks/task-1 with error param", loc)
	}
}

func TestWebHandlerPostStartShapingSession_NoDispatcher(t *testing.T) {
	svc := &stubWebService{taskDetail: &TaskDetailView{Task: &orchestrator.Task{
		ID: "task-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{},
	}}}
	r := newTestWebHandlerWithShaping(svc, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/shape", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

// TestWebHandler_TaskDetail_ShowsTriageChildren covers the gap nose hit
// after the Shape launcher's first real dispatch: nothing on the task
// detail page showed whether a triaged card's children had been specced
// yet, so Go got pressed with no way to check dispatch-readiness first
// (cross-project-issue-triage 実地テスト, 2026-08-14).
func TestWebHandler_TaskDetail_ShowsTriageChildren(t *testing.T) {
	detail := json.RawMessage(`{"children":[
		{"id":"c1","title":"specced child","status":"specced","spec":{"project":"proj-a"}},
		{"id":"c2","title":"open child","status":"open"}
	]}`)
	svc := &stubWebService{taskDetail: &TaskDetailView{Task: &orchestrator.Task{
		ID:     "task-1",
		Type:   orchestrator.TaskTypeCard,
		Title:  "card title",
		Status: orchestrator.TaskStatusParked,
		Card:   &orchestrator.CardAttrs{},
	}}}
	triage := &stubTaskTriageStore{triage: &orchestrator.CardAttrs{TaskID: "task-1", Detail: detail}}
	h := &WebHandler{Service: svc, TaskTriage: triage}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	for _, want := range []string{"specced child", "open child", "no spec yet"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
}

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

// TestWebHandler_TaskDetail_Card_MovementRow_ShowsTransitionEdgeWithVerb
// pins §3.1 部品A's movement row contract: when a suggestion is live, the
// row must show the verb on the edge itself ("parked —go→ working"), not a
// bare arrow — the design doc requires the verb stay visible because "go"
// and "working" both land on the same target status (working) but mean
// very different things (dispatch vs. a bare manual declaration).
func TestWebHandler_TaskDetail_Card_MovementRow_ShowsTransitionEdgeWithVerb(t *testing.T) {
	svc := &stubWebService{taskDetail: makeCardTaskDetailView("task-1", orchestrator.TaskStatusParked)}
	triage := &stubTriageStore{rows: map[string]*orchestrator.CardAttrs{
		"task-1": {TaskID: "task-1", Detail: []byte(`{"suggestion":{"verb":"go","reason":"children specced"}}`)},
	}}
	h := &WebHandler{Service: svc, TaskTriage: triage}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if !strings.Contains(body, "—go→") {
		t.Errorf("movement row should show the verb-labeled transition edge \"—go→\", got: %s", body)
	}
	if !strings.Contains(body, `badge-parked`) || !strings.Contains(body, `badge-working`) {
		t.Errorf("movement row should show both the current status badge (parked) and the target status badge (working), got: %s", body)
	}
}

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

// TestTaskDetailFragment_Status_RendersSuggestion is a wiring test for the
// suggestion card on the fragment path (Opus review finding, 2026-08-18):
// TestTaskDetailFragment_Status above sits right next to this code but
// never wired a TaskTriage store, so it could not have caught the
// suggestion parameter being dropped or left at its zero value.
//
// webui-detail-list-redesign PR-1: the fixture is now a Card (was an
// Execution task with a triage row bolted on by the stub — a combination
// the real DB can never produce, since CardStore.GetTaskTriage only ever
// matches a `type='card'` row). The entity split branches TaskDetailFragment
// on task.Type (§7 PR-1 — "分岐軸は task.Type"), and suggestion rendering is
// card-only, so this test now needs a fixture that reflects that.
func TestTaskDetailFragment_Status_RendersSuggestion(t *testing.T) {
	svc := &stubWebService{taskDetail: makeCardTaskDetailView("task-1", orchestrator.TaskStatusDone)}
	triage := &stubTriageStore{rows: map[string]*orchestrator.CardAttrs{
		"task-1": {TaskID: "task-1", Detail: []byte(`{"suggestion":{"verb":"reopen","action":"re-triage now","reason":"source event fired","basis":"issue #42 reopened"}}`)},
	}}
	h := &WebHandler{Service: svc, TaskTriage: triage}
	r := chi.NewRouter()
	r.Get("/tasks/{id}/fragment", h.TaskDetailFragment)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/fragment?kind=status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"badge-verb-reopen", "re-triage now", "source event fired", "issue #42 reopened"} {
		if !strings.Contains(body, want) {
			t.Errorf("status fragment missing %q; got: %s", want, body)
		}
	}
}

// TestTaskDetail_RendersSuggestion covers the full-page path (not just the
// HTMX fragment) — the same wiring gap as above, but for TaskDetail →
// templates.TaskDetail's threaded suggestion parameter. Card fixture for
// the same reason as TestTaskDetailFragment_Status_RendersSuggestion above.
func TestTaskDetail_RendersSuggestion(t *testing.T) {
	svc := &stubWebService{taskDetail: makeCardTaskDetailView("task-1", orchestrator.TaskStatusDone)}
	triage := &stubTriageStore{rows: map[string]*orchestrator.CardAttrs{
		"task-1": {TaskID: "task-1", Detail: []byte(`{"suggestion":{"verb":"reopen","reason":"source event fired"}}`)},
	}}
	h := &WebHandler{Service: svc, TaskTriage: triage}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "badge-verb-reopen") {
		t.Errorf("task detail page should render the suggestion verb badge, got: %s", body)
	}
	if !strings.Contains(body, "source event fired") {
		t.Errorf("task detail page should render the suggestion reason, got: %s", body)
	}
}

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

func TestWebHandler_PostTaskCreate_AgentAndModel(t *testing.T) {
	newTask := &orchestrator.Task{ID: "new-task-id", Title: "My Task"}
	svc := &stubWebService{createTaskResult: newTask}
	r := newTestWebHandlerWithTaskCreate(svc)

	body := url.Values{
		"title": {"My Task"},
		"agent": {"claude-code"},
		"model": {"opus"},
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
	if len(call.Instructions) == 0 {
		t.Fatal("Instructions should be set when agent and model are provided")
	}
	var insts orchestrator.Instructions
	if err := json.Unmarshal(call.Instructions, &insts); err != nil {
		t.Fatalf("failed to unmarshal Instructions: %v", err)
	}
	if len(insts) != 1 {
		t.Fatalf("Instructions length = %d, want 1", len(insts))
	}
	if insts[0].Agent != "claude-code" {
		t.Errorf("Agent = %q, want claude-code", insts[0].Agent)
	}
	if insts[0].Model != "opus" {
		t.Errorf("Model = %q, want opus", insts[0].Model)
	}
}

func TestWebHandler_PostTaskCreate_AgentOnly(t *testing.T) {
	newTask := &orchestrator.Task{ID: "new-task-id", Title: "My Task"}
	svc := &stubWebService{createTaskResult: newTask}
	r := newTestWebHandlerWithTaskCreate(svc)

	body := url.Values{
		"title": {"My Task"},
		"agent": {"claude-code"},
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
	if len(call.Instructions) == 0 {
		t.Fatal("Instructions should be set when agent is provided")
	}
	var insts orchestrator.Instructions
	if err := json.Unmarshal(call.Instructions, &insts); err != nil {
		t.Fatalf("failed to unmarshal Instructions: %v", err)
	}
	if insts[0].Agent != "claude-code" {
		t.Errorf("Agent = %q, want claude-code", insts[0].Agent)
	}
	if insts[0].Model != "" {
		t.Errorf("Model = %q, want empty (unset)", insts[0].Model)
	}
}

func TestWebHandler_PostTaskCreate_NoAgentNoModel(t *testing.T) {
	newTask := &orchestrator.Task{ID: "new-task-id", Title: "My Task"}
	svc := &stubWebService{createTaskResult: newTask}
	r := newTestWebHandlerWithTaskCreate(svc)

	body := url.Values{
		"title": {"My Task"},
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
	if len(call.Instructions) != 0 {
		t.Errorf("Instructions should be nil when neither agent nor model is provided, got: %s", call.Instructions)
	}
}

func TestWebHandler_TaskNew_RendersAgentAndModelFields(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerWithTaskCreate(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/new", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `name="agent"`) {
		t.Error("form should contain agent field")
	}
	if !strings.Contains(body, `name="model"`) {
		t.Error("form should contain model field")
	}
}

func TestWebHandler_PostTaskCreate_ValidationError_PreservesAgentModel(t *testing.T) {
	svc := &stubWebService{
		projects: []*orchestrator.Project{{ID: "proj-1"}},
	}
	r := newTestWebHandlerWithTaskCreate(svc)

	body := url.Values{
		"title": {""},
		"agent": {"claude-code"},
		"model": {"sonnet"},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, `value="claude-code"`) {
		t.Errorf("response should preserve agent value, got: %s", respBody)
	}
	if !strings.Contains(respBody, `value="sonnet"`) {
		t.Errorf("response should preserve model value, got: %s", respBody)
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
