package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type ProjectHandler struct {
	Service ProjectService
}

func (h *ProjectHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.Create)
	r.Get("/", h.List)
	r.Post("/reload", h.Reload)
	r.Put("/{id}/workspace", h.SetWorkspace)
	r.Get("/{id}", h.Get)
	r.Delete("/{id}", h.Delete)
	return r
}

type CreateProjectRequest struct {
	WorkDir string `json:"work_dir"`
}

type SetProjectWorkspaceRequest struct {
	WorkspaceID string `json:"workspace_id"`
}

func (h *ProjectHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreateProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.WorkDir == "" {
		writeError(w, http.StatusBadRequest, "work_dir is required")
		return
	}

	project, err := h.Service.CreateProject(req.WorkDir)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, project)
}

func (h *ProjectHandler) List(w http.ResponseWriter, r *http.Request) {
	projects, err := h.Service.ListProjects(r.URL.Query().Get("workspace_id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

func (h *ProjectHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	project, err := h.Service.GetProject(id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (h *ProjectHandler) SetWorkspace(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req SetProjectWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	project, err := h.Service.SetProjectWorkspace(id, req.WorkspaceID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (h *ProjectHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Service.DeleteProject(id); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *ProjectHandler) Reload(w http.ResponseWriter, r *http.Request) {
	result, err := h.Service.ReloadProjects()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
