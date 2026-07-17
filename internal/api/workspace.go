package api

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
