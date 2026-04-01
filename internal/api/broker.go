package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type BrokerRegisterRequest struct {
	Commands        map[string]orchestrator.CommandDef `json:"commands"`
	BuiltinCommands []string                           `json:"builtin_commands,omitempty"`
	ProjectDir      string                             `json:"project_dir,omitempty"`
	WorktreeDir     string                             `json:"worktree_dir,omitempty"`
}

type BrokerRegisterResponse struct {
	Token  string `json:"token"`
	Socket string `json:"socket"`
}

type BrokerHandler struct {
	Registry BrokerRegistry
}

func (h *BrokerHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/register", h.Register)
	return r
}

func (h *BrokerHandler) Register(w http.ResponseWriter, r *http.Request) {
	if h.Registry == nil {
		writeError(w, http.StatusServiceUnavailable, "broker not available")
		return
	}

	var req BrokerRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if len(req.Commands) == 0 && len(req.BuiltinCommands) == 0 {
		writeError(w, http.StatusBadRequest, "no commands or builtins")
		return
	}

	resp, err := h.Registry.RegisterBrokerCommands(req.Commands, req.BuiltinCommands, req.ProjectDir, req.WorktreeDir)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
