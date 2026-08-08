package api

import (
	"context"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

type MetaStore interface {
	Get(id string) (*orchestrator.ProjectMeta, bool)
	// GetWithWorkspace returns the project meta with workspace.yaml (kits,
	// env, capabilities) hydrated in. Use this whenever the caller dispatches
	// hooks or otherwise needs the resolved runtime view.
	GetWithWorkspace(ctx context.Context, projectID string) (*orchestrator.ProjectMeta, error)
}

type DispatchCoordinator interface {
	DispatchAndAdvance(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) (*orchestrator.DispatchResult, error)
	ReplayHook(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, hookID string) (*orchestrator.ReplayResult, error)
}

// HookService provides hook replay and hook listing operations.
type HookService interface {
	ReplayHook(ctx context.Context, taskID string, req ReplayHookRequest) (*ReplayHookResult, error)
	ListHooksForStatus(taskID, status string) ([]orchestrator.Hook, error)
}

// ReplayHookRequest is the input for hook replay.
type ReplayHookRequest struct {
	HookID string
	Status string // optional: override task.Status before replay
}

// ReplayHookResult is the output of a hook replay.
type ReplayHookResult struct {
	Task        *orchestrator.Task        `json:"task"`
	FiredEvents []orchestrator.FiredEvent `json:"fired_events,omitempty"`
}

type BrokerRegistry interface {
	RegisterBrokerCommands(commands map[string]orchestrator.HostCommandSpec, builtinPolicies map[string]sandbox.BuiltinPolicy, projectID string) (*BrokerRegisterResponse, error)
}

type ProjectService interface {
	CreateProject(workDir string) (*orchestrator.Project, error)
	// CreateProjectFromGitURL registers a project from a git remote URL
	// (docs/plans/volume-only-daemon.md §論点a, POST /api/projects/git —
	// `boid project add <git-url> --workspace=<name>`). workspaceSlug is
	// required (unlike CreateProject's eager default-workspace assign);
	// nameOverride empty derives the project name from the URL's last path
	// component. *StatusError{400} for a missing url/workspace or an
	// unparseable URL, {404} for an unknown workspace, {409} if a project is
	// already registered at the computed bare-repo path, {502} for a clone
	// failure, {500} if the daemon has no git-clone/data-dir wiring.
	CreateProjectFromGitURL(ctx context.Context, gitURL, workspaceSlug, nameOverride string) (*orchestrator.Project, error)
	// FetchProject runs `git fetch --all` inside id's bare repo and reloads
	// its project.yaml (POST /api/projects/{id}/fetch — `boid project fetch
	// <id>`). *StatusError{404} unknown id, {400} for a legacy (non-bare,
	// pre-cutover) project or a project.yaml that fails to parse after
	// fetch, {502} for a fetch failure. Never deletes id's row on failure —
	// see the method's own doc comment for the on-startup-auto-prune-
	// retirement invariant this preserves.
	FetchProject(ctx context.Context, id string) (*orchestrator.Project, error)
	ListProjects(workspaceID string) ([]*orchestrator.Project, error)
	ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error)
	GetProject(id string) (*orchestrator.Project, error)
	SetProjectWorkspace(id, workspaceID string) (*orchestrator.Project, error)
	DeleteProject(id string) error
	ReloadProjects() (*ProjectReloadResult, error)
	// ResolveProjectRef resolves a ref string to one or more matching projects.
	// Priority: id exact match > name exact match > name substring match (case-insensitive).
	// Returns 1 project on unambiguous match, multiple on ambiguous match, StatusError{404} on no match.
	ResolveProjectRef(ref string) ([]*orchestrator.Project, error)
	// ExplainProject returns id's field-provenance view (docs/plans/
	// workspace-default-project.md 論点e, PR6, GET /api/projects/{id}/explain
	// — `boid project show --explain`): for each of the 4
	// workspace-default-mergeable fields, whether the effective value came
	// from project.yaml or the linked workspace's default project
	// definition. *StatusError{404} for an unknown id.
	ExplainProject(id string) (*orchestrator.ProjectExplain, error)

	// CreateWorkspace inserts a brand-new workspace (docs/plans/
	// workspace-db-consolidation.md PR4, POST /api/workspaces). Returns a
	// *StatusError{409} when slug already has a row, {400} for an invalid
	// slug.
	CreateWorkspace(slug string, meta *orchestrator.WorkspaceMeta) (*WorkspaceDetail, error)
	// GetWorkspace returns slug's meta, revision, and assigned project ids
	// (GET /api/workspaces/{slug}). *StatusError{404} when slug is unknown.
	GetWorkspace(slug string) (*WorkspaceDetail, error)
	// UpdateWorkspace replaces slug's meta wholesale (PUT
	// /api/workspaces/{slug}), enforcing optimistic concurrency via ifMatch
	// against the workspace's current revision unless force is true.
	// *StatusError{428} missing ifMatch, {412} stale ifMatch, {404} unknown
	// slug.
	UpdateWorkspace(slug string, meta *orchestrator.WorkspaceMeta, ifMatch string, force bool) (*WorkspaceDetail, error)
	// RemoveWorkspace deletes slug's row AND its init.sh (DELETE
	// /api/workspaces/{slug}), reporting what became of the latter.
	// *StatusError{400} for the reserved default slug, {404} unknown slug.
	//
	// The two deletions are one operation because they have to be excluded
	// from a concurrent apply together (PR9 codex round 3, Major 3) — see
	// ProjectAppService.RemoveWorkspace. They are still not atomic with each
	// other, hence the result: an error inside it means the row is gone and
	// the script is not.
	RemoveWorkspace(slug string) (*WorkspaceRemoval, error)

	// ApplyWorkspace upserts one boid.dev/v1 Workspace envelope document's
	// metadata and (when apply.FieldsPresent["projects"] is true) project
	// assignments atomically, in a single DB transaction (docs/plans/
	// volume-only-daemon.md PR-1d codex round-1 Blocker 2, POST
	// /api/workspaces/apply). dryRun exercises the same validation/DB
	// statements but rolls back instead of committing (Major 1).
	ApplyWorkspace(apply *orchestrator.WorkspaceEnvelopeApply, dryRun bool) (*orchestrator.WorkspaceApplyResult, error)

	// ExportWorkspaceEnvelopes returns one or more boid.dev/v1 Workspace
	// yaml documents ("---"-separated when more than one), built from a
	// single atomic DB snapshot (docs/plans/volume-only-daemon.md PR-1d
	// codex round-1 Blocker 3, GET /api/workspaces/export?all=true or
	// ?name=<slug>). slugs nil/empty exports every workspace; otherwise
	// exactly the given slugs. *StatusError{404} when a requested slug does
	// not exist. Each document carries the workspace's init.sh in
	// spec.init_script (PR9 of docs/plans/workspace-home-volume-persistence.md,
	// 論点 d) — an export without it is not a restorable backup.
	ExportWorkspaceEnvelopes(slugs []string) ([]byte, error)

	// GetWorkspaceInitScript returns slug's init.sh (GET
	// /api/workspaces/{slug}/init-script, PR9 of
	// docs/plans/workspace-home-volume-persistence.md 論点 d). A workspace
	// with no script is NOT an error: the result carries Exists=false and
	// Revision=WorkspaceInitScriptAbsentRevision, and the handler turns that
	// into a 404 with the sentinel ETag. *StatusError{400} for an invalid
	// slug, {404} for an unknown workspace, {501} when the daemon has no init
	// script store wired.
	GetWorkspaceInitScript(slug string) (*WorkspaceInitScript, error)
	// SetWorkspaceInitScript replaces slug's init.sh with content (PUT
	// /api/workspaces/{slug}/init-script), enforcing optimistic concurrency
	// via ifMatch against the current revision unless force is true — the
	// same 428/412 contract UpdateWorkspace and ApplyConfigYAML use. An
	// EMPTY content clears the script (see
	// workspaceInitScriptContentIsAbsent). *StatusError{400} for an invalid
	// slug or a script containing a NUL byte, {404} unknown workspace,
	// {428} missing ifMatch, {412} stale ifMatch, {501} no store wired.
	SetWorkspaceInitScript(slug string, content []byte, ifMatch string, force bool) (*WorkspaceInitScriptResult, error)
}

// WorkspaceRemoval reports what RemoveWorkspace did to the half of a workspace
// that is not a row (PR9 codex round 2 Major 1, moved inside the row's critical
// section by round 3 Major 3).
//
// It exists because a file unlink and a database transaction cannot commit
// together: by the time the script is touched the row is already gone, so a
// failure has to be described rather than rolled back or raised. The handler
// copies both fields into WorkspaceRemoveResponse, which is where their
// operator-facing meaning is documented.
type WorkspaceRemoval struct {
	// InitScriptDeleted is true only when the workspace HAD an init.sh and it
	// was removed. false covers a workspace that never had one, a daemon with
	// no init script store wired, and a failed deletion.
	InitScriptDeleted bool
	// InitScriptError is the unlink failure, if any. Deliberately NOT a
	// *StatusError: the row removal has already committed, so this must not
	// become the response's status — see
	// WorkspaceRemoveResponse.InitScriptDeleteError. The message carries the
	// daemon-side path, which is what an operator needs to clean up by hand.
	InitScriptError error
}

// WorkspaceDetail is the response shape for the workspace create/show/update
// endpoints: the parsed meta plus enough bookkeeping (revision, assigned
// project ids) for a caller to display or re-PUT it with the correct
// If-Match header. docs/plans/workspace-db-consolidation.md Step C/D/E.
type WorkspaceDetail struct {
	Slug     string                      `json:"slug"`
	Meta     *orchestrator.WorkspaceMeta `json:"meta"`
	Revision string                      `json:"revision,omitempty"`
	// ProjectCount mirrors len(AssignedProjects); kept as its own field so
	// callers that only need the count (e.g. a future list-style summary
	// view) don't need to len() the slice themselves.
	ProjectCount     int      `json:"project_count"`
	AssignedProjects []string `json:"assigned_projects"`
	// Home reports the workspace home VOLUME's size, read from the engine's
	// own disk-usage accounting (docs/plans/workspace-home-volume-persistence.md
	// 論点 a-2, PR7 — it was a directory-tree walk under Phase 4 PR5, before
	// the home became a docker named volume). Populated only by GET
	// /api/workspaces/{slug} (WorkspaceHandler.Show) — Create/Update/Import
	// leave it nil, since reading it costs an engine round trip and none of
	// those callers need it. nil (omitted from the JSON body) when the
	// handler has no home store, i.e. no engine handle to ask.
	Home *WorkspaceHomeSize `json:"home,omitempty"`
}

// WorkspaceStore provides direct CRUD over a single workspace's
// WorkspaceMeta, independent of the project-assignment bookkeeping that
// lives on ProjectRepository below (docs/plans/workspace-db-consolidation.md
// PR4). Implemented by *orchestrator.WorkspaceStore (via
// ProjectStore.WorkspaceStore()), wired in internal/server/wire.go.
type WorkspaceStore interface {
	Load(slug string) (*orchestrator.WorkspaceMeta, error)
	Save(slug string, meta *orchestrator.WorkspaceMeta) error
	// Create is insert-only: an error wrapping os.ErrExist when slug already
	// has a row (see orchestrator.WorkspaceRepository.Create).
	Create(slug string, meta *orchestrator.WorkspaceMeta) error
	Remove(slug string) error
	// LoadWithRevision returns meta and its revision from a single atomic
	// snapshot (docs/plans/workspace-db-consolidation.md MAJOR 1, codex
	// review), used by GET /api/workspaces/{slug} so meta and revision can
	// never straddle a concurrent write. See
	// orchestrator.WorkspaceRepository.LoadWithRevision's doc comment.
	LoadWithRevision(slug string) (*orchestrator.WorkspaceMeta, string, error)
	// UpdateIfRevisionMatches performs a compare-and-swap update: meta is
	// written only if slug's current revision equals expectedRevision,
	// atomically with the check. matched=false covers both "no such slug"
	// and "revision mismatch" — see
	// orchestrator.WorkspaceRepository.UpdateIfRevisionMatches's doc comment.
	UpdateIfRevisionMatches(slug string, expectedRevision string, meta *orchestrator.WorkspaceMeta) (newRevision string, matched bool, err error)
}

// HostCommandsProvider exposes the daemon's live aggregated host_commands
// snapshot (name -> spec) for reference-name validation on workspace
// create/update (docs/plans/workspace-db-consolidation.md MAJOR 2, codex
// review: an unresolvable meta.HostCommands reference must be rejected with
// 400 at write time rather than silently persisted and later warned-about +
// skipped at dispatch). Implemented by *server.Server, which already
// exposes this exact method for HostCommandsService above.
type HostCommandsProvider interface {
	HostCommands() map[string]orchestrator.HostCommandSpec
}

type ProjectRepository interface {
	CreateProject(project *orchestrator.Project) error
	GetProject(id string) (*orchestrator.Project, error)
	ListProjects() ([]*orchestrator.Project, error)
	SetProjectWorkspace(projectID, workspaceID string) error
	ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error)
	DeleteProject(id string) error
	// SetProjectUpstreamURL persists a project's captured upstream_url (see
	// docs/plans/git-gateway-cutover.md PR2). Used by ReloadProjects'
	// recapture and by the daemon-startup backfill.
	SetProjectUpstreamURL(projectID, upstreamURL string) error
	// WorkspaceExists reports whether workspaceID refers to an existing
	// workspaces table row. Used by ProjectAppService.SetProjectWorkspace to
	// reject assignment to a nonexistent slug (docs/plans/
	// workspace-db-consolidation.md MAJOR 5 codex review fix).
	WorkspaceExists(workspaceID string) (bool, error)
	// AssignWorkspaceIfExists atomically checks-then-assigns in a single DB
	// transaction (docs/plans/workspace-db-consolidation.md MAJOR 3, codex
	// review), replacing the WorkspaceExists+SetProjectWorkspace two-step
	// ProjectAppService.SetProjectWorkspace used before: a DELETE landing
	// between those two separate calls could leave a dangling
	// project_workspaces reference. Returns an error wrapping os.ErrNotExist
	// when workspaceID has no corresponding row (DefaultWorkspaceSlug and ""
	// are exempt from the check — see the implementation's doc comment).
	AssignWorkspaceIfExists(projectID, workspaceID string) error
	// GetWorkspaceSummary returns a single workspace's project count and
	// revision, or an error wrapping os.ErrNotExist. Used by the workspace
	// CRUD handlers (docs/plans/workspace-db-consolidation.md PR4) to build
	// responses and to read the current revision for the PUT If-Match check.
	GetWorkspaceSummary(slug string) (*orchestrator.WorkspaceSummary, error)
}

// ProjectWorkDirLookup provides read access to a project's working directory.
type ProjectWorkDirLookup interface {
	GetProject(id string) (*orchestrator.Project, error)
}
