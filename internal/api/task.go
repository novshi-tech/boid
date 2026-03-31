package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type TaskHandler struct {
	DB *db.DB
}

func (h *TaskHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.Create)
	r.Get("/", h.List)
	r.Get("/{id}", h.Get)
	return r
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

	t := &orchestrator.Task{
		ProjectID:    req.ProjectID,
		Title:        req.Title,
		Description:  req.Description,
		Behavior:     req.Behavior,
		RemoteID:     req.RemoteID,
		DataSourceID: req.DataSourceID,
		Payload:      req.Payload,
	}

	if err := orchestrator.CreateTask(h.DB.Conn, t); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, t)
}

func (h *TaskHandler) List(w http.ResponseWriter, r *http.Request) {
	filter := orchestrator.TaskFilter{
		Status:    r.URL.Query().Get("status"),
		ProjectID: r.URL.Query().Get("project_id"),
	}

	tasks, err := orchestrator.ListTasks(h.DB.Conn, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tasks == nil {
		tasks = []*orchestrator.Task{}
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (h *TaskHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	t, err := orchestrator.GetTask(h.DB.Conn, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}
