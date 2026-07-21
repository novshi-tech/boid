package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

type BrokerRegisterRequest struct {
	Commands        map[string]orchestrator.HostCommandSpec `json:"commands"`
	BuiltinPolicies map[string]sandbox.BuiltinPolicy        `json:"builtin_policies,omitempty"`
	ProjectID       string                                  `json:"project_id,omitempty"`
}

type BrokerRegisterResponse struct {
	Token  string `json:"token"`
	Socket string `json:"socket"`
	// ResolvedHostCommands used to echo back
	// dispatcher.ResolveHostCommands' absolute-path-keyed map so `boid exec`
	// could feed it into SandboxRuntimeInfo (matching shim bind-mount
	// targets to broker policy keys without re-resolving on the client
	// side). That map has been dead weight since the Phase 5 5a-3 cutover
	// retired the shim's absolute-host-path bind mount scheme
	// (SandboxRuntimeInfo.ResolvedHostCommands was deleted, hostCommandMounts
	// and buildHostCommandNamesEnv with it — see
	// docs/plans/phase5-shim-and-task-context.md 5a PR3). No client reads
	// this field any more; it is retained as omitempty for one release so
	// any stale caller decoding the response doesn't blow up on a missing
	// key, and will be removed outright once no versions of the CLI still
	// speak the older shape.
	ResolvedHostCommands map[string]orchestrator.CommandDef `json:"resolved_host_commands,omitempty"`
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
	if len(req.Commands) == 0 && len(req.BuiltinPolicies) == 0 {
		writeError(w, http.StatusBadRequest, "no commands or builtins")
		return
	}
	if req.ProjectID == "" {
		writeError(w, http.StatusBadRequest, "project_id is required")
		return
	}

	resp, err := h.Registry.RegisterBrokerCommands(req.Commands, req.BuiltinPolicies, req.ProjectID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
