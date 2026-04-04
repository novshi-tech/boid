package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type GCAppService struct {
	Store GCStore
}

func (s *GCAppService) Run(olderThan time.Duration, dryRun bool) (*orchestrator.GCResult, error) {
	result, err := s.Store.GC(olderThan, dryRun)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return result, nil
}

type GCHandler struct {
	Service GCService
}

func (h *GCHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.Run)
	return r
}

type gcRequest struct {
	OlderThan string `json:"older_than,omitempty"`
	DryRun    bool   `json:"dry_run,omitempty"`
}

type gcResponse struct {
	Tasks     int64 `json:"tasks"`
	Jobs      int64 `json:"jobs"`
	Actions   int64 `json:"actions"`
	Worktrees int64 `json:"worktrees"`
	DryRun    bool  `json:"dry_run,omitempty"`
}

func (h *GCHandler) Run(w http.ResponseWriter, r *http.Request) {
	var req gcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	olderThan := 30 * 24 * time.Hour
	if req.OlderThan != "" {
		d, err := time.ParseDuration(req.OlderThan)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid older_than: "+err.Error())
			return
		}
		olderThan = d
	}

	result, err := h.Service.Run(olderThan, req.DryRun)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gcResponse{
		Tasks:     result.Tasks,
		Jobs:      result.Jobs,
		Actions:   result.Actions,
		Worktrees: result.Worktrees,
		DryRun:    req.DryRun,
	})
}
