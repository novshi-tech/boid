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
			ID:        j.ID,
			Role:      j.Role,
			HandlerID: j.HandlerID,
			Status:    string(j.Status),
			ExitCode:  j.ExitCode,
			CreatedAt: j.CreatedAt,
			UpdatedAt: j.UpdatedAt,
		})
	}
	return timeline.Build(detail.Task, detail.Actions, infos)
}


type WebHandler struct {
	Service    WebService
	Hub        *TaskEventHub
	Dispatcher CommandDispatcher
	Registry   *auth.ConnectionRegistry
}

func (h *WebHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.TaskList)
	r.Get("/tasks/new", h.TaskNew)
	r.Post("/tasks", h.PostTaskCreate)
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Get("/tasks/{id}/fragment", h.TaskDetailFragment)
	r.Post("/tasks/{id}/edit/description", h.PostEditDescription)
	r.Post("/tasks/{id}/action", h.PostAction)
	r.Post("/tasks/{id}/duplicate", h.PostDuplicate)
	r.Post("/tasks/{id}/rerun", h.PostRerun)
	r.Post("/tasks/{id}/delete", h.PostDelete)
	r.Get("/tasks/{id}/gates", h.GateReplayList)
	r.Post("/tasks/{id}/gates/{gate_id}/replay", h.PostGateReplay)
	r.Get("/jobs/{id}", h.JobDetail)
	r.Get("/jobs/{id}/terminal", h.JobTerminal)
	r.Get("/projects/{id}/commands", h.ProjectCommandList)
	r.Post("/projects/{id}/commands/{name}/execute", h.PostProjectExecuteCommand)
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
		ProjectID:        r.FormValue("project_id"),
		Title:            title,
		Behavior:         r.FormValue("behavior"),
		Description:      r.FormValue("description"),
		ParentID:         r.FormValue("parent_id"),
		DependsOnPayload: r.FormValue("depends_on_payload"),
		AutoStart:        r.FormValue("auto_start") == "on",
	}

	if raw := strings.TrimSpace(r.FormValue("depends_on")); raw != "" {
		req.DependsOn = strings.Fields(raw)
	}

	if raw := strings.TrimSpace(r.FormValue("traits")); raw != "" {
		req.Traits = strings.Fields(raw)
	}

	if v := r.FormValue("base_branch"); v != "" {
		req.BaseBranch = &v
	}
	if v := r.FormValue("branch_prefix"); v != "" {
		req.BranchPrefix = &v
	}

	if r.FormValue("worktree") == "on" {
		t := true
		req.Worktree = &t
	}
	if r.FormValue("readonly") == "on" {
		t := true
		req.Readonly = &t
	}

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
	// ?mode=edit switches an editable tab (currently: description) into
	// inline edit mode: the tab body becomes a form and the action bar
	// primary flips to save/cancel. Ignored on read-only tabs.
	editMode := r.URL.Query().Get("mode") == "edit" && tab == "description"
	errorMsg := r.URL.Query().Get("error")
	timelineGroups := detailTimelineGroups(detail)
	if r.Header.Get("HX-Request") == "true" {
		// Tab clicks swap the entire #tabs section so the active class on
		// the visible tabs and the "more" summary label stay in sync.
		depsUp, depsDown := buildDepsTreeRows(detail.DependsOnTree, detail.DependentsTree)
		templates.TaskDetailTabsSection(detail.Task, timelineGroups, jobs, depsUp, depsDown, detail.AvailableActions, tab, editMode).Render(r.Context(), w)
		return
	}
	projectName := h.lookupProjectName(detail.Task.ProjectID)
	depsUp, depsDown := buildDepsTreeRows(detail.DependsOnTree, detail.DependentsTree)
	templates.TaskDetail(detail.Task, timelineGroups, jobs, depsUp, depsDown, detail.AvailableActions, errorMsg, tab, editMode, projectName).Render(r.Context(), w)
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
		templates.TaskDetailTimelineSection(detailTimelineGroups(detail)).Render(r.Context(), w)
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

func (h *WebHandler) PostEditDescription(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	description := r.FormValue("description")
	req := UpdateTaskRequest{Description: description}
	target := "/tasks/" + id + "?tab=description"
	if err := h.Service.UpdateTask(id, req); err != nil {
		target = "/tasks/" + id + "?tab=description&mode=edit&error=" + url.QueryEscape(err.Error())
	}
	// HTMX requests receive HX-Redirect so the client performs a full
	// navigation (re-renders the tab out of edit mode). Non-HTMX falls
	// back to a regular 303 redirect for older clients / direct POSTs.
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

func (h *WebHandler) GateReplayList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	status := r.URL.Query().Get("status")
	gates, err := h.Service.ListGatesForStatus(id, status)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	errorMsg := r.URL.Query().Get("error")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.GateReplayList(id, status, gates, errorMsg).Render(r.Context(), w)
}

func (h *WebHandler) PostGateReplay(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	gateID, err := url.PathUnescape(chi.URLParam(r, "gate_id"))
	if err != nil {
		http.Error(w, "invalid gate id", http.StatusBadRequest)
		return
	}
	_, err = h.Service.ReplayGate(r.Context(), id, ReplayGateRequest{GateID: gateID})
	if err != nil {
		http.Redirect(w, r, "/tasks/"+id+"/gates?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/tasks/"+id, http.StatusSeeOther)
}

func isGateRole(role string) bool {
	return role == "gate" || role == "exit_gate" || role == "entry_gate"
}

func (h *WebHandler) JobDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	job, err := h.Service.GetJob(id)
	if err != nil {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}
	gateID := ""
	if isGateRole(job.Role) {
		gateID = job.HandlerID
	}
	view := &templates.JobContextView{
		ID:          job.ID,
		TaskID:      job.TaskID,
		TaskTitle:   job.TaskTitle,
		HandlerID:   job.HandlerID,
		Role:        job.Role,
		GateID:      gateID,
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
	if !job.Interactive {
		templates.TerminalNotReady(id, "このジョブはインタラクティブではありません。").Render(r.Context(), w)
		return
	}
	if job.Status != JobStatusRunning {
		templates.TerminalNotReady(id, "現在 attach できる状態ではありません（ジョブが実行中ではありません）。").Render(r.Context(), w)
		return
	}
	gateID := ""
	if isGateRole(job.Role) {
		gateID = job.HandlerID
	}
	view := &templates.JobContextView{
		ID:          job.ID,
		TaskID:      job.TaskID,
		TaskTitle:   job.TaskTitle,
		HandlerID:   job.HandlerID,
		Role:        job.Role,
		GateID:      gateID,
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
