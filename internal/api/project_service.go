package api

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type ProjectAppService struct {
	Projects ProjectRepository
	Meta     interface {
		Load(workDir string) (*orchestrator.ProjectMeta, error)
		// LoadBareRepo reads .boid/project.yaml from a daemon-managed bare
		// git repository's HEAD (docs/plans/volume-only-daemon.md §論点a/b)
		// — the bare-repo counterpart to Load, used by
		// CreateProjectFromGitURL (first load after `git clone --mirror`),
		// which has no prior expected id to validate the freshly-read meta
		// against (see LoadBareRepoExpectingID's doc comment for why that
		// call site is the one exception).
		LoadBareRepo(bareRepoPath string) (*orchestrator.ProjectMeta, error)
		// LoadBareRepoExpectingID is LoadBareRepo's id-validated counterpart
		// (PR-2a codex round-3 M2 fix), used by FetchProject (`boid project
		// fetch <id>`, reload after `git fetch --all`) — the one LoadBareRepo
		// caller that already has a specific id to check the freshly-read
		// meta against. See orchestrator.ProjectStore.LoadBareRepoExpectingID's
		// doc comment for the cache-corruption bug this closes: the meta is
		// only committed to the shared cache once expectedID is confirmed to
		// match, so an id drifted to collide with an unrelated already-
		// registered project can never clobber (or, on cleanup, delete) that
		// other project's cache entry.
		LoadBareRepoExpectingID(bareRepoPath, expectedID, upstreamURL string) (*orchestrator.ProjectMeta, error)
		Get(id string) (*orchestrator.ProjectMeta, bool)
		Remove(id string)
		LoadAll(projects []*orchestrator.Project) []error
		SetWorkspaceID(projectID, workspaceID string)
		// Status / MarkDegraded expose the on-startup-auto-prune-retirement
		// invariant's in-memory status tracking (docs/plans/
		// volume-only-daemon.md §論点a). hydrateProject reads Status to
		// populate Project.Status/StatusMessage on every read path;
		// CreateProjectFromGitURL/FetchProject call MarkDegraded when a
		// clone/fetch/load step fails, preserving the DB row per the
		// invariant instead of ever deleting it.
		Status(id string) orchestrator.ProjectStatus
		MarkDegraded(id, message string)
		// SetSynthesizedMeta caches a meta value synthesized (not read from a
		// project.yaml) for a project.yaml-less project (docs/plans/
		// workspace-default-project.md PR5) — used by CreateProjectFromGitURL
		// (initial registration, GitHeadReadFailurePathAbsent) and FetchProject
		// (post-fetch reload, same failure kind) in place of LoadBareRepo /
		// LoadBareRepoExpectingID. Also clears any prior degraded status — see
		// orchestrator.ProjectStore.SetSynthesizedMeta's own doc comment.
		SetSynthesizedMeta(id string, meta *orchestrator.ProjectMeta)
		// Explain returns id's field-provenance view (docs/plans/
		// workspace-default-project.md 論点e, PR6) — see
		// orchestrator.ProjectStore.Explain's own doc comment.
		Explain(id string) (*orchestrator.ProjectExplain, error)
	}
	// CloneBareRepo clones a git URL as a daemon-managed bare (mirror)
	// repository at destPath, injecting forge credentials scoped to
	// namespace (the registering project's workspace slug) — wired in
	// internal/server/wire.go to dispatcher.CloneBareRepo, closed over the
	// daemon's gitgateway.CredentialProvider. nil (e.g. in tests that do not
	// exercise CreateProjectFromGitURL) makes that method fail with a clear
	// "not wired" error instead of panicking.
	CloneBareRepo func(ctx context.Context, url, destPath, namespace string) error
	// FetchBareRepo runs `git fetch --all` inside an existing daemon-managed
	// bare repo — wired in internal/server/wire.go to
	// dispatcher.FetchBareRepo. Same nil-safety convention as CloneBareRepo.
	FetchBareRepo func(ctx context.Context, bareRepoPath, namespace string) error
	// DataDir is the daemon's data-home (docs/plans/volume-only-daemon.md
	// §論点b: "<data_dir>/repos/<workspace-slug>/<project-name>.git/"),
	// wired in internal/server/wire.go to dataHomeFor(cfg) — the same value
	// TaskGCStore.WithAttachmentsRoot uses. Required by
	// CreateProjectFromGitURL to compute a new project's bare-repo path.
	DataDir string
	// Hydrator is optional. When set, callers that need fully-resolved meta
	// (workspace-level capabilities / host_commands / env merged in +
	// SecretNamespace injected) go through GetWithWorkspace instead of the
	// bare Meta.Get path.
	// Wired in internal/server/wire.go alongside the workspace store.
	Hydrator orchestrator.MetaHydrator
	// CaptureUpstreamURL reads a project work_dir's git origin remote and
	// normalizes it to HTTPS (docs/plans/git-gateway-cutover.md PR2). Wired
	// in internal/server/wire.go to dispatcher.CaptureUpstreamURL. When nil
	// (e.g. in tests that do not exercise this path), upstream_url capture
	// is skipped entirely rather than panicking.
	CaptureUpstreamURL func(workDir string) (string, error)
	// Workspaces provides direct CRUD over a single workspace's
	// WorkspaceMeta (docs/plans/workspace-db-consolidation.md PR4). Wired in
	// internal/server/wire.go to ProjectStore.WorkspaceStore(). Required for
	// the workspace create/show/update/remove endpoints; callers that never
	// exercise those (most existing tests) can leave it nil.
	Workspaces WorkspaceStore
	// HostCommands, when set, returns the daemon's live aggregated
	// host_commands snapshot (name -> spec). CreateWorkspace/UpdateWorkspace
	// validate every meta.HostCommands reference against this snapshot,
	// rejecting an unknown name with 400 (docs/plans/
	// workspace-db-consolidation.md MAJOR 2, codex review: an unresolvable
	// reference was previously persisted silently and only warned-about +
	// skipped at dispatch time). Wired in internal/server/wire.go to
	// Server.HostCommands. nil skips the validation entirely — tests that do
	// not exercise it can leave this unset, same convention as
	// CaptureUpstreamURL/Hydrator above.
	HostCommands func() map[string]orchestrator.HostCommandSpec
	// WorkspacesConn is the daemon's single *sql.DB handle (docs/plans/
	// volume-only-daemon.md PR-1d codex round-1 Blocker 2/3). ApplyWorkspace
	// and ExportWorkspaceEnvelopes need a raw connection — not the
	// WorkspaceStore/ProjectRepository interfaces above — because both
	// operations span workspace-meta AND project-assignment writes/reads
	// that must run inside ONE transaction (orchestrator.
	// ApplyWorkspaceEnvelope / orchestrator.SnapshotWorkspacesForExport),
	// which those two separate, narrower interfaces have no way to express.
	// Wired in internal/server/wire.go to srv.db, the same connection every
	// other repository in this daemon is already built from. nil skips both
	// endpoints with a 500 (same "not wired" convention as Workspaces
	// above) — tests that do not exercise them can leave this unset.
	WorkspacesConn *sql.DB
	// InitScripts reads and writes a workspace's init.sh at the path the
	// DISPATCHER reads it from (PR9 of
	// docs/plans/workspace-home-volume-persistence.md, 論点 d). Wired in
	// internal/server/wire.go to dispatcher.WorkspaceInitScriptStore{}.
	//
	// nil is the feature's off switch, the same convention Workspaces /
	// WorkspacesConn above use: GetWorkspaceInitScript and
	// SetWorkspaceInitScript answer 501, and ApplyWorkspace warns about a
	// spec.init_script it cannot honor rather than dropping it silently.
	//
	// One field, held by the service rather than by the handler, because
	// three call sites need it — the two endpoints, ApplyWorkspace's
	// hydration, and ExportWorkspaceEnvelopes' — and two of those are inside
	// this type. Wiring a second copy onto WorkspaceHandler would be
	// harmless here (the store is stateless) but would put two answers to
	// "where does this daemon keep init.sh" in the wiring graph, which is the
	// thing PR8's WorkspaceHomeImporter comment argues against for a case
	// where it was NOT harmless.
	InitScripts WorkspaceInitScriptStore
	// mu serializes every workspace-mutating entry point — CreateWorkspace,
	// UpdateWorkspace, RemoveWorkspace, and SetProjectWorkspace — against
	// each other. It started out narrower (MAJOR 3, codex review round 1)
	// as protection for just SetProjectWorkspace's assign against
	// RemoveWorkspace's snapshot-then-cache-sync sequence: without it, a
	// concurrent SetProjectWorkspace reassigning one of RemoveWorkspace's
	// snapshotted projects to a different, still-existing workspace could
	// have its in-memory cache write clobbered by RemoveWorkspace's own
	// (stale-snapshot) cache loop landing after it — see RemoveWorkspace's
	// doc comment for the full scenario. The DB-level assign+existence-check
	// race (a DELETE landing between them) is closed separately by
	// Projects.AssignWorkspaceIfExists running as a single transaction; that
	// part of this mutex's job only ever protected the Go-side in-memory
	// cache (s.Meta) from a second, independent race.
	//
	// MAJOR 1 (codex review round 2, docs/plans/workspace-db-consolidation.md
	// PR4): the scope was widened to also cover CreateWorkspace and
	// UpdateWorkspace, because UpdateWorkspace's force=true path is not a
	// single atomic statement — it does Workspaces.Load (existence check)
	// then, separately, Workspaces.Save (an unconditional upsert). A DELETE
	// landing in that window would previously go completely unnoticed: the
	// subsequent Save does not re-check existence, so it silently recreates
	// (resurrects) the row RemoveWorkspace just deleted. Wrapping
	// UpdateWorkspace's whole body in s.mu closes that window by construction
	// — RemoveWorkspace cannot start (or finish) its own critical section
	// while a force-path Load-then-Save is in flight, and vice versa.
	// CreateWorkspace was widened alongside it for the same reason
	// (consistency across every mutation path) even though its own
	// Workspaces.Create call is a single insert-only statement with no
	// internal Load-then-Save split — see the fix's own test,
	// TestCreateWorkspace_BlocksMidRemove, for the concrete scenario this
	// closes (a Create in flight for a slug a concurrent Remove targets).
	// These four methods never call each other, so nesting the same
	// non-reentrant mutex across all of them carries no deadlock risk.
	//
	// MAJOR 1 (codex review round 3, docs/plans/workspace-db-consolidation.md
	// PR4): widened again to cover the two remaining workspace-cache writers
	// that had been left out — ReloadProjects and CreateProject's default-
	// workspace-assign step. ReloadProjects snapshots every project's
	// workspace_id via ListProjects and then writes it into s.Meta's cache via
	// LoadAll, outside any lock; a concurrent SetProjectWorkspace/
	// RemoveWorkspace landing in that window could have its own (fresher)
	// cache write clobbered by this reload's now-stale snapshot landing after
	// it — the same class of bug MAJOR 3 already closed between
	// RemoveWorkspace and SetProjectWorkspace. CreateProject's eager
	// assign-to-default (SetWorkspaceID after a successful
	// Projects.SetProjectWorkspace) is exactly the same kind of cache write
	// and needed the same protection. Only the ListProjects+LoadAll pair in
	// ReloadProjects is covered (not the subsequent per-project
	// upstream_url recapture loop, which never touches s.Meta) — see that
	// method's own comment. Neither ReloadProjects nor CreateProject calls
	// any other mu-guarded method, so this nests with no deadlock risk, same
	// as every prior widening.
	mu sync.Mutex
	// projectMu (renamed from registerMu — PR-2a codex round-2 review
	// MAJOR 1: its scope now spans every project create/delete entry
	// point, not just git-URL registration) serializes the whole
	// create-or-delete pipeline of CreateProject, CreateProjectFromGitURL,
	// and DeleteProject against EACH OTHER, deliberately SEPARATE from mu
	// above: mu protects only the in-memory workspace-assignment cache
	// across a handful of quick, DB-only operations, whereas
	// CreateProjectFromGitURL's critical section includes a full
	// `git clone` — reusing mu here would block every unrelated workspace
	// mutation (SetProjectWorkspace, CreateWorkspace, ...) daemon-wide for
	// the duration of a slow clone, for no correctness benefit to those
	// operations.
	//
	// Without SOME lock across this whole pipeline, two concurrent
	// registrations whose project.yaml `id:` values happen to collide (most
	// plausibly: the SAME repo added twice under two different --name
	// values before the first add's 409 same-path check would catch it, or
	// two different repos that happen to share an id) can both call
	// s.Meta.LoadBareRepo — which caches by meta.ID, not by bareRepoPath —
	// then both call s.Projects.CreateProject; the DB's primary key makes
	// exactly one of those inserts win, but the LOSING call's rollback
	// unconditionally executes s.Meta.Remove(meta.ID), deleting the WINNING
	// call's cache entry too (Remove has no way to tell "my own cache write"
	// from "some other call's cache write for the same id" apart). The
	// winner is left with a valid DB row but no cached Meta until the next
	// `boid project reload`. projectMu makes this impossible by
	// construction: only one create-or-delete body executes at a time, so a
	// second call's cache write can never land inside a first call's
	// clone-to-rollback (or delete's get-then-remove) window in the first
	// place.
	//
	// MAJOR 1 (codex round-2 review): round-1 only ever applied this lock
	// to CreateProjectFromGitURL. The legacy dir-based CreateProject and
	// DeleteProject remained completely unguarded, so the EXACT SAME
	// cache-corruption race remained reachable across the OTHER
	// registration/removal forms — e.g. a legacy `project add <dir>` and a
	// git-URL `project add <url>` racing for the same project.yaml `id:`,
	// or a `project rm` landing in the tiny window between a concurrent
	// registration's DB insert and its workspace-assign step (deleting the
	// row — and, if isManagedBareRepoPath, the bare repo directory — a
	// still-in-flight CreateProjectFromGitURL call believes it just
	// successfully created). All three now share this one lock.
	//
	// M3 (PR-2a codex round-3 review): FetchProject now also holds this
	// lock for its whole get -> git-fetch -> reload -> cache-commit
	// sequence — see that method's own doc comment for the concrete
	// "half-old/half-new Project object" race this closes against a
	// concurrent DeleteProject + re-registration at the same WorkDir.
	//
	// A single global lock (rather than sharding by, say, project name or
	// derived id) trades registration/removal throughput for simplicity:
	// these are rare, interactively-invoked operations, not a hot path, so
	// fully serializing them daemon-wide is an acceptable cost for a
	// correctness guarantee that is otherwise fiddly to get right per-key.
	projectMu sync.Mutex
}

// hydrateProject fills project.Meta from the cached raw meta. Use this when
// the caller only needs project-yaml-level fields (e.g. Name for ref
// matching) and the extra workspace.yaml read would be wasted.
func (s *ProjectAppService) hydrateProject(project *orchestrator.Project) *orchestrator.Project {
	if project == nil {
		return nil
	}
	if meta, ok := s.Meta.Get(project.ID); ok {
		project.Meta = *meta
	}
	return s.applyStatus(project)
}

// applyStatus populates project.Status/StatusMessage from the in-memory
// ProjectStore health snapshot (docs/plans/volume-only-daemon.md §論点a).
// Every read path (hydrateProject and hydrateProjectWithWorkspace's success
// branch) funnels through this so `boid project list`/`show` — and any API
// client — can see a degraded project without the daemon ever having
// deleted its row.
func (s *ProjectAppService) applyStatus(project *orchestrator.Project) *orchestrator.Project {
	if project == nil {
		return nil
	}
	st := s.Meta.Status(project.ID)
	if st.State != orchestrator.StatusReady {
		project.Status = st.State
		project.StatusMessage = st.Message
	} else {
		project.Status = ""
		project.StatusMessage = ""
	}
	return project
}

// removeCachedMetaUnlessRegistered rolls back a Meta cache write made by a
// registration attempt that then failed — but only when no project row
// actually owns that id.
//
// s.Meta caches by project.yaml's `id:`, not by the directory it was read
// from, so an attempt to register a repo whose id is ALREADY registered
// overwrites the established project's cache entry on the way in. An
// unconditional Remove on the failure path then deletes that established
// project's meta, leaving it with a valid DB row and no cached meta:
// `boid project list` prints an empty name, ref-by-name lookup stops
// resolving, and task_behaviors read as empty until the next
// `boid project reload`. Observed on the real daemon (2026-07-28) from
// `boid project add <already-registered url> --workspace=<other slug>` —
// re-adding the same repo cannot be caught by the pre-flight same-path 409
// check when the path differs, and --name cannot avoid the collision because
// the id comes from the committed project.yaml.
//
// projectMu (see its own doc comment) serializes concurrent registrations,
// which is the only half of this class its "impossible by construction"
// claim actually covers: in the sequential case the winning cache write
// landed in a long-since-finished call, so there is no window to exclude.
//
// The discriminator is a row lookup rather than "did this call write the
// entry": whoever owns the row owns the cached meta, so an id backed by a
// live row must keep whatever meta that row's project loaded. Callers that
// have already deleted their own row (the workspace-assign rollback in
// CreateProjectFromGitURL) are correct to call s.Meta.Remove directly.
func (s *ProjectAppService) removeCachedMetaUnlessRegistered(id string) {
	if existing, err := s.Projects.GetProject(id); err == nil && existing != nil {
		return
	}
	s.Meta.Remove(id)
}

// hydrateProjectWithWorkspace fills project.Meta with workspace-aware
// hydration when a Hydrator is wired; otherwise it falls back to the bare
// cache. Use this for paths that build sandbox specs (session / exec /
// hook), where workspace Capabilities / Env / SecretNamespace must take
// effect.
func (s *ProjectAppService) hydrateProjectWithWorkspace(ctx context.Context, project *orchestrator.Project) *orchestrator.Project {
	if project == nil {
		return nil
	}
	if s.Hydrator != nil {
		meta, err := s.Hydrator.GetWithWorkspace(ctx, project.ID)
		if err == nil && meta != nil {
			project.Meta = *meta
			return s.applyStatus(project)
		}
		// Fall back to bare meta on hydration error so the API stays usable
		// even when workspace.yaml is malformed. The hydrator already logs.
		slog.Warn("project meta hydration failed; falling back to raw meta",
			"project_id", project.ID, "error", err)
	}
	return s.hydrateProject(project)
}

func (s *ProjectAppService) CreateProject(workDir string) (*orchestrator.Project, error) {
	// MAJOR 1 (PR-2a codex round-2 review): serialize this whole
	// load -> DB-insert -> default-workspace-assign pipeline against
	// CreateProjectFromGitURL and DeleteProject's own critical sections —
	// see projectMu's doc comment for the exact cache-corruption /
	// dangling-registration race this closes.
	s.projectMu.Lock()
	defer s.projectMu.Unlock()

	meta, err := s.Meta.Load(workDir)
	if err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	project := &orchestrator.Project{
		ID:      meta.ID,
		WorkDir: workDir,
	}

	// Capture the project's git origin remote as upstream_url (SSH → HTTPS
	// normalized). docs/plans/git-gateway-cutover.md PR2 makes this mandatory:
	// a project with no remote cannot be registered ("新しい意味論"). Reject
	// with a clear remediation message rather than silently registering a
	// project that will need a remote before it can ever dispatch under the
	// eventual gateway cutover.
	if s.CaptureUpstreamURL != nil {
		upstreamURL, err := s.CaptureUpstreamURL(workDir)
		if err != nil {
			s.removeCachedMetaUnlessRegistered(meta.ID)
			return nil, &StatusError{
				Code: http.StatusBadRequest,
				Message: fmt.Sprintf(
					"project %q has no git remote configured (%v); add one (e.g. `git remote add origin <url>`) and re-run `boid project add`",
					meta.ID, err,
				),
			}
		}
		project.UpstreamURL = upstreamURL
	}

	if err := s.Projects.CreateProject(project); err != nil {
		s.removeCachedMetaUnlessRegistered(meta.ID)
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	// Assign to the default workspace eagerly so the project is always
	// linked. The caller may overwrite this with `boid workspace assign`
	// (or the --workspace flag on `project add`) immediately after. A
	// failure here is non-fatal: daemon startup runs the same migration
	// idempotently, so the project will be linked on next boot.
	//
	// MAJOR 1 (codex review round 3): this cache write races the same way
	// every other workspace-cache writer does — see the mu field's doc
	// comment — so it must run under the same mutex as
	// SetProjectWorkspace/CreateWorkspace/UpdateWorkspace/RemoveWorkspace/
	// ReloadProjects.
	s.mu.Lock()
	if err := s.Projects.SetProjectWorkspace(project.ID, orchestrator.DefaultWorkspaceSlug); err != nil {
		slog.Warn("CreateProject: default workspace assignment failed (project will be linked on next daemon start)",
			"project_id", project.ID, "error", err)
	} else {
		project.WorkspaceID = orchestrator.DefaultWorkspaceSlug
		s.Meta.SetWorkspaceID(project.ID, orchestrator.DefaultWorkspaceSlug)
	}
	s.mu.Unlock()

	project.Meta = *meta
	return project, nil
}

// CreateProjectFromGitURL registers a project from a git remote URL
// (docs/plans/volume-only-daemon.md §論点a: `boid project add <git-url>
// --workspace=<name> [--name=<project-name>]`, the CLI form's server-side
// counterpart). Unlike CreateProject (still used by `boid project init`'s
// dir-based flow — see cmd/project.go's own doc comment on why that path is
// left alone for this PR), workspaceSlug is REQUIRED, not eagerly defaulted:
// there is no host-filesystem project.yaml sitting around to "just register
// as-is" here, so an unassigned bare clone would be an orphaned volume
// directory with no way back to it.
//
// This follows the plan doc's §論点a unresolved-point recommendation
// ("Failure means the register itself fails, no half-added project"):
// clone, project.yaml load, and DB/workspace-assignment all happen
// synchronously in this one call, and any failure rolls back everything
// this call itself created (the bare repo directory, the in-memory Meta
// cache entry, the DB row) so a failed `project add` never leaves a
// half-registered project behind. This is a DIFFERENT case from the
// on-startup-auto-prune-retirement invariant (docs/plans/
// volume-only-daemon.md §論点a) that governs already-registered projects
// going degraded later (FetchProject, daemon startup) — a project that
// never successfully finished registering has no DB row to preserve in the
// first place.
func (s *ProjectAppService) CreateProjectFromGitURL(ctx context.Context, gitURL, workspaceSlug, nameOverride string) (*orchestrator.Project, error) {
	gitURL = strings.TrimSpace(gitURL)
	if gitURL == "" {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "url is required"}
	}
	// Normalize to HTTPS (or leave a file:// URL as-is — see
	// dispatcher.NormalizeOriginURL's own doc comment) BEFORE deriving the
	// name / cloning / storing UpstreamURL, not just at storage time: the
	// daemon container has no SSH agent/keys, so an scp-like or ssh://
	// argument MUST become an https:// URL for dispatcher.CloneBareRepo's
	// forge-credential injection (HTTPS Basic auth via gitgateway) to have
	// any chance of authenticating — cloning the literal ssh:// form the
	// user typed would silently fall back to SSH transport and fail with a
	// confusing auth error instead of this clear one. A URL that cannot be
	// normalized at all (unrecognized form) is rejected outright rather
	// than attempted.
	normalizedURL, normErr := dispatcher.NormalizeOriginURL(gitURL)
	if normErr != nil {
		// BLOCKER 1 sibling (PR-2a codex round-2 review): the raw gitURL
		// argument is exactly what a caller submitting
		// "https://user:SECRET@host/org/repo.git" (or any of the other
		// credential-embedding forms NormalizeOriginURL's own error message
		// deliberately never echoes) supplied verbatim — interpolating it
		// via %q here, unconditionally, used to defeat that carefulness by
		// putting the credential right back into this 400's message (and
		// from there, the CLI/API caller's terminal output and any log that
		// captures it). SanitizeURLForLogging redacts every recognized
		// credential shape before it is ever displayed.
		return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("unrecognized or credential-embedded git URL %q: %v", dispatcher.SanitizeURLForLogging(gitURL), normErr)}
	}
	gitURL = normalizedURL
	if strings.TrimSpace(workspaceSlug) == "" {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "workspace is required (git-URL registration has no host directory to eagerly default into the default workspace the way `boid project init` does)"}
	}
	if err := orchestrator.ValidWorkspaceSlug(workspaceSlug); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if s.CloneBareRepo == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "git clone not wired"}
	}
	if s.DataDir == "" {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "daemon data directory not wired"}
	}

	name := strings.TrimSpace(nameOverride)
	nameWasExplicit := name != ""
	if name == "" {
		derived, err := orchestrator.DeriveProjectNameFromURL(gitURL)
		if err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
		}
		name = derived
	}

	// Refuse before any write (PR-1b/1d learning: a refuse must land BEFORE
	// any DB/filesystem mutation, never after a partial write) when the
	// target workspace does not exist — `boid workspace create <slug>`
	// first, same contract SetProjectWorkspace's AssignWorkspaceIfExists
	// already enforces for the legacy flow. DefaultWorkspaceSlug always
	// exists (EnsureDefault at startup), so this only ever blocks a typo'd
	// or not-yet-created custom slug.
	exists, err := s.Projects.WorkspaceExists(workspaceSlug)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if !exists {
		return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("workspace %q not found; create it first with `boid workspace create %s`", workspaceSlug, workspaceSlug)}
	}

	// BLOCKER 2 (codex round-1 review): SafeBareRepoPath validates name as a
	// safe single path segment (rejecting a traversal shape like
	// "../../../../tmp/owned") AND verifies the resulting join still
	// resolves beneath <data_dir>/repos/<workspaceSlug> before this method
	// ever touches disk or the DB — see that function's own doc comment.
	bareRepoPath, err := orchestrator.SafeBareRepoPath(s.DataDir, workspaceSlug, name)
	if err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	// MAJOR 3 (codex round-1 review): serialize the whole clone -> load ->
	// DB-insert -> workspace-assign pipeline below — see the projectMu
	// field's doc comment for the concrete cache-corruption race this
	// closes. Held for the rest of this method (through every return path).
	s.projectMu.Lock()
	defer s.projectMu.Unlock()

	if _, statErr := os.Stat(bareRepoPath); statErr == nil {
		return nil, &StatusError{Code: http.StatusConflict, Message: fmt.Sprintf("a project is already registered at %s; remove it first (`boid project rm`) or choose a different --name", bareRepoPath)}
	}

	if err := s.CloneBareRepo(ctx, gitURL, bareRepoPath, workspaceSlug); err != nil {
		return nil, &StatusError{Code: http.StatusBadGateway, Message: fmt.Sprintf("clone %q: %v", gitURL, err)}
	}

	meta, err := s.Meta.LoadBareRepo(bareRepoPath)
	// noYAMLMeta is non-nil exactly when EITHER of the two id-synthesis
	// fallbacks below fired — the wholly-missing-project.yaml path (PR5,
	// 論点b/c/d) or the present-but-id-less-project.yaml path (PR7, 論点h
	// 案1). In both cases its cache write is deliberately deferred until
	// AFTER CreateProject succeeds (see the call site below), unlike the
	// ordinary LoadBareRepo success path (which writes the cache as a side
	// effect of the read itself, before CreateProject is even attempted).
	// Writing it here instead would overwrite an ALREADY-REGISTERED
	// project's cache entry the moment this call's derived id collides with
	// one (Codex review, PR5 round 1 P1): removeCachedMetaUnlessRegistered's
	// failure-path check only stops the entry from being REMOVED afterward,
	// it does not restore whatever this call clobbered on the way in, and
	// SetSynthesizedMeta additionally clears any degraded status that
	// established project may have been carrying.
	var noYAMLMeta *orchestrator.ProjectMeta
	if err != nil {
		// docs/plans/workspace-default-project.md PR5 (論点b/c/d): a
		// project.yaml that was simply never committed
		// (GitHeadReadFailurePathAbsent) falls back to the workspace's
		// default project definition instead of failing registration — every
		// OTHER failure kind (HeadUnresolved / Other: HEAD unresolved, a
		// corrupt repo, or a project.yaml that IS present but fails to
		// parse/validate) still fails the whole registration exactly as
		// before, per the synchronous-validation contract in this method's
		// doc comment.
		var headErr *orchestrator.GitHeadReadError
		if errors.As(err, &headErr) && headErr.Kind == orchestrator.GitHeadReadFailurePathAbsent {
			synthesized, synthErr := s.synthesizeNoYAMLProjectMeta(gitURL, name, workspaceSlug, nameWasExplicit)
			if synthErr != nil {
				if rmErr := os.RemoveAll(bareRepoPath); rmErr != nil {
					slog.Warn("CreateProjectFromGitURL: failed to roll back bare repo after workspace-default fallback rejection",
						"path", bareRepoPath, "error", rmErr)
				}
				return nil, synthErr
			}
			noYAMLMeta = synthesized
			meta = synthesized
		} else {
			// Synchronous-validation contract (see this method's doc comment):
			// a project.yaml that fails to load at add time fails the whole
			// registration, not a half-added degraded row. Roll back the clone.
			if rmErr := os.RemoveAll(bareRepoPath); rmErr != nil {
				slog.Warn("CreateProjectFromGitURL: failed to roll back bare repo after project.yaml load failure",
					"path", bareRepoPath, "error", rmErr)
			}
			return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("load project.yaml from %s (%s): %v", gitURL, bareRepoPath, err)}
		}
	} else if meta.ID == "" {
		// docs/plans/workspace-default-project.md 論点h 案1 (PR7):
		// project.yaml IS present and parsed fine, it simply omits `id:`.
		// Unlike the wholly-missing-project.yaml case above, every other
		// field the file declared (task_behaviors, etc.) is kept as-is —
		// only the id is synthesized, via the exact same URL-derivation
		// deriveProjectIDFromURL already uses for the no-project.yaml path.
		// s.Meta.LoadBareRepo deliberately left this meta uncached (its own
		// doc comment) precisely so this id can be assigned before the
		// cache is ever touched.
		derivedID, derr := deriveProjectIDFromURL(gitURL)
		if derr != nil {
			if rmErr := os.RemoveAll(bareRepoPath); rmErr != nil {
				slog.Warn("CreateProjectFromGitURL: failed to roll back bare repo after id-less project.yaml derivation failure",
					"path", bareRepoPath, "error", rmErr)
			}
			return nil, &StatusError{Code: http.StatusBadRequest, Message: derr.Error()}
		}
		meta.ID = derivedID
		noYAMLMeta = meta
	}

	project := &orchestrator.Project{
		ID:          meta.ID,
		WorkDir:     bareRepoPath,
		UpstreamURL: gitURL,
	}

	if err := s.Projects.CreateProject(project); err != nil {
		// Not an unconditional Remove: the insert most plausibly failed
		// because this id is ALREADY registered, and the cache entry then
		// belongs to that established project, not to this call. See
		// removeCachedMetaUnlessRegistered's doc comment.
		s.removeCachedMetaUnlessRegistered(meta.ID)
		// bareRepoPath stays an unconditional removal: it is this call's own
		// freshly cloned directory (the pre-flight check above already 409s
		// when something is registered at this exact path), so it is never
		// the established project's repo even when the ids collide.
		if rmErr := os.RemoveAll(bareRepoPath); rmErr != nil {
			slog.Warn("CreateProjectFromGitURL: failed to roll back bare repo after DB insert failure",
				"path", bareRepoPath, "error", rmErr)
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	// Either id-synthesis fallback's meta (no-YAML or id-less-YAML) is
	// cached only now, having just confirmed via the successful insert
	// above that this id is NOT already owned by another registered
	// project (see noYAMLMeta's own doc comment above for the collision
	// this ordering avoids).
	if noYAMLMeta != nil {
		s.Meta.SetSynthesizedMeta(noYAMLMeta.ID, noYAMLMeta)
	}

	// MAJOR 1 (codex review round 3, mirrors CreateProject above): this
	// cache write races every other workspace-cache writer the same way —
	// see the mu field's doc comment. projectMu (held for this whole
	// method) and mu are deliberately different locks — see projectMu's
	// doc comment — so nesting mu inside projectMu here carries no
	// deadlock risk (nothing holds mu before trying to acquire projectMu).
	//
	// MAJOR 2 (PR-2a codex round-2 review): AssignWorkspaceIfExists, not the
	// plain SetProjectWorkspace this used to call — workspaceSlug's
	// existence was already checked once, ABOVE, before the (potentially
	// slow) clone; SetProjectWorkspace performs no existence check of its
	// own, so a `boid workspace rm <slug>` landing anywhere in that window
	// would previously let this line commit a project_workspaces row
	// pointing at a slug with no corresponding workspaces row (no FK
	// enforces this at the DB level) — a successful `project add` silently
	// leaving a dangling assignment behind. AssignWorkspaceIfExists
	// re-checks atomically, in the same transaction as the assign itself,
	// right here at commit time.
	s.mu.Lock()
	if err := s.Projects.AssignWorkspaceIfExists(project.ID, workspaceSlug); err != nil {
		s.mu.Unlock()
		// Unlike CreateProject's eager default-workspace assign (best-effort,
		// non-fatal — see that method's comment), workspaceSlug is a REQUIRED
		// input here, already validated to exist above; a failure at this
		// point is unexpected enough (a DELETE landing in the window between
		// the existence check and here) that leaving a workspace-less
		// project row around would violate this method's whole "workspace
		// required" contract. Roll back everything.
		//
		// M1 (PR-2a codex round-3 review): the bare repo is removed BEFORE
		// the DB row and Meta cache entry, not after — mirroring
		// DeleteProject's own MAJOR 3 fix (codex round-2) and for the exact
		// same reason. The pre-fix order deleted the DB row FIRST and only
		// logged an os.RemoveAll failure as a warning, so a permission- or
		// I/O-failing bare repo directory was orphaned on disk with no row
		// left to retry cleanup through, and re-adding the same URL/name
		// then 409'd forever (this method's own pre-flight "already
		// registered at this path" check). Removing the bare repo first
		// means a removal failure now fails the WHOLE call with the DB row
		// (and cached Meta) intact — the operator fixes the underlying
		// issue and retries `boid project rm` to finish cleanup.
		if rmErr := os.RemoveAll(bareRepoPath); rmErr != nil {
			slog.Error("CreateProjectFromGitURL: workspace assignment failed AND bare-repo rollback removal also failed; leaving the project row and its cached meta in place so the bare repo can still be removed on retry",
				"project_id", project.ID, "workspace", workspaceSlug, "path", bareRepoPath, "assign_error", err, "remove_error", rmErr)
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf(
				"assign workspace %q failed (%v) and automatic bare-repo rollback also failed (%v); project %q was left registered — fix the underlying issue and retry `boid project rm %s` to finish cleanup, or `boid project show %s` to inspect its state",
				workspaceSlug, err, rmErr, project.ID, project.ID, project.ID,
			)}
		}
		// The bare repo is gone; the DB row and cached Meta are now safe to
		// remove too. If DeleteProject itself fails here (MAJOR 6, codex
		// round-1 review — e.g. the DB is unreachable), the row survives
		// pointing at a bareRepoPath that no longer exists on disk; that is
		// still strictly better than the pre-fix "row gone, directory
		// orphaned, nothing left to retry through" state, since
		// `boid project rm` remains available to finish the job.
		if delErr := s.Projects.DeleteProject(project.ID); delErr != nil {
			slog.Error("CreateProjectFromGitURL: workspace assignment failed; bare repo was rolled back but DB row deletion also failed; retry `boid project rm` to finish removing the dangling row",
				"project_id", project.ID, "workspace", workspaceSlug, "assign_error", err, "delete_error", delErr)
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf(
				"assign workspace %q failed (%v) and DB row cleanup also failed (%v); the bare repo has already been removed — retry `boid project rm %s` to finish removing the dangling row",
				workspaceSlug, err, delErr, project.ID,
			)}
		}
		s.Meta.Remove(meta.ID)
		if errors.Is(err, os.ErrNotExist) {
			return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf(
				"workspace %q was removed while this registration was in progress; retry `boid project add` once the workspace exists again", workspaceSlug,
			)}
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("assign workspace %q: %v", workspaceSlug, err)}
	}
	project.WorkspaceID = workspaceSlug
	s.Meta.SetWorkspaceID(project.ID, workspaceSlug)
	s.mu.Unlock()

	project.Meta = *meta
	return s.applyStatus(project), nil
}

// FetchProject runs `git fetch --all` inside id's bare repo and reloads
// project.yaml (`boid project fetch <id>`, docs/plans/volume-only-daemon.md
// §論点b fetch 経路). Unlike CreateProjectFromGitURL, a failure here does
// NOT roll anything back or delete the project — id is already a registered
// project the on-startup-auto-prune-retirement invariant (§論点a) protects;
// a fetch or reload failure marks it StatusDegraded (via s.Meta.MarkDegraded)
// and returns an error, leaving the DB row and any previously-loaded Meta
// exactly as they were.
func (s *ProjectAppService) FetchProject(ctx context.Context, id string) (*orchestrator.Project, error) {
	// M3 (PR-2a codex round-3 review): serialize this whole
	// get -> filesystem-fetch -> reload -> cache-commit sequence against
	// CreateProject/CreateProjectFromGitURL/DeleteProject's own critical
	// sections — see projectMu's doc comment. Round-2 only widened projectMu
	// to those three create/delete entry points, leaving FetchProject able
	// to read id's DB row here, then have a concurrent DeleteProject +
	// re-registration at the SAME WorkDir land before this method's own
	// fetch/reload resumed — producing a returned Project that mixes id's
	// OLD DB fields (read before the race) with the NEW bare repo's
	// metadata (loaded after it). FetchProject never calls any of
	// CreateProject/CreateProjectFromGitURL/DeleteProject (nor do they call
	// it), so this widening carries no new deadlock risk.
	s.projectMu.Lock()
	defer s.projectMu.Unlock()

	project, err := s.Projects.GetProject(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	if s.FetchBareRepo == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "git fetch not wired"}
	}
	// MAJOR 2 (codex round-1 review): classify legacy-vs-managed by whether
	// WorkDir falls under this daemon's own bare-repo storage root
	// (isManagedBareRepoPath), NOT by orchestrator.IsBareRepoDir alone.
	// IsBareRepoDir requires a HEALTHY HEAD file AND objects/ directory; if
	// either disappears after registration (disk corruption, an operator
	// accidentally deleting the wrong thing), a managed project would
	// otherwise misclassify as a legacy host-dir project right here and
	// return a plain 400 with no MarkDegraded call — leaving `boid project
	// list`/`show` reporting a stale "ready" status forever, since nothing
	// ever recorded the failure. A WorkDir under the reposRoot IS this
	// daemon's own bare repo, corrupt or not; routing it through
	// FetchBareRepo's ordinary failure path below (which DOES MarkDegraded)
	// instead of this legacy-rejection message fixes that.
	if !s.isManagedBareRepoPath(project.WorkDir) {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("project %q is not a git-URL registered project (legacy host-dir project; re-add via `boid project add <git-url> --workspace=<name>`)", id)}
	}

	if err := s.FetchBareRepo(ctx, project.WorkDir, project.WorkspaceID); err != nil {
		msg := fmt.Sprintf("fetch failed: %v", err)
		s.Meta.MarkDegraded(id, msg)
		return nil, &StatusError{Code: http.StatusBadGateway, Message: msg}
	}

	// M2 (PR-2a codex round-3 review): LoadBareRepoExpectingID, not the
	// plain LoadBareRepo this used to call — see that method's own doc
	// comment. The freshly-read meta is only committed to the shared cache
	// once its `id:` is confirmed to match id (the DB row this fetch was
	// invoked against); a mismatch below is guaranteed to have left every
	// existing cache entry, including one already registered under the
	// drifted id, completely untouched.
	meta, err := s.Meta.LoadBareRepoExpectingID(project.WorkDir, id, project.UpstreamURL)
	if err != nil {
		// docs/plans/workspace-default-project.md PR5, §現状の実測5: a
		// project.yaml that was never committed (GitHeadReadFailurePathAbsent)
		// must not degrade a project registered via the no-project.yaml path
		// — it falls back to a synthesized meta (workspace default is applied
		// dynamically at GetWithWorkspace hydrate time, same as every other
		// reload of this kind) instead of MarkDegraded. Every other failure
		// kind (HeadUnresolved / Other) keeps the existing degrade-on-failure
		// behavior unchanged.
		//
		// Gated on id being VERIFIED url-derived (Codex review, PR5 round 2
		// Major, tightened by round-4 Major fix — PR7) — see that constant's
		// own doc comment: without this gate, an ORDINARY project.yaml-
		// bearing project whose file was deleted upstream by mistake would
		// also match GitHeadReadFailurePathAbsent and get silently switched
		// to the workspace default with no degraded signal, instead of
		// degrading as it always has. Checking id's PREFIX alone is not
		// enough either (round-4 finding): a hand-authored `id: url-custom`
		// that was committed at some point, then had its project.yaml
		// deleted upstream, would also match a bare prefix check — confirm
		// id actually equals deriveProjectIDFromURL(project.UpstreamURL),
		// the same verification orchestrator.ProjectStore applies for a
		// still-present project.yaml's id: drift (LoadAll/
		// reconcileExpectedProjectID).
		var headErr *orchestrator.GitHeadReadError
		derivedID, derivedErr := deriveProjectIDFromURL(project.UpstreamURL)
		isVerifiedURLDerivedID := strings.HasPrefix(id, orchestrator.URLDerivedProjectIDPrefix) && derivedErr == nil && derivedID == id
		if errors.As(err, &headErr) && headErr.Kind == orchestrator.GitHeadReadFailurePathAbsent && isVerifiedURLDerivedID {
			// Recovery order mirrors orchestrator.ProjectStore's
			// synthesizeMetaForReload (Codex review, PR5 round 1 P1): the
			// cached Name first, else project.WorkDir's own basename (the
			// bare-repo directory name IS the exact name — explicit --name or
			// derived — CreateProjectFromGitURL used at registration, so this
			// reproduces an explicit --name a bare UpstreamURL re-derivation
			// never could), else UpstreamURL re-derivation as a last resort.
			name := ""
			nameSource := ""
			if cached, ok := s.Meta.Get(id); ok && cached.Name != "" {
				name = cached.Name
				nameSource = "cached"
			} else if project.WorkDir != "" {
				name = strings.TrimSuffix(filepath.Base(project.WorkDir), ".git")
				nameSource = "basename"
			} else if derived, derr := orchestrator.DeriveProjectNameFromURL(project.UpstreamURL); derr == nil {
				name = derived
				nameSource = "url"
			}
			synthesized := &orchestrator.ProjectMeta{ID: id, Name: name, NameSource: nameSource}
			s.Meta.SetSynthesizedMeta(id, synthesized)
			project.Meta = *synthesized
			return s.hydrateProjectWithWorkspace(ctx, project), nil
		}
		msg := fmt.Sprintf("project.yaml load failed after fetch: %v", err)
		s.Meta.MarkDegraded(id, msg)
		return nil, &StatusError{Code: http.StatusBadRequest, Message: msg}
	}

	// MAJOR 4 (PR-2a codex round-2 review): refuse when project.yaml's own
	// `id:` no longer matches id, the DB row this fetch was invoked against.
	// LoadAll hits the exact same mismatch on every subsequent daemon
	// restart (see that method's own comment) — the two call sites share
	// this failure mode by construction, since both key off
	// LoadExpectingID/LoadBareRepoExpectingID's return value the same way.
	if meta.ID != id {
		msg := fmt.Sprintf(
			"project.yaml id changed upstream from %q to %q; re-register manually (`boid project rm %s` then `boid project add <git-url> --workspace=%s`) rather than continue under a stale id",
			id, meta.ID, id, project.WorkspaceID,
		)
		s.Meta.MarkDegraded(id, msg)
		// M2 fix: LoadBareRepoExpectingID never cached this mismatched meta
		// under its own (drifted) id in the first place — unlike before this
		// fix, there is no phantom cache entry to remove here, and no risk
		// of clobbering an unrelated already-registered project whose id
		// happens to equal meta.ID.
		return nil, &StatusError{Code: http.StatusConflict, Message: msg}
	}

	project.Meta = *meta
	return s.hydrateProjectWithWorkspace(ctx, project), nil
}

// isManagedBareRepoPath reports whether workDir falls under this daemon's
// bare-repo storage root (<DataDir>/repos) — i.e. it is (or was, before
// possibly going corrupt) a git-URL-registered project's daemon-managed
// bare repo, as opposed to a legacy host-filesystem checkout registered via
// `boid project init` / the dir-based form of `boid project add` (MAJOR 2,
// codex round-1 review). This is a structural (path-prefix) check,
// independent of whether the directory currently looks like a valid bare
// repo — orchestrator.IsBareRepoDir answers a DIFFERENT question ("is this
// readable right now") that FetchProject also needs, but must not use for
// THIS classification (see FetchProject's own call-site comment for the
// misclassification that produces).
//
// Falls back to the plain orchestrator.IsBareRepoDir structural check when
// s.DataDir is unset (some tests never wire it) — weaker (misses a
// corrupt/missing bare repo, same as the pre-fix behavior) but still
// correct for a healthy one, and DataDir is always wired in production
// (internal/server/wire.go).
//
// BLOCKER 2 sibling (PR-2a codex round-2 review): routes through
// orchestrator.PathIsUnderResolved, not the plain (lexical-only)
// PathIsUnder round-1 left here — this is the ONE classification both
// FetchProject (fetch-time) and DeleteProject (rm-time, which follows this
// answer straight into an os.RemoveAll) share, so fixing it here closes the
// symlinked-ancestor escape for both call sites at once. A resolution
// error (as opposed to "not under root") fails CLOSED: refuse to classify
// workDir as daemon-managed rather than risk a write or delete against a
// path that could not be safely verified. FetchProject then falls through
// to the ordinary legacy-rejection message; DeleteProject skips the
// RemoveAll and only removes the DB row, exactly as it already does for any
// WorkDir that was never daemon-managed at all.
// WithProjectLock runs fn while holding projectMu — the exact same lock
// CreateProject/CreateProjectFromGitURL/DeleteProject/FetchProject already
// serialize their own create/delete/fetch critical sections against (see
// projectMu's own doc comment). Exported so a caller OUTSIDE this package
// can share the identical serialization contract without duplicating it —
// specifically, dispatcher.Runner.Dispatch's own project-registry-guarded
// dispatch section (docs/plans/volume-only-daemon.md §論点b, PR-2b):
// gateway-token registration, the selfProject lookup, the gatewayCloneURL/
// peerAdvertise snapshot, managed-bare-repo classification, and the
// FetchBareRepo + PrepareJobCheckout pair all read/derive from a project
// snapshot and then clone straight off its WorkDir. Round-2 (PR834 PR-2b
// round-2 codex review Major 1) wrapped only the final FetchBareRepo +
// PrepareJobCheckout call in this lock, leaving the lookup and URL/token
// snapshot that FEED it unguarded; round-3 (Major 1) widened Dispatch's own
// closure to cover the whole sequence, because a concurrent `project rm` +
// re-add at the identical managed path could otherwise land in the window
// between dispatch's snapshot and the (locked) clone, producing a mixed
// checkout (one project's gateway URL/credentials against a DIFFERENT,
// just-re-registered project's bare-repo content) — see Dispatch's own
// "project-registry-guarded dispatch section" comment for the full
// rationale. internal/dispatcher cannot import this package directly
// (internal/api already imports internal/dispatcher for wiring — the
// reverse would be an import cycle), so internal/server/wire.go closes over
// this exact method and hands it to Runner.WithProjectLock as a plain
// function value instead — see that field's own doc comment and wire.go's
// assignment (next to runner.GatewayCredentials, which shares the same
// gwCreds this package's own FetchBareRepo closure was wired from).
//
// A caller with no error path of its own can ignore the returned error by
// wrapping a niladic function; every current caller already threads one
// through.
func (s *ProjectAppService) WithProjectLock(fn func() error) error {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	return fn()
}

func (s *ProjectAppService) isManagedBareRepoPath(workDir string) bool {
	if s.DataDir == "" {
		return orchestrator.IsBareRepoDir(workDir)
	}
	underRoot, err := orchestrator.PathIsUnderResolved(orchestrator.ReposRoot(s.DataDir), workDir)
	if err != nil {
		slog.Warn("isManagedBareRepoPath: symlink resolution failed; treating as NOT daemon-managed (fail closed)",
			"work_dir", workDir, "error", err)
		return false
	}
	return underRoot
}

func (s *ProjectAppService) ListProjects(workspaceID string) ([]*orchestrator.Project, error) {
	projects, err := s.Projects.ListProjects()
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	var result []*orchestrator.Project
	for _, project := range projects {
		s.hydrateProject(project)
		if workspaceID != "" && project.WorkspaceID != workspaceID {
			continue
		}
		result = append(result, project)
	}
	if result == nil {
		result = []*orchestrator.Project{}
	}
	return result, nil
}

func (s *ProjectAppService) GetProject(id string) (*orchestrator.Project, error) {
	project, err := s.Projects.GetProject(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	// GET /api/projects/{id} is the canonical "give me a sandbox-ready
	// snapshot" endpoint — `cmd/exec.go` and any future client builds a
	// SessionJobInput straight from the returned Meta. Hydrate with
	// workspace so Capabilities / Env / SecretNamespace are populated.
	return s.hydrateProjectWithWorkspace(context.Background(), project), nil
}

// ExplainProject handles GET /api/projects/{id}/explain (`boid project show
// --explain`, docs/plans/workspace-default-project.md 論点e, PR6). Confirms
// id is a registered project (same 404 shape as GetProject) before
// delegating the actual field-provenance computation to
// orchestrator.ProjectStore.Explain.
func (s *ProjectAppService) ExplainProject(id string) (*orchestrator.ProjectExplain, error) {
	if _, err := s.Projects.GetProject(id); err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	explain, err := s.Meta.Explain(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	return explain, nil
}

func (s *ProjectAppService) SetProjectWorkspace(id, workspaceID string) (*orchestrator.Project, error) {
	// API-layer validation per plan (3-layer defense). Empty string clears the
	// assignment and is handled at the repository layer; non-empty values must
	// satisfy ValidWorkspaceSlug or we return 400 Bad Request early.
	if workspaceID != "" {
		if err := orchestrator.ValidWorkspaceSlug(workspaceID); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
		}
	}
	project, err := s.Projects.GetProject(id)
	if err != nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	// MAJOR 3 (codex review): serialize the assign+cache-sync critical
	// section against RemoveWorkspace's own snapshot-then-cache-sync
	// sequence — see the mu field's doc comment and RemoveWorkspace's for
	// the exact race this closes.
	s.mu.Lock()
	defer s.mu.Unlock()

	// MAJOR 5 (codex review), folded into MAJOR 3's atomic fix: reject
	// assignment to a slug with no corresponding workspaces row. Before
	// MAJOR 5, assigning a project to a nonexistent slug (typo, or a
	// workspace removed out from under it) silently left a dangling
	// project_workspaces reference: dispatch then runs in a permanently
	// degraded window (GetWithWorkspace logs "workspace.yaml not found" on
	// every call), and — because MigrateWorkspaceYAMLToDB only re-validates
	// project->workspace references once (state=committed skips every
	// subsequent run) — restarting the daemon never self-heals it.
	// MAJOR 5's original fix ran the existence check (WorkspaceExists) and
	// the assign (SetProjectWorkspace) as two separate statements, leaving
	// a window where a concurrent DELETE could land in between and still
	// produce the same dangling reference. AssignWorkspaceIfExists folds
	// both into a single DB transaction so that window no longer exists.
	// DefaultWorkspaceSlug (and "" — clearing) are exempt from the
	// existence check inside AssignWorkspaceIfExists itself, mirroring the
	// previous inline exemptions here.
	//
	// PR3 had to revert this check (e2e regression: no create path
	// existed yet outside the migration, so a freshly-dropped
	// workspace yaml with no DB row 404'd on assign). PR4 reinstates
	// it now that POST /api/workspaces exists — cmd/workspace.go's
	// `boid workspace assign` auto-creates a DB row from the legacy
	// yaml (if present) before calling this endpoint, so the existing
	// "drop a yaml, then assign" flow keeps working.
	if err := s.Projects.AssignWorkspaceIfExists(id, workspaceID); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("workspace %q not found", workspaceID)}
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	// Propagate the workspace association into the in-memory ProjectStore so
	// the next GetWithWorkspace call sees the new value (otherwise the stale
	// empty workspaceID stays cached until daemon restart).
	s.Meta.SetWorkspaceID(id, workspaceID)
	project.WorkspaceID = workspaceID
	return s.hydrateProject(project), nil
}

func (s *ProjectAppService) ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error) {
	workspaces, err := s.Projects.ListWorkspaces()
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if workspaces == nil {
		workspaces = []*orchestrator.WorkspaceSummary{}
	}
	return workspaces, nil
}

// CreateWorkspace inserts a brand-new workspace at slug
// (docs/plans/workspace-db-consolidation.md PR4 Step C, POST
// /api/workspaces). See the ProjectService interface doc comment for the
// status code contract.
func (s *ProjectAppService) CreateWorkspace(slug string, meta *orchestrator.WorkspaceMeta) (*WorkspaceDetail, error) {
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if s.Workspaces == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "workspace store not wired"}
	}

	// MAJOR 1 (codex review round 2): serialize this whole create against
	// UpdateWorkspace/RemoveWorkspace/SetProjectWorkspace's own critical
	// sections — see the mu field's doc comment for the exact race this
	// closes.
	s.mu.Lock()
	defer s.mu.Unlock()

	// MAJOR 2 (codex review): validate every meta.HostCommands reference
	// against the daemon's live aggregated snapshot. See the HostCommands
	// field's doc comment for why an unknown name must be rejected here
	// rather than silently persisted.
	//
	// Phase 2.5 PR7 (docs/plans/workspace-db-consolidation.md, decision 12):
	// this used to also call orchestrator.MaterializeWorkspaceKitsForPersist
	// here to expand a legacy Kits list the wire body might still carry.
	// WorkspaceMeta.Kits no longer exists and workspaceMetaStrict no longer
	// accepts a `kits:` key at all, so no wire body can carry one any
	// more — a caller that still sends `kits:` now gets a 400 straight out
	// of the strict decoder, before this method ever runs. The one
	// remaining legacy-kits caller (cmd/workspace.go's
	// ensureWorkspaceExistsForAssign, `boid workspace assign`'s auto-create
	// convenience path) resolves it client-side before submitting an
	// already-materialized body.
	if err := s.validateHostCommandRefs(meta.HostCommands); err != nil {
		return nil, err
	}
	if err := s.Workspaces.Create(slug, meta); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, &StatusError{Code: http.StatusConflict, Message: fmt.Sprintf("workspace %q already exists", slug)}
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return s.buildWorkspaceDetail(slug, meta)
}

// GetWorkspace returns slug's meta, revision, and assigned project ids
// (docs/plans/workspace-db-consolidation.md PR4 Step D, GET
// /api/workspaces/{slug}).
//
// MAJOR 1 (codex review): meta and revision are read from a single atomic
// snapshot (Workspaces.LoadWithRevision) rather than two separate queries
// (a plain Load for meta, then GetWorkspaceSummary for revision) — the
// pre-fix two-query approach could straddle a concurrent PUT and return a
// meta/revision pair that never coexisted in the DB.
func (s *ProjectAppService) GetWorkspace(slug string) (*WorkspaceDetail, error) {
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if s.Workspaces == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "workspace store not wired"}
	}
	meta, revision, err := s.Workspaces.LoadWithRevision(slug)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("workspace %q not found", slug)}
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return s.buildWorkspaceDetailFromRevision(slug, meta, revision)
}

// UpdateWorkspace replaces slug's meta wholesale (docs/plans/
// workspace-db-consolidation.md PR4 Step E, PUT /api/workspaces/{slug}),
// enforcing optimistic concurrency: ifMatch must equal the workspace's
// current revision unless force is true (decision 17 — PUT + If-Match, no
// PATCH). A missing ifMatch (and !force) is rejected with 428 Precondition
// Required rather than silently proceeding, since accepting an unconditional
// PUT by default would make the ETag check opt-in instead of the intended
// opt-out (--force).
//
// MAJOR 1 (codex review): the non-force path no longer reads the current
// revision and then Saves unconditionally as two separate statements — that
// window let two concurrent PUTs starting from the same If-Match both pass
// their check before either had written, silently losing one writer's
// update, and let a DELETE landing between a GET and a subsequent PUT
// resurrect the workspace via Save's upsert semantics. Instead,
// Workspaces.UpdateIfRevisionMatches performs the check-and-write as one
// atomic UPDATE; matched=false is then disambiguated into 404 (slug is
// truly gone) vs 412 (slug exists, but ifMatch is stale) with a follow-up
// existence check — that follow-up races nothing of consequence, since it
// only picks the HTTP status code for an already-failed write, not any data
// mutation.
//
// force=true intentionally keeps the old "check existence, then
// unconditional Save" shape: force explicitly opts out of the revision
// guarantee (last-write-wins), so there is no CAS invariant left to protect
// — but Save is upsert-on-conflict, so a plain Save would silently *create*
// slug if it did not already exist, which PUT (unlike POST) must not do;
// hence the explicit existence check before it.
func (s *ProjectAppService) UpdateWorkspace(slug string, meta *orchestrator.WorkspaceMeta, ifMatch string, force bool) (*WorkspaceDetail, error) {
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if s.Workspaces == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "workspace store not wired"}
	}
	if !force && ifMatch == "" {
		return nil, &StatusError{Code: http.StatusPreconditionRequired, Message: "If-Match header is required (or pass ?force=true)"}
	}

	// MAJOR 1 (codex review round 2): serialize the whole update — including
	// the force path's Load-then-Save pair below — against
	// CreateWorkspace/RemoveWorkspace/SetProjectWorkspace's own critical
	// sections. Without this, a DELETE could land between the force path's
	// existence check (Load) and its unconditional Save, and Save's
	// upsert-on-conflict semantics would silently resurrect the
	// just-deleted workspace. See the mu field's doc comment for the full
	// scenario and TestUpdateWorkspace_ForcePathBlocksMidDelete for the
	// regression test.
	s.mu.Lock()
	defer s.mu.Unlock()

	// MAJOR 2 (codex review): same validation as CreateWorkspace — see that
	// call site's comment (and its Phase 2.5 PR7 update: no legacy Kits
	// expansion happens here any more, since the wire body can no longer
	// carry a `kits:` key at all).
	if err := s.validateHostCommandRefs(meta.HostCommands); err != nil {
		return nil, err
	}

	if force {
		if _, err := s.Workspaces.Load(slug); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("workspace %q not found", slug)}
			}
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		if err := s.Workspaces.Save(slug, meta); err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		return s.buildWorkspaceDetail(slug, meta)
	}

	newRevision, matched, err := s.Workspaces.UpdateIfRevisionMatches(slug, ifMatch, meta)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if !matched {
		if _, existErr := s.Workspaces.Load(slug); existErr != nil {
			if errors.Is(existErr, os.ErrNotExist) {
				return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("workspace %q not found", slug)}
			}
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: existErr.Error()}
		}
		return nil, &StatusError{Code: http.StatusPreconditionFailed, Message: fmt.Sprintf("revision mismatch: If-Match %q does not match the current revision", ifMatch)}
	}
	return s.buildWorkspaceDetailFromRevision(slug, meta, newRevision)
}

// validateHostCommandRefs rejects a meta.HostCommands reference that does
// not exist in the daemon's live aggregated host_commands snapshot (MAJOR 2,
// codex review). No-op (nil) when s.HostCommands is unset (test opt-out,
// same convention as CaptureUpstreamURL/Hydrator) or when refs is empty.
func (s *ProjectAppService) validateHostCommandRefs(refs []string) error {
	if s.HostCommands == nil || len(refs) == 0 {
		return nil
	}
	snapshot := s.HostCommands()
	var unknown []string
	for _, name := range refs {
		if _, ok := snapshot[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return &StatusError{
		Code:    http.StatusBadRequest,
		Message: fmt.Sprintf("unknown host_commands reference(s): %s (run `boid host-commands list` to see known names)", strings.Join(unknown, ", ")),
	}
}

// RemoveWorkspace deletes slug (docs/plans/workspace-db-consolidation.md PR4
// Step F, DELETE /api/workspaces/{slug}). The reserved default workspace
// cannot be removed (decision 8); any project still assigned to slug is
// re-pointed at default by orchestrator.WorkspaceRepository.Remove's
// transaction, so this never leaves a dangling project_workspaces reference
// at the DB layer.
//
// That DB-level transaction has no way to reach into the daemon's in-memory
// ProjectStore cache (s.Meta), though — so without the snapshot-then-sync
// dance below, every project that *was* assigned to slug would keep serving
// GetWithWorkspace hydration off a stale cached workspace_id that no longer
// has a corresponding workspaces row, silently degrading (host_commands/env/
// capabilities not injected, same as the pre-existing "workspace.yaml not
// found" degraded-mode path) until the next daemon restart. Snapshotting the
// assigned project ids *before* the delete (list ordering matters: after the
// delete, ListWorkspaces/ListProjects would already report them as
// belonging to default and we'd have nothing left to distinguish "was
// reassigned, needs a cache sync" from "already was on default") then
// calling s.Meta.SetWorkspaceID for each afterward mirrors the exact same
// cache update SetProjectWorkspace already performs for a single explicit
// assignment.
//
// MAJOR 3 (codex review): the whole snapshot -> DB remove -> cache-sync
// sequence below runs under s.mu (see that field's doc comment), serialized
// against SetProjectWorkspace's own critical section. Without this, a
// concurrent SetProjectWorkspace reassigning one of the snapshotted
// projects to a *different*, still-existing workspace could have its cache
// write clobbered by this method's stale-snapshot loop landing after it —
// leaving the in-memory cache pointing at "default" while the DB (and any
// fresh GetWithWorkspace hydration) says the project belongs to the new
// workspace.
//
// # The init.sh goes with the row, inside the same critical section
//
// PR9 codex round 3, Major 3. The script's deletion started life as a second
// service call made by WorkspaceHandler.Remove after this one returned, which
// left a window with the mutex free: a `workspace apply` could upsert the same
// slug, write a new init.sh and answer 200 in it, and the removal — still in
// flight — then deleted that script. The end state was a workspace row that
// existed with a committed apply's script silently missing, indistinguishable
// from a workspace that never had one.
//
// Doing it here makes the two atomic with respect to every other
// workspace-mutating entry point, because they all take this mutex: an apply
// is now either entirely before the removal (its row and its script both go —
// correct) or entirely after it (a fresh workspace keeps the script it was
// just given — correct). The HOME volume's deletion stays outside, in the
// handler: it needs an engine this type does not have, and it is excluded from
// the migration it actually races by a different mechanism
// (BeginWorkspaceHomeRemoval, which brackets this call too).
//
// It is still NOT atomic with the row — a file unlink and a database
// transaction cannot commit together — so the outcome is REPORTED rather than
// pretended away, which is why this returns a WorkspaceRemoval instead of
// folding a failed unlink into the error. See WorkspaceRemoval.InitScriptError.
func (s *ProjectAppService) RemoveWorkspace(slug string) (*WorkspaceRemoval, error) {
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if slug == orchestrator.DefaultWorkspaceSlug {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("workspace %q is reserved and cannot be removed", slug)}
	}
	if s.Workspaces == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "workspace store not wired"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	assigned, err := s.ListProjects(slug)
	if err != nil {
		return nil, err
	}

	if err := s.Workspaces.Remove(slug); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("workspace %q not found", slug)}
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	for _, p := range assigned {
		s.Meta.SetWorkspaceID(p.ID, orchestrator.DefaultWorkspaceSlug)
	}

	// AFTER the row is gone, still holding the mutex. After, because a failed
	// unlink must not be able to abort a row removal that has already
	// committed; still holding, because that is the whole of the fix above.
	removal := &WorkspaceRemoval{}
	removal.InitScriptDeleted, removal.InitScriptError = deleteWorkspaceInitScript(s.InitScripts, slug)
	return removal, nil
}

// buildWorkspaceDetail assembles the WorkspaceDetail response for callers
// that do not already know the fresh revision after their write
// (CreateWorkspace, and UpdateWorkspace's force path) — Create/Save do not
// hand back the new revision, so a follow-up GetWorkspaceSummary read is
// unavoidable here. Callers that already have a fresh revision in hand
// (GetWorkspace's LoadWithRevision, UpdateWorkspace's non-force
// UpdateIfRevisionMatches) should use buildWorkspaceDetailFromRevision
// instead and skip this redundant round trip — see MAJOR 1's doc comments
// on those methods for why avoiding it matters.
func (s *ProjectAppService) buildWorkspaceDetail(slug string, meta *orchestrator.WorkspaceMeta) (*WorkspaceDetail, error) {
	summary, err := s.Projects.GetWorkspaceSummary(slug)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return s.buildWorkspaceDetailFromRevision(slug, meta, summary.Revision)
}

// buildWorkspaceDetailFromRevision assembles a WorkspaceDetail from a
// meta/revision pair the caller already has (no GetWorkspaceSummary round
// trip). ProjectCount is derived from len(AssignedProjects) rather than a
// separate count query — see WorkspaceDetail.ProjectCount's doc comment
// ("mirrors len(AssignedProjects)").
func (s *ProjectAppService) buildWorkspaceDetailFromRevision(slug string, meta *orchestrator.WorkspaceMeta, revision string) (*WorkspaceDetail, error) {
	projects, err := s.ListProjects(slug)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		ids = append(ids, p.ID)
	}
	return &WorkspaceDetail{
		Slug:             slug,
		Meta:             meta,
		Revision:         revision,
		ProjectCount:     len(ids),
		AssignedProjects: ids,
	}, nil
}

// ApplyWorkspace upserts apply's workspace metadata and (when
// apply.FieldsPresent["projects"] is true) reconciles its project
// assignments atomically, in a single DB transaction (docs/plans/
// volume-only-daemon.md PR-1d codex round-1 Blocker 2, POST
// /api/workspaces/apply).
//
// Two things happen here BEFORE the transaction opens, deliberately outside
// orchestrator.ApplyWorkspaceEnvelope's own DB tx: validateHostCommandRefs
// (needs the daemon's live in-memory host_commands snapshot, not a DB
// table) and resolveWorkspaceApplyProjectNames (needs the in-memory
// ProjectMeta cache to turn a project NAME into an ID — see that method's
// doc comment). Neither runs conditionally on dryRun (MAJOR 1, codex
// round-1): a dry run must hit the exact same validation a real apply
// would, not silently skip it and report success on a document real apply
// would go on to reject.
func (s *ProjectAppService) ApplyWorkspace(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error) {
	slug := apply.Envelope.Metadata.Name
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if s.WorkspacesConn == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "workspace db connection not wired"}
	}

	if apply.FieldsPresent["host_commands"] {
		if err := s.validateHostCommandRefs(apply.Envelope.Spec.HostCommands); err != nil {
			return nil, err
		}
	}
	// spec.init_script's content check belongs HERE, with the other
	// pre-transaction validations, even though the script itself is written
	// after the commit — see validateWorkspaceApplyInitScript for why the two
	// halves are split (PR9 of
	// docs/plans/workspace-home-volume-persistence.md, 論点 d).
	if err := validateWorkspaceApplyInitScript(apply); err != nil {
		return nil, err
	}

	var projectIDsByName map[string]string
	var missing []string
	if apply.FieldsPresent["projects"] {
		var resolveErr error
		projectIDsByName, missing, resolveErr = s.resolveWorkspaceApplyProjectNames(apply.Envelope.Spec.Projects)
		if resolveErr != nil {
			// PR-1d codex round-2 Blocker 2: refuse the WHOLE apply before
			// any DB write when a name is ambiguous — this return happens
			// before s.mu.Lock()/ApplyWorkspaceEnvelope below, so nothing
			// has been touched yet (no side effects to unwind).
			return nil, resolveErr
		}
	}

	// Same critical-section discipline as Create/Update/Remove/Import — see
	// the mu field's doc comment: apply is just as much a workspace- (and
	// now also project-assignment-) mutating entry point as those.
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := orchestrator.ApplyWorkspaceEnvelope(s.WorkspacesConn, apply, projectIDsByName, dryRun)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	result.MissingProjects = missing

	if !dryRun {
		// Propagate the new assignments into the in-memory ProjectStore cache
		// (same convention as SetProjectWorkspace/RemoveWorkspace above) so
		// the next GetWithWorkspace call sees them without a daemon restart.
		for _, id := range result.AttachedProjects {
			s.Meta.SetWorkspaceID(id, slug)
		}
		for _, id := range result.DetachedProjects {
			s.Meta.SetWorkspaceID(id, orchestrator.DefaultWorkspaceSlug)
		}
	}

	// spec.init_script, LAST and outside the transaction (PR9 of
	// docs/plans/workspace-home-volume-persistence.md, 論点 d): it is a file on
	// the daemon, not a row. See applyWorkspaceInitScript for why this order
	// rather than the other one, and for what a failure here means — the
	// metadata is already committed at this point, so the error says so
	// instead of implying the whole apply was rejected.
	action, warning, err := s.applyWorkspaceInitScript(apply, dryRun)
	if err != nil {
		return nil, fmt.Errorf(
			"workspace %q: metadata and project assignments were applied, but spec.init_script was not: %w", slug, err)
	}
	result.InitScriptAction = action
	if warning != "" {
		// Both channels, the same way WorkspaceHandler.Apply handles a dropped
		// spec.additional_bindings: the response field is what a direct HTTP
		// caller sees, and the log is what an operator finds afterwards when a
		// workspace turns out not to have the script its document specified.
		// `boid workspace apply` prints neither (see its own comment on why it
		// does not echo result.Warnings) — this warning describes a daemon
		// that is missing wiring, not a problem with the document.
		slog.Warn("workspace apply: spec.init_script could not be applied", "workspace", slug, "reason", warning)
		result.Warnings = append(result.Warnings, warning)
	}
	return result, nil
}

// resolveWorkspaceApplyProjectNames resolves each spec.projects[].name entry
// to a registered project ID via resolveProjectByNameExact — STRICT
// name-only matching, deliberately NOT ResolveProjectRef's flexible
// id-or-name-or-substring resolution (PR-1d codex round-3 Blocker; see
// resolveProjectByNameExact's own doc comment for the concrete destructive
// scenario ID-priority matching created here). A name that resolves to NO
// project is reported in missing rather than failing the whole apply (the
// plan doc's cross-PR coordination note: a not-yet-registered project must
// never fail apply).
//
// A name that resolves to MORE THAN ONE project is a different case
// entirely and is NOT folded into missing (PR-1d codex round-2 Blocker 2):
// two registered projects sharing a project.yaml name (e.g. both "api")
// previously made every lookup for that name ambiguous, which the old
// `err != nil || len(matches) != 1` fold treated identically to "not
// registered" — the target set ended up empty and BOTH projects were
// destructively detached to the default workspace, even though both were
// explicitly listed in the applied document. Instead, an ambiguous name
// aborts resolveWorkspaceApplyProjectNames immediately with a StatusError
// naming every colliding project ID, and the caller (ApplyWorkspace) must
// refuse the entire apply before touching the DB rather than proceed with
// a wrong/partial resolution.
//
// PR-1d codex round-5 Major: every check and lookup below reads from ONE
// snapshotRegisteredProjects() call, captured up front. The pre-fix version
// called s.Projects.ListProjects()+hydrate ONCE for
// refuseIfAnyRegisteredProjectNameUnavailable's availability check, then
// AGAIN — separately, once per spec.projects[] entry — inside
// resolveProjectByNameExact. Those were two (or more) independently timed
// reads of live, mutable state (s.Meta's in-memory cache), with s.mu not
// held across any of it. A concurrent ReloadProjects landing in the window
// between the check and a later per-name lookup could remove a project's
// cached Meta.Name AFTER the check already verified every name was
// available, and the stale per-name lookup would then report that project
// "missing" — the exact "cannot verify" -> "does not exist" mistranslation
// the round-4 guard exists to prevent, just reopened one level down by the
// TWO snapshots not agreeing. A single up-front snapshot, reused for both
// the check and every lookup, makes that disagreement structurally
// impossible: there is no second read in which the picture could differ.
func (s *ProjectAppService) resolveWorkspaceApplyProjectNames(projects []orchestrator.WorkspaceEnvelopeProject) (ids map[string]string, missing []string, err error) {
	snapshot, err := s.snapshotRegisteredProjects()
	if err != nil {
		return nil, nil, err
	}

	// PR-1d codex round-4 Blocker: refuse the whole apply up front when the
	// registered-project name set is incomplete — see
	// refuseIfAnyRegisteredProjectNameUnavailable's doc comment for why an
	// unavailable name must never be allowed to fall through to
	// resolveProjectByNameExact's ordinary "no match" path below. Reads the
	// SAME snapshot the per-name lookups below use (round-5 Major, see this
	// function's own doc comment).
	if err := refuseIfAnyRegisteredProjectNameUnavailable(snapshot); err != nil {
		return nil, nil, err
	}

	ids = make(map[string]string, len(projects))
	for _, ep := range projects {
		// Defense in depth (PR-1d codex round-2 Major): decodeWorkspaceEnvelopeSpec
		// already rejects an empty spec.projects[].name at parse time, but an
		// empty name must never be looked up regardless — refuse it here too
		// rather than relying solely on the decode-time guard holding for
		// every caller forever.
		if strings.TrimSpace(ep.Name) == "" {
			missing = append(missing, ep.Name)
			continue
		}
		matches := resolveProjectByNameExact(snapshot, ep.Name)
		if len(matches) == 0 {
			missing = append(missing, ep.Name)
			continue
		}
		if len(matches) > 1 {
			matchIDs := make([]string, len(matches))
			for i, m := range matches {
				matchIDs[i] = m.ID
			}
			return nil, nil, &StatusError{
				Code: http.StatusConflict,
				Message: fmt.Sprintf(
					"project name %q is ambiguous: it matches %d registered projects (%s) — apply refused to avoid detaching the wrong one(s); give these projects distinct project.yaml name: values, or drop spec.projects from this document and manage the assignment via `boid project assign` directly",
					ep.Name, len(matches), strings.Join(matchIDs, ", "),
				),
			}
		}
		ids[ep.Name] = matches[0].ID
	}
	return ids, missing, nil
}

// snapshotRegisteredProjects reads every registered project via a SINGLE
// s.Projects.ListProjects() call and hydrates each with Meta from the
// in-memory ProjectMeta cache — the one and only DB/cache read
// resolveWorkspaceApplyProjectNames performs (PR-1d codex round-5 Major).
// A repository failure here (e.g. a transient 500) aborts the whole apply
// before any DB write, matching round-3 Major's "any resolution failure
// that is not a genuine 404 must abort, never fold into missing" discipline
// — there is no per-name repository call left to fail separately.
func (s *ProjectAppService) snapshotRegisteredProjects() ([]*orchestrator.Project, error) {
	projects, err := s.Projects.ListProjects()
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	for _, p := range projects {
		s.hydrateProject(p)
	}
	return projects, nil
}

// refuseIfAnyRegisteredProjectNameUnavailable refuses to proceed when the
// registered-project name set is incomplete: some project's project.yaml
// could not be hydrated (removed from disk, unreadable, or malformed),
// leaving ProjectStore.LoadAll's cached Meta.Name empty while that
// project's DB row AND any existing workspace assignment are left
// untouched (PR-1d codex round-4 Blocker). snapshot is the caller's single
// snapshotRegisteredProjects() read (PR-1d codex round-5 Major) — this
// function performs no lookup of its own.
//
// Without this check, resolveProjectByNameExact's exact-match loop simply
// never matches this project — an empty Meta.Name can never equal a
// document's non-empty spec.projects[].name — which
// resolveWorkspaceApplyProjectNames then folds into "missing" the exact
// same way it folds a genuinely-never-registered name. The caller then
// proceeds to reconcile project assignments as if this project does not
// exist at all, silently detaching it from wherever it is currently
// attached even though the document may have explicitly listed it (under
// its real, but currently unreadable, name). This is the apply-side half
// of the same destructive scenario ExportWorkspaceEnvelopes'/
// projectCollisions' own empty-name refusal closes on the export side:
// "cannot verify this project's name right now" must never be translated
// into "this project doesn't exist" on either side of the export/apply
// round trip.
//
// Deliberately refuses the WHOLE apply rather than only when the
// unavailable project happens to be named in this specific document: a
// degraded project's real name is, by definition, unknown here, so there
// is no way to tell whether the document's silence about it is intentional
// (a genuinely unrelated project) or accidental (the document DOES mean
// this project, under its real name, which just cannot be verified) — the
// only safe response to "I cannot tell" is to refuse before touching the
// DB, exactly as resolveWorkspaceApplyProjectNames' ambiguous-name and
// non-404-error cases already do.
func refuseIfAnyRegisteredProjectNameUnavailable(snapshot []*orchestrator.Project) error {
	for _, p := range snapshot {
		if p.Meta.Name == "" {
			return &StatusError{
				Code: http.StatusConflict,
				Message: fmt.Sprintf(
					"project %q in the workspace's target has no authoritative name; cannot resolve safely — its project.yaml is unavailable, fix or unregister it before applying",
					p.ID,
				),
			}
		}
	}
	return nil
}

// resolveProjectByNameExact resolves name to every project in snapshot whose
// project.yaml name matches EXACTLY, using ONLY name matching — deliberately
// not ResolveProjectRef's id-priority-then-name-exact-then-name-substring
// behavior (PR-1d codex round-3 Blocker). snapshot is the caller's single
// snapshotRegisteredProjects() read (PR-1d codex round-5 Major) — this
// function performs no lookup of its own, so calling it once per
// spec.projects[] entry costs no extra DB/cache round trip and, more
// importantly, cannot observe a different picture than a sibling call made
// moments earlier against the same snapshot.
//
// spec.projects[].name is defined by the Workspace envelope schema
// (docs/plans/volume-only-daemon.md) as a project.yaml `name:` value, never
// a project ID — resolving it through ResolveProjectRef's flexible
// id-or-name convenience matching (built for CLI/API ref arguments, where a
// user reasonably wants to type either) let a project whose NAME happens to
// equal a DIFFERENT project's ID hijack that other project's identity via
// ResolveProjectRef's id-match-first precedence. Concrete failure (codex
// round-3 review): project A (id=proj-a, name=proj-b) and project B
// (id=proj-b) both registered; a document listing `name: proj-b` for A
// instead resolved to B via ResolveProjectRef's ID-exact-match step (which
// runs BEFORE any name check), attaching B and destructively detaching A —
// the exact opposite of what the document specified. Substring matching is
// excluded too, not just ID priority: apply must be deterministic from the
// document alone, never fuzzy CLI convenience matching some unrelated
// project by partial name.
//
// Returns nil when no project in snapshot has this exact name — the caller
// folds that into "missing" (PR-1d codex round-3 Major: any OTHER
// resolution failure must abort the whole apply instead; there is no
// longer a per-name repository call here that could fail that way, so the
// only failure mode left is snapshotRegisteredProjects' own, handled once
// by the caller before this function is ever invoked).
func resolveProjectByNameExact(snapshot []*orchestrator.Project, name string) []*orchestrator.Project {
	var matches []*orchestrator.Project
	for _, p := range snapshot {
		if p.Meta.Name == name {
			matches = append(matches, p)
		}
	}
	return matches
}

// ExportWorkspaceEnvelopes returns one or more boid.dev/v1 Workspace yaml
// documents ("---"-separated when more than one), built from a single
// atomic DB snapshot (docs/plans/volume-only-daemon.md PR-1d codex round-1
// Blocker 3, GET /api/workspaces/export?all=true|?name=<slug>). slugs
// nil/empty exports every workspace.
func (s *ProjectAppService) ExportWorkspaceEnvelopes(slugs []string) ([]byte, error) {
	if s.WorkspacesConn == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "workspace db connection not wired"}
	}

	snapshots, allProjects, err := orchestrator.SnapshotWorkspacesForExport(s.WorkspacesConn, slugs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	// PR-1d codex round-2 Blocker 2 (export-side half) / round-3 regression
	// fix: detect (a) a project.yaml name shared by two or more registered
	// projects, and (b) a project.yaml name that collides with a DIFFERENT
	// registered project's ID (round-3 Blocker, see
	// resolveProjectByNameExact's doc comment for the concrete destructive
	// scenario), BEFORE emitting a spec.projects[] entry a later `apply`
	// could not unambiguously (or correctly) resolve back. allProjects comes
	// from the SAME transaction snapshots did (SnapshotWorkspacesForExport's
	// own doc comment) — round-3's regression fix: this used to run a
	// separate, post-snapshot ListProjects() call here, which could describe
	// a different instant than the export snapshot under concurrent project
	// create/delete. Only the Meta.Name hydration itself still comes from
	// the in-memory ProjectMeta cache (not the DB transaction) — a
	// project.yaml name is not a DB column at all.
	//
	// effectiveNames captures each project's Meta.Name ONCE, at this single
	// hydration pass (PR-1d codex round-4 regression note): the emit loop
	// below used to call s.hydrateProject a SECOND time per project (via the
	// now-removed workspaceEnvelopeProjectExportName helper), which re-reads
	// the live in-memory ProjectMeta cache — a concurrent metadata reload
	// between this collision-detection pass and the emit loop could then
	// make the two passes observe different Meta.Name values for the same
	// project (the collision map built from one snapshot, the emitted name
	// from another). Reusing effectiveNames in the loop closes that window:
	// every name emitted is provably the exact same value collision
	// detection already reasoned about.
	duplicateNames, nameCollidesWithOtherID, effectiveNames := s.projectCollisions(allProjects)

	docs := make([][]byte, 0, len(snapshots))
	for _, snap := range snapshots {
		envProjects := make([]orchestrator.WorkspaceEnvelopeProject, 0, len(snap.ProjectIDs))
		for _, id := range snap.ProjectIDs {
			// Blocker 3: p comes from the SAME transaction snap.ProjectIDs
			// did (WorkspaceExportSnapshot.Projects's own doc comment) —
			// the round-1 version instead re-queried each id with a
			// separate, post-transaction GetProject call and silently
			// `continue`d past any error, which could drop an assignment
			// from an otherwise-"consistent" snapshot (e.g. a concurrent
			// delete landing in that window). A missing entry here would
			// mean that guarantee itself broke, so fail loudly rather than
			// silently omit.
			p, ok := snap.Projects[id]
			if !ok || p == nil {
				return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("workspace %q: project %q is assigned but missing from the export snapshot (data inconsistency)", snap.Slug, id)}
			}
			name := effectiveNames[id]
			// PR-1d codex round-4 Blocker: refuse rather than fall back to
			// the work_dir basename when this project's project.yaml could
			// not be hydrated (ProjectStore.LoadAll failure leaves the
			// project's DB row + workspace assignment in place but its
			// cached Meta.Name empty). The old basename fallback made the
			// export "succeed" with a name that could never round-trip back
			// through apply's strict name resolution — reapplying it later
			// would report this project as unresolvable and detach it (or,
			// worse, attach a different project the basename happened to
			// collide with).
			//
			// PR-1d codex round-5 Minor: strings.TrimSpace, not a bare == ""
			// comparison — project.yaml validation now refuses a
			// whitespace-only name at load time (internal/orchestrator/
			// spec_loader.go), but a project registered by an older boid
			// build (before that fix landed) could already have one
			// persisted. A bare == "" check would let such a name through
			// here, emit `name: "  "` in the document, and then have apply's
			// own strings.TrimSpace(ep.Name) == "" check (resolveWorkspace
			// ApplyProjectNames) fold it into "missing" on the way back in —
			// the same export-succeeds/apply-fails round-trip gap the
			// name-is-required fix closes for newly-created projects,
			// defense-in-depth here for ones that predate it.
			if strings.TrimSpace(name) == "" {
				return nil, &StatusError{Code: http.StatusConflict, Message: fmt.Sprintf(
					"project %q has no authoritative name (project.yaml unavailable or whitespace-only); fix or unregister the project before exporting",
					id,
				)}
			}
			if collidingIDs, dup := duplicateNames[name]; dup {
				return nil, &StatusError{Code: http.StatusConflict, Message: fmt.Sprintf(
					"workspace %q: project name %q is ambiguous across %d registered projects (%s) — export refused because a later `apply` could not unambiguously resolve it back; give these projects distinct project.yaml name: values",
					snap.Slug, name, len(collidingIDs), strings.Join(collidingIDs, ", "),
				)}
			}
			// PR-1d codex round-3 Blocker (export-side half): name equals
			// ANOTHER registered project's ID. Applying this export back
			// would resolve `name` via ID-priority matching (if the caller
			// still used flexible resolution) or simply be indistinguishable
			// from that other project's own identity — refuse rather than
			// emit a document that round-trips to the wrong project.
			if collidingIDs, dup := nameCollidesWithOtherID[name]; dup {
				return nil, &StatusError{Code: http.StatusConflict, Message: fmt.Sprintf(
					"workspace %q: project %q's name %q collides with another registered project's id (%s) — export refused because applying this document could attach/detach the wrong project; give %q a project.yaml name: value that does not equal another project's id",
					snap.Slug, id, name, strings.Join(collidingIDs, ", "), id,
				)}
			}
			envProjects = append(envProjects, orchestrator.WorkspaceEnvelopeProject{
				Name: name,
				URL:  p.UpstreamURL,
			})
		}
		// The workspace's init.sh (PR9 of
		// docs/plans/workspace-home-volume-persistence.md, 論点 d). Read here
		// rather than inside SnapshotWorkspacesForExport's transaction because
		// it is a FILE, not a row — there is no snapshot to be part of, and
		// pretending otherwise by threading it through the tx signature would
		// suggest an atomicity that does not exist. The window it opens is
		// small and benign: a script rewritten between the snapshot and this
		// read exports the newer script alongside the older metadata, and both
		// halves are independently valid.
		initScript, err := s.exportWorkspaceInitScript(snap.Slug)
		if err != nil {
			return nil, err
		}
		envelope := orchestrator.NewWorkspaceEnvelopeFromMeta(snap.Slug, snap.Meta, envProjects, initScript)
		// MarshalWorkspaceEnvelope, not yaml.Marshal: it re-parses its own
		// output and refuses to hand back a document that does not read back as
		// the workspace it describes (PR9 codex round 4, Major 1). That is the
		// restorability half of "an export can always be applied"; the size half
		// is checkWorkspaceEnvelopeIsApplicable, below.
		data, err := orchestrator.MarshalWorkspaceEnvelope(envelope)
		if err != nil {
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("marshal workspace %q: %v", snap.Slug, err)}
		}
		if err := checkWorkspaceEnvelopeIsApplicable(snap.Slug, data, len(initScript)); err != nil {
			return nil, err
		}
		docs = append(docs, data)
	}
	return bytes.Join(docs, []byte("---\n")), nil
}

// projectCollisions detects two kinds of export-unsafe project.yaml name
// collision across allProjects (PR-1d codex round-3): every entry is
// hydrated with Meta from the in-memory ProjectMeta cache (a project.yaml
// name is not a DB column), but the SET of projects being compared comes
// entirely from allProjects — the caller's tx-snapshotted
// SnapshotWorkspacesForExport result, not a separate ListProjects() call
// (round-3 regression fix: the pre-fix version's separate post-snapshot
// ListProjects() could describe a different instant than the export
// snapshot under concurrent project create/delete).
//
// byName maps a project.yaml name shared by two or more registered projects
// to every colliding project ID (PR-1d codex round-2 Blocker 2, export-side
// detection).
//
// nameCollidesWithOtherID maps a project.yaml name to the ID(s) of OTHER
// registered projects (never the project whose own name this is) whose id
// equals that name (PR-1d codex round-3 Blocker — see
// resolveProjectByNameExact's doc comment for the concrete destructive
// apply-side scenario this closes on the export side).
//
// effectiveNames maps every project ID in allProjects to its hydrated
// Meta.Name (possibly "" when hydration failed to produce one) — captured
// HERE, at this function's single hydration pass, so a caller can reuse the
// exact same observed name later without a second, independently-timed
// s.hydrateProject call (PR-1d codex round-4 regression note: re-hydrating
// during emit could observe a different in-memory ProjectMeta cache state
// than collision detection did, under a concurrent metadata reload).
func (s *ProjectAppService) projectCollisions(allProjects map[string]*orchestrator.Project) (byName map[string][]string, nameCollidesWithOtherID map[string][]string, effectiveNames map[string]string) {
	for _, p := range allProjects {
		s.hydrateProject(p)
	}

	effectiveNames = make(map[string]string, len(allProjects))
	byNameIDs := map[string][]string{}
	for id, p := range allProjects {
		effectiveNames[id] = p.Meta.Name
		if p.Meta.Name == "" {
			continue
		}
		byNameIDs[p.Meta.Name] = append(byNameIDs[p.Meta.Name], id)
	}

	byName = map[string][]string{}
	for name, ids := range byNameIDs {
		if len(ids) > 1 {
			byName[name] = ids
		}
	}

	nameCollidesWithOtherID = map[string][]string{}
	for name, ids := range byNameIDs {
		other, exists := allProjects[name]
		if !exists {
			continue
		}
		// other.ID == name by construction (allProjects is keyed by ID).
		// Skip when the ONLY project using this name IS other itself (a
		// project whose own Meta.Name equals its own ID is self-consistent
		// — ResolveProjectRef's ID-exact-match step would still find the
		// SAME project either way, so there is nothing to collide with).
		if len(ids) == 1 && ids[0] == other.ID {
			continue
		}
		nameCollidesWithOtherID[name] = []string{other.ID}
	}
	return byName, nameCollidesWithOtherID, effectiveNames
}

// DeleteProject removes id's DB row and, when id is a git-URL-registered
// project, the daemon-managed bare repository backing it (MAJOR 1, codex
// round-1 review). Before that fix, remove deleted only the DB row and
// in-memory Meta cache entry, leaving the bare repo directory behind on
// disk — so a plain `boid project rm` followed by re-adding the SAME
// URL/name always 409'd (CreateProjectFromGitURL's pre-flight "already
// registered at this path" check), making the documented degraded-recovery
// procedure ("rm the project, re-add it") ineffective in practice.
//
// A legacy dir-based project's WorkDir is the user's own host checkout —
// never daemon-owned — so it is deliberately left untouched; only a WorkDir
// under this daemon's bare-repo storage root is removed (same
// isManagedBareRepoPath classification FetchProject's MAJOR 2 fix uses).
//
// MAJOR 3 (PR-2a codex round-2 review): the bare repo is removed BEFORE the
// DB row, not after. The pre-fix order deleted the DB row FIRST and only
// logged an os.RemoveAll failure as a warning — so a read-only or otherwise
// permission-failing repo directory stayed on disk forever (nothing left
// to retry through, since the row that named it was already gone) while
// `boid project rm` still reported SUCCESS, and a subsequent
// `boid project add` of the same URL/name still 409'd with no remaining row
// through which cleanup could be retried. Removing the repo first means a
// removal failure now fails the WHOLE call with the DB row (and cached
// Meta) intact — the exact same "fix the issue, retry `boid project rm`"
// recovery path this method's own MAJOR-1 fix restored for the 409 case
// above.
func (s *ProjectAppService) DeleteProject(id string) error {
	// MAJOR 1 (PR-2a codex round-2 review): serialize this whole
	// get -> [remove bare repo] -> DB-delete -> cache-remove pipeline
	// against CreateProject/CreateProjectFromGitURL's own critical
	// sections — see projectMu's doc comment for the exact
	// dangling-registration race this closes.
	s.projectMu.Lock()
	defer s.projectMu.Unlock()

	project, getErr := s.Projects.GetProject(id)
	if getErr != nil {
		return &StatusError{Code: http.StatusNotFound, Message: getErr.Error()}
	}

	if s.isManagedBareRepoPath(project.WorkDir) {
		if rmErr := os.RemoveAll(project.WorkDir); rmErr != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf(
				"remove daemon-managed bare repo at %s: %v; project %q was NOT removed — fix the underlying issue and retry `boid project rm`",
				project.WorkDir, rmErr, id,
			)}
		}
	}

	if err := s.Projects.DeleteProject(id); err != nil {
		return &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	s.Meta.Remove(id)
	return nil
}

// ResolveProjectRef resolves ref to matching projects with the following priority:
//  1. id exact match (returns immediately on first hit)
//  2. name exact match (all projects with that name)
//  3. name substring match, case-insensitive
//
// Returns a single-element slice on unambiguous match, a multi-element slice on
// ambiguous match, or StatusError{404} when nothing matches.
func (s *ProjectAppService) ResolveProjectRef(ref string) ([]*orchestrator.Project, error) {
	projects, err := s.Projects.ListProjects()
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	// Hydrate all projects so Meta.Name is available for name matching.
	for _, p := range projects {
		s.hydrateProject(p)
	}

	// 1. id exact match — highest priority, return immediately.
	for _, p := range projects {
		if p.ID == ref {
			return []*orchestrator.Project{p}, nil
		}
	}

	// 2. name exact match.
	var nameExact []*orchestrator.Project
	for _, p := range projects {
		if p.Meta.Name == ref {
			nameExact = append(nameExact, p)
		}
	}
	if len(nameExact) > 0 {
		return nameExact, nil
	}

	// 3. name substring match (case-insensitive).
	refLower := strings.ToLower(ref)
	var namePartial []*orchestrator.Project
	for _, p := range projects {
		if strings.Contains(strings.ToLower(p.Meta.Name), refLower) {
			namePartial = append(namePartial, p)
		}
	}
	if len(namePartial) > 0 {
		return namePartial, nil
	}

	return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("no project matches ref %q", ref)}
}

func (s *ProjectAppService) ReloadProjects() (*ProjectReloadResult, error) {
	// MAJOR 1 (codex review round 3, docs/plans/workspace-db-consolidation.md
	// PR4): the ListProjects snapshot and the LoadAll cache write it feeds
	// must run as one critical section under s.mu — see the mu field's doc
	// comment for the exact race this closes (a concurrent SetProjectWorkspace/
	// RemoveWorkspace landing in this window could have its own cache write
	// clobbered by this now-stale snapshot's LoadAll pass landing after it).
	// The subsequent upstream_url recapture loop below is deliberately left
	// outside the lock: it never touches s.Meta's workspace cache, and it can
	// be slow (one CaptureUpstreamURL call per project), so holding s.mu
	// across it would needlessly block every other workspace mutation for no
	// correctness benefit.
	s.mu.Lock()
	projects, err := s.Projects.ListProjects()
	if err != nil {
		s.mu.Unlock()
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	errs := s.Meta.LoadAll(projects)
	s.mu.Unlock()

	var messages []string
	for _, err := range errs {
		messages = append(messages, err.Error())
	}

	// Re-capture upstream_url for every project on reload (docs/plans/
	// git-gateway-cutover.md PR2: "capture タイミング: project add / project
	// reload 時"). This both keeps upstream_url in sync with a project's
	// current origin remote and heals rows left NULL by the startup backfill
	// (e.g. a remote added after registration). Capture failures are logged
	// and surfaced as reload warnings, never fatal to the reload as a whole.
	if s.CaptureUpstreamURL != nil {
		for _, p := range projects {
			upstreamURL, err := s.CaptureUpstreamURL(p.WorkDir)
			if err != nil {
				slog.Warn("project reload: could not capture upstream_url; add a git remote and reload again",
					"project_id", p.ID, "work_dir", p.WorkDir, "error", err)
				messages = append(messages, fmt.Sprintf("project %q: upstream_url not captured: %v", p.ID, err))
				continue
			}
			if upstreamURL == p.UpstreamURL {
				continue
			}
			if err := s.Projects.SetProjectUpstreamURL(p.ID, upstreamURL); err != nil {
				slog.Warn("project reload: failed to persist captured upstream_url",
					"project_id", p.ID, "error", err)
				messages = append(messages, fmt.Sprintf("project %q: failed to persist upstream_url: %v", p.ID, err))
			}
		}
	}

	if len(messages) == 0 {
		return &ProjectReloadResult{Status: "ok"}, nil
	}
	return &ProjectReloadResult{
		Status: "partial",
		Errors: messages,
	}, nil
}
