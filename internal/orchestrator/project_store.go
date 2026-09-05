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
	// hostCommands is the daemon's aggregated host_commands config: the
	// full HostCommandSpec definitions keyed by name, which
	// WorkspaceMeta.HostCommands (a []string of reference names only) is
	// resolved against at GetWithWorkspace hydration time. Wired via
	// SetHostCommands.
	hostCommands map[string]HostCommandSpec
	// statuses tracks each project's in-memory health. A project with no
	// entry is implicitly "ready" (StatusReady is the zero value of
	// ProjectStatus.State) — see Status's doc comment. Intentionally not
	// persisted to the DB; it resets to "ready" for every project on daemon
	// restart, since LoadAll re-derives it on every startup anyway.
	statuses map[string]ProjectStatus
	// reposRoot is <data_dir>/repos, set via SetReposRoot. LoadAll uses it
	// purely for a nicer degraded-status message: a project whose WorkDir
	// falls outside this prefix AND fails to load as a bare repo is likely a
	// pre-cutover project registered against a host filesystem directory,
	// so it gets pointed at `boid project add <git-url>` instead of a
	// generic parse/read error. Empty (never configured) just skips that
	// message upgrade.
	reposRoot string
	// deriveProjectID, when set via SetDeriveProjectIDFunc, recomputes the
	// id a git-URL registration would derive from a project's current
	// UpstreamURL — used to verify that an id carrying
	// URLDerivedProjectIDPrefix was actually derived from a URL, rather than
	// hand-authored to merely look that way. Wired from
	// internal/server/wire.go's buildProjectStore to
	// internal/api.DeriveProjectIDFromURL; orchestrator cannot import
	// internal/api directly (would cycle through internal/dispatcher). nil
	// in any ProjectStore built without this wiring;
	// reconcileExpectedProjectID treats "cannot verify" as "do not ignore
	// the drift".
	deriveProjectID func(upstreamURL string) (string, error)
}

// SetDeriveProjectIDFunc wires the git-URL-to-project-id derivation used by
// reconcileExpectedProjectID to verify that a url-derived expectedID was
// ACTUALLY derived from the project's current UpstreamURL, not merely a
// hand-authored id: value that happens to share the prefix.
func (s *ProjectStore) SetDeriveProjectIDFunc(fn func(upstreamURL string) (string, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deriveProjectID = fn
}

// ProjectStatus is a project's in-memory health snapshot. See
// Status/MarkDegraded/MarkReady.
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
	// StatusDegraded means the project's row is preserved but its
	// project.yaml/bare-repo could not be loaded — see ProjectStatus.Message
	// for why.
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
// loaded, without touching its DB row or its (possibly still-cached) Meta.
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
	if meta.ID == "" {
		// This is the legacy host-filesystem `boid project add <dir>` path,
		// which has no URL to derive a fallback id from (unlike the
		// bare-repo registration path), so an id-less project.yaml must
		// fail outright — caching under the empty-string key would
		// silently corrupt the map and let a project register with an
		// empty DB primary key.
		return nil, fmt.Errorf("%s: project.yaml: id is required (this legacy host-directory registration path has no git URL to derive a fallback id from — see docs/plans/workspace-default-project.md 論点h)", workDir)
	}
	s.mu.Lock()
	s.metas[meta.ID] = meta
	s.mu.Unlock()
	s.MarkReady(meta.ID)
	return meta, nil
}

// LoadBareRepo reads .boid/project.yaml from a daemon-managed bare git
// repository's HEAD and stores the meta in memory — the bare-repo
// counterpart to Load. Used both by ProjectAppService.CreateProjectFromGitURL
// (first load right after a fresh `git clone --bare`) and
// ProjectAppService.FetchProject (`boid project fetch <id>`, reload after
// `git fetch --all`).
func (s *ProjectStore) LoadBareRepo(bareRepoPath string) (*ProjectMeta, error) {
	meta, err := ReadProjectMetaFromBareRepo(bareRepoPath)
	if err != nil {
		return nil, err
	}
	if meta.ID == "" {
		// project.yaml exists but omits `id:`. There is no id to cache
		// under yet (caching under "" would corrupt the map) — the only
		// caller of this method, CreateProjectFromGitURL, derives a
		// URL-based id and caches it itself once that id is confirmed free.
		return meta, nil
	}
	s.mu.Lock()
	s.metas[meta.ID] = meta
	s.mu.Unlock()
	s.MarkReady(meta.ID)
	return meta, nil
}

// reconcileExpectedProjectID implements the rule that project.yaml's own
// `id:` field is optional, and once a project has been registered under an
// id ACTUALLY derived from its git upstream URL (verified below, not merely
// carrying URLDerivedProjectIDPrefix), that id must never change out from
// under it — existing tasks reference it directly.
//
//   - meta.ID == "": project.yaml never declared an id (or dropped it) —
//     always adopt expectedID, the id this project is already registered
//     under. Unconditional regardless of expectedID's provenance: an
//     id-less project.yaml has nothing of its own to prefer either way.
//   - meta.ID != expectedID AND expectedID is verified to have been
//     DERIVED FROM upstreamURL (via s.deriveProjectID, when wired): a
//     project.yaml that gained (or kept) an explicit `id:` after this
//     project was registered via the no-project.yaml path. Ignored (with a
//     warning), not rejected — switching to project.yaml's id would detach
//     every existing task from this project.
//   - meta.ID != expectedID otherwise — including when expectedID merely
//     LOOKS url-derived (carries the prefix) but does not actually match
//     what s.deriveProjectID(upstreamURL) produces, or when
//     s.deriveProjectID is unset/errors: left untouched. The caller still
//     treats this as a mismatch and rejects it.
func (s *ProjectStore) reconcileExpectedProjectID(meta *ProjectMeta, expectedID, upstreamURL string) {
	if meta.ID == "" {
		meta.ID = expectedID
		return
	}
	if meta.ID == expectedID {
		return
	}
	if !s.isVerifiedURLDerivedID(expectedID, upstreamURL) {
		return
	}
	slog.Warn("project.yaml declares an id that differs from this project's url-derived registered id; ignoring project.yaml's id (a url-derived project's id never changes after registration)",
		"registered_id", expectedID, "project_yaml_id", meta.ID)
	meta.ID = expectedID
}

// isVerifiedURLDerivedID reports whether id was ACTUALLY derived from
// upstreamURL via s.deriveProjectID — carrying URLDerivedProjectIDPrefix is
// not, on its own, proof of provenance. Shared by reconcileExpectedProjectID
// (a still-present project.yaml whose id: drifted) and LoadAll's
// missing-project.yaml fallback, so a hand-authored `id: url-custom` that
// never actually came from a URL derivation gets the ordinary drift/degrade
// treatment in both places.
//
// Returns false (never ignore) whenever verification cannot be performed —
// id doesn't even look url-derived, upstreamURL is empty, no derive func is
// wired, or the derivation itself errors — never when it merely could not
// be confirmed.
func (s *ProjectStore) isVerifiedURLDerivedID(id, upstreamURL string) bool {
	if !strings.HasPrefix(id, URLDerivedProjectIDPrefix) {
		return false
	}
	s.mu.RLock()
	deriveFn := s.deriveProjectID
	s.mu.RUnlock()
	if deriveFn == nil || upstreamURL == "" {
		return false
	}
	derived, err := deriveFn(upstreamURL)
	return err == nil && derived == id
}

// LoadBareRepoExpectingID is LoadBareRepo's id-validated counterpart. Unlike
// LoadBareRepo, it never caches the freshly-read meta under its own id
// before confirming it matches expectedID — otherwise, if project A's
// project.yaml `id:` drifted to collide with an already-registered,
// unrelated project B, A's load could silently overwrite B's cache entry.
//
// Used by FetchProject (via the Meta interface) and LoadAll below — both
// call sites already have a specific expected id to validate the freshly-
// read meta against, unlike LoadBareRepo's other caller
// (CreateProjectFromGitURL's first load right after a fresh clone), which
// has no prior id to compare against: a brand-new registration adopts
// whatever id project.yaml declares, so that call site is deliberately left
// on the original LoadBareRepo (unconditional cache commit).
//
// Metadata is read into a scratch value first (ReadProjectMetaFromBareRepo
// does not touch the cache); the shared cache is only mutated once
// expectedID has been confirmed to match. A mismatch still returns the
// scratch meta — so the caller can report both the old and new id in its own
// error message, same as before this fix — but leaves EVERY existing cache
// entry, including any prior entry registered under the drifted id itself,
// completely untouched.
func (s *ProjectStore) LoadBareRepoExpectingID(bareRepoPath, expectedID, upstreamURL string) (*ProjectMeta, error) {
	meta, err := ReadProjectMetaFromBareRepo(bareRepoPath)
	if err != nil {
		return nil, err
	}
	s.reconcileExpectedProjectID(meta, expectedID, upstreamURL)
	if meta.ID != expectedID {
		return meta, nil
	}
	s.mu.Lock()
	s.metas[meta.ID] = meta
	s.mu.Unlock()
	s.MarkReady(meta.ID)
	return meta, nil
}

// LoadExpectingID is Load's id-validated counterpart — the legacy host-dir
// registration path LoadAll dispatches to here.
func (s *ProjectStore) LoadExpectingID(workDir, expectedID string) (*ProjectMeta, error) {
	meta, err := ReadProjectMetaWithKits(workDir)
	if err != nil {
		return nil, err
	}
	if meta.ID == "" {
		// Byte-for-byte match Load's own explicit rejection above, rather
		// than silently falling through to the generic mismatch branch
		// below, so this gives its own specific, actionable error.
		return nil, fmt.Errorf("%s: project.yaml: id is required (this legacy host-directory registration path has no git URL to derive a fallback id from — see docs/plans/workspace-default-project.md 論点h)", workDir)
	}
	// Deliberately NOT reconcileExpectedProjectID: `id:` is optional only
	// for the git-URL / bare-repo path, which has a URL to derive a
	// fallback id from. This legacy host-directory path must keep rejecting
	// an id-less or drifted project.yaml outright rather than silently
	// inheriting expectedID.
	if meta.ID != expectedID {
		return meta, nil
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
// capabilities, host_commands, env, SecretNamespace injection, and the
// workspace's default project definition (task_behaviors / base_branch /
// fork_point / default_task_behavior), which project.yaml's own values
// override field by field and canonical-behavior-name by name.
//
// Hydration rules:
//   - If the project has no linked workspace, returns the cached meta unchanged.
//   - If linked: always injects meta.SecretNamespace = workspaceID.
//   - On workspace.yaml load success: merges Capabilities, host_commands,
//     Env, and the workspace default project definition.
//   - On os.ErrNotExist (degraded window): logs a warning, returns meta with
//     only SecretNamespace injected (no error) — the workspace default is
//     NOT applied in this window either, same as every other workspace-level
//     field.
//   - On other errors: returns nil and the error.
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

	// Expand ${VAR} placeholders in ws.Env before anything below reads it.
	// The DB/yaml-stored ws value stays raw (see
	// expandWorkspaceRuntimeForDispatch's doc comment); ws is reassigned to
	// a clone here so every block below transparently sees expanded values.
	ws = expandWorkspaceRuntimeForDispatch(ws)

	// Capabilities: workspace overrides project (e.g. enables docker proxy).
	if ws.Capabilities.Docker != nil {
		out.Capabilities = ws.Capabilities
	}

	// workspace default project definition: a workspace-level task_behaviors
	// entry fills in any behavior name project.yaml does not already define.
	// A name project.yaml ALSO defines wins outright — no per-field merge
	// inside one behavior. Names are compared verbatim on both sides.
	//
	// This MUST run before the host_commands/env blocks below: they iterate
	// out.TaskBehaviors to inject ws.HostCommands/ws.Env into every
	// behavior's own HostCommands/Env fields, and a workspace-default-
	// supplied behavior needs that same per-behavior injection a
	// project.yaml-defined one gets — running this merge afterward would
	// leave a workspace-only behavior's HostCommands/Env unpopulated.
	if len(ws.TaskBehaviors) > 0 {
		if out.TaskBehaviors == nil {
			out.TaskBehaviors = make(map[string]TaskBehavior)
		}
		for name, behavior := range ws.TaskBehaviors {
			if _, exists := out.TaskBehaviors[name]; exists {
				continue
			}
			out.TaskBehaviors[name] = behavior
		}
	}

	// workspace.HostCommands is a []string of reference names into the
	// daemon's aggregated host_commands config (s.hostCommands, wired via
	// SetHostCommands). Each resolved name is merged into the top-level
	// meta.HostCommands (session jobs bypass behaviors and read this
	// directly) and into every TaskBehavior.HostCommands (task hooks read
	// behavior.HostCommands via the planner); project.yaml wins on a name
	// conflict. Checking only ws.HostCommands here (not also
	// len(s.hostCommands) > 0) means every referenced name always gets
	// resolved-or-warned, even when the aggregated config is empty.
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
			for name, behavior := range out.TaskBehaviors {
				behavior.HostCommands = mergeHostCommands(resolved, behavior.HostCommands)
				out.TaskBehaviors[name] = behavior
			}
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
		for name, behavior := range out.TaskBehaviors {
			behavior.Env = mergeStringMaps(ws.Env, behavior.Env)
			out.TaskBehaviors[name] = behavior
		}
	}

	// BaseBranch / ForkPoint / DefaultTaskBehavior: empty means "unspecified"
	// — project.yaml wins whenever it set a non-empty value; otherwise the
	// workspace default (itself possibly also empty) is inherited. Each is
	// gated on out.X being empty first so ws.X never clobbers an
	// already-set project.yaml value. There is deliberately no way to
	// explicitly un-set an inherited value back to empty.
	if out.BaseBranch == "" {
		out.BaseBranch = ws.BaseBranch
	}
	if out.ForkPoint == "" {
		out.ForkPoint = ws.ForkPoint
	}
	if out.DefaultTaskBehavior == "" {
		out.DefaultTaskBehavior = ws.DefaultTaskBehavior
	}

	warnSignalConnectorServicesNotEnabled(projectID, workspaceID, out.Triggers, ws.Services)

	return out, nil
}

// Explain returns the field-provenance view of projectID's effective meta —
// for each of the 4 workspace-default-mergeable fields, whether the
// effective value came from project.yaml or the workspace's default
// project definition. Unlike GetWithWorkspace, this never returns an error
// for a missing/unreadable workspace: the degraded window is reported as
// "no workspace default applied" (ProvenanceUnset). A real load failure
// (not os.ErrNotExist) is reported as ProvenanceUnavailable instead —
// distinct from "unset" because this function genuinely cannot tell
// whether a workspace default would have supplied a value (see
// ComputeProjectExplain's own doc comment for the classification rule).
func (s *ProjectStore) Explain(projectID string) (*ProjectExplain, error) {
	s.mu.RLock()
	meta, ok := s.metas[projectID]
	workspaceID := s.workspaceIDs[projectID]
	s.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("project %q: meta not loaded", projectID)
	}

	var wsMeta *WorkspaceMeta
	workspaceUnavailable := false
	if workspaceID != "" && s.workspaceStore != nil {
		if loaded, err := s.workspaceStore.Load(workspaceID); err == nil {
			wsMeta = loaded
		} else if errors.Is(err, os.ErrNotExist) {
			slog.Warn("project explain: workspace.yaml not found; reporting as no workspace default applied",
				"project_id", projectID, "workspace_id", workspaceID)
		} else {
			workspaceUnavailable = true
			slog.Warn("project explain: workspace load failed; reporting affected fields as unavailable rather than unset",
				"project_id", projectID, "workspace_id", workspaceID, "error", err)
		}
	}

	return ComputeProjectExplain(projectID, workspaceID, meta, wsMeta, workspaceUnavailable), nil
}

// Set stores meta directly.
func (s *ProjectStore) Set(id string, meta *ProjectMeta) {
	s.mu.Lock()
	s.metas[id] = meta
	s.mu.Unlock()
}

// SetSynthesizedMeta caches a meta value that was synthesized rather than
// read from a project.yaml — the project.yaml-less registration path
// (CreateProjectFromGitURL) and its reload counterpart (FetchProject). Unlike
// the bare Set above, this also clears any prior degraded status: a
// successful synthesis means the project is usable again (the workspace
// default resolved), so a stale StatusDegraded left over from an earlier
// failed reload must not linger.
func (s *ProjectStore) SetSynthesizedMeta(id string, meta *ProjectMeta) {
	s.mu.Lock()
	s.metas[id] = meta
	s.mu.Unlock()
	s.MarkReady(id)
}

// synthesizeMetaForReload builds a minimal ProjectMeta for a project.yaml-less
// project on daemon startup/reload — LoadAll's fallback when gitShowHEAD
// classifies the failure as GitHeadReadFailurePathAbsent (project.yaml was
// never committed, not a corrupt repo). Only ID and Name are populated here:
// TaskBehaviors / BaseBranch / ForkPoint / DefaultTaskBehavior are
// deliberately left empty so GetWithWorkspace's existing workspace-default
// merge (composed dynamically on every hydrate, not snapshotted here) fills
// them in from whatever the workspace's CURRENT default project definition
// is — exactly the same as a project.yaml-bearing project whose own fields
// are empty.
//
// Name is recovered in priority order (a first-ever reload has no
// previously-cached Name to fall back on; re-deriving from UpstreamURL
// alone would then silently discard an explicit --name given at
// registration time, since the derived and explicit names can legitimately
// differ):
//
//  1. the project's own previously-cached Name (a reload after the first —
//     the common case — always has this).
//  2. candidate.WorkDir's own directory basename, ".git" stripped — the bare
//     repo's last path segment IS the exact name (explicit --name or
//     derived) CreateProjectFromGitURL used at registration time.
//  3. re-derived from the DB-persisted UpstreamURL via
//     DeriveProjectNameFromURL, only when WorkDir's basename cannot be used
//     (essentially unreachable in practice; kept as a last-resort fallback).
//  4. empty (still better than removing the project outright).
func (s *ProjectStore) synthesizeMetaForReload(candidate *Project) *ProjectMeta {
	s.mu.RLock()
	prev, hadPrev := s.metas[candidate.ID]
	s.mu.RUnlock()

	name := ""
	nameSource := ""
	switch {
	case hadPrev && prev.Name != "":
		name = prev.Name
		nameSource = "cached"
	case candidate.WorkDir != "":
		base := filepath.Base(candidate.WorkDir)
		name = strings.TrimSuffix(base, ".git")
		nameSource = "basename"
	case candidate.UpstreamURL != "":
		if derived, err := DeriveProjectNameFromURL(candidate.UpstreamURL); err == nil {
			name = derived
			nameSource = "url"
		}
	}
	return &ProjectMeta{ID: candidate.ID, Name: name, NameSource: nameSource}
}

// MetaProjectIDs returns the ids of every project linked to workspaceID
// (via SetWorkspaceID/the workspaceIDs map) whose cached meta declares
// signals.sources[] — i.e. workspaceID's metaprojects. Satisfies
// MetaProjectResolver (signal_ingest_bridge.go); nil (never a non-nil empty
// slice) when workspaceID has none, so a caller's len()==0 check works
// either way.
//
// A project whose meta was never Set (in-memory cache miss — e.g. one still
// mid-registration) is simply absent from this scan, not an error: the same
// "cannot tell, so behave as if this project has nothing to declare" posture
// Get's own (meta, false) return already gives every other caller.
func (s *ProjectStore) MetaProjectIDs(workspaceID string) []string {
	if workspaceID == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ids []string
	for projectID, wsID := range s.workspaceIDs {
		if wsID != workspaceID {
			continue
		}
		meta, ok := s.metas[projectID]
		if !ok || meta == nil || len(meta.Signals.Sources) == 0 {
			continue
		}
		ids = append(ids, projectID)
	}
	return ids
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
// still supported for `boid project init`; existing dir-based registrations
// are left alone, not auto-migrated) — and records each project's
// workspaceID so that GetWithWorkspace can hydrate at call time.
//
// Per-project errors are returned in the original order. When the inner
// error is a *ProjectMigrationError, the candidate's project ID is stamped
// onto every Issue in the returned error so downstream callers can drive
// auto-migration via errors.As — this remains the one fail-fast case.
// Every OTHER per-project failure (missing dir, YAML parse error, corrupt
// or unreachable bare repo, project.yaml missing from a bare repo's HEAD...)
// is wrapped as *ProjectMissingError and, in addition to being returned
// here, is recorded via MarkDegraded before this method returns — callers
// need no second pass: LoadAll already leaves every failed candidate in the
// StatusDegraded state with its DB row (and any previously cached Meta)
// untouched.
func (s *ProjectStore) LoadAll(projects []*Project) []error {
	s.mu.RLock()
	reposRoot := s.reposRoot
	s.mu.RUnlock()

	var errs []error
	for _, candidate := range projects {
		var meta *ProjectMeta
		var loadErr error
		if IsBareRepoDir(candidate.WorkDir) {
			meta, loadErr = s.LoadBareRepoExpectingID(candidate.WorkDir, candidate.ID, candidate.UpstreamURL)
		} else {
			meta, loadErr = s.LoadExpectingID(candidate.WorkDir, candidate.ID)
		}
		// A mismatch here (upstream project.yaml id: changed) is treated
		// exactly like any other load failure for candidate.ID.
		// LoadBareRepoExpectingID/LoadExpectingID never cache a mismatched
		// meta under its own (drifted) id in the first place, so there is
		// no phantom entry to clean up and no risk of clobbering an
		// unrelated already-registered project.
		if loadErr == nil && meta.ID != candidate.ID {
			loadErr = fmt.Errorf("project.yaml id %q does not match the registered project id %q; re-register manually", meta.ID, candidate.ID)
		}
		if loadErr != nil {
			// A project.yaml that was simply never committed
			// (GitHeadReadFailurePathAbsent) must NOT degrade/remove a
			// project registered via the no-project.yaml path
			// (CreateProjectFromGitURL's own workspace-default fallback) —
			// every OTHER failure kind still falls through to the existing
			// remove+degrade handling below unchanged.
			//
			// Gated on candidate.ID being VERIFIED url-derived:
			// GitHeadReadFailurePathAbsent alone cannot tell "registered
			// without a project.yaml on purpose" apart from "an ordinary
			// project.yaml-bearing project whose file was deleted upstream
			// by mistake" — both read back identically, and checking
			// candidate.ID's prefix alone would also let a hand-authored
			// `id: url-custom` whose project.yaml was later deleted slip
			// through undetected. isVerifiedURLDerivedID additionally
			// confirms candidate.ID equals what candidate.UpstreamURL
			// actually derives to.
			var headErr *GitHeadReadError
			if errors.As(loadErr, &headErr) && headErr.Kind == GitHeadReadFailurePathAbsent && s.isVerifiedURLDerivedID(candidate.ID, candidate.UpstreamURL) {
				synthesized := s.synthesizeMetaForReload(candidate)
				s.mu.Lock()
				s.metas[candidate.ID] = synthesized
				s.workspaceIDs[candidate.ID] = candidate.WorkspaceID
				s.mu.Unlock()
				s.MarkReady(candidate.ID)
				continue
			}
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

// degradedMessageFor upgrades a generic load-failure message to migration-
// path guidance when the project's WorkDir is neither a bare repo (LoadAll
// already ruled that out via IsBareRepoDir before calling this) nor under
// the daemon's own bare-repo storage root — i.e. it looks like a
// pre-cutover, host-filesystem project registration. reposRoot empty (never
// configured) skips the upgrade entirely; the plain wrapped error text is
// still informative on its own.
func degradedMessageFor(candidate *Project, wrapped error, reposRoot string) string {
	if reposRoot == "" {
		return wrapped.Error()
	}
	// This decision has no destructive side effect (it only picks which
	// wording to show), so a resolution error just falls back to the
	// lexical-only PathIsUnder rather than failing the whole startup load
	// loop over a message-wording concern.
	underRoot, err := PathIsUnderResolved(reposRoot, candidate.WorkDir)
	if err != nil {
		underRoot = PathIsUnder(reposRoot, candidate.WorkDir)
	}
	if !underRoot {
		return fmt.Sprintf("legacy project registered from host dir %q; re-add via `boid project add <git-url> --workspace=<name>` (%v)", candidate.WorkDir, wrapped)
	}
	return wrapped.Error()
}

// PathIsUnder reports whether path is root itself or a descendant of it.
// Exported so internal/api's ProjectAppService can reuse the exact same
// containment check degradedMessageFor already relies on, both to defend
// BareRepoPath's computed destination against a traversal-capable
// projectName (SafeBareRepoPath below) and to classify a project's WorkDir
// as daemon-managed vs. legacy.
//
// This is a purely LEXICAL check (filepath.Rel on the two strings as
// given) — it does not resolve symlinks, so a symlinked ANCESTOR directory
// can make an out-of-tree location appear to be "under root". Callers that
// go on to WRITE or DELETE at path based on this answer — SafeBareRepoPath
// below and internal/api's isManagedBareRepoPath — MUST use
// PathIsUnderResolved instead; this lexical-only form remains appropriate
// for degradedMessageFor's cosmetic (no side effect) message-wording
// decision below.
func PathIsUnder(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// PathIsUnderResolved is PathIsUnder with symlink resolution applied to
// BOTH root and path before the lexical comparison, so a symlinked ancestor
// directory cannot make an escaped location appear contained. Two
// different resolution strategies are
// needed depending on whether root/path already exist on disk:
//
//   - Already exists (the common rm-time/fetch-time case: an
//     already-registered project's bare repo, or the daemon's own
//     <dataDir>/repos root): filepath.EvalSymlinks resolves every symlink
//     along the way.
//   - Does not exist yet (the register-time case: SafeBareRepoPath is
//     called BEFORE the clone that will create the bare repo directory —
//     there is nothing there yet that COULD be a symlink): resolves the
//     deepest EXISTING ancestor via EvalSymlinks and rejoins the
//     not-yet-created suffix unresolved (nothing below an ancestor that
//     does not exist could itself be a symlink).
//
// Returns an error only for a resolution failure unrelated to "the path
// does not exist yet" (e.g. a permission error, or a symlink loop) —
// callers should treat that as "could not verify containment" and fail
// closed (refuse the write/delete), not silently fall back to the
// lexical-only check.
func PathIsUnderResolved(root, path string) (bool, error) {
	resolvedRoot, err := resolveExistingAncestor(root)
	if err != nil {
		return false, fmt.Errorf("resolve %q: %w", root, err)
	}
	resolvedPath, err := resolveExistingAncestor(path)
	if err != nil {
		return false, fmt.Errorf("resolve %q: %w", path, err)
	}
	return PathIsUnder(resolvedRoot, resolvedPath), nil
}

// resolveExistingAncestor walks p upward until it finds a directory
// component that actually exists, resolves symlinks on THAT ancestor via
// filepath.EvalSymlinks, then rejoins the not-yet-existing suffix (if any)
// back onto the resolved form unmodified — replicating what
// filepath.EvalSymlinks itself would do for a fully-existing path, for the
// register-time case where p's leaf (and possibly several of its parents)
// have not been created yet. A p every component of which is missing all
// the way up to the filesystem root returns p unchanged (nothing to
// resolve against).
func resolveExistingAncestor(p string) (string, error) {
	clean := filepath.Clean(p)
	var missingSuffix []string
	cur := clean
	for {
		if _, err := os.Lstat(cur); err == nil {
			resolved, evalErr := filepath.EvalSymlinks(cur)
			if evalErr != nil {
				return "", evalErr
			}
			for i := len(missingSuffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missingSuffix[i])
			}
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root with nothing found to exist —
			// every component of p is missing. Nothing to resolve against;
			// return the cleaned form as-is.
			return clean, nil
		}
		missingSuffix = append(missingSuffix, filepath.Base(cur))
		cur = parent
	}
}

// wrapPerProjectLoadErr attaches the project ID to a per-project load
// error. Two classifications:
//   - *ProjectMigrationError: schema migration is needed. Preserved as the
//     typed error with ProjectID filled on each Issue so callers can drive
//     auto-migration via errors.As. This remains the one fail-fast case —
//     everything else below is degrade-not-fail-fast.
//   - everything else: returned as *ProjectMissingError (see that type's
//     doc comment for why the name stayed even though its scope broadened
//     well past "missing") so callers mark the project degraded — a
//     missing dir, a YAML parse error, and a corrupt/unreachable bare repo
//     are all handled identically: preserve the DB row, surface the
//     failure via status, and let daemon startup continue.
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
