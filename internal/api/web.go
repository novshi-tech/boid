package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/web/templates"
)

type WebHandler struct {
	Tasks    TaskStore
	Actions  ActionStore
	Jobs     JobStore
	Projects ProjectRepository
	Store    *orchestrator.ProjectStore
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
	tasks, err := h.Tasks.ListTasks(orchestrator.TaskFilter{Status: filter})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.TaskList(tasks, filter).Render(r.Context(), w)
}

func (h *WebHandler) TaskDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	task, err := h.Tasks.GetTask(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	actions, _ := h.Actions.ListActionsByTask(task.ID)
	jobs, _ := h.Jobs.ListJobsByTask(task.ID)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.TaskDetail(task, actions, jobs).Render(r.Context(), w)
}

func (h *WebHandler) ProjectList(w http.ResponseWriter, r *http.Request) {
	projects, err := h.Projects.ListProjects()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, p := range projects {
		if meta, ok := h.Store.Get(p.ID); ok {
			p.Meta = *meta
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.ProjectList(projects).Render(r.Context(), w)
}
