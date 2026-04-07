package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type TaskHandler struct {
	Service TaskService
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
	return r
}

type UpdateTaskRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

type CreateTaskRequest struct {
	ProjectID    string          `json:"project_id"`
	Title        string          `json:"title"`
	Description  string          `json:"description,omitempty"`
	Behavior     string          `json:"behavior"`
	RemoteID     string          `json:"remote_id,omitempty"`
	DataSourceID string          `json:"datasource_id,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
}

func (h *TaskHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreateTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID == "" || req.Title == "" || req.Behavior == "" {
		writeError(w, http.StatusBadRequest, "project_id, title, and behavior are required")
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
	filter := orchestrator.TaskFilter{
		Status:    r.URL.Query().Get("status"),
		ProjectID: r.URL.Query().Get("project_id"),
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
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
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
