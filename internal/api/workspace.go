package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

type WorkspaceHandler struct {
	Service ProjectService
}

func (h *WorkspaceHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	return r
}

func (h *WorkspaceHandler) List(w http.ResponseWriter, r *http.Request) {
	workspaces, err := h.Service.ListWorkspaces()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workspaces)
}
