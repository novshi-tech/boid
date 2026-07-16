package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// ConfigService is the daemon-side dependency behind GET /api/config/kits-dir
// (MAJOR 1, codex review round 1, docs/plans/workspace-db-consolidation.md
// Phase 2.5 PR7): a small read-only surface exposing daemon startup config
// that only the running daemon actually knows, since it may have been
// overridden at `boid start` time (--kits-dir) and never reaches whatever
// default a CLI client-side helper would otherwise (re-)derive on its own.
// Implemented directly by *server.Server (its KitsDir() method already
// matches this shape) — no adapter type is needed, mirroring
// HostCommandsService's own convention in this package.
type ConfigService interface {
	// KitsDir returns the daemon's effective base directory for installed
	// kits (server.Config.KitsDir, resolved once at startup — see
	// cmd/start.go's defaultKitsDir()/--kits-dir flag).
	KitsDir() string
}

type ConfigHandler struct {
	Service ConfigService
}

func (h *ConfigHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/kits-dir", h.KitsDir)
	return r
}

// configKitsDirResponse is the GET /api/config/kits-dir response body.
type configKitsDirResponse struct {
	KitsDir string `json:"kits_dir"`
}

// KitsDir handles GET /api/config/kits-dir: the daemon's effective KitsDir,
// so a CLI client-side helper (cmd/workspace.go's ensureWorkspaceExistsForAssign,
// MAJOR 1) can resolve a legacy workspace.yaml's `kits:` references against
// the *running daemon's* kits directory instead of independently re-deriving
// a default that silently disagrees with it whenever the daemon was started
// with a custom --kits-dir.
func (h *ConfigHandler) KitsDir(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, configKitsDirResponse{KitsDir: h.Service.KitsDir()})
}
