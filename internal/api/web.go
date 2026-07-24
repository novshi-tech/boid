package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
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

// toJobViews converts Job records into the JobView shape used by the task
// detail templates.
func toJobViews(jobs []*Job) []*templates.JobView {
	views := make([]*templates.JobView, 0, len(jobs))
	for _, job := range jobs {
		views = append(views, &templates.JobView{
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
	return views
}


type WebHandler struct {
	Service           WebService
	Hub               *TaskEventHub
	SessionDispatcher SessionDispatcher
	Registry          *auth.ConnectionRegistry

	// AttachmentsRoot is the data-home directory under which per-task
	// attachments (`tasks/<id>/attachments`) are persisted. When empty (e.g.
	// :memory: DB during tests) the multipart code path falls back to
	// rejecting attachments while still accepting plain form-urlencoded
	// submissions.
	AttachmentsRoot string

	// ConfigService backs GET /settings (web_settings.go,
	// docs/plans/volume-only-daemon.md §論点 f) — nil in any test/wiring
	// that never registers the /settings route.
	ConfigService SettingsConfigService
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
	r.Get("/sessions/new", h.SessionNew)
	r.Get("/jobs/{id}", h.JobDetail)
	r.Get("/jobs/{id}/terminal", h.JobTerminal)
	r.Post("/projects/{id}/sessions/start", h.PostStartSession)
	r.Get("/settings", h.Settings)
	return r
}

// redirectTask redirects the client to the task detail page.
func redirectTask(w http.ResponseWriter, r *http.Request, id string) {
	http.Redirect(w, r, "/tasks/"+id, http.StatusSeeOther)
}

// redirectTaskErr redirects the client to the task detail page with err
// surfaced via the ?error= query parameter.
func redirectTaskErr(w http.ResponseWriter, r *http.Request, id string, err error) {
	http.Redirect(w, r, "/tasks/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
}

// redirectOrHXRedirect redirects to target. htmx form posts get the
// HX-Redirect response header plus a 200 status instead of a 3xx response,
// since htmx does not follow standard redirects for non-GET requests.
func redirectOrHXRedirect(w http.ResponseWriter, r *http.Request, target string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// renderTaskNewErr re-renders the "new task" form with msg surfaced as a
// validation error, preserving the previously submitted form values.
func (h *WebHandler) renderTaskNewErr(w http.ResponseWriter, r *http.Request, msg string, form url.Values) {
	projects, _ := h.Service.ListProjects()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	templates.TaskNew(projects, msg, form).Render(r.Context(), w)
}

func (h *WebHandler) TaskNew(w http.ResponseWriter, r *http.Request) {
	projects, _ := h.Service.ListProjects()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.TaskNew(projects, "", nil).Render(r.Context(), w)
}

func (h *WebHandler) PostTaskCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseTaskForm(r); err != nil {
		h.renderTaskNewErr(w, r, "リクエストの解析に失敗しました", nil)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		h.renderTaskNewErr(w, r, "タイトルは必須です", r.PostForm)
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

	agent := strings.TrimSpace(r.FormValue("agent"))
	model := strings.TrimSpace(r.FormValue("model"))
	if agent != "" || model != "" {
		instsJSON, err := json.Marshal(orchestrator.Instructions{{Agent: agent, Model: model}})
		if err != nil {
			h.renderTaskNewErr(w, r, err.Error(), r.PostForm)
			return
		}
		req.Instructions = instsJSON
	}

	// Phase 2-3: task-row overrides for base_branch / branch_prefix / worktree /
	// readonly were removed. Values are derived from the behavior type and
	// project-level defaults; the Web form no longer exposes these inputs.

	// Attachments are validated before task creation so a bad upload
	// (oversized, bad extension) doesn't leave a half-created task.
	uploads := taskFormAttachments(r)
	if len(uploads) > 0 {
		if h.AttachmentsRoot == "" {
			h.renderTaskNewErr(w, r, "添付ファイルを保存する場所が設定されていません", r.PostForm)
			return
		}
		if err := ValidateAttachmentHeaders(uploads); err != nil {
			h.renderTaskNewErr(w, r, err.Error(), r.PostForm)
			return
		}
	}

	task, err := h.Service.CreateTask(req)
	if err != nil {
		h.renderTaskNewErr(w, r, err.Error(), r.PostForm)
		return
	}

	if len(uploads) > 0 {
		if _, err := SaveMultipartAttachments(h.AttachmentsRoot, task.ID, uploads); err != nil {
			// Task is already created — surface the error via ?error= so the
			// user sees the task page with the failure context and can decide
			// whether to retry, delete, or proceed.
			redirectTaskErr(w, r, task.ID, fmt.Errorf("attachment save failed: %w", err))
			return
		}
	} else if h.AttachmentsRoot != "" {
		// Always pre-create the attachments dir so subsequent task-ask
		// answers can drop files into a live-bound location. Failure here is
		// non-fatal — the bind has an optional guard and the worst case is
		// the user re-attaches after we recover.
		_, _ = EnsureAttachmentsDir(h.AttachmentsRoot, task.ID)
	}

	redirectTask(w, r, task.ID)
}

// parseTaskForm dispatches on Content-Type so the same handler accepts both
// the legacy application/x-www-form-urlencoded submissions (still used by
// older clients and HTML form fallbacks) and the new multipart/form-data
// uploads coming from the clipboard-paste flow.
//
// net/http's ParseMultipartForm returns ErrNotMultipart for non-multipart
// bodies, so blindly calling it would break every existing form post — keep
// the explicit branch.
func parseTaskForm(r *http.Request) error {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/") {
		// 32 MB ceiling — slightly above the 30 MB per-task total cap so the
		// multipart parser doesn't reject a borderline-legal request before
		// our per-file / per-task limits kick in.
		return r.ParseMultipartForm(32 << 20)
	}
	return r.ParseForm()
}

// taskFormAttachments extracts uploaded files from the "attachments" multipart
// field. Safe to call when the request has no multipart body — it returns
// nil in that case.
func taskFormAttachments(r *http.Request) []*multipart.FileHeader {
	if r.MultipartForm == nil || r.MultipartForm.File == nil {
		return nil
	}
	return r.MultipartForm.File["attachments"]
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

func (h *WebHandler) SessionNew(w http.ResponseWriter, r *http.Request) {
	projects, _ := h.Service.ListProjects()
	selectedProjectID := r.URL.Query().Get("project")
	errorMsg := r.URL.Query().Get("error")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.SessionNew(projects, selectedProjectID, errorMsg).Render(r.Context(), w)
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
	jobs := toJobViews(detail.Jobs)
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
	templates.TaskDetail(detail.Task, timelineGroups, jobs, detail.AvailableActions, errorMsg, tab, projectName).Render(r.Context(), w)
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

	jobs := toJobViews(detail.Jobs)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	kind := r.URL.Query().Get("kind")
	switch kind {
	case "timeline":
		templates.TaskDetailTimelineSection(detail.Task, detailTimelineGroups(detail)).Render(r.Context(), w)
	case "status":
		projectName := h.lookupProjectName(detail.Task.ProjectID)
		templates.TaskDetailStatusSection(detail.Task, detail.AvailableActions, "", projectName).Render(r.Context(), w)
	case "jobs":
		templates.TaskDetailJobsSection(detail.Task, jobs).Render(r.Context(), w)
	default:
		http.Error(w, "unknown fragment kind", http.StatusBadRequest)
	}
}

func (h *WebHandler) PostAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actionType := r.FormValue("type")
	if actionType == "" {
		redirectTaskErr(w, r, id, errors.New("type is required"))
		return
	}
	if err := h.Service.ApplyAction(id, actionType); err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	redirectTask(w, r, id)
}

func (h *WebHandler) GetTaskEdit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}
	if detail.Task.Status != orchestrator.TaskStatusPending {
		redirectTask(w, r, id)
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

	redirectOrHXRedirect(w, r, target)
}

func (h *WebHandler) PostDuplicate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	newID, err := h.Service.DuplicateTask(id)
	if err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	redirectTask(w, r, newID)
}

func (h *WebHandler) PostRerun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Service.RerunTask(id, RerunTaskRequest{}); err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	redirectTask(w, r, id)
}

func (h *WebHandler) ReopenForm(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	if detail.Task.Status != orchestrator.TaskStatusDone && detail.Task.Status != orchestrator.TaskStatusAborted {
		redirectTaskErr(w, r, id, errors.New("reopen is only available for done or aborted tasks"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.TaskReopen(detail.Task).Render(r.Context(), w)
}

func (h *WebHandler) PostReopen(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	message := strings.TrimSpace(r.FormValue("message"))
	if err := h.Service.ReopenTask(id, ReopenTaskRequest{Message: message}); err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	redirectTask(w, r, id)
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
	if err := parseTaskForm(r); err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	questionID := r.FormValue("question_id")
	answer := strings.TrimSpace(r.FormValue("answer"))

	// Validate + persist attachments before submitting the answer so the
	// running agent observes a consistent view: any file referenced in the
	// answer text (via the `[attachment: <name>]` marker inserted by the
	// paste-attach JS) must already be on disk by the time AnswerTask wakes
	// up the task, so a `boid task attachments get <name>` call issued right
	// after the wake-up finds it immediately.
	uploads := taskFormAttachments(r)
	if len(uploads) > 0 {
		if h.AttachmentsRoot == "" {
			redirectTaskErr(w, r, id, errors.New("attachments root not configured"))
			return
		}
		if err := ValidateAttachmentHeaders(uploads); err != nil {
			redirectTaskErr(w, r, id, err)
			return
		}
		if _, err := SaveMultipartAttachments(h.AttachmentsRoot, id, uploads); err != nil {
			redirectTaskErr(w, r, id, fmt.Errorf("attachment save failed: %w", err))
			return
		}
	}

	target := "/tasks/" + id
	if err := h.Service.AnswerTask(r.Context(), id, questionID, answer); err != nil {
		target = "/tasks/" + id + "?error=" + url.QueryEscape(err.Error())
	}
	redirectOrHXRedirect(w, r, target)
}

// PostDelete deletes the task and redirects to the task list.
// Errors are surfaced via ?error= on the same task page so the user sees the
// reason (e.g. dependents exist).
func (h *WebHandler) PostDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Service.DeleteTask(id, false); err != nil {
		redirectTaskErr(w, r, id, err)
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
	redirectTask(w, r, id)
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

// JobTerminal redirects legacy deep-links (/jobs/{id}/terminal) to the job
// detail page (/jobs/{id}), which now renders the terminal inline.
func (h *WebHandler) JobTerminal(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	http.Redirect(w, r, "/jobs/"+id, http.StatusFound)
}

// PostStartSession launches a HarnessAdapter-backed session for the project
// from the Web UI's [New Session] dialog. Phase 3-d (PR1) introduced this
// alongside PostProjectExecuteCommand so users can start a claude / codex /
// opencode session without going through the legacy commands path.
func (h *WebHandler) PostStartSession(w http.ResponseWriter, r *http.Request) {
	if h.SessionDispatcher == nil {
		http.Error(w, "session dispatcher not wired", http.StatusNotImplemented)
		return
	}
	projectID := chi.URLParam(r, "id")
	_ = r.ParseForm()
	req := StartSessionRequest{
		ProjectID:   projectID,
		HarnessType: strings.TrimSpace(r.FormValue("harness_type")),
		Model:       strings.TrimSpace(r.FormValue("model")),
		Readonly:    r.FormValue("readonly") == "on",
		DisplayName: strings.TrimSpace(r.FormValue("name")),
	}
	if msg := validateHarnessType(req.HarnessType); msg != "" {
		backURL := "/sessions/new?project=" + url.QueryEscape(projectID) + "&error=" + url.QueryEscape(msg)
		http.Redirect(w, r, backURL, http.StatusSeeOther)
		return
	}
	result, err := h.SessionDispatcher.StartSession(r.Context(), req)
	if err != nil {
		backURL := "/sessions/new?project=" + url.QueryEscape(projectID) + "&error=" + url.QueryEscape(err.Error())
		http.Redirect(w, r, backURL, http.StatusSeeOther)
		return
	}
	jobURL := "/jobs/" + result.JobID
	redirectOrHXRedirect(w, r, jobURL)
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
		_ = json.NewDecoder(r.Body).Decode(&req) // label is optional
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
	_ = json.NewEncoder(w).Encode(resp) // best-effort; client may have disconnected
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
	_ = json.NewEncoder(w).Encode(resp) // best-effort; client may have disconnected
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
