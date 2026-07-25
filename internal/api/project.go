package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type ProjectHandler struct {
	Service           ProjectService
	SessionDispatcher SessionDispatcher // optional; nil disables the start-session endpoint
	ExecDispatcher    ExecDispatcher    // optional; nil disables the exec endpoint
}

type projectCandidate struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	WorkDir string `json:"work_dir"`
}

// resolveRef resolves a project ref to exactly one project.
// On multiple matches it writes HTTP 409 and returns nil.
// On no match it writes HTTP 404 and returns nil.
func (h *ProjectHandler) resolveRef(w http.ResponseWriter, ref string) *orchestrator.Project {
	projects, err := h.Service.ResolveProjectRef(ref)
	if err != nil {
		writeServiceError(w, err)
		return nil
	}
	if len(projects) == 1 {
		return projects[0]
	}
	candidates := make([]projectCandidate, 0, len(projects))
	for _, p := range projects {
		candidates = append(candidates, projectCandidate{ID: p.ID, Name: p.Meta.Name, WorkDir: p.WorkDir})
	}
	writeJSON(w, http.StatusConflict, map[string]interface{}{
		"error":      "multiple projects match",
		"candidates": candidates,
	})
	return nil
}

func (h *ProjectHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.Create)
	r.Post("/git", h.CreateFromGitURL)
	r.Get("/", h.List)
	r.Post("/reload", h.Reload)
	r.Put("/{id}/workspace", h.SetWorkspace)
	r.Post("/{id}/sessions", h.StartSession)
	r.Post("/{id}/exec", h.StartExec)
	r.Post("/{id}/fetch", h.Fetch)
	r.Get("/{id}", h.Get)
	r.Delete("/{id}", h.Delete)
	return r
}

type CreateProjectRequest struct {
	WorkDir string `json:"work_dir"`
}

// CreateProjectFromGitURLRequest is POST /api/projects/git's body
// (docs/plans/volume-only-daemon.md §論点a — `boid project add <git-url>
// --workspace=<name> [--name=<project-name>]`). Workspace is required; Name
// empty derives the project name from URL's last path component.
type CreateProjectFromGitURLRequest struct {
	URL       string `json:"url"`
	Workspace string `json:"workspace"`
	Name      string `json:"name,omitempty"`
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

// CreateFromGitURL handles POST /api/projects/git (docs/plans/
// volume-only-daemon.md §論点a — `boid project add <git-url>
// --workspace=<name>`, the new git-URL registration model).
func (h *ProjectHandler) CreateFromGitURL(w http.ResponseWriter, r *http.Request) {
	var req CreateProjectFromGitURLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	if req.Workspace == "" {
		writeError(w, http.StatusBadRequest, "workspace is required")
		return
	}

	project, err := h.Service.CreateProjectFromGitURL(r.Context(), req.URL, req.Workspace, req.Name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, project)
}

// Fetch handles POST /api/projects/{id}/fetch (docs/plans/
// volume-only-daemon.md §論点b fetch 経路 — `boid project fetch <id>`).
func (h *ProjectHandler) Fetch(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "id")
	project := h.resolveRef(w, ref)
	if project == nil {
		return
	}
	updated, err := h.Service.FetchProject(r.Context(), project.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
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
	ref := chi.URLParam(r, "id")
	project := h.resolveRef(w, ref)
	if project == nil {
		return
	}
	// resolveRef → ResolveProjectRef uses bare hydration (workspace kits not
	// merged) so name-matching stays cheap. The canonical GET endpoint is
	// expected to return a sandbox-ready snapshot — re-fetch with workspace
	// hydration so callers (cmd/exec.go, project show, etc.) see kit-merged
	// Meta.AdditionalBindings / Env / HostCommands.
	hydrated, err := h.Service.GetProject(project.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hydrated)
}

func (h *ProjectHandler) SetWorkspace(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "id")
	project := h.resolveRef(w, ref)
	if project == nil {
		return
	}

	var req SetProjectWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	updated, err := h.Service.SetProjectWorkspace(project.ID, req.WorkspaceID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *ProjectHandler) Delete(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "id")
	project := h.resolveRef(w, ref)
	if project == nil {
		return
	}
	if err := h.Service.DeleteProject(project.ID); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// StartSession handles POST /api/projects/{id}/sessions. The project is
// resolved from the URL ref (so refs like a name or short id work the same
// way other project routes do); the body specifies harness_type and the
// optional knobs (instruction / readonly / model / session_id / display_name).
func (h *ProjectHandler) StartSession(w http.ResponseWriter, r *http.Request) {
	if h.SessionDispatcher == nil {
		writeError(w, http.StatusNotImplemented, "session dispatcher not wired")
		return
	}
	ref := chi.URLParam(r, "id")
	project := h.resolveRef(w, ref)
	if project == nil {
		return
	}
	var req StartSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.ProjectID = project.ID
	if msg := validateHarnessType(req.HarnessType); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	result, err := h.SessionDispatcher.StartSession(r.Context(), req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// StartExec handles POST /api/projects/{id}/exec. The project is resolved
// from the URL ref the same way every other project route does; the body
// specifies argv and the optional readonly / interactive / display_name
// knobs. Unlike StartSession there is no top-level /api/exec — every exec is
// project-scoped by construction (`boid exec -p <ref> -- argv...`).
func (h *ProjectHandler) StartExec(w http.ResponseWriter, r *http.Request) {
	if h.ExecDispatcher == nil {
		writeError(w, http.StatusNotImplemented, "exec dispatcher not wired")
		return
	}
	ref := chi.URLParam(r, "id")
	project := h.resolveRef(w, ref)
	if project == nil {
		return
	}
	var req StartExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Argv) == 0 {
		writeError(w, http.StatusBadRequest, "argv is required")
		return
	}
	req.ProjectID = project.ID
	result, err := h.ExecDispatcher.StartExec(r.Context(), req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (h *ProjectHandler) Reload(w http.ResponseWriter, r *http.Request) {
	result, err := h.Service.ReloadProjects()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
