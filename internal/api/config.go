package api

import (
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// ConfigService is the daemon-side dependency behind /api/config
// (MAJOR 1, codex review round 1, docs/plans/workspace-db-consolidation.md
// Phase 2.5 PR7's read-only KitsDir surface, extended by docs/plans/
// volume-only-daemon.md §論点 f's `boid config get/set/unset/apply/edit`
// CLI with a full read+validate+persist+hot-reload surface). Implemented
// directly by *server.Server — no adapter type is needed, mirroring
// HostCommandsService's own convention in this package.
type ConfigService interface {
	// KitsDir returns the daemon's effective base directory for installed
	// kits (server.Config.KitsDir, resolved once at startup — see
	// cmd/start.go's defaultKitsDir()/--kits-dir flag).
	KitsDir() string
	// ConfigYAML returns the daemon's current effective config.yaml
	// document — the same bytes `boid config get` (no key) prints
	// verbatim, and what every `boid config get/set/unset/apply/edit`
	// invocation fetches first as its "before" snapshot.
	ConfigYAML() ([]byte, error)
	// ApplyConfigYAML validates a full replacement config.yaml document
	// (internal/config.ValidateYAML), and on success atomically persists
	// it and hot-applies whichever changed keys the daemon can apply live
	// (docs/plans/volume-only-daemon.md §論点 f's reload-semantics table),
	// returning operator-facing warning lines for anything that could not
	// be hot-applied (gateway.forges.* — restart required; sandbox.backend
	// — retirement notice). A validation failure leaves the daemon's live
	// config and on-disk config.yaml untouched.
	ApplyConfigYAML(data []byte) (ConfigApplyResult, error)
}

// ConfigApplyResult is POST /api/config's response body.
type ConfigApplyResult struct {
	// Warnings are pre-formatted, operator-facing lines (docs/plans/
	// volume-only-daemon.md §論点 f's exact wording for the
	// restart-required and sandbox.backend-retirement cases) — the CLI
	// prints these verbatim rather than reconstructing them client-side,
	// so the daemon (which alone knows exactly which leaf paths actually
	// changed) is the single source of truth for when a warning fires.
	Warnings []string `json:"warnings,omitempty"`
}

type ConfigHandler struct {
	Service ConfigService
}

func (h *ConfigHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/kits-dir", h.KitsDir)
	r.Get("/", h.Get)
	r.Post("/", h.Apply)
	return r
}

// configBodyMaxBytes caps a config.yaml request body at 1 MiB — the same
// bound internal/api/workspace.go's workspaceBodyMaxBytes uses for the same
// reason (a hand-authored config.yaml is a handful of KB at most; anything
// larger is either a mistake or an attempt to make the daemon buffer an
// unbounded body in memory).
const configBodyMaxBytes = 1 << 20 // 1 MiB

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

// Get handles GET /api/config: the daemon's current effective config.yaml,
// verbatim, as application/yaml. `boid config get`/`set`/`unset`/`edit` all
// fetch this first as their working copy.
func (h *ConfigHandler) Get(w http.ResponseWriter, r *http.Request) {
	data, err := h.Service.ConfigYAML()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// Apply handles POST /api/config: `boid config apply -f`/`edit`/the
// synthesized whole-document body `set`/`unset` build client-side after
// mutating their local copy of GET /api/config's response. See
// ConfigService.ApplyConfigYAML's doc comment for the validate-persist-
// reload contract.
func (h *ConfigHandler) Apply(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, configBodyMaxBytes)
	data, err := io.ReadAll(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("request body unreadable or exceeds %d bytes: %v", configBodyMaxBytes, err))
		return
	}
	result, err := h.Service.ApplyConfigYAML(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
