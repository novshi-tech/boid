package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TaskIdentityRequest selects a repository scope or a repository-free workspace scope.
type TaskIdentityRequest struct {
	ProjectID   string  `json:"project_id,omitempty"`
	WorkspaceID string  `json:"workspace_id,omitempty"`
	Identity    string  `json:"identity"`
	TaskID      string  `json:"task_id,omitempty"`
	URL         *string `json:"url,omitempty"`
	DisplayName *string `json:"display_name,omitempty"`
}

func (h *TaskHandler) Identity(w http.ResponseWriter, r *http.Request) {
	var req TaskIdentityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	scope := orchestrator.IdentityScope{ProjectID: req.ProjectID, WorkspaceID: req.WorkspaceID}
	if req.ProjectID == "" {
		if req.WorkspaceID == "" {
			req.WorkspaceID = orchestrator.DefaultWorkspaceSlug
		}
		if err := orchestrator.ValidWorkspaceSlug(req.WorkspaceID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		scope.WorkspaceID = req.WorkspaceID
	}
	var err error
	switch chi.URLParam(r, "operation") {
	case "link":
		err = h.Identities.LinkIdentityInScope(r.Context(), scope, req.Identity, req.TaskID, req.URL, req.DisplayName)
	case "unlink":
		err = h.Identities.UnlinkIdentityInScope(r.Context(), scope, req.Identity)
	case "resolve":
		var task *orchestrator.Task
		task, err = h.Identities.ResolveIdentityInScope(scope, req.Identity)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]string{"task_id": task.ID, "status": string(task.Status)})
			return
		}
	default:
		writeError(w, http.StatusNotFound, "unknown identity operation")
		return
	}
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *TaskHandler) ResolveOrCapture(w http.ResponseWriter, r *http.Request) {
	var req ResolveOrCaptureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID == "" && req.WorkspaceID == "" {
		req.WorkspaceID = orchestrator.DefaultWorkspaceSlug
	}
	result, err := h.Capture.ResolveOrCapture(r.Context(), req)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeIdentityError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, orchestrator.ErrTaskNotFound) {
		code = http.StatusNotFound
	}
	if errors.Is(err, orchestrator.ErrIdentityConflict) {
		code = http.StatusConflict
	}
	writeError(w, code, err.Error())
}
