package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// ConfigService is the daemon-side dependency behind /api/config
// (MAJOR 1, codex review round 1, docs/plans/workspace-db-consolidation.md
// Phase 2.5 PR7's read-only KitsDir surface, extended by docs/plans/
// volume-only-daemon.md §論点 f's `boid config get/set/unset/apply/edit`
// CLI with a full read+validate+persist+hot-reload surface, and further
// extended by BLOCKER 1's codex review round 1 fix: a server-side mutate
// endpoint plus ETag/If-Match concurrency on the full-document endpoints).
// Implemented directly by *server.Server — no adapter type is needed,
// mirroring HostCommandsService's own convention in this package.
type ConfigService interface {
	// KitsDir returns the daemon's effective base directory for installed
	// kits (server.Config.KitsDir, resolved once at startup — see
	// cmd/start.go's defaultKitsDir()/--kits-dir flag).
	KitsDir() string
	// ConfigYAML returns the daemon's current effective config.yaml
	// document, alongside its current revision — the same bytes `boid
	// config get` (no key) prints verbatim, and what every `boid config
	// get/apply/edit` invocation fetches first as its "before" snapshot.
	// revision is the same value GET /api/config's ETag response header
	// carries; round-tripping it into a later POST's If-Match is BLOCKER
	// 1's optimistic-concurrency guard (codex review round 1).
	ConfigYAML() (data []byte, revision string, err error)
	// ApplyConfigYAML validates a full replacement config.yaml document
	// (internal/config.ValidateYAML), and on success atomically persists
	// it and hot-applies whichever changed keys the daemon can apply live
	// (docs/plans/volume-only-daemon.md §論点 f's reload-semantics table),
	// returning operator-facing warning lines for anything that could not
	// be hot-applied (gateway.forges.* — restart required; sandbox.backend
	// — retirement notice). A validation failure leaves the daemon's live
	// config and on-disk config.yaml untouched.
	//
	// Unless force is true, ifMatch must be non-empty and equal the
	// current revision — the same ETag/If-Match convention `boid workspace
	// edit` already established (see UpdateWorkspace) — otherwise the call
	// fails with a *StatusError (428 Precondition Required when ifMatch is
	// empty, 412 Precondition Failed when it is stale) and neither
	// validates nor persists data.
	ApplyConfigYAML(data []byte, ifMatch string, force bool) (ConfigApplyResult, error)
	// MutateConfig performs a single dotted-path set/unset as one
	// atomic, server-side read-modify-write (BLOCKER 1, codex review round
	// 1) — `boid config set/unset` route here instead of a client-side
	// GET → mutate → POST round trip, which left a window for two
	// concurrent calls to silently lose one's change. See
	// internal/server/config_edit.go's MutateConfig doc comment for the
	// full rationale.
	MutateConfig(req ConfigMutateRequest) (ConfigMutateResult, error)
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

// ConfigMutateOp names a POST /api/config/mutate operation.
type ConfigMutateOp string

const (
	// ConfigMutateSet sets a scalar (1 Value) or replaces an array
	// wholesale (multiple Values) at Key.
	ConfigMutateSet ConfigMutateOp = "set"
	// ConfigMutateUnset removes Key (Value is ignored).
	ConfigMutateUnset ConfigMutateOp = "unset"
)

// ConfigMutateRequest is POST /api/config/mutate's request body — `boid
// config set <key> <value...>` / `unset <key>` (BLOCKER 1, codex review
// round 1).
type ConfigMutateRequest struct {
	Op    ConfigMutateOp `json:"op"`
	Key   string         `json:"key"`
	Value []string       `json:"value,omitempty"`
}

// ConfigMutateResult is POST /api/config/mutate's response body: the same
// Warnings ConfigApplyResult carries, plus the resulting full document and
// its new revision (for a caller that wants to keep editing without a
// follow-up GET — not currently consumed by `boid config set/unset`, which
// only prints Warnings, but useful to a future Web UI settings page per
// docs/plans/volume-only-daemon.md §論点 f).
type ConfigMutateResult struct {
	ConfigApplyResult
	YAML     []byte `json:"yaml"`
	Revision string `json:"revision"`
}

type ConfigHandler struct {
	Service ConfigService
}

func (h *ConfigHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/kits-dir", h.KitsDir)
	r.Get("/", h.Get)
	r.Post("/", h.Apply)
	r.Post("/mutate", h.Mutate)
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
// verbatim, as application/yaml, with an ETag response header mirroring the
// revision (BLOCKER 1, codex review round 1) so a caller can round-trip it
// straight into a subsequent POST's If-Match. `boid config
// get`/`apply`/`edit` all fetch this first as their working copy (`set`/
// `unset` route through POST /api/config/mutate instead — see Mutate).
func (h *ConfigHandler) Get(w http.ResponseWriter, r *http.Request) {
	data, revision, err := h.Service.ConfigYAML()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if revision != "" {
		w.Header().Set("ETag", `"`+revision+`"`)
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// Apply handles POST /api/config[?force=true]: `boid config apply -f`/
// `edit`'s whole-document replace, gated by If-Match unless ?force=true is
// passed — the same convention PUT /api/workspaces/{slug} already
// established (internal/api/workspace.go's Update). See
// ConfigService.ApplyConfigYAML's doc comment for the validate-persist-
// reload-and-revision-check contract.
func (h *ConfigHandler) Apply(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, configBodyMaxBytes)
	data, err := io.ReadAll(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("request body unreadable or exceeds %d bytes: %v", configBodyMaxBytes, err))
		return
	}
	force := r.URL.Query().Get("force") == "true"
	ifMatch := unquoteETag(r.Header.Get("If-Match"))
	result, err := h.Service.ApplyConfigYAML(data, ifMatch, force)
	if err != nil {
		writeConfigServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// Mutate handles POST /api/config/mutate: `boid config set/unset`'s single
// dotted-path operation, performed server-side as one atomic
// read-modify-write (BLOCKER 1, codex review round 1). See
// ConfigService.MutateConfig's doc comment.
func (h *ConfigHandler) Mutate(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, configBodyMaxBytes)
	var req ConfigMutateRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	result, err := h.Service.MutateConfig(req)
	if err != nil {
		writeConfigServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// writeConfigServiceError renders an error from ApplyConfigYAML/
// MutateConfig: a *StatusError carries its own status code (BLOCKER 1's
// 428/412 If-Match contract), while anything else — a config.ValidateYAML
// failure, an unknown dotted-path key, a coercion error — is a 400 Bad
// Request, the convention this endpoint has always used for "the document/
// operation itself is invalid" (as opposed to writeServiceError's generic
// 500-unless-StatusError fallback used elsewhere in this package, which
// would misclassify an ordinary validation failure as a server error).
func writeConfigServiceError(w http.ResponseWriter, err error) {
	if serr, ok := err.(*StatusError); ok {
		writeError(w, serr.Code, serr.Message)
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}
