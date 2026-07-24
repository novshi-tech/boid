package api

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type WorkspaceHandler struct {
	Service ProjectService
	// RuntimesDir, when non-empty, is server/wire.go's runtimesDirFor(cfg) —
	// used to resolve each workspace's home directory (docs/plans/
	// home-workspace-volume.md Phase 4 PR5) for Show's size reporting and
	// Remove's home-directory deletion. Left empty, both features degrade
	// gracefully: Show omits WorkspaceDetail.Home, Remove reports
	// home_deleted=false with no attempt made.
	RuntimesDir string
}

func (h *WorkspaceHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Post("/import", h.Import)
	// "/export" and "/apply" are static path segments at this same router
	// level as "/{slug}" — chi's radix tree gives a static segment priority
	// over a wildcard one at the same depth (the same precedent "/import"
	// above already establishes for POST), so these never collide with a
	// workspace literally named "export"/"apply".
	r.Get("/export", h.ExportEnvelope)
	r.Post("/apply", h.Apply)
	r.Get("/{slug}", h.Show)
	r.Get("/{slug}/export", h.Export)
	r.Put("/{slug}", h.Update)
	r.Delete("/{slug}", h.Remove)
	return r
}

// workspaceBodyMaxBytes caps a workspace yaml request body at 1 MiB
// (docs/plans/workspace-db-consolidation.md 「API 追加」設計判断: 「body 上限:
// 1 MiB (workspace yaml は数 KB 想定、DoS 防御)」). Workspace yaml documents
// are a handful of KB at most — anything larger is either a mistake or an
// attempt to make the daemon buffer an unbounded body in memory.
const workspaceBodyMaxBytes = 1 << 20 // 1 MiB

func (h *WorkspaceHandler) List(w http.ResponseWriter, r *http.Request) {
	workspaces, err := h.Service.ListWorkspaces()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workspaces)
}

// readWorkspaceYAMLBody validates the request's Content-Type (must be yaml,
// or unset) and reads the body through http.MaxBytesReader so an
// oversized payload is rejected rather than buffered in full. On any
// rejection it writes the HTTP response itself and returns ok=false; the
// caller should return immediately without writing anything further.
func readWorkspaceYAMLBody(w http.ResponseWriter, r *http.Request) (data []byte, ok bool) {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mediaType := ct
		if i := strings.IndexByte(ct, ';'); i >= 0 {
			mediaType = ct[:i]
		}
		mediaType = strings.TrimSpace(strings.ToLower(mediaType))
		switch mediaType {
		case "application/yaml", "application/x-yaml", "text/yaml", "text/x-yaml":
			// accepted
		default:
			writeError(w, http.StatusBadRequest, fmt.Sprintf("Content-Type must be application/yaml, got %q", ct))
			return nil, false
		}
	}

	body := http.MaxBytesReader(w, r.Body, workspaceBodyMaxBytes)
	data, err := io.ReadAll(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("request body unreadable or exceeds %d bytes: %v", workspaceBodyMaxBytes, err))
		return nil, false
	}
	return data, true
}

// unquoteETag strips a surrounding pair of double quotes from an ETag/
// If-Match header value, matching the HTTP convention of quoted entity
// tags (`If-Match: "rev-1"`) while still accepting a bare unquoted value
// for CLI/script convenience.
func unquoteETag(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

// setWorkspaceETag sets the ETag response header from detail's revision,
// quoted per HTTP convention. No-op when Revision is empty (should not
// normally happen once a workspaces row exists, but callers should not
// assume it non-empty).
func setWorkspaceETag(w http.ResponseWriter, detail *WorkspaceDetail) {
	if detail != nil && detail.Revision != "" {
		w.Header().Set("ETag", `"`+detail.Revision+`"`)
	}
}

// Create handles POST /api/workspaces (docs/plans/workspace-db-consolidation.md
// PR4 Step C). The body is a yaml document with the target slug inlined
// (`slug: foo`) alongside the workspace meta fields — there is no URL
// parameter for the new slug, since the daemon does not yet know it.
func (h *WorkspaceHandler) Create(w http.ResponseWriter, r *http.Request) {
	data, ok := readWorkspaceYAMLBody(w, r)
	if !ok {
		return
	}
	slug, meta, err := orchestrator.DecodeWorkspaceCreateStrict(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if slug == "" {
		writeError(w, http.StatusBadRequest, "slug is required (top-level \"slug:\" key in the request body)")
		return
	}

	detail, err := h.Service.CreateWorkspace(slug, meta)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	setWorkspaceETag(w, detail)
	writeJSON(w, http.StatusOK, detail)
}

// Show handles GET /api/workspaces/{slug} (Step D): meta + summary
// (revision, assigned project ids), with an ETag response header mirroring
// the revision so a client can round-trip it straight into a subsequent
// PUT's If-Match.
func (h *WorkspaceHandler) Show(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	detail, err := h.Service.GetWorkspace(slug)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if h.RuntimesDir != "" {
		home := computeWorkspaceHomeSize(h.RuntimesDir, slug)
		detail.Home = &home
	}
	setWorkspaceETag(w, detail)
	writeJSON(w, http.StatusOK, detail)
}

// Update handles PUT /api/workspaces/{slug} (Step E): whole-document
// replace, gated by If-Match unless ?force=true is passed (decision 17 —
// PUT + If-Match, no PATCH). See ProjectAppService.UpdateWorkspace for the
// exact status code contract (428 missing If-Match, 412 stale If-Match).
func (h *WorkspaceHandler) Update(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	data, ok := readWorkspaceYAMLBody(w, r)
	if !ok {
		return
	}
	meta, err := orchestrator.DecodeWorkspaceMetaStrict(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	force := r.URL.Query().Get("force") == "true"
	ifMatch := unquoteETag(r.Header.Get("If-Match"))

	detail, err := h.Service.UpdateWorkspace(slug, meta, ifMatch, force)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	setWorkspaceETag(w, detail)
	writeJSON(w, http.StatusOK, detail)
}

// Export handles GET /api/workspaces/{slug}/export (docs/plans/
// workspace-db-consolidation.md PR5 Step A): the response body is the raw
// yaml the service returns verbatim (the marshaled WorkspaceMeta with a
// top-level "slug:" key inlined — the exact same shape POST
// /api/workspaces/import accepts, so an export → import round-trip needs
// no translation step — see ProjectAppService.ExportWorkspace's doc
// comment for the rationale). An ETag header mirrors the revision so a
// caller can round-trip it into a subsequent PUT's If-Match if it chooses
// that route instead of POST import.
func (h *WorkspaceHandler) Export(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	data, revision, err := h.Service.ExportWorkspace(slug)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if revision != "" {
		w.Header().Set("ETag", `"`+revision+`"`)
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// Import handles POST /api/workspaces/import?mode=<create-only|replace>
// (docs/plans/workspace-db-consolidation.md PR5 Step B): the body shape
// mirrors Create's (top-level "slug:" key alongside the meta fields, decoded
// by the same DecodeWorkspaceCreateStrict). mode defaults to "create-only"
// (the safe choice — never overwrites an existing workspace) when the query
// param is omitted; an unrecognized mode value is rejected by
// ImportWorkspace itself with 400.
func (h *WorkspaceHandler) Import(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = workspaceImportModeCreateOnly
	}

	data, ok := readWorkspaceYAMLBody(w, r)
	if !ok {
		return
	}
	slug, meta, err := orchestrator.DecodeWorkspaceCreateStrict(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if slug == "" {
		writeError(w, http.StatusBadRequest, "slug is required (top-level \"slug:\" key in the request body)")
		return
	}

	detail, err := h.Service.ImportWorkspace(slug, meta, mode)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	setWorkspaceETag(w, detail)
	writeJSON(w, http.StatusOK, detail)
}

// Remove handles DELETE /api/workspaces/{slug} (Step F). The reserved
// default slug and re-assignment of any still-assigned project are enforced
// at the service/repository layer (ProjectAppService.RemoveWorkspace →
// orchestrator.WorkspaceRepository.Remove's transaction).
//
// docs/plans/home-workspace-volume.md Phase 4 PR5 adds home directory
// deletion on top of the pre-existing row removal: the workspace row is
// always removed first (via h.Service.RemoveWorkspace), and only then does
// this attempt to delete slug's home directory on disk — trusted-side
// deletion, since a sandboxed job or a remote CLI client has no way to
// remove a path on the daemon's own filesystem itself. A home-deletion
// failure (e.g. permission denied) is reported in the response but does not
// turn this into an error response: the row is already gone, and the plan
// doc explicitly allows this "part-completed" outcome (see
// WorkspaceRemoveResponse's doc comment) rather than trying to make the two
// deletions atomic.
func (h *WorkspaceHandler) Remove(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if err := h.Service.RemoveWorkspace(slug); err != nil {
		writeServiceError(w, err)
		return
	}

	resp := WorkspaceRemoveResponse{Status: "removed"}
	if h.RuntimesDir != "" {
		info, deleted, delErr := deleteWorkspaceHome(h.RuntimesDir, slug)
		resp.HomePath = info.Path
		resp.HomeBytes = info.Bytes
		resp.HomeSizeError = info.SizeError
		resp.HomeDeleted = deleted
		if info.SizeError != "" {
			slog.Warn("workspace remove: home directory size lookup failed (deletion proceeds regardless, best-effort)",
				"slug", slug, "path", info.Path, "error", info.SizeError)
		}
		if delErr != nil {
			resp.HomeDeleteError = delErr.Error()
			slog.Warn("workspace remove: home directory deletion failed",
				"slug", slug, "path", info.Path, "error", delErr)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// Apply handles POST /api/workspaces/apply?dry_run=<bool> (docs/plans/
// volume-only-daemon.md PR-1d codex round-1 Blocker 2/Major 1): the body is
// exactly one apiVersion:boid.dev/v1 kind:Workspace yaml document (a
// multi-document body — more than one "---"-separated document — is
// rejected; `boid workspace apply` sends one request per document, which is
// exactly what gives each document its own all-or-nothing DB transaction
// rather than sharing one across an entire multi-workspace file).
// dry_run=true runs every read/write the real apply would (same
// validation, same DB statements) but rolls back instead of committing.
//
// ?dry_run is parsed with strconv.ParseBool (PR-1d codex round-2 Major: the
// previous `== "true"` check failed OPEN — "True", "1", or a typo silently
// fell through to a real commit instead of the requested preview). An
// unparseable value is now rejected with 400 rather than defaulting to
// mutation, and which mode ran is always logged at INFO so a caller can
// confirm from the server log which path a given request actually took.
//
// Presence, not non-emptiness, gates parsing (PR-1d codex round-3 Major):
// the round-2 fix still checked `raw != ""`, so `?dry_run=` (empty value) or
// `?dry_run` (no `=` at all — reachable via `?dry_run=${DRY_RUN}` with an
// unset shell variable) produced raw=="" and fell through as if the
// parameter were absent entirely, silently performing a real commit for a
// caller who explicitly asked for dry_run. url.Values.Has reports whether
// the key was present in the query string at all, independent of its value,
// so an explicitly-empty dry_run now reaches strconv.ParseBool("") — which
// fails — and is rejected with 400, same as any other unparseable value.
//
// The query string is parsed explicitly with url.ParseQuery rather than the
// convenience r.URL.Query() (PR-1d codex round-4 Minor): (1) Query() returns
// only the FIRST value for a repeated key, so `?dry_run=false&dry_run=true`
// silently used "false" and performed a real commit even though the
// request also said "true" elsewhere — ambiguous/contradictory input, now
// rejected with 400 rather than picking one arbitrarily; (2) Query() also
// silently discards the underlying ParseQuery error (e.g. an unescaped ';'
// separator, rejected by net/url since Go 1.17), which can make a key
// disappear from the parsed result entirely rather than surface the
// malformed request — a malformed query string is now itself a 400 instead
// of silently degrading to "no dry_run param at all" (a real commit).
func (h *WorkspaceHandler) Apply(w http.ResponseWriter, r *http.Request) {
	dryRun := false
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid query string: %v", err))
		return
	}
	if values, ok := query["dry_run"]; ok {
		if len(values) > 1 {
			writeError(w, http.StatusBadRequest, "dry_run: ambiguous parameter (given more than once)")
			return
		}
		parsed, err := strconv.ParseBool(values[0])
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("dry_run: invalid boolean %q", values[0]))
			return
		}
		dryRun = parsed
	}
	slog.Info("workspace apply", "dry_run", dryRun)

	data, ok := readWorkspaceYAMLBody(w, r)
	if !ok {
		return
	}
	docs, err := orchestrator.DecodeWorkspaceEnvelopeDocuments(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(docs) != 1 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("expected exactly one Workspace document per request, got %d", len(docs)))
		return
	}

	// PR-1d codex round-2 Minor: `boid workspace apply` (cmd/workspace_apply.go)
	// already warns client-side when a document still carries a retired
	// spec.additional_bindings key, but a caller hitting this endpoint
	// directly (not through the CLI) never saw that warning at all — the
	// key was silently parsed and discarded. Surface the same warning here
	// so every caller of this endpoint gets it, not just the CLI's own
	// pre-parse.
	if docs[0].AdditionalBindingsDropped {
		slog.Warn("workspace apply: spec.additional_bindings is no longer supported and was dropped",
			"workspace", docs[0].Envelope.Metadata.Name)
	}

	result, err := h.Service.ApplyWorkspace(docs[0], dryRun)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if docs[0].AdditionalBindingsDropped {
		result.Warnings = append(result.Warnings, fmt.Sprintf("spec.additional_bindings is no longer supported (retired) — dropped for workspace %q", docs[0].Envelope.Metadata.Name))
	}
	writeJSON(w, http.StatusOK, result)
}

// ExportEnvelope handles GET /api/workspaces/export?all=true or
// ?name=<slug> (docs/plans/volume-only-daemon.md PR-1d codex round-1
// Blocker 3): returns one or more apiVersion:boid.dev/v1 kind:Workspace
// yaml documents built from a single atomic DB snapshot
// (ProjectAppService.ExportWorkspaceEnvelopes / orchestrator.
// SnapshotWorkspacesForExport), "---"-separated when more than one.
func (h *WorkspaceHandler) ExportEnvelope(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "true"
	name := r.URL.Query().Get("name")
	if all == (name != "") {
		writeError(w, http.StatusBadRequest, "exactly one of ?all=true or ?name=<slug> is required")
		return
	}

	var slugs []string
	if !all {
		if err := orchestrator.ValidWorkspaceSlug(name); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		slugs = []string{name}
	}

	data, err := h.Service.ExportWorkspaceEnvelopes(slugs)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
