package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type ProjectAppService struct {
	Projects ProjectRepository
	Meta     interface {
		Load(workDir string) (*orchestrator.ProjectMeta, error)
		Get(id string) (*orchestrator.ProjectMeta, bool)
		Remove(id string)
		LoadAll(projects []*orchestrator.Project) []error
		SetWorkspaceID(projectID, workspaceID string)
	}
	// Hydrator is optional. When set, callers that need fully-resolved meta
	// (workspace-level capabilities / kits / env merged in + SecretNamespace
	// injected) go through GetWithWorkspace instead of the bare Meta.Get path.
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
	// KitsDir is the base directory for installed kits (cfg.KitsDir), used
	// by CreateWorkspace/UpdateWorkspace to materialize a legacy Kits
	// reference before persisting (orchestrator.MaterializeWorkspaceKitsForPersist)
	// — see that function's doc comment for why this step cannot be
	// skipped. Empty is fine when the incoming meta never carries Kits
	// (the common case for anything authored fresh, post-cutover).
	KitsDir string
	// HostCommands, when set, returns the daemon's live aggregated
	// host_commands snapshot (name -> spec). CreateWorkspace/UpdateWorkspace
	// validate every meta.HostCommands reference against this snapshot
	// *after* kit expansion, rejecting an unknown name with 400
	// (docs/plans/workspace-db-consolidation.md MAJOR 2, codex review: an
	// unresolvable reference was previously persisted silently and only
	// warned-about + skipped at dispatch time). Wired in
	// internal/server/wire.go to Server.HostCommands. nil skips the
	// validation entirely — tests that do not exercise it can leave this
	// unset, same convention as CaptureUpstreamURL/Hydrator above.
	HostCommands func() map[string]orchestrator.HostCommandSpec
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
	return project
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
			return project
		}
		// Fall back to bare meta on hydration error so the API stays usable
		// even when workspace.yaml is malformed. The hydrator already logs.
		slog.Warn("project meta hydration failed; falling back to raw meta",
			"project_id", project.ID, "error", err)
	}
	return s.hydrateProject(project)
}

func (s *ProjectAppService) CreateProject(workDir string) (*orchestrator.Project, error) {
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
			s.Meta.Remove(meta.ID)
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
		s.Meta.Remove(meta.ID)
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

	// A body carrying a legacy Kits list (e.g. relayed verbatim by `boid
	// workspace assign`'s auto-create from an old workspace.yaml) must be
	// expanded before it reaches the DB — see
	// MaterializeWorkspaceKitsForPersist's doc comment for why silently
	// skipping this would drop the kit's env/host_commands/bindings for
	// good. No-op when meta.Kits is empty (the common case).
	if err := orchestrator.MaterializeWorkspaceKitsForPersist(s.KitsDir, meta); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("resolve workspace kits: %v", err)}
	}
	// MAJOR 2 (codex review): validate every meta.HostCommands reference
	// against the daemon's live aggregated snapshot *after* kit expansion,
	// so a kit-derived name (just added to meta.HostCommands by
	// MaterializeWorkspaceKitsForPersist above) is covered too. See the
	// HostCommands field's doc comment for why an unknown name must be
	// rejected here rather than silently persisted.
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

	// Same rationale as CreateWorkspace: a Kits-bearing body must be
	// expanded before it reaches the DB, or the reference is silently lost.
	if err := orchestrator.MaterializeWorkspaceKitsForPersist(s.KitsDir, meta); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("resolve workspace kits: %v", err)}
	}
	// MAJOR 2 (codex review): same post-expansion validation as
	// CreateWorkspace — see that call site's comment.
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
func (s *ProjectAppService) RemoveWorkspace(slug string) error {
	if err := orchestrator.ValidWorkspaceSlug(slug); err != nil {
		return &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if slug == orchestrator.DefaultWorkspaceSlug {
		return &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("workspace %q is reserved and cannot be removed", slug)}
	}
	if s.Workspaces == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "workspace store not wired"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	assigned, err := s.ListProjects(slug)
	if err != nil {
		return err
	}

	if err := s.Workspaces.Remove(slug); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("workspace %q not found", slug)}
		}
		return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	for _, p := range assigned {
		s.Meta.SetWorkspaceID(p.ID, orchestrator.DefaultWorkspaceSlug)
	}
	return nil
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

func (s *ProjectAppService) DeleteProject(id string) error {
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
