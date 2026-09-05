package api

import (
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// HostCommandsService is the daemon-side dependency behind
// GET /api/host_commands and POST /api/host_commands/reload. Implemented
// directly by *server.Server.
type HostCommandsService interface {
	// HostCommands returns a snapshot of the daemon's aggregated
	// host_commands config (raw/unexpanded — see
	// orchestrator.ExpandHostCommandsForDispatch's doc comment for why).
	HostCommands() map[string]orchestrator.HostCommandSpec
	// ReloadHostCommands re-reads ~/.config/boid/host_commands.yaml from
	// disk and swaps it in, live, without a daemon restart.
	ReloadHostCommands() error
}

type HostCommandsHandler struct {
	Service HostCommandsService
}

func (h *HostCommandsHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/reload", h.Reload)
	return r
}

// List handles GET /api/host_commands: returns the sorted list of
// host_commands names known to the daemon, for Web UI / CLI (`boid
// host-commands list`) validation of a workspace's host_commands references.
func (h *HostCommandsHandler) List(w http.ResponseWriter, r *http.Request) {
	commands := h.Service.HostCommands()
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	writeJSON(w, http.StatusOK, names)
}

// Reload handles POST /api/host_commands/reload: re-reads the aggregated
// config from disk after a hand edit.
func (h *HostCommandsHandler) Reload(w http.ResponseWriter, r *http.Request) {
	if err := h.Service.ReloadHostCommands(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
}
