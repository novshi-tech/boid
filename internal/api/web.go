package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/api/auth"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/timeline"
	"github.com/novshi-tech/boid/web/templates"
	"github.com/novshi-tech/boid/web/templates/components"
)

// detailTimelineGroups builds the status-grouped timeline for the Web UI
// task detail page. The shared timeline package groups actions and jobs
// into status-visit sections; we convert api.Job → timeline.JobInfo here
// so the timeline package stays api-free (which keeps it importable from
// web/templates without cycling through internal/api).
func detailTimelineGroups(detail *TaskDetailView) []timeline.StatusGroup {
	if detail == nil || detail.Task == nil {
		return nil
	}
	infos := make([]*timeline.JobInfo, 0, len(detail.Jobs))
	for _, j := range detail.Jobs {
		if j == nil {
			continue
		}
		infos = append(infos, &timeline.JobInfo{
			ID:          j.ID,
			Role:        j.Role,
			HandlerID:   j.HandlerID,
			DisplayName: j.DisplayName,
			Status:      string(j.Status),
			ExitCode:    j.ExitCode,
			CreatedAt:   j.CreatedAt,
			UpdatedAt:   j.UpdatedAt,
		})
	}
	return timeline.Build(detail.Task, detail.Actions, infos)
}


type WebHandler struct {
	Service        WebService
	Hub            *TaskEventHub
	Dispatcher     CommandDispatcher
	TaskDispatcher TaskCommandDispatcher
	Registry       *auth.ConnectionRegistry
}

func (h *WebHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.TaskList)
	r.Get("/tasks/new", h.TaskNew)
	r.Post("/tasks", h.PostTaskCreate)
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Get("/tasks/{id}/fragment", h.TaskDetailFragment)
	r.Get("/tasks/{id}/edit", h.GetTaskEdit)
	r.Post("/tasks/{id}/edit", h.PostEdit)
	r.Post("/tasks/{id}/action", h.PostAction)
	r.Post("/tasks/{id}/duplicate", h.PostDuplicate)
	r.Post("/tasks/{id}/rerun", h.PostRerun)
	r.Get("/tasks/{id}/reopen", h.ReopenForm)
	r.Post("/tasks/{id}/reopen", h.PostReopen)
	r.Post("/tasks/{id}/delete", h.PostDelete)
	r.Post("/tasks/{id}/answer", h.PostAnswer)
	r.Get("/tasks/{id}/questions/{question_id}", h.QuestionPage)
	r.Get("/tasks/{id}/hooks", h.HookReplayList)
	r.Post("/tasks/{id}/hooks/{hook_id}/replay", h.PostHookReplay)
	r.Get("/sessions", h.SessionList)
	r.Get("/jobs/{id}", h.JobDetail)
	r.Get("/jobs/{id}/terminal", h.JobTerminal)
	r.Get("/projects/{id}/commands", h.ProjectCommandList)
	r.Post("/projects/{id}/commands/{name}/execute", h.PostProjectExecuteCommand)
	r.Post("/tasks/{id}/commands/{name}/execute", h.PostTaskExecuteCommand)
	return r
}

func (h *WebHandler) TaskNew(w http.ResponseWriter, r *http.Request) {
	projects, _ := h.Service.ListProjects()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.TaskNew(projects, "", nil).Render(r.Context(), w)
}

func (h *WebHandler) PostTaskCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		projects, _ := h.Service.ListProjects()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		templates.TaskNew(projects, "リクエストの解析に失敗しました", nil).Render(r.Context(), w)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		projects, _ := h.Service.ListProjects()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		templates.TaskNew(projects, "タイトルは必須です", r.PostForm).Render(r.Context(), w)
		return
	}

	req := CreateTaskRequest{
		ProjectID:   r.FormValue("project_id"),
		Title:       title,
		Behavior:    r.FormValue("behavior"),
		Description: r.FormValue("description"),
		RemoteID:    r.FormValue("remote_id"),
		ParentID:    r.FormValue("parent_id"),
		AutoStart:   r.FormValue("auto_start") == "on",
	}

	if raw := strings.TrimSpace(r.FormValue("traits")); raw != "" {
		req.Traits = strings.Fields(raw)
	}

	// Phase 2-3: task-row overrides for base_branch / branch_prefix / worktree /
	// readonly were removed. Values are derived from the behavior type and
	// project-level defaults; the Web form no longer exposes these inputs.

	task, err := h.Service.CreateTask(req)
	if err != nil {
		projects, _ := h.Service.ListProjects()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		templates.TaskNew(projects, err.Error(), r.PostForm).Render(r.Context(), w)
		return
	}

	http.Redirect(w, r, "/tasks/"+task.ID, http.StatusSeeOther)
}

func (h *WebHandler) TaskList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := orchestrator.TaskFilter{
		Status:      q.Get("status"),
		ProjectID:   q.Get("project"),
		Behavior:    q.Get("behavior"),
		WorkspaceID: q.Get("workspace"),
		Title:       q.Get("q"),
	}
	// Web UI defaults to "open" when no status is specified.
	if filter.Status == "" {
		filter.Status = "open"
	}

	projects, _ := h.Service.ListProjects()
	projects = filterProjectsByWorkspace(projects, filter.WorkspaceID)
	// Clear project filter when the selected project is not in the workspace.
	if !projectInList(projects, filter.ProjectID) {
		filter.ProjectID = ""
	}

	tasks, err := h.Service.ListTasks(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	projectNames := projectNameMap(projects)
	var items []components.TreeItem
	if filter.Status == "closed" {
		items = BuildFlatItems(tasks, projectNames)
	} else {
		items = BuildTreeItems(tasks, projectNames)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if r.Header.Get("HX-Target") == "main-content" {
		workspaces, _ := h.Service.ListWorkspaces()
		templates.TaskListContent(items, filter, projects, workspaces, r.URL.RequestURI()).Render(r.Context(), w)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		templates.TaskListFragment(items, filter, r.URL.RequestURI()).Render(r.Context(), w)
		return
	}

	workspaces, _ := h.Service.ListWorkspaces()
	templates.TaskList(items, filter, projects, workspaces, r.URL.RequestURI()).Render(r.Context(), w)
}

func (h *WebHandler) SessionList(w http.ResponseWriter, r *http.Request) {
	projectFilter := r.URL.Query().Get("project")
	jobs, err := h.Service.ListSessions()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if projectFilter != "" {
		filtered := jobs[:0]
		for _, j := range jobs {
			if j.ProjectID == projectFilter {
				filtered = append(filtered, j)
			}
		}
		jobs = filtered
	}
	sessions := make([]templates.SessionView, 0, len(jobs))
	for _, j := range jobs {
		sessions = append(sessions, templates.SessionView{
			ID:          j.ID,
			ProjectID:   j.ProjectID,
			ProjectName: j.ProjectName,
			HandlerID:   j.HandlerID,
			DisplayName: j.DisplayName,
			CreatedAt:   j.CreatedAt,
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.SessionList(sessions, projectFilter).Render(r.Context(), w)
}

// filterProjectsByWorkspace filters projects to only those in the given workspace.
// If workspaceID is empty, all projects are returned.
func filterProjectsByWorkspace(projects []*orchestrator.Project, workspaceID string) []*orchestrator.Project {
	if workspaceID == "" {
		return projects
	}
	filtered := make([]*orchestrator.Project, 0, len(projects))
	for _, p := range projects {
		if p.WorkspaceID == workspaceID {
			filtered = append(filtered, p)
		}
	}
	return filtered
}

// projectInList returns true if projectID is empty or found in the project list.
func projectInList(projects []*orchestrator.Project, projectID string) bool {
	if projectID == "" {
		return true
	}
	for _, p := range projects {
		if p.ID == projectID {
			return true
		}
	}
	return false
}

func (h *WebHandler) TaskDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	jobs := make([]*templates.JobView, 0, len(detail.Jobs))
	for _, job := range detail.Jobs {
		jobs = append(jobs, &templates.JobView{
			ID:        job.ID,
			HandlerID: job.HandlerID,
			Role:      job.Role,
			Status:    string(job.Status),
			ExitCode:  job.ExitCode,
			CreatedAt: job.CreatedAt,
			UpdatedAt: job.UpdatedAt,
			Output:    job.Output,
		})
	}
	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "timeline"
	}
	errorMsg := r.URL.Query().Get("error")
	timelineGroups := detailTimelineGroups(detail)
	if r.Header.Get("HX-Request") == "true" {
		// Tab clicks swap the entire #tabs section so the active class on
		// the visible tabs and the "more" summary label stay in sync.
		templates.TaskDetailTabsSection(detail.Task, timelineGroups, jobs, detail.AvailableActions, tab).Render(r.Context(), w)
		return
	}
	projectName := h.lookupProjectName(detail.Task.ProjectID)
	cmdSummaries, _ := h.Service.ListTaskBehaviorCommands(id)
	cmdViews := make([]templates.CommandView, len(cmdSummaries))
	for i, c := range cmdSummaries {
		cmdViews[i] = templates.CommandView{Name: c.Name, Command: c.Command, Readonly: c.Readonly}
	}
	templates.TaskDetail(detail.Task, timelineGroups, jobs, detail.AvailableActions, errorMsg, tab, projectName, cmdViews).Render(r.Context(), w)
}

// lookupProjectName resolves a project ID to its display name (Meta.Name),
// returning "" when the project or name is missing.
func (h *WebHandler) lookupProjectName(projectID string) string {
	if projectID == "" {
		return ""
	}
	projects, err := h.Service.ListProjects()
	if err != nil {
		return ""
	}
	for _, p := range projects {
		if p.ID == projectID {
			return p.Meta.Name
		}
	}
	return ""
}

// TaskDetailFragment returns a partial HTML fragment for the task detail page.
// The `kind` query parameter selects which section to render:
//   - "timeline": action history section
//   - "status":   status card + available actions
//   - "jobs":     jobs section
func (h *WebHandler) TaskDetailFragment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	jobs := make([]*templates.JobView, 0, len(detail.Jobs))
	for _, job := range detail.Jobs {
		jobs = append(jobs, &templates.JobView{
			ID:        job.ID,
			HandlerID: job.HandlerID,
			Role:      job.Role,
			Status:    string(job.Status),
			ExitCode:  job.ExitCode,
			CreatedAt: job.CreatedAt,
			UpdatedAt: job.UpdatedAt,
			Output:    job.Output,
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	kind := r.URL.Query().Get("kind")
	switch kind {
	case "timeline":
		templates.TaskDetailTimelineSection(detail.Task, detailTimelineGroups(detail)).Render(r.Context(), w)
	case "status":
		projectName := h.lookupProjectName(detail.Task.ProjectID)
		templates.TaskDetailStatusSection(detail.Task, detail.AvailableActions, "", projectName).Render(r.Context(), w)
	case "jobs":
		templates.TaskDetailJobsSection(jobs).Render(r.Context(), w)
	default:
		http.Error(w, "unknown fragment kind", http.StatusBadRequest)
	}
}

func (h *WebHandler) PostAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actionType := r.FormValue("type")
	if actionType == "" {
		http.Redirect(w, r, "/tasks/"+id+"?error=type+is+required", http.StatusSeeOther)
		return
	}
	if err := h.Service.ApplyAction(id, actionType); err != nil {
		http.Redirect(w, r, "/tasks/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/tasks/"+id, http.StatusSeeOther)
}

func (h *WebHandler) GetTaskEdit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}
	if detail.Task.Status != orchestrator.TaskStatusPending {
		http.Redirect(w, r, "/tasks/"+id, http.StatusSeeOther)
		return
	}
	projects, _ := h.Service.ListProjects()
	errorMsg := r.URL.Query().Get("error")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.TaskEditPage(detail.Task, projects, errorMsg).Render(r.Context(), w)
}

func (h *WebHandler) PostEdit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/tasks/"+id+"/edit?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Redirect(w, r, "/tasks/"+id+"/edit?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	projectID := strings.TrimSpace(r.FormValue("project_id"))
	description := r.FormValue("description")
	message := r.FormValue("message")
	model := strings.TrimSpace(r.FormValue("model"))
	agent := strings.TrimSpace(r.FormValue("agent"))

	insts := detail.Task.Instructions
	if len(insts) == 0 {
		insts = orchestrator.Instructions{{
			Agent:   agent,
			Message: message,
			Model:   model,
		}}
	} else {
		clone := make(orchestrator.Instructions, len(insts))
		copy(clone, insts)
		active := clone[len(clone)-1]
		active.Message = message
		active.Model = model
		active.Agent = agent
		clone[len(clone)-1] = active
		insts = clone
	}

	instsJSON, err := json.Marshal(insts)
	if err != nil {
		http.Redirect(w, r, "/tasks/"+id+"/edit?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	remoteID := r.FormValue("remote_id")
	req := UpdateTaskRequest{
		Title:        title,
		ProjectID:    projectID,
		Description:  description,
		RemoteID:     &remoteID,
		Instructions: json.RawMessage(instsJSON),
	}

	target := "/tasks/" + id
	if err := h.Service.UpdateTask(id, req); err != nil {
		target = "/tasks/" + id + "/edit?error=" + url.QueryEscape(err.Error())
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *WebHandler) PostDuplicate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	newID, err := h.Service.DuplicateTask(id)
	if err != nil {
		http.Redirect(w, r, "/tasks/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/tasks/"+newID, http.StatusSeeOther)
}

func (h *WebHandler) PostRerun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Service.RerunTask(id, RerunTaskRequest{}); err != nil {
		http.Redirect(w, r, "/tasks/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/tasks/"+id, http.StatusSeeOther)
}

func (h *WebHandler) ReopenForm(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Redirect(w, r, "/tasks/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if detail.Task.Status != orchestrator.TaskStatusDone && detail.Task.Status != orchestrator.TaskStatusAborted {
		http.Redirect(w, r, "/tasks/"+id+"?error=reopen+is+only+available+for+done+or+aborted+tasks", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.TaskReopen(detail.Task).Render(r.Context(), w)
}

func (h *WebHandler) PostReopen(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/tasks/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	message := strings.TrimSpace(r.FormValue("message"))
	if err := h.Service.ReopenTask(id, ReopenTaskRequest{Message: message}); err != nil {
		http.Redirect(w, r, "/tasks/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/tasks/"+id, http.StatusSeeOther)
}

// QuestionPage renders the dedicated Q&A turn page at
// `/tasks/{id}/questions/{question_id}`. The notification deep-link from
// `boid task notify --ask` lands here. The page shows the question and either
// an answer form (when this is the active awaiting turn) or the recorded
// answer (when an answer action exists for the same question_id).
func (h *WebHandler) QuestionPage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	questionID := chi.URLParam(r, "question_id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	var (
		question string
		answer   string
		found    bool
	)
	for _, a := range detail.Actions {
		ap := orchestrator.GetAwaitingPayload(a.Payload)
		if ap.QuestionID != questionID {
			continue
		}
		switch a.Type {
		case "ask":
			question = ap.Question
			found = true
		case "answer":
			if ap.PendingAnswer != "" {
				answer = ap.PendingAnswer
			}
		}
	}
	if !found {
		http.Error(w, "Question not found", http.StatusNotFound)
		return
	}

	currentAwaiting := orchestrator.GetAwaitingPayload(detail.Task.Payload)
	isActive := detail.Task.Status == orchestrator.TaskStatusAwaiting && currentAwaiting.QuestionID == questionID

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.QuestionPage(detail.Task, templates.QuestionTurn{
		QuestionID: questionID,
		Question:   question,
		Answer:     answer,
		IsActive:   isActive,
		WasAborted: detail.Task.Status == orchestrator.TaskStatusAborted && answer == "",
	}).Render(r.Context(), w)
}

func (h *WebHandler) PostAnswer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/tasks/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	questionID := r.FormValue("question_id")
	answer := strings.TrimSpace(r.FormValue("answer"))
	target := "/tasks/" + id
	if err := h.Service.AnswerTask(r.Context(), id, questionID, answer); err != nil {
		target = "/tasks/" + id + "?error=" + url.QueryEscape(err.Error())
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// PostDelete deletes the task and redirects to the task list.
// Errors are surfaced via ?error= on the same task page so the user sees the
// reason (e.g. dependents exist).
func (h *WebHandler) PostDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Service.DeleteTask(id, false); err != nil {
		http.Redirect(w, r, "/tasks/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *WebHandler) HookReplayList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	status := r.URL.Query().Get("status")
	hooks, err := h.Service.ListHooksForStatus(id, status)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	errorMsg := r.URL.Query().Get("error")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.HookReplayList(id, status, hooks, errorMsg).Render(r.Context(), w)
}

func (h *WebHandler) PostHookReplay(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	hookID, err := url.PathUnescape(chi.URLParam(r, "hook_id"))
	if err != nil {
		http.Error(w, "invalid hook id", http.StatusBadRequest)
		return
	}
	_, err = h.Service.ReplayHook(r.Context(), id, ReplayHookRequest{HookID: hookID})
	if err != nil {
		http.Redirect(w, r, "/tasks/"+id+"/hooks?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/tasks/"+id, http.StatusSeeOther)
}

func (h *WebHandler) JobDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	job, err := h.Service.GetJob(id)
	if err != nil {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}
	hookID := ""
	if job.Role == "hook" {
		hookID = job.HandlerID
	}
	view := &templates.JobContextView{
		ID:          job.ID,
		TaskID:      job.TaskID,
		ProjectID:   job.ProjectID,
		TaskTitle:   job.TaskTitle,
		HandlerID:   job.HandlerID,
		DisplayName: job.DisplayName,
		Role:        job.Role,
		HookID:      hookID,
		Status:      string(job.Status),
		ExitCode:    job.ExitCode,
		Interactive: job.Interactive,
		CreatedAt:   job.CreatedAt,
		UpdatedAt:   job.UpdatedAt,
		Output:      job.Output,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.JobDetail(view).Render(r.Context(), w)
}

func (h *WebHandler) JobTerminal(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	job, err := h.Service.GetJob(id)
	if err != nil {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if job.Status != JobStatusRunning {
		templates.TerminalNotReady(id, "現在 attach できる状態ではありません（ジョブが実行中ではありません）。").Render(r.Context(), w)
		return
	}
	view := &templates.JobContextView{
		ID:          job.ID,
		TaskID:      job.TaskID,
		ProjectID:   job.ProjectID,
		TaskTitle:   job.TaskTitle,
		HandlerID:   job.HandlerID,
		DisplayName: job.DisplayName,
		Role:        job.Role,
		Status:      string(job.Status),
		ExitCode:    job.ExitCode,
		Interactive: job.Interactive,
		CreatedAt:   job.CreatedAt,
		UpdatedAt:   job.UpdatedAt,
		Output:      job.Output,
	}
	wsPath := "/api/jobs/" + id + "/attach/ws"
	templates.TerminalPage(buildJobTitle(view), "/jobs/"+id, id, wsPath).Render(r.Context(), w)
}

func (h *WebHandler) ProjectCommandList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	project, err := h.Service.GetProjectByID(id)
	if err != nil {
		http.Error(w, "Project not found", http.StatusNotFound)
		return
	}

	commands, err := h.Service.ListProjectCommands(id)
	var errorMsg string
	if err != nil {
		errorMsg = err.Error()
	}

	views := make([]templates.CommandView, len(commands))
	for i, cmd := range commands {
		views[i] = templates.CommandView{
			Name:     cmd.Name,
			Command:  cmd.Command,
			Readonly: cmd.Readonly,
		}
	}

	projectName := project.Meta.Name
	if projectName == "" {
		projectName = project.ID
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.ProjectCommandList(projectName, id, views, errorMsg).Render(r.Context(), w)
}

func (h *WebHandler) PostTaskExecuteCommand(w http.ResponseWriter, r *http.Request) {
	if h.TaskDispatcher == nil {
		http.Error(w, "command execution not available", http.StatusNotImplemented)
		return
	}
	taskID := chi.URLParam(r, "id")
	commandName := chi.URLParam(r, "name")

	result, err := h.TaskDispatcher.ExecuteTaskBehaviorCommand(r.Context(), taskID, commandName)
	if err != nil {
		backURL := "/tasks/" + taskID + "?error=" + url.QueryEscape(err.Error())
		http.Redirect(w, r, backURL, http.StatusSeeOther)
		return
	}

	termURL := "/jobs/" + result.JobID + "/terminal"
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", termURL)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, termURL, http.StatusSeeOther)
}

func (h *WebHandler) PostProjectExecuteCommand(w http.ResponseWriter, r *http.Request) {
	if h.Dispatcher == nil {
		http.Error(w, "command execution not available", http.StatusNotImplemented)
		return
	}
	projectID := chi.URLParam(r, "id")
	commandName := chi.URLParam(r, "name")

	result, err := h.Dispatcher.ExecuteCommand(r.Context(), projectID, commandName)
	if err != nil {
		backURL := "/projects/" + projectID + "/commands?error=" + url.QueryEscape(err.Error())
		http.Redirect(w, r, backURL, http.StatusSeeOther)
		return
	}

	termURL := "/jobs/" + result.JobID + "/terminal"
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", termURL)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, termURL, http.StatusSeeOther)
}

// buildJobTitle returns a display title for the job terminal page.
// Mirrors the jobPageTitle logic in web/templates/jobs.templ.
func buildJobTitle(job *templates.JobContextView) string {
	switch {
	case job.Role != "" && job.HandlerID != "":
		return "[" + job.Role + "] " + job.HandlerID
	case job.HandlerID != "":
		return job.HandlerID
	case job.Role != "":
		return "[" + job.Role + "]"
	default:
		if len(job.ID) > 8 {
			return "Job " + job.ID[:8]
		}
		return "Job " + job.ID
	}
}

// WebManagementHandler serves the CLI management API at /api/web/*.
// All routes are accessible only via UNIX socket (CLI control plane).
// Pairer issues pairing codes.
type Pairer interface {
	Issue(ctx context.Context, label string) (string, error)
}

type WebManagementHandler struct {
	Pairing   Pairer
	Store     *auth.Store
	PublicURL string
	Registry  *auth.ConnectionRegistry
}

func (h *WebManagementHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/pair", h.PostPair)
	r.Get("/devices", h.GetDevices)
	r.Delete("/devices/{id}", h.DeleteDevice)
	r.Delete("/devices", h.DeleteAllDevices)
	return r
}

type pairResponse struct {
	Code      string `json:"code"`
	URL       string `json:"url,omitempty"`
	ExpiresIn int    `json:"expires_in"`
}

func (h *WebManagementHandler) PostPair(w http.ResponseWriter, r *http.Request) {
	var req auth.PairRequest
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req) //nolint: errcheck — label is optional
	}
	code, err := h.Pairing.Issue(r.Context(), req.Label)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := pairResponse{
		Code:      code,
		ExpiresIn: 300,
	}
	if h.PublicURL != "" {
		resp.URL = h.PublicURL + "/auth?token=" + url.QueryEscape(code)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

type deviceResponse struct {
	ID         string `json:"id"`
	Label      string `json:"label,omitempty"`
	CreatedAt  string `json:"created_at"`
	LastSeenAt string `json:"last_seen_at"`
}

func (h *WebManagementHandler) GetDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := h.Store.ListDevices(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := make([]deviceResponse, 0, len(devices))
	for _, d := range devices {
		if d.RevokedAt != nil {
			continue
		}
		resp = append(resp, deviceResponse{
			ID:         d.ID,
			Label:      d.Label,
			CreatedAt:  d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			LastSeenAt: d.LastSeenAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *WebManagementHandler) DeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	err := h.Store.RevokeDevice(r.Context(), id)
	if errors.Is(err, auth.ErrDeviceNotFound) {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if h.Registry != nil {
		h.Registry.RevokeDevice(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *WebManagementHandler) DeleteAllDevices(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.RevokeAllDevices(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if h.Registry != nil {
		h.Registry.RevokeAll()
	}
	w.WriteHeader(http.StatusNoContent)
}

// loginPairing redeems a one-time pairing code.
type loginPairing interface {
	Redeem(ctx context.Context, code string) (string, error)
}

// loginSigner issues a session cookie.
type loginSigner interface {
	Issue(w http.ResponseWriter, deviceID string) error
}

// loginDeviceStore persists a new device after successful pairing.
type loginDeviceStore interface {
	InsertDevice(ctx context.Context, id, label string, cookieHash []byte) error
}

// loginRateLimiter guards against brute-force attempts.
type loginRateLimiter interface {
	Allowed(ip string) bool
	RecordFailure(ip string)
}

// LoginHandler handles /login and /auth.
type LoginHandler struct {
	Pairing loginPairing
	Signer  loginSigner
	Store   loginDeviceStore
	Limiter loginRateLimiter
}

func (h *LoginHandler) GetLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.Login(r.URL.Query().Get("error")).Render(r.Context(), w)
}

func (h *LoginHandler) PostLogin(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	if !h.Limiter.Allowed(ip) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	code := r.FormValue("code")
	label, err := h.Pairing.Redeem(r.Context(), code)
	if err != nil {
		h.Limiter.RecordFailure(ip)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		templates.Login("無効なペアリングコードです").Render(r.Context(), w)
		return
	}
	if err := h.issueSession(w, r, label); err != nil {
		h.Limiter.RecordFailure(ip)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *LoginHandler) GetAuth(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	if !h.Limiter.Allowed(ip) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	token := r.URL.Query().Get("token")
	label, err := h.Pairing.Redeem(r.Context(), token)
	if err != nil {
		h.Limiter.RecordFailure(ip)
		http.Redirect(w, r, "/login?error=invalid_code", http.StatusFound)
		return
	}
	if err := h.issueSession(w, r, label); err != nil {
		h.Limiter.RecordFailure(ip)
		http.Redirect(w, r, "/login?error=invalid_code", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// issueSession creates a new device row and sets the session cookie.
func (h *LoginHandler) issueSession(w http.ResponseWriter, r *http.Request, label string) error {
	if h.Signer == nil {
		return fmt.Errorf("session signer not configured")
	}
	deviceID := uuid.New().String()
	sum := sha256.Sum256([]byte(deviceID))
	if err := h.Store.InsertDevice(r.Context(), deviceID, label, sum[:]); err != nil {
		return err
	}
	return h.Signer.Issue(w, deviceID)
}

// remoteIP extracts the real client IP for per-client rate limiting.
// It checks proxy headers in order so that cloudflared and other reverse proxies
// get a fair per-client bucket. This is best-effort extraction, not spoof prevention.
func remoteIP(r *http.Request) string {
	// CF-Connecting-IP: set by Cloudflare edge, overwritten at ingress — most reliable.
	if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
		if ip := net.ParseIP(cf); ip != nil {
			return ip.String()
		}
	}
	// X-Forwarded-For: leftmost entry is the originating client.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
			return ip.String()
		}
	}
	// Fallback to the TCP peer address.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
