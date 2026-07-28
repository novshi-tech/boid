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
	// volumes (docs/plans/workspace-home-volume-persistence.md 論点 a-2, PR7)
	// used for Show's size reporting and Remove's home deletion. Left nil,
	// both features degrade gracefully: Show omits WorkspaceDetail.Home,
	// Remove reports home_deleted=false with no attempt made — the same
	// degradation an empty RuntimesDir used to produce, now keyed on the
	// feature's only actual dependency. See WorkspaceHomeStore's doc comment.
	Homes WorkspaceHomeStore

	// HomeImporter, when non-nil, serves POST /api/workspaces/{slug}/home/import
	// — PR8's migration of a pre-PR6 host home directory into the workspace's
	// HOME volume (docs/plans/workspace-home-volume-persistence.md 論点 f).
	// Left nil the route answers 501, the same "the daemon has no runner to do
	// this" degradation a nil Homes produces for the size/delete surface.
	HomeImporter WorkspaceHomeImporter
}

func (h *WorkspaceHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Create)
	// "/export" and "/apply" are static path segments at this same router
	// level as "/{slug}" — chi's radix tree gives a static segment priority
	// over a wildcard one at the same depth, but only for the METHOD each
	// static route actually registers, not the path as a whole: GET
	// /api/workspaces/export resolves to ExportEnvelope below (never
	// Show), but PUT/DELETE /api/workspaces/export — no static route
	// registers those methods on "/export" — fall through to "/{slug}"
	// and hit Update/Remove("export") instead, same as any other slug.
	// Likewise GET /api/workspaces/apply falls through to Show("apply"),
	// since only POST is registered here (this is the exact shape "/import"
	// is in after its own POST registration was removed 2026-07-28 — see
	// TestWorkspaceHandler_ImportRouteRemoved). ValidWorkspaceSlug
	// (workspace_slug.go) has no reserved-word list, so a workspace can
	// really be created named "export"/"apply"/"import": it stays reachable
	// through every method this router does not shadow for that literal
	// path, and is unreachable through the ones it does — `boid workspace
	// show export` in particular resolves to ExportEnvelope and answers 400
	// instead of showing the workspace.
	//
	// That was raised as a follow-up when this comment was first written and
	// has since been CLOSED AS ACCEPTED (nose, 2026-07-28) rather than fixed
	// — this is the decision record, not a still-open item. Both alternatives
	// cost more than the collision does. Reserving the names turns a workspace
	// already created under one of them into a workspace the daemon then
	// refuses to accept, trading a partially-shadowed slug for a hard failure
	// on data that already exists. Moving the envelope routes off the bare
	// "/{name}" segment is an API change every client — the CLI included —
	// would have to follow, to buy back three names nobody has asked for.
	// TestWorkspaceRouteShadowing_AcceptedCollision pins the accepted
	// behaviour so a later change to it is a deliberate one.
	r.Get("/export", h.ExportEnvelope)
	r.Post("/apply", h.Apply)
	r.Get("/{slug}", h.Show)
	// PR8 (論点 f). Nested under the slug rather than a top-level
	// "/import-home" so the target is a path parameter like every other
	// per-workspace operation, and so a future "/{slug}/home/..." surface
	// (export, prune) has an obvious place to live.
	r.Post("/{slug}/home/import", h.ImportHome)
	// PR9 (論点 d), the surface PR8's comment above anticipated: the
	// workspace's init.sh, read and written through the daemon because the
	// file lives in the daemon's own volume. See workspace_init_script.go.
	r.Get("/{slug}/init-script", h.GetInitScript)
	r.Put("/{slug}/init-script", h.PutInitScript)
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
	if h.Homes != nil {
		home := computeWorkspaceHomeSize(r.Context(), h.Homes, slug)
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

// Remove handles DELETE /api/workspaces/{slug} (Step F). The reserved
// default slug and re-assignment of any still-assigned project are enforced
// at the service/repository layer (ProjectAppService.RemoveWorkspace →
// orchestrator.WorkspaceRepository.Remove's transaction).
//
// docs/plans/home-workspace-volume.md Phase 4 PR5 adds home deletion on top
// of the pre-existing row removal: the workspace row is always removed first
// (via h.Service.RemoveWorkspace), and only then does this attempt to delete
// slug's home — trusted-side deletion, since a sandboxed job or a remote CLI
// client has no way to reach the daemon's docker socket itself (and, since
// PR1 of docs/plans/workspace-home-volume-persistence.md, is denied the
// boid-ws- volume namespace outright even when it has one). A home-deletion
// failure — most plausibly the engine's 409 for a volume a running job still
// holds, see WorkspaceRemoveResponse.HomeDeleteError — is reported in the
// response but does not turn this into an error response: the row is already
// gone, and the plan doc explicitly allows this "part-completed" outcome (see
// WorkspaceRemoveResponse's doc comment) rather than trying to make the two
// deletions atomic.
//
// What that tolerance does NOT extend to is interleaving with
// `boid workspace import-home` (PR8 round-2 codex review, Major 1): a
// part-completed remove leaves a volume an operator can delete by hand, while an
// overlapping migration leaves a volume full of credentials that no dispatch can
// mount and this very endpoint can no longer target. Both deletions therefore
// run inside the exclusion taken below.
//
// A workspace has a THIRD piece — its init.sh — and that one is deleted by
// RemoveWorkspace itself, not here (PR9 codex round 3, Major 3). Not left
// behind: dispatch resolves a script from the SLUG alone, so the next
// workspace created with the same name would inherit the removed one's script
// and run it against a brand-new, empty HOME volume, with no way to clean it
// up first (every /{slug}/init-script route 404s once the row is gone). Not
// deleted from here either, because a second service call means a second
// critical section, and an apply landing between the two upserted the slug,
// wrote a script, answered 200 — and had that script deleted by the removal
// still finishing. The service does both under one hold of its mutex; this
// function only reports the outcome.
func (h *WorkspaceHandler) Remove(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	// Both deletions run inside this bracket (codex round 2 of PR8, Major 1).
	//
	// The two of them are not atomic and are not meant to be — the part-completed
	// outcome is documented above — but they must not INTERLEAVE with
	// `boid workspace import-home`, which deletes this workspace's HOME volume,
	// re-creates it under the same name and extracts the operator's credentials
	// into it. A migration that lands anywhere around these two statements leaves
	// a populated HOME volume whose workspace row is gone: `boid gc` reports it as
	// an orphan, no dispatch can ever mount it, and `boid workspace remove` — the
	// obvious cleanup — 404s on the row it no longer has. Held across BOTH
	// statements rather than just the volume deletion, because it is the ROW's
	// disappearance that the migration has to be excluded from.
	//
	// Refused rather than queued when a migration is already running: see
	// dispatcher's workspaceHomeInFlight.beginRemoval for why this one refuses
	// where a dispatch waits. Nothing has been changed at that point, so the
	// operator loses only the retry.
	//
	// A nil HomeImporter means no runner is wired, so no migration can be running
	// in this daemon either and there is nothing to exclude — the same off-switch
	// reading a nil Homes gets.
	if h.HomeImporter != nil {
		release, err := h.HomeImporter.BeginWorkspaceHomeRemoval(slug)
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		defer release()
	}

	// The row AND the workspace's init.sh, in one call because they have to be
	// excluded from a concurrent apply together (PR9 codex round 3, Major 3 —
	// see ProjectAppService.RemoveWorkspace). The script's fate comes back in
	// the result rather than being raised: by the time it is touched the row
	// is already gone.
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
