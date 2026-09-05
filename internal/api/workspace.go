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
	// Homes, when non-nil, is the engine-backed view of workspace HOME
	// volumes used for Show's size reporting and Remove's home deletion.
	// Left nil, both features degrade gracefully: Show omits
	// WorkspaceDetail.Home, Remove reports home_deleted=false.
	Homes WorkspaceHomeStore

	// HomeImporter, when non-nil, serves
	// POST /api/workspaces/{slug}/home/import, migrating a pre-existing host
	// home directory into the workspace's HOME volume. Left nil, the route
	// answers 501.
	HomeImporter WorkspaceHomeImporter
}

func (h *WorkspaceHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Create)
	// "/export" and "/apply" are static path segments at this router level,
	// so GET/POST on those literal names always resolve here (ExportEnvelope
	// / Apply) rather than falling through to "/{slug}" (Show / Create) —
	// the shadowed set is exactly {"export","apply"} on the methods each
	// static route claims; other methods (and slugs like "import", whose
	// POST route was removed) fall through normally.
	//
	// ValidWorkspaceSlug has no reserved-word list, so a workspace really
	// can be created under either name. That collision is an accepted
	// tradeoff, not a bug: reserving the names would make an already-
	// existing workspace of that name unreadable (ValidWorkspaceSlug runs
	// on the read path too), which costs more than the collision does. See
	// TestWorkspaceRouteShadowing_AcceptedCollision.
	r.Get("/export", h.ExportEnvelope)
	r.Post("/apply", h.Apply)
	r.Get("/{slug}", h.Show)
	// Nested under the slug rather than a top-level "/import-home" so the
	// target is a path parameter like every other per-workspace operation.
	r.Post("/{slug}/home/import", h.ImportHome)
	// The workspace's init.sh, read and written through the daemon because
	// the file lives in the daemon's own volume. See workspace_init_script.go.
	r.Get("/{slug}/init-script", h.GetInitScript)
	r.Put("/{slug}/init-script", h.PutInitScript)
	r.Put("/{slug}", h.Update)
	r.Delete("/{slug}", h.Remove)
	return r
}

// workspaceBodyMaxBytes caps a workspace yaml request body at 1 MiB.
// Workspace yaml documents are a handful of KB at most — anything larger
// is either a mistake or an attempt to make the daemon buffer an
// unbounded body in memory.
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

// Create handles POST /api/workspaces. The body is a yaml document with
// the target slug inlined (`slug: foo`) alongside the workspace meta
// fields — there is no URL parameter for the new slug, since the daemon
// does not yet know it.
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
	if h.Homes != nil {
		home := computeWorkspaceHomeSize(r.Context(), h.Homes, slug)
		detail.Home = &home
	}
	setWorkspaceETag(w, detail)
	writeJSON(w, http.StatusOK, detail)
}

// Update handles PUT /api/workspaces/{slug}: whole-document replace,
// gated by If-Match unless ?force=true is passed. See
// ProjectAppService.UpdateWorkspace for the exact status code contract
// (428 missing If-Match, 412 stale If-Match).
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

// Remove handles DELETE /api/workspaces/{slug}. The reserved default slug
// and re-assignment of any still-assigned project are enforced at the
// service/repository layer (ProjectAppService.RemoveWorkspace →
// orchestrator.WorkspaceRepository.Remove's transaction).
//
// The workspace row is removed first (h.Service.RemoveWorkspace); only
// then does this attempt to delete the workspace's home volume —
// trusted-side, since only the daemon can reach its own docker socket. A
// home-deletion failure (e.g. the engine's 409 for a volume a running job
// still holds) is reported in the response rather than turned into an
// error: the row is already gone, and a part-completed outcome here is an
// accepted tradeoff over trying to make the two deletions atomic.
//
// Both deletions run inside the exclusion held below so they cannot
// interleave with `boid workspace import-home`, which would otherwise
// leave a volume full of credentials that no dispatch can mount and this
// endpoint can no longer target.
//
// The workspace's init.sh is a third piece, deleted by RemoveWorkspace
// itself (not here) under the service's own mutex, so it can't be
// inherited by a later workspace created under the same slug before
// cleanup runs.
func (h *WorkspaceHandler) Remove(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	// Held across both the row removal and the home-volume deletion below —
	// not just the volume deletion — because it is the row's disappearance
	// that `boid workspace import-home` must be excluded from. Refused
	// (not queued) when a migration is already running: nothing has
	// changed yet, so the operator only loses a retry. A nil HomeImporter
	// means no migration can be running in this daemon either, so there is
	// nothing to exclude.
	if h.HomeImporter != nil {
		release, err := h.HomeImporter.BeginWorkspaceHomeRemoval(slug)
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		defer release()
	}

	// The row and the workspace's init.sh are removed together (see
	// ProjectAppService.RemoveWorkspace) since both must be excluded from a
	// concurrent apply. The script's fate comes back in the result rather
	// than as an error: by the time it's touched the row is already gone.
	removal, err := h.Service.RemoveWorkspace(slug)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	resp := WorkspaceRemoveResponse{Status: "removed"}
	resp.InitScriptDeleted = removal.InitScriptDeleted
	if removal.InitScriptError != nil {
		resp.InitScriptDeleteError = removal.InitScriptError.Error()
		slog.Warn("workspace remove: init.sh deletion failed (the workspace row is already gone; delete it by hand or a later workspace of the same name will inherit it)",
			"slug", slug, "error", removal.InitScriptError)
	}

	if h.Homes != nil {
		info, deleted, delErr := deleteWorkspaceHome(r.Context(), h.Homes, slug)
		resp.HomeVolume = info.Volume
		resp.HomeBytes = info.Bytes
		resp.HomeSizeError = info.SizeError
		resp.HomeDeleted = deleted
		if info.SizeError != "" {
			slog.Warn("workspace remove: home volume size lookup failed (deletion proceeds regardless, best-effort)",
				"slug", slug, "volume", info.Volume, "error", info.SizeError)
		}
		if delErr != nil {
			resp.HomeDeleteError = delErr.Error()
			slog.Warn("workspace remove: home volume deletion failed",
				"slug", slug, "volume", info.Volume, "error", delErr)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// Apply handles POST /api/workspaces/apply?dry_run=<bool>: the body is
// exactly one apiVersion:boid.dev/v1 kind:Workspace yaml document (a
// multi-document body is rejected; `boid workspace apply` sends one
// request per document, giving each its own all-or-nothing DB
// transaction). dry_run=true runs every read/write a real apply would but
// rolls back instead of committing.
//
// dry_run is parsed carefully because a permissive parse here would
// silently turn into a real commit instead of the requested preview:
//   - strconv.ParseBool rejects an unparseable value with 400 rather than
//     defaulting to mutation.
//   - Presence (url.Values.Has), not non-emptiness, gates parsing, so
//     `?dry_run=` or `?dry_run` (no value) is rejected rather than treated
//     as absent.
//   - The query string is parsed explicitly via url.ParseQuery rather than
//     r.URL.Query(), which silently takes only the first value of a
//     repeated key and swallows a malformed-query error — both now surface
//     as 400 instead of falling through to a real commit.
//
// Which mode ran is always logged at INFO so a caller can confirm from the
// server log which path a given request actually took.
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

	// `boid workspace apply` already warns client-side about a retired
	// spec.additional_bindings key, but a caller hitting this endpoint
	// directly never saw that warning — surface it here too.
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
// ?name=<slug>: returns one or more apiVersion:boid.dev/v1 kind:Workspace
// yaml documents built from a single atomic DB snapshot
// (ProjectAppService.ExportWorkspaceEnvelopes), "---"-separated when more
// than one.
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
