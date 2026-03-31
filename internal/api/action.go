package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type ActionHandler struct {
	Service WorkflowService
}

func (h *ActionHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.Apply)
	return r
}

type ApplyActionRequest struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func (h *ActionHandler) Apply(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req ApplyActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Type == "" {
		writeError(w, http.StatusBadRequest, "type is required")
		return
	}

	result, err := h.Service.ApplyAction(r.Context(), taskID, req)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, result)
}
