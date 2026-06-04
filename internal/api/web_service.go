package api

import (
	"context"
	"encoding/json"
	"fmt"
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
	dependents, _ := s.Tasks.FindDependentTasks(task.ID)

	return &TaskDetailView{
		Task:             task,
		Actions:          actions,
		Jobs:             jobs,
		AvailableActions: orchestrator.DefaultMachine().AvailableActions(task.Status),
		Dependents:       dependents,
	}, nil
}

func (s *WebAppService) ListProjects() ([]*orchestrator.Project, error) {
	projects, err := s.Projects.ListProjects()
	if err != nil {
		return nil, err
	}
	for _, project := range projects {
		if meta, ok := s.Meta.Get(project.ID); ok {
			project.Meta = *meta
		}
	}
	return projects, nil
}

// DuplicateTask delegates to the shared TaskService so the Web UI uses the
// same duplication semantics as the JSON API: a fresh task is created via
// CreateTask + resolveBehavior so that Instructions and Payload come from
// the behavior's DefaultInstruction / DefaultPayload, not from the source
// task's runtime state. Without this delegation the duplicate inherited
// the source's runtime payload (claude_code.sessions, awaiting trait) and
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
	_, err := s.Workflow.ApplyAction(context.Background(), taskID, ApplyActionRequest{Type: actionType})
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
	_, err := s.Workflow.ApplyAction(context.Background(), id, ApplyActionRequest{Type: "reopen", Payload: payload})
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
	if meta, ok := s.Meta.Get(id); ok {
		project.Meta = *meta
	}
	return project, nil
}

func (s *WebAppService) ListProjectCommands(projectID string) ([]CommandSummary, error) {
	meta, ok := s.Meta.Get(projectID)
	if !ok {
		return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("project %q meta not loaded", projectID)}
	}
	summaries := make([]CommandSummary, 0, len(meta.Commands))
	for name, cmd := range meta.Commands {
		summaries = append(summaries, CommandSummary{Name: name, Command: cmd.ResolvedCommand, Readonly: cmd.Readonly})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })
	return summaries, nil
}

func (s *WebAppService) ListTaskBehaviorCommands(taskID string) ([]CommandSummary, error) {
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	meta, ok := s.Meta.Get(task.ProjectID)
	if !ok {
		return []CommandSummary{}, nil
	}
	behavior, _, ok := orchestrator.LookupBehaviorWithAlias(meta, task.Behavior)
	if !ok {
		return []CommandSummary{}, nil
	}
	summaries := make([]CommandSummary, 0, len(behavior.Commands))
	for name, cmd := range behavior.Commands {
		summaries = append(summaries, CommandSummary{Name: name, Command: cmd.ResolvedCommand, Readonly: cmd.Readonly})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })
	return summaries, nil
}

func (s *WebAppService) AnswerTask(ctx context.Context, taskID, questionID, answer string) error {
	if s.Answerer == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "answer service not configured"}
	}
	return s.Answerer.AnswerTask(ctx, taskID, questionID, answer)
}
