package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/api/auth"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/web/templates"
	"github.com/novshi-tech/boid/web/templates/components"
)


type WebHandler struct {
	Service WebService
	Hub     *TaskEventHub
}

func (h *WebHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.TaskList)
	r.Get("/tasks/new", h.TaskNew)
	r.Post("/tasks", h.PostTaskCreate)
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Get("/tasks/{id}/fragment", h.TaskDetailFragment)
	r.Get("/tasks/{id}/edit/description", h.EditDescription)
	r.Post("/tasks/{id}/edit/description", h.PostEditDescription)
	r.Get("/tasks/{id}/edit/payload", h.EditPayloadList)
	r.Get("/tasks/{id}/edit/payload/{section}", h.EditPayloadSection)
	r.Post("/tasks/{id}/edit/payload/{section}", h.PostEditPayloadSection)
	r.Get("/tasks/{id}/edit/instructions", h.EditInstructionsList)
	r.Get("/tasks/{id}/edit/instructions/{role}", h.EditInstructionsRole)
	r.Post("/tasks/{id}/edit/instructions/{role}", h.PostEditInstructionsRole)
	r.Get("/tasks/{id}/edit/deps", h.EditDeps)
	r.Post("/tasks/{id}/edit/deps", h.PostEditDeps)
	r.Post("/tasks/{id}/action", h.PostAction)
	r.Post("/tasks/{id}/duplicate", h.PostDuplicate)
	r.Post("/tasks/{id}/rerun", h.PostRerun)
	r.Get("/tasks/{id}/gates", h.GateReplayList)
	r.Post("/tasks/{id}/gates/{gate_id}/replay", h.PostGateReplay)
	r.Get("/jobs/{id}", h.JobDetail)
	return r
}

func (h *WebHandler) TaskNew(w http.ResponseWriter, r *http.Request) {
	projects, _ := h.Service.ListProjects()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.TaskNew(projects, "").Render(r.Context(), w)
}

func (h *WebHandler) PostTaskCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		projects, _ := h.Service.ListProjects()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		templates.TaskNew(projects, "リクエストの解析に失敗しました").Render(r.Context(), w)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		projects, _ := h.Service.ListProjects()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		templates.TaskNew(projects, "タイトルは必須です").Render(r.Context(), w)
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
		templates.TaskNew(projects, err.Error()).Render(r.Context(), w)
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

	var items []components.TreeItem
	if filter.Status == "closed" {
		items = BuildFlatItems(tasks)
	} else {
		items = BuildTreeItems(tasks)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if r.Header.Get("HX-Target") == "main-content" {
		behaviors, _ := h.Service.ListBehaviors()
		workspaces, _ := h.Service.ListWorkspaces()
		templates.TaskListContent(items, filter, projects, behaviors, workspaces, r.URL.RequestURI()).Render(r.Context(), w)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		templates.TaskListFragment(items, r.URL.RequestURI()).Render(r.Context(), w)
		return
	}

	behaviors, _ := h.Service.ListBehaviors()
	workspaces, _ := h.Service.ListWorkspaces()
	templates.TaskList(items, filter, projects, behaviors, workspaces, r.URL.RequestURI()).Render(r.Context(), w)
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
	if r.Header.Get("HX-Request") == "true" {
		templates.TaskDetailTabPanel(detail.Task, detail.Actions, jobs, tab).Render(r.Context(), w)
		return
	}
	templates.TaskDetail(detail.Task, detail.Actions, jobs, detail.AvailableActions, errorMsg, tab).Render(r.Context(), w)
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
		templates.TaskDetailTimelineSection(detail.Actions).Render(r.Context(), w)
	case "status":
		templates.TaskDetailStatusSection(detail.Task, detail.AvailableActions, "").Render(r.Context(), w)
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

func (h *WebHandler) EditDescription(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.EditDescription(detail.Task, "").Render(r.Context(), w)
}

func (h *WebHandler) PostEditDescription(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	description := r.FormValue("description")
	req := UpdateTaskRequest{Description: description}
	if err := h.Service.UpdateTask(id, req); err != nil {
		detail, detailErr := h.Service.GetTaskDetail(id)
		if detailErr != nil {
			http.Error(w, "Task not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		templates.EditDescription(detail.Task, err.Error()).Render(r.Context(), w)
		return
	}
	http.Redirect(w, r, "/tasks/"+id, http.StatusSeeOther)
}

func (h *WebHandler) EditPayloadList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.PayloadSectionList(detail.Task).Render(r.Context(), w)
}

func (h *WebHandler) EditPayloadSection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	section := chi.URLParam(r, "section")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	var yamlText string
	if len(detail.Task.Payload) > 0 && string(detail.Task.Payload) != "null" {
		raw := make(map[string]json.RawMessage)
		if err := json.Unmarshal(detail.Task.Payload, &raw); err == nil {
			if sectionData, ok := raw[section]; ok {
				if y, err := jsonToYAML(sectionData); err == nil {
					yamlText = strings.TrimRight(y, "\n")
				}
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.PayloadSectionEdit(detail.Task, section, yamlText, "").Render(r.Context(), w)
}

func (h *WebHandler) PostEditPayloadSection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	section := chi.URLParam(r, "section")

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	yamlText := r.FormValue("yaml_text")
	sectionJSON, err := yamlToJSON(yamlText)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		templates.PayloadSectionEdit(detail.Task, section, yamlText, "YAML parse error: "+err.Error()).Render(r.Context(), w)
		return
	}

	mergedPayload, err := mergeSectionIntoPayload(detail.Task.Payload, section, sectionJSON)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		templates.PayloadSectionEdit(detail.Task, section, yamlText, "payload error: "+err.Error()).Render(r.Context(), w)
		return
	}

	req := UpdateTaskRequest{Payload: mergedPayload}
	if err := h.Service.UpdateTask(id, req); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		templates.PayloadSectionEdit(detail.Task, section, yamlText, "update failed: "+err.Error()).Render(r.Context(), w)
		return
	}

	http.Redirect(w, r, "/tasks/"+id+"/edit/payload", http.StatusSeeOther)
}

func (h *WebHandler) EditInstructionsList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}
	defaults := webGetDefaultInstructions(h.Service, detail.Task)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.InstructionRoleList(detail.Task, defaults).Render(r.Context(), w)
}

func (h *WebHandler) EditInstructionsRole(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	role := chi.URLParam(r, "role")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}
	inst := orchestrator.Instruction{}
	if detail.Task.Instructions != nil {
		if existing, ok := detail.Task.Instructions[role]; ok {
			inst = existing
		}
	}
	if inst.Type == "" {
		defaults := webGetDefaultInstructions(h.Service, detail.Task)
		if def, ok := defaults[role]; ok {
			inst = def
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.InstructionRoleEdit(detail.Task, role, inst, "").Render(r.Context(), w)
}

func (h *WebHandler) PostEditInstructionsRole(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	role := chi.URLParam(r, "role")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}
	consumer := strings.TrimSpace(r.FormValue("consumer"))
	inst := orchestrator.Instruction{
		Type:        orchestrator.InstructionType(r.FormValue("type")),
		Consumer:    consumer,
		Name:        r.FormValue("name"),
		Message:     r.FormValue("message"),
		Interactive: r.FormValue("interactive") == "on",
		Model:       r.FormValue("model"),
	}
	if consumer == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		templates.InstructionRoleEdit(detail.Task, role, inst, "consumer は必須です").Render(r.Context(), w)
		return
	}
	instJSON, err := json.Marshal(inst)
	if err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}
	patch := map[string]json.RawMessage{role: instJSON}
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}
	if err := h.Service.UpdateTask(id, UpdateTaskRequest{Instructions: patchJSON}); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		templates.InstructionRoleEdit(detail.Task, role, inst, "更新に失敗しました: "+err.Error()).Render(r.Context(), w)
		return
	}
	http.Redirect(w, r, "/tasks/"+id+"/edit/instructions", http.StatusSeeOther)
}

// webGetDefaultInstructions returns the project behavior's DefaultInstructions for the task.
func webGetDefaultInstructions(svc WebService, task *orchestrator.Task) map[string]orchestrator.Instruction {
	if task.ProjectID == "" {
		return nil
	}
	projects, err := svc.ListProjects()
	if err != nil {
		return nil
	}
	for _, p := range projects {
		if p.ID == task.ProjectID {
			if tb, ok := p.Meta.TaskBehaviors[task.Behavior]; ok {
				return tb.DefaultInstructions
			}
			break
		}
	}
	return nil
}

func (h *WebHandler) EditDeps(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.EditDeps(detail.Task, detail.DependsOnResolved, "").Render(r.Context(), w)
}

func (h *WebHandler) PostEditDeps(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	var dependsOn []string
	if raw := strings.TrimSpace(r.FormValue("depends_on")); raw != "" {
		dependsOn = strings.Fields(raw)
	} else {
		dependsOn = []string{}
	}
	dependsOnPayload := r.FormValue("depends_on_payload")
	parentID := r.FormValue("parent_id")

	req := UpdateTaskRequest{
		DependsOn:        dependsOn,
		DependsOnPayload: &dependsOnPayload,
		ParentID:         &parentID,
	}

	if err := h.Service.UpdateTask(id, req); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		templates.EditDeps(detail.Task, detail.DependsOnResolved, err.Error()).Render(r.Context(), w)
		return
	}
	http.Redirect(w, r, "/tasks/"+id, http.StatusSeeOther)
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
	gateID := chi.URLParam(r, "gate_id")
	_, err := h.Service.ReplayGate(r.Context(), id, ReplayGateRequest{GateID: gateID})
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
		ID:        job.ID,
		TaskID:    job.TaskID,
		TaskTitle: job.TaskTitle,
		HandlerID: job.HandlerID,
		Role:      job.Role,
		GateID:    gateID,
		Status:    string(job.Status),
		ExitCode:  job.ExitCode,
		CreatedAt: job.CreatedAt,
		UpdatedAt: job.UpdatedAt,
		Output:    job.Output,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.JobDetail(view).Render(r.Context(), w)
}

// WebManagementHandler serves the CLI management API at /api/web/*.
// All routes are accessible only via UNIX socket (CLI control plane).
type WebManagementHandler struct {
	Pairing   *auth.PairingManager
	Store     *auth.Store
	PublicURL string
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
	label := r.URL.Query().Get("label")
	code, err := h.Pairing.Issue(r.Context(), label)
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
	ID         string  `json:"id"`
	Label      string  `json:"label,omitempty"`
	CreatedAt  string  `json:"created_at"`
	LastSeenAt string  `json:"last_seen_at"`
	RevokedAt  *string `json:"revoked_at,omitempty"`
}

func (h *WebManagementHandler) GetDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := h.Store.ListDevices(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := make([]deviceResponse, 0, len(devices))
	for _, d := range devices {
		dr := deviceResponse{
			ID:         d.ID,
			Label:      d.Label,
			CreatedAt:  d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			LastSeenAt: d.LastSeenAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if d.RevokedAt != nil {
			s := d.RevokedAt.UTC().Format("2006-01-02T15:04:05Z")
			dr.RevokedAt = &s
		}
		resp = append(resp, dr)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *WebManagementHandler) DeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Store.RevokeDevice(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *WebManagementHandler) DeleteAllDevices(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.RevokeAllDevices(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
	Allow(ip string) bool
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
	if !h.Limiter.Allow(remoteIP(r)) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	code := r.FormValue("code")
	label, err := h.Pairing.Redeem(r.Context(), code)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		templates.Login("無効なペアリングコードです").Render(r.Context(), w)
		return
	}
	if err := h.issueSession(w, r, label); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *LoginHandler) GetAuth(w http.ResponseWriter, r *http.Request) {
	if !h.Limiter.Allow(remoteIP(r)) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	token := r.URL.Query().Get("token")
	label, err := h.Pairing.Redeem(r.Context(), token)
	if err != nil {
		http.Redirect(w, r, "/login?error=invalid_code", http.StatusFound)
		return
	}
	if err := h.issueSession(w, r, label); err != nil {
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

// remoteIP extracts the host part from r.RemoteAddr.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
