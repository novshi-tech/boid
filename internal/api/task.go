package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TaskNotifyService dispatches an agent-driven notification for a task.
// Wired to *TaskAppService at runtime; left optional on TaskHandler so
// existing tests do not need to satisfy this interface.
type TaskNotifyService interface {
	NotifyTask(ctx context.Context, taskID, message string) error
}

type TaskHandler struct {
	Service  TaskService
	Gates    GateService       // optional: enables gate replay/list when set
	Notifier TaskNotifyService // optional: enables POST /{id}/notify when set
}

func (h *TaskHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.Create)
	r.Post("/import", h.Import)
	r.Get("/", h.List)
	r.Get("/{id}/detail", h.Detail)
	r.Get("/{id}", h.Get)
	r.Patch("/{id}", h.Patch)
	r.Delete("/{id}", h.Delete)
	r.Post("/{id}/duplicate", h.Duplicate)
	r.Post("/{id}/rerun", h.Rerun)
	if h.Gates != nil {
		r.Get("/{id}/gates", h.ListGates)
		r.Post("/{id}/gates/{gate_id}/replay", h.ReplayGate)
	}
	if h.Notifier != nil {
		r.Post("/{id}/notify", h.Notify)
	}
	return r
}

type NotifyTaskRequest struct {
	Message string `json:"message"`
}

func (h *TaskHandler) Notify(w http.ResponseWriter, r *http.Request) {
	var req NotifyTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	taskID := chi.URLParam(r, "id")
	if err := h.Notifier.NotifyTask(r.Context(), taskID, req.Message); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type UpdateTaskRequest struct {
	Title            string          `json:"title"`
	Description      string          `json:"description"`
	Payload          json.RawMessage `json:"payload,omitempty"`
	Instructions     json.RawMessage `json:"instructions,omitempty"`
	BaseBranch       *string         `json:"base_branch,omitempty"`
	BranchPrefix     *string         `json:"branch_prefix,omitempty"`
	Worktree         *bool           `json:"worktree,omitempty"`
	DependsOn        []string        `json:"depends_on,omitempty"`
	DependsOnPayload *string         `json:"depends_on_payload,omitempty"`
	ParentID         *string         `json:"parent_id,omitempty"`
}

type CreateTaskRequest struct {
	ID           string                     `json:"id,omitempty"`
	ProjectID    string                     `json:"project_id"`
	Title        string                     `json:"title"`
	Description  string                     `json:"description,omitempty"`
	Behavior     string                     `json:"behavior,omitempty"`
	BehaviorSpec *orchestrator.BehaviorSpec `json:"behavior_spec,omitempty"`
	RemoteID     string                     `json:"remote_id,omitempty"`
	DataSourceID string                     `json:"datasource_id,omitempty"`
	Payload      json.RawMessage            `json:"payload,omitempty"`
	Instructions json.RawMessage            `json:"instructions,omitempty"`
	AutoStart    bool                       `json:"auto_start,omitempty"`
	Traits       []string                   `json:"traits,omitempty"`
	Readonly     *bool                      `json:"readonly,omitempty"`
	Worktree     *bool                      `json:"worktree,omitempty"`
	BranchPrefix *string                    `json:"branch_prefix,omitempty"`
	BaseBranch   *string                    `json:"base_branch,omitempty"`
	DependsOn        []string               `json:"depends_on,omitempty"`
	DependsOnPayload string                 `json:"depends_on_payload,omitempty"`
	Ref              string                 `json:"ref,omitempty"`
	ParentID     string                     `json:"parent_id,omitempty"`
}

func (h *TaskHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreateTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID == "" || req.Title == "" {
		writeError(w, http.StatusBadRequest, "project_id and title are required")
		return
	}

	task, err := h.Service.CreateTask(req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

func (h *TaskHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := orchestrator.TaskFilter{
		Status:       q.Get("status"),
		ProjectID:    q.Get("project_id"),
		Behavior:     q.Get("behavior"),
		WorkspaceID:  q.Get("workspace_id"),
		HasDependsOn: q.Get("has_depends_on") == "true",
		NoDependsOn:  q.Get("no_depends_on") == "true",
	}

	tasks, err := h.Service.ListTasks(filter)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (h *TaskHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	task, err := h.Service.GetTask(id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (h *TaskHandler) Detail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (h *TaskHandler) Patch(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req UpdateTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" && req.Description == "" && len(req.Payload) == 0 && len(req.Instructions) == 0 && req.BaseBranch == nil && req.BranchPrefix == nil && req.Worktree == nil && req.DependsOn == nil && req.DependsOnPayload == nil && req.ParentID == nil {
		writeError(w, http.StatusBadRequest, "at least one of title, description, payload, instructions, base_branch, branch_prefix, worktree, depends_on, depends_on_payload, or parent_id is required")
		return
	}
	task, err := h.Service.UpdateTask(id, req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (h *TaskHandler) Import(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	var reqs []CreateTaskRequest

	if strings.Contains(ct, "application/x-ndjson") {
		scanner := bufio.NewScanner(r.Body)
		lineNum := 0
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			lineNum++
			var req CreateTaskRequest
			if err := json.Unmarshal([]byte(line), &req); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("line %d: invalid JSON: %s", lineNum, err))
				return
			}
			reqs = append(reqs, req)
		}
		if err := scanner.Err(); err != nil {
			writeError(w, http.StatusBadRequest, "reading request body: "+err.Error())
			return
		}
	} else {
		if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	result, err := h.Service.ImportTasks(reqs)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *TaskHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	force := r.URL.Query().Get("force") == "true"
	if err := h.Service.DeleteTask(id, force); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type DuplicateTaskRequest struct {
	AutoStart bool `json:"auto_start"`
}

func (h *TaskHandler) Duplicate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req DuplicateTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	task, err := h.Service.DuplicateTask(id, req.AutoStart)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

type RerunTaskRequest struct {
	AutoStart            bool            `json:"auto_start,omitempty"`
	InstructionsOverride json.RawMessage `json:"instructions_override,omitempty"`
}

func (h *TaskHandler) Rerun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req RerunTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	task, err := h.Service.RerunTask(id, req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// replayGateBody is the optional request body for gate replay.
type replayGateBody struct {
	Status string `json:"status,omitempty"`
}

func (h *TaskHandler) ReplayGate(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "id")
	// gate IDs contain '/' (kit-name/gate-name); the CLI encodes them as %2F
	// so chi treats them as a single path segment. chi.URLParam returns the
	// raw value so we have to undo the encoding here.
	gateID, err := url.PathUnescape(chi.URLParam(r, "gate_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid gate id")
		return
	}

	var body replayGateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	result, err := h.Gates.ReplayGate(r.Context(), taskID, ReplayGateRequest{
		GateID: gateID,
		Status: body.Status,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *TaskHandler) ListGates(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "id")
	status := r.URL.Query().Get("status")

	gates, err := h.Gates.ListGatesForStatus(taskID, status)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gates)
}
