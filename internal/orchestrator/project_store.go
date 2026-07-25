package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ProjectStore holds project metadata in memory, loaded from project.yaml files.
type ProjectStore struct {
	mu             sync.RWMutex
	metas          map[string]*ProjectMeta
	workspaceIDs   map[string]string // projectID → workspaceID (empty if unlinked)
	workspaceStore *WorkspaceStore
	// hostCommands is the daemon's aggregated host_commands config
	// (docs/plans/workspace-db-consolidation.md host_commands 実定義の集約先):
	// the full HostCommandSpec definitions keyed by name, which
	// WorkspaceMeta.HostCommands (a []string of reference names only) is
	// resolved against at GetWithWorkspace hydration time. Wired via
	// SetHostCommands — see internal/server/wire.go's buildProjectStore.
	hostCommands map[string]HostCommandSpec
	// statuses tracks each project's in-memory health (docs/plans/
	// volume-only-daemon.md §論点a: "status field ('ready'/'degraded' 相当)
	// の in-memory ... 実装"). A project with no entry is implicitly "ready"
	// (StatusReady is the zero value of ProjectStatus.State) — see Status's
	// doc comment. This is intentionally NOT persisted to the DB (the plan
	// doc calls out either choice as invariant-satisfying); it resets to
	// "ready" for every project on daemon restart, which is fine because
	// LoadAll re-derives it on every startup anyway.
	statuses map[string]ProjectStatus
	// reposRoot is <data_dir>/repos (docs/plans/volume-only-daemon.md §論点b
	// layout), set via SetReposRoot. LoadAll uses it purely for a nicer
	// degraded-status message: a project whose WorkDir falls outside this
	// prefix AND fails to load as a bare repo is very likely a pre-cutover
	// project registered against a host filesystem directory (the "移行
	// path" §論点a describes), so it gets pointed at `boid project add
	// <git-url>` instead of a generic parse/read error. Empty (the
	// zero value) just skips that message upgrade — every other
	// invariant-driven behavior (never hard-delete, always mark degraded)
	// holds regardless of whether this was ever configured.
	reposRoot string
}

// ProjectStatus is a project's in-memory health snapshot (docs/plans/
// volume-only-daemon.md §論点a). See Status/MarkDegraded/MarkReady.
type ProjectStatus struct {
	// State is StatusReady or StatusDegraded.
	State string
	// Message explains why State is StatusDegraded (empty for StatusReady).
	Message string
}

const (
	// StatusReady means project.yaml loaded successfully and no
	// fetch/load failure has been observed since (the zero value / default
	// for any project with no statuses entry).
	StatusReady = "ready"
	// StatusDegraded means the project's row is preserved (per the
	// on-startup-auto-prune-retirement invariant, docs/plans/
	// volume-only-daemon.md §論点a) but its project.yaml/bare-repo could not
	// be loaded — see ProjectStatus.Message for why.
	StatusDegraded = "degraded"
)

// NewProjectStore creates a new store.
func NewProjectStore() *ProjectStore {
	return &ProjectStore{
		metas:        make(map[string]*ProjectMeta),
		workspaceIDs: make(map[string]string),
	}
}

// SetReposRoot configures the daemon's bare-repo storage root, used by
// LoadAll to recognize a legacy (pre-volume-only, host-filesystem) project
// registration and upgrade its degraded-status message accordingly. See the
// reposRoot field's doc comment.
func (s *ProjectStore) SetReposRoot(dir string) {
	s.mu.Lock()
	s.reposRoot = dir
	s.mu.Unlock()
}

// MarkDegraded records that projectID's project.yaml/bare-repo could not be
// loaded, without touching its DB row or its (possibly still-cached) Meta —
// this is the "エラー化 (status field で可視化)" half of docs/plans/
// volume-only-daemon.md §論点a's on-startup-auto-prune-retirement invariant.
// Overwrites any prior status for projectID.
func (s *ProjectStore) MarkDegraded(projectID, message string) {
	s.mu.Lock()
	if s.statuses == nil {
		s.statuses = make(map[string]ProjectStatus)
	}
	s.statuses[projectID] = ProjectStatus{State: StatusDegraded, Message: message}
	s.mu.Unlock()
}

// MarkReady clears any degraded status for projectID (back to the implicit
// "ready" default). Called automatically by Load/LoadBareRepo on a
// successful load — most callers never need to call this directly; it is
// exported for the rare caller that needs to clear a status without a full
// reload (none exist yet, kept for symmetry with MarkDegraded).
func (s *ProjectStore) MarkReady(projectID string) {
	s.mu.Lock()
	delete(s.statuses, projectID)
	s.mu.Unlock()
}

// Status returns projectID's current health snapshot. A project with no
// tracked status (never failed to load, or never registered at all) reports
// {State: StatusReady}.
func (s *ProjectStore) Status(projectID string) ProjectStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st, ok := s.statuses[projectID]; ok {
		return st
	}
	return ProjectStatus{State: StatusReady}
}

// SetWorkspaceStore configures the workspace store used by GetWithWorkspace.
// Call this before LoadAll when workspace hydration is desired.
func (s *ProjectStore) SetWorkspaceStore(ws *WorkspaceStore) {
	s.workspaceStore = ws
}

// WorkspaceStore returns the configured workspace store (or nil when none
// has been wired). Exposed so server-side wiring can hand the same store
// to dispatcher.Runner for workspace-scoped proxy allowlist resolution at
// dispatch time, without having to construct a second one.
//
// Note: callers that pass the result into an interface-typed field should
// guard against the typed-nil trap by checking the concrete return for nil
// before assigning — see internal/server/wire.go (resolveDispatcherWorkspaceLookup).
func (s *ProjectStore) WorkspaceStore() *WorkspaceStore {
	return s.workspaceStore
}

// SetHostCommands configures the daemon's aggregated host_commands map
// used to resolve WorkspaceMeta.HostCommands reference names in
// GetWithWorkspace. Call this before dispatch when workspace host_commands
// hydration is desired — symmetric with SetWorkspaceStore.
//
// Guarded by s.mu (docs/plans/workspace-db-consolidation.md PR4 Step G,
// `boid host-commands reload` / POST /api/host_commands/reload): before that
// endpoint existed, this was only ever called once at startup, strictly
// before request-serving began, so an unsynchronized field write/read was
// harmless in practice. A live reload can now race an in-flight
// GetWithWorkspace call reading the same field, so both sides go through
// s.mu — see hostCommandSpec below for the read side.
func (s *ProjectStore) SetHostCommands(hostCommands map[string]HostCommandSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostCommands = hostCommands
}

// hostCommandSpec looks up name in the aggregated host_commands map under
// s.mu (see SetHostCommands' doc comment for why this needs to be
// synchronized now that the map can be swapped live via
// POST /api/host_commands/reload).
func (s *ProjectStore) hostCommandSpec(name string) (HostCommandSpec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	spec, ok := s.hostCommands[name]
	return spec, ok
}

// Load reads project.yaml from the work_dir and stores the meta in memory.
func (s *ProjectStore) Load(workDir string) (*ProjectMeta, error) {
	meta, err := ReadProjectMetaWithKits(workDir)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.metas[meta.ID] = meta
	s.mu.Unlock()
	s.MarkReady(meta.ID)
	return meta, nil
}

// LoadBareRepo reads .boid/project.yaml from a daemon-managed bare git
// repository's HEAD (docs/plans/volume-only-daemon.md §論点a/b) and stores
// the meta in memory — the bare-repo counterpart to Load. Used both by
// ProjectAppService.CreateProjectFromGitURL (first load right after a fresh
// `git clone --bare`) and ProjectAppService.FetchProject (`boid project
// fetch <id>`, reload after `git fetch --all`).
func (s *ProjectStore) LoadBareRepo(bareRepoPath string) (*ProjectMeta, error) {
	meta, err := ReadProjectMetaFromBareRepo(bareRepoPath)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.metas[meta.ID] = meta
	s.mu.Unlock()
	s.MarkReady(meta.ID)
	return meta, nil
}

// Get returns the cached meta for a project.
func (s *ProjectStore) Get(id string) (*ProjectMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	meta, ok := s.metas[id]
	return meta, ok
}

// GetWithWorkspace returns a ProjectMeta hydrated with workspace-level
// capabilities, host_commands, additional_bindings, env, and SecretNamespace
// injection.
//
// Hydration rules:
//   - If the project has no linked workspace, returns the cached meta unchanged.
//   - If linked: always injects meta.SecretNamespace = workspaceID.
//   - On workspace.yaml load success: merges Capabilities, host_commands,
//     additional_bindings, and Env.
//   - On os.ErrNotExist (degraded window): logs a warning, returns meta with
//     only SecretNamespace injected (no error).
//   - On other errors: returns nil and the error.
//
// The kit mechanism (ws.Kits resolved via a KitResolver and merged in here)
// was retired in docs/plans/workspace-db-consolidation.md Phase 2.5 PR6; see
// the NOTE in the body below for what used to live here.
//
// The returned *ProjectMeta is a fresh copy when hydration occurs; callers
// must not mutate the value returned when workspaceID is empty (it is the
// cached pointer).
func (s *ProjectStore) GetWithWorkspace(_ context.Context, projectID string) (*ProjectMeta, error) {
	s.mu.RLock()
	meta, ok := s.metas[projectID]
	workspaceID := s.workspaceIDs[projectID]
	s.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("project %q: meta not loaded", projectID)
	}
	if workspaceID == "" {
		return meta, nil
	}

	// Shallow-clone meta so we can mutate runtime-only fields without
	// corrupting the shared cached copy.
	out := cloneProjectMeta(meta)

	// Always inject SecretNamespace, even in the degraded (workspace.yaml
	// missing) window, so secret routing is stable regardless of disk state.
	out.SecretNamespace = workspaceID

	if s.workspaceStore == nil {
		slog.Warn("workspace store not configured; skipping workspace hydration",
			"project_id", projectID, "workspace_id", workspaceID)
		return out, nil
	}

	ws, err := s.workspaceStore.Load(workspaceID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("workspace.yaml not found; running in degraded mode (capabilities/kits/env not injected)",
				"project_id", projectID, "workspace_id", workspaceID)
			return out, nil
		}
		return nil, fmt.Errorf("project %q: load workspace %q: %w", projectID, workspaceID, err)
	}

	// MAJOR 1 (codex review, 3rd pass): expand ${VAR} placeholders in ws.Env
	// before anything below reads it. The DB/yaml-stored ws value stays raw
	// (see expandWorkspaceRuntimeForDispatch's doc comment) — ws is
	// reassigned to a clone here so every block below (the Env merge)
	// transparently sees expanded values without needing its own expansion
	// step. Mirrors ExpandHostCommandsForDispatch's clone+expand treatment of
	// workspace.HostCommands' resolved definitions.
	ws = expandWorkspaceRuntimeForDispatch(ws)

	// Capabilities: workspace overrides project (e.g. enables docker proxy).
	if ws.Capabilities.Docker != nil {
		out.Capabilities = ws.Capabilities
	}

	// NOTE (docs/plans/workspace-db-consolidation.md Phase 2.5 PR6): this used
	// to be where ws.Kits was resolved (resolveKitRef/ReadKitMeta) and merged
	// into out.HostCommands/AdditionalBindings/Env and every TaskBehavior via
	// MergeKitRuntime/MergeKitMetaIntoBehavior. That whole kit-aggregation
	// path was already dead on the committed/DB-backed path before PR6 (see
	// workspace_migration.go's materializeKitRuntimeIntoWorkspace doc
	// comment: the workspaces table has no Kits column, so a DB-backed
	// WorkspaceMeta always comes back with Kits == nil) — PR6 removed the
	// code itself as part of retiring the kit mechanism. ws.Kits is kept as a
	// vestigial field (dead, never populated by DB-backed rows) until PR7
	// deletes it outright.

	// NOTE (docs/plans/home-workspace-volume.md Phase 4 PR4): this used to be
	// where ws.AdditionalBindings (BLOCKER 2, docs/plans/
	// workspace-db-consolidation.md codex review) was merged into both
	// out.AdditionalBindings and every TaskBehavior.AdditionalBindings — a
	// workspace-level vestige of the kit mechanism (decision 4). That field
	// was retired outright in Phase 4 PR4: the $HOME workspace volume
	// contract (Phase 4 PR1/PR2) removes the need for a workspace to declare
	// extra host bind mounts at all, so there is nothing left for this block
	// to read — WorkspaceMeta has no AdditionalBindings field any more (see
	// its own doc comment). out.AdditionalBindings / every
	// TaskBehavior.AdditionalBindings can still be populated today, but only
	// via project.yaml's own AdditionalBindings (see spec_loader.go's
	// ReadProjectMetaWithKits, which merges meta.AdditionalBindings into each
	// behavior — this workspace-level merge was its only other feed).

	// workspace.HostCommands (docs/plans/workspace-db-consolidation.md PR3
	// cutover) is a []string of reference names into the daemon's
	// aggregated host_commands config (s.hostCommands, wired via
	// SetHostCommands) — unlike ws.Kits above, no path/allow/env/reject
	// definitions travel with the workspace itself. Each resolved name is
	// merged the same way workspace kits are merged above: into the
	// top-level meta.HostCommands (session jobs bypass behaviors and read
	// this directly) and into every TaskBehavior.HostCommands (task hooks
	// read behavior.HostCommands via the planner). Precedence matches the
	// kit block: project.yaml wins on a name conflict. In practice the two
	// paths should never actually conflict on a *different* definition for
	// the same name — the aggregated config is exactly the union of every
	// installed kit's host_commands (see LoadHostCommandsFromKits), so a
	// name resolved here carries the identical HostCommandSpec a workspace
	// kit reference would have produced.
	// MINOR (codex review): the condition used to also require
	// len(s.hostCommands) > 0, which meant that when the daemon's
	// aggregated host_commands map was empty (no kits installed / not yet
	// wired), a workspace.HostCommands reference silently produced neither
	// the "unresolved" warning below nor any error — the whole block was
	// skipped outright. Checking only ws.HostCommands here means every
	// referenced name always gets resolved-or-warned, including the
	// aggregated-empty case.
	if len(ws.HostCommands) > 0 {
		resolved := make(HostCommands, len(ws.HostCommands))
		for _, name := range ws.HostCommands {
			spec, ok := s.hostCommandSpec(name)
			if !ok {
				slog.Warn("workspace host_commands reference unresolved; skipping",
					"project_id", projectID, "workspace_id", workspaceID, "name", name)
				continue
			}
			resolved[name] = spec
		}
		if len(resolved) > 0 {
			out.HostCommands = mergeHostCommands(resolved, out.HostCommands)
			if err := validateBuiltinHostConflict("workspace host_commands", out.HostCommands); err != nil {
				return nil, fmt.Errorf("project %q: %w", projectID, err)
			}
			if err := validateRejectRules(out.HostCommands); err != nil {
				return nil, fmt.Errorf("project %q: workspace host_commands: %w", projectID, err)
			}

			if out.TaskBehaviors == nil {
				out.TaskBehaviors = make(map[string]TaskBehavior)
			}
			out.TaskBehaviors = stripAliasMirrors(out.TaskBehaviors)
			for name, behavior := range out.TaskBehaviors {
				behavior.HostCommands = mergeHostCommands(resolved, behavior.HostCommands)
				out.TaskBehaviors[name] = behavior
			}
			out.TaskBehaviors = addAliasMirrors(out.TaskBehaviors)
		}
	}

	// workspace.Env is applied on top of kit env but below project.yaml env.
	// The merge above (mergeStringMaps(rt.Env, out.Env)) has already placed
	// project env in out.Env; applying workspace env as the new base preserves
	// that precedence: mergeStringMaps(ws.Env, out.Env) → out.Env wins.
	if len(ws.Env) > 0 {
		out.Env = mergeStringMaps(ws.Env, out.Env)
		// Workspace env must also reach each behavior's Env so the planner's
		// PlanHook (which only reads behavior.Env, not meta.Env) picks it up.
		out.TaskBehaviors = stripAliasMirrors(out.TaskBehaviors)
		for name, behavior := range out.TaskBehaviors {
			behavior.Env = mergeStringMaps(ws.Env, behavior.Env)
			out.TaskBehaviors[name] = behavior
		}
		out.TaskBehaviors = addAliasMirrors(out.TaskBehaviors)
	}

	return out, nil
}

// Set stores meta directly.
func (s *ProjectStore) Set(id string, meta *ProjectMeta) {
	s.mu.Lock()
	s.metas[id] = meta
	s.mu.Unlock()
}

// SetWorkspaceID updates the cached workspace association for a project.
// Empty workspaceID clears the association. Subsequent GetWithWorkspace calls
// will hydrate using the new value (or return the cached meta unchanged when
// cleared).
func (s *ProjectStore) SetWorkspaceID(projectID, workspaceID string) {
	s.mu.Lock()
	if workspaceID == "" {
		delete(s.workspaceIDs, projectID)
	} else {
		s.workspaceIDs[projectID] = workspaceID
	}
	s.mu.Unlock()
}

// Remove deletes a project's meta from the store.
func (s *ProjectStore) Remove(id string) {
	s.mu.Lock()
	delete(s.metas, id)
	delete(s.workspaceIDs, id)
	s.mu.Unlock()
}

// LoadAll reads project.yaml for each registered project — dispatching to
// LoadBareRepo for a git-URL registered project (IsBareRepoDir) or Load for
// a filesystem-checkout registered project (the pre-volume-only model,
// still supported for `boid project init` — see docs/plans/
// volume-only-daemon.md §論点a's migration-path note: existing dir-based
// registrations are left alone, not auto-migrated) — and records each
// project's workspaceID so that GetWithWorkspace can hydrate at call time.
//
// Per-project errors are returned in the original order. When the inner
// error is a *ProjectMigrationError, the candidate's project ID is stamped
// onto every Issue in the returned error so downstream callers (e.g. the
// boid start parent picking issues out via errors.As) can drive
// auto-migration without parsing strings — this is the one case
// internal/server/wire.go's buildProjectStore still treats as fail-fast.
// Every OTHER per-project failure (missing dir, YAML parse error, corrupt
// or unreachable bare repo, project.yaml missing from a bare repo's HEAD...)
// is wrapped as *ProjectMissingError and, in addition to being returned
// here, is recorded via MarkDegraded before this method returns — callers
// no longer need a second pass to implement the docs/plans/
// volume-only-daemon.md §論点a auto-prune-retirement invariant themselves;
// LoadAll already leaves every failed candidate in the StatusDegraded state
// with its DB row (and any previously cached Meta) untouched.
func (s *ProjectStore) LoadAll(projects []*Project) []error {
	s.mu.RLock()
	reposRoot := s.reposRoot
	s.mu.RUnlock()

	var errs []error
	for _, candidate := range projects {
		var loadErr error
		if IsBareRepoDir(candidate.WorkDir) {
			_, loadErr = s.LoadBareRepo(candidate.WorkDir)
		} else {
			_, loadErr = s.Load(candidate.WorkDir)
		}
		if loadErr != nil {
			s.Remove(candidate.ID)
			wrapped := wrapPerProjectLoadErr(candidate.ID, candidate.WorkDir, loadErr)
			s.MarkDegraded(candidate.ID, degradedMessageFor(candidate, wrapped, reposRoot))
			errs = append(errs, wrapped)
			continue
		}
		// Record workspace association (empty for unlinked projects).
		s.mu.Lock()
		s.workspaceIDs[candidate.ID] = candidate.WorkspaceID
		s.mu.Unlock()
	}
	return errs
}

// degradedMessageFor upgrades a generic load-failure message to the
// docs/plans/volume-only-daemon.md §論点a migration-path guidance when the
// project's WorkDir is neither a bare repo (LoadAll already ruled that out
// via IsBareRepoDir before calling this) nor under the daemon's own
// bare-repo storage root — i.e. it looks like a pre-cutover, host-filesystem
// project registration. reposRoot empty (never configured — e.g. most
// tests, which do not call SetReposRoot) skips the upgrade entirely; the
// plain wrapped error text is still informative on its own.
func degradedMessageFor(candidate *Project, wrapped error, reposRoot string) string {
	if reposRoot != "" && !pathIsUnder(reposRoot, candidate.WorkDir) {
		return fmt.Sprintf("legacy project registered from host dir %q; re-add via `boid project add <git-url> --workspace=<name>` (%v)", candidate.WorkDir, wrapped)
	}
	return wrapped.Error()
}

// pathIsUnder reports whether path is root itself or a descendant of it.
func pathIsUnder(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// wrapPerProjectLoadErr attaches the project ID to a per-project load
// error. Two classifications:
//   - *ProjectMigrationError: schema migration is needed. Preserved as the
//     typed error with ProjectID filled on each Issue so callers can drive
//     auto-migration via errors.As. This remains the one fail-fast case
//     (internal/server/wire.go's buildProjectStore still refuses daemon
//     startup for it) — everything else below is degrade-not-fail-fast.
//   - everything else: returned as *ProjectMissingError (see that type's
//     doc comment for why the name stayed even though its scope broadened
//     well past "missing") so callers mark the project degraded — per
//     docs/plans/volume-only-daemon.md §論点a's invariant, a missing dir, a
//     YAML parse error, and a corrupt/unreachable bare repo are all handled
//     identically: preserve the DB row, surface the failure via status, and
//     let daemon startup continue.
//
// dir is the project work directory, used to populate
// ProjectMissingError.Dir for diagnostics. It is ignored on the migration
// branch.
func wrapPerProjectLoadErr(projectID, dir string, err error) error {
	var migErr *ProjectMigrationError
	if errors.As(err, &migErr) {
		stamped := &ProjectMigrationError{
			Projects: make([]ProjectMigrationIssue, len(migErr.Projects)),
		}
		for i, p := range migErr.Projects {
			if p.ProjectID == "" {
				p.ProjectID = projectID
			}
			stamped.Projects[i] = p
		}
		return stamped
	}
	return &ProjectMissingError{
		ProjectID: projectID,
		Dir:       dir,
		Err:       err,
	}
}
