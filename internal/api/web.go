package api

import (
	"net/http"
	"net/url"

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
	r.Post("/tasks/{id}/action", h.PostAction)
	r.Get("/projects", h.ProjectList)
	r.Get("/jobs", h.JobList)
	r.Get("/jobs/{id}", h.JobDetail)
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
	errorMsg := r.URL.Query().Get("error")
	templates.TaskDetail(detail.Task, detail.Actions, jobs, detail.AvailableActions, errorMsg).Render(r.Context(), w)
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

func (h *WebHandler) ProjectList(w http.ResponseWriter, r *http.Request) {
	projects, err := h.Service.ListProjects()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.ProjectList(projects).Render(r.Context(), w)
}

func (h *WebHandler) JobList(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("status")
	jobs, err := h.Service.ListJobs(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views := make([]*templates.JobContextView, 0, len(jobs))
	for _, j := range jobs {
		views = append(views, &templates.JobContextView{
			ID:        j.ID,
			TaskID:    j.TaskID,
			TaskTitle: j.TaskTitle,
			HandlerID: j.HandlerID,
			Role:      j.Role,
			Status:    string(j.Status),
			ExitCode:  j.ExitCode,
			CreatedAt: j.CreatedAt,
			UpdatedAt: j.UpdatedAt,
			Output:    j.Output,
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.JobList(views, filter).Render(r.Context(), w)
}

func (h *WebHandler) JobDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	job, err := h.Service.GetJob(id)
	if err != nil {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}
	view := &templates.JobContextView{
		ID:        job.ID,
		TaskID:    job.TaskID,
		TaskTitle: job.TaskTitle,
		HandlerID: job.HandlerID,
		Role:      job.Role,
		Status:    string(job.Status),
		ExitCode:  job.ExitCode,
		CreatedAt: job.CreatedAt,
		UpdatedAt: job.UpdatedAt,
		Output:    job.Output,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.JobDetail(view).Render(r.Context(), w)
}
