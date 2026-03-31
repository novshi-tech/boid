package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/projectspec"
)

type ProjectHandler struct {
	DB    *db.DB
	Store *orchestrator.ProjectStore
}

func (h *ProjectHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.Create)
	r.Get("/", h.List)
	r.Post("/reload", h.Reload)
	r.Get("/{id}", h.Get)
	r.Delete("/{id}", h.Delete)
	return r
}

type CreateProjectRequest struct {
	WorkDir string `json:"work_dir"`
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

	meta, err := h.Store.Load(req.WorkDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	p := &projectspec.Project{
		ID:      meta.ID,
		WorkDir: req.WorkDir,
	}

	if err := orchestrator.CreateProject(h.DB.Conn, p); err != nil {
		h.Store.Remove(meta.ID)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	p.Meta = *meta
	writeJSON(w, http.StatusCreated, p)
}

func (h *ProjectHandler) List(w http.ResponseWriter, r *http.Request) {
	wsID := r.URL.Query().Get("workspace_id")

	projects, err := orchestrator.ListProjects(h.DB.Conn)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Attach meta from store and filter by workspace_id if requested
	var result []*projectspec.Project
	for _, p := range projects {
		if meta, ok := h.Store.Get(p.ID); ok {
			p.Meta = *meta
		}
		if wsID != "" && p.Meta.WorkspaceID != wsID {
			continue
		}
		result = append(result, p)
	}
	if result == nil {
		result = []*projectspec.Project{}
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *ProjectHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, err := orchestrator.GetProject(h.DB.Conn, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if meta, ok := h.Store.Get(p.ID); ok {
		p.Meta = *meta
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *ProjectHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := orchestrator.DeleteProject(h.DB.Conn, id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	h.Store.Remove(id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *ProjectHandler) Reload(w http.ResponseWriter, r *http.Request) {
	projects, err := orchestrator.ListProjects(h.DB.Conn)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	errs := h.Store.LoadAll(projects)
	if len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "partial",
			"errors": msgs,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
