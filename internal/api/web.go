package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/web/templates"
)

type WebHandler struct {
	Service WebService
}

func (h *WebHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.TaskList)
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Get("/projects", h.ProjectList)
	return r
}

func (h *WebHandler) TaskList(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("status")
	tasks, err := h.Service.ListTasks(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.TaskList(tasks, filter).Render(r.Context(), w)
}

func (h *WebHandler) TaskDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.TaskDetail(detail.Task, detail.Actions, detail.Jobs).Render(r.Context(), w)
}

func (h *WebHandler) ProjectList(w http.ResponseWriter, r *http.Request) {
	projects, err := h.Service.ListProjects()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.ProjectList(projects).Render(r.Context(), w)
}
