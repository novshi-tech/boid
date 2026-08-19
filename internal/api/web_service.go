package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type WebAppService struct {
	Tasks      TaskStore
	Actions    ActionStore
	Jobs       JobStore
	GlobalJobs GlobalJobStore
	Projects   ProjectRepository
	Meta       MetaStore
	Workflow   WorkflowService
	TaskSvc    TaskService
	Hooks      HookService
	Answerer   TaskAnswerService // optional: enables POST /tasks/{id}/answer
}

func (s *WebAppService) CreateTask(req CreateTaskRequest) (*orchestrator.Task, error) {
	if s.TaskSvc == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "task service not configured"}
	}
	return s.TaskSvc.CreateTask(req)
}

func (s *WebAppService) UpdateTask(id string, req UpdateTaskRequest) error {
	if s.TaskSvc == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "task service not configured"}
	}
	_, err := s.TaskSvc.UpdateTask(id, req)
	return err
}

func (s *WebAppService) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return s.Tasks.ListTasks(filter)
}

func (s *WebAppService) ListBehaviors() ([]string, error) {
	tasks, err := s.Tasks.ListTasks(orchestrator.TaskFilter{})
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var behaviors []string
	for _, t := range tasks {
		if t.Behavior != "" && !seen[t.Behavior] {
			seen[t.Behavior] = true
			behaviors = append(behaviors, t.Behavior)
		}
	}
	sort.Strings(behaviors)
	return behaviors, nil
}

func (s *WebAppService) ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error) {
	return s.Projects.ListWorkspaces()
}

func (s *WebAppService) GetTaskDetail(id string) (*TaskDetailView, error) {
	task, err := s.Tasks.GetTask(id)
	if err != nil {
		return nil, err
	}

	actions, _ := s.Actions.ListActionsByTask(task.ID)
	rawJobs, _ := s.Jobs.ListJobsByTask(task.ID)
	for _, j := range rawJobs {
		enrichJobDisplayName(j, task.Behavior, s.Meta)
	}
	jobs := rawJobs

	return &TaskDetailView{
		Task:             task,
		Actions:          actions,
		Jobs:             jobs,
		AvailableActions: orchestrator.DefaultMachine().AvailableActions(task.Status),
	}, nil
}

func (s *WebAppService) ListProjects() ([]*orchestrator.Project, error) {
	projects, err := s.Projects.ListProjects()
	if err != nil {
		return nil, err
	}
	for _, project := range projects {
		s.hydrateProjectMeta(project)
	}
	return projects, nil
}

// hydrateProjectMeta populates project.Meta with workspace.yaml merged in
// (docs/plans/workspace-default-project.md §PR分割案 PR2, §現状の実測4's
// "Web UI project 一覧"/"Web UI project 単体" rows — fable review M1: this
// feeds the task-creation form's behavior dropdown, task_form.templ:32,50,
// not just display). Falls back to bare Get on any hydration failure —
// same idiom as ProjectAppService.hydrateProjectWithWorkspace
// (project_service.go) and TaskAppService.CreateTask (task_create.go)
// already use — reproducing the pre-PR2 "meta not found → leave Meta
// unset, no error" tolerance for a true cache miss, while degrading to the
// un-hydrated meta (rather than blanking Meta outright) for the two new
// failure modes GetWithWorkspace can produce that bare Get never could (a
// corrupt workspace.yaml, a host_commands conflict).
func (s *WebAppService) hydrateProjectMeta(project *orchestrator.Project) {
	if hydrated, err := s.Meta.GetWithWorkspace(context.Background(), project.ID); err == nil && hydrated != nil {
		project.Meta = *hydrated
		return
	}
	if meta, ok := s.Meta.Get(project.ID); ok {
		project.Meta = *meta
	}
}

// DuplicateTask delegates to the shared TaskService so the Web UI uses the
// same duplication semantics as the JSON API: a fresh task is created via
// CreateTask + resolveBehavior so that Instructions and Payload come from
// the behavior's DefaultInstruction / DefaultPayload, not from the source
// task's runtime state. Without this delegation the duplicate inherited
// the source's runtime payload (awaiting trait and harness session state) and
// missing Instructions caused the hook evaluator to skip the agent hook,
// so no hook fired on Start.
//
// The Web UI button does not auto-start the duplicate; the user clicks
// Start separately.
func (s *WebAppService) DuplicateTask(id string) (string, error) {
	if s.TaskSvc == nil {
		return "", &StatusError{Code: http.StatusInternalServerError, Message: "task service not configured"}
	}
	task, err := s.TaskSvc.DuplicateTask(id, false)
	if err != nil {
		return "", err
	}
	return task.ID, nil
}

// DeleteTask delegates to the shared TaskService so the web UI uses the
// same delete semantics as the JSON API and TUI.
func (s *WebAppService) DeleteTask(id string, force bool) error {
	if s.TaskSvc == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "task service not configured"}
	}
	return s.TaskSvc.DeleteTask(id, force)
}

func (s *WebAppService) ApplyAction(taskID string, actionType string) error {
	if s.Workflow == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "workflow service not configured"}
	}
	_, err := s.Workflow.ApplyAction(orchestrator.WithActor(context.Background(), orchestrator.ActorHuman), taskID, ApplyActionRequest{Type: actionType})
	return err
}

// Wake delegates to TaskWorkflowService.Wake — see WebService.Wake's own
// doc comment (store.go) for why this is a dedicated method rather than
// going through ApplyAction.
func (s *WebAppService) Wake(taskID string) error {
	if s.Workflow == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "workflow service not configured"}
	}
	_, err := s.Workflow.Wake(orchestrator.WithActor(context.Background(), orchestrator.ActorHuman), taskID)
	return err
}

func (s *WebAppService) ListJobs(status string) ([]JobWithContext, error) {
	jobs, err := s.GlobalJobs.ListJobsWithContext(JobListFilter{Status: status})
	if err != nil {
		return nil, err
	}
	if jobs == nil {
		jobs = []JobWithContext{}
	}
	return jobs, nil
}

func (s *WebAppService) ListSessions() ([]JobWithContext, error) {
	jobs, err := s.GlobalJobs.ListJobsWithContext(JobListFilter{Status: "running", TasklessOnly: true})
	if err != nil {
		return nil, err
	}
	if jobs == nil {
		jobs = []JobWithContext{}
	}
	return jobs, nil
}

func (s *WebAppService) GetJob(id string) (*JobWithContext, error) {
	job, err := s.Jobs.GetJob(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	result := &JobWithContext{Job: *job}
	if task, err := s.Tasks.GetTask(job.TaskID); err == nil {
		result.TaskTitle = task.Title
		enrichJobDisplayName(&result.Job, task.Behavior, s.Meta)
	}
	return result, nil
}

func (s *WebAppService) RerunTask(id string, req RerunTaskRequest) error {
	if s.TaskSvc == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "task service not configured"}
	}
	_, err := s.TaskSvc.RerunTask(id, req)
	return err
}

type ReopenTaskRequest struct {
	Message string `json:"message,omitempty"`
}

func (s *WebAppService) ReopenTask(id string, req ReopenTaskRequest) error {
	if s.Workflow == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "workflow service not configured"}
	}
	var payload json.RawMessage
	if req.Message != "" {
		b, err := json.Marshal(map[string]any{
			"instruction": map[string]any{"message": req.Message},
		})
		if err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: "payload encode: " + err.Error()}
		}
		payload = b
	}
	_, err := s.Workflow.ApplyAction(orchestrator.WithActor(context.Background(), orchestrator.ActorHuman), id, ApplyActionRequest{Type: "reopen", Payload: payload})
	return err
}

func (s *WebAppService) ListHooksForStatus(taskID, status string) ([]orchestrator.Hook, error) {
	if s.Hooks == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "hook service not configured"}
	}
	return s.Hooks.ListHooksForStatus(taskID, status)
}

func (s *WebAppService) ReplayHook(ctx context.Context, taskID string, req ReplayHookRequest) (*ReplayHookResult, error) {
	if s.Hooks == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "hook service not configured"}
	}
	return s.Hooks.ReplayHook(ctx, taskID, req)
}

func (s *WebAppService) GetProjectByID(id string) (*orchestrator.Project, error) {
	project, err := s.Projects.GetProject(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	s.hydrateProjectMeta(project)
	return project, nil
}

func (s *WebAppService) AnswerTask(ctx context.Context, taskID, questionID, answer string) error {
	if s.Answerer == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "answer service not configured"}
	}
	return s.Answerer.AnswerTask(ctx, taskID, questionID, answer)
}

// AnswerSuggestionRequest is the Web UI's accept/reject form payload for a
// task_triage suggestion (docs/plans/ingestion-identity.md PR-3, J-6). Answer
// is required; Verb/Basis are optional (recorded, never validated by the
// daemon — see answeredPayload's own doc comment).
type AnswerSuggestionRequest struct {
	Answer string `json:"answer"`
	Verb   string `json:"verb,omitempty"`
	Basis  string `json:"basis,omitempty"`
}

// AnswerSuggestion sends an "answered" action for taskID, recording nose's
// accept/reject of the currently-shown suggestion (J-6's "既に開いている穴"
// — see WebService.AnswerSuggestion's own doc comment for why this is a
// dedicated method rather than routing through the generic ApplyAction,
// mirroring ReopenTask's own instruction-payload pattern just above).
func (s *WebAppService) AnswerSuggestion(taskID string, req AnswerSuggestionRequest) error {
	if s.Workflow == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "workflow service not configured"}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "payload encode: " + err.Error()}
	}
	_, err = s.Workflow.ApplyAction(orchestrator.WithActor(context.Background(), orchestrator.ActorHuman), taskID, ApplyActionRequest{Type: "answered", Payload: payload})
	return err
}
