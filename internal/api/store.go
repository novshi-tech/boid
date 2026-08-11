package api

import (
	"context"
	"syscall"
	"time"

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

type JobLifecycle interface {
	CompleteJob(jobID string, result JobCompletion)
	UnregisterJob(jobID string)
	CleanupTaskWindow(taskID string)
	StopJobRuntime(runtimeID string)
	// SignalJobRuntime delivers a single Unix signal to the runtime's process
	// group. Phase 3-b uses it to graceful-stop the agent (SIGUSR1) without
	// tearing down the surrounding sandbox runtime: claude.Adapter.Run() has a
	// signal.Notify(SIGUSR1) handler that translates the group signal into a
	// SIGTERM toward the claude child, then normalises the resulting exit
	// status into Result.StoppedByDaemon=true.
	SignalJobRuntime(runtimeID string, sig syscall.Signal)
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

type TaskService interface {
	CreateTask(req CreateTaskRequest) (*orchestrator.Task, error)
	ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error)
	GetTask(id string) (*orchestrator.Task, error)
	GetTaskDetail(id string) (*TaskDetailView, error)
	GetTaskField(id, path string) (string, error)
	UpdateTask(id string, req UpdateTaskRequest) (*orchestrator.Task, error)
	DeleteTask(id string, force bool) error
	ImportTasks(reqs []CreateTaskRequest) (*ImportResult, error)
	DuplicateTask(sourceID string, autoStart bool) (*orchestrator.Task, error)
	RerunTask(id string, req RerunTaskRequest) (*orchestrator.Task, error)
}

type WebService interface {
	ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error)
	GetTaskDetail(id string) (*TaskDetailView, error)
	ListProjects() ([]*orchestrator.Project, error)
	ListBehaviors() ([]string, error)
	ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error)
	ApplyAction(taskID string, actionType string) error
	DuplicateTask(id string) (string, error)
	DeleteTask(id string, force bool) error
	ListJobs(status string) ([]JobWithContext, error)
	ListSessions() ([]JobWithContext, error)
	GetJob(id string) (*JobWithContext, error)
	CreateTask(req CreateTaskRequest) (*orchestrator.Task, error)
	UpdateTask(id string, req UpdateTaskRequest) error
	RerunTask(id string, req RerunTaskRequest) error
	ReopenTask(id string, req ReopenTaskRequest) error
	AnswerTask(ctx context.Context, taskID, questionID, answer string) error
	ListHooksForStatus(taskID, status string) ([]orchestrator.Hook, error)
	ReplayHook(ctx context.Context, taskID string, req ReplayHookRequest) (*ReplayHookResult, error)
	GetProjectByID(id string) (*orchestrator.Project, error)
}

type WorkflowService interface {
	ApplyAction(ctx context.Context, taskID string, req ApplyActionRequest) (*ActionApplication, error)
	CompleteJob(ctx context.Context, jobID string, req JobDoneRequest) (*Job, error)
	// StopAgent asks the agent backing runtimeID to terminate gracefully,
	// without tearing down the surrounding runner-inner-child. The broker's
	// `boid job done` call still fires normally, preserving any payload
	// patch the agent wrote up to that point. NotifyTask calls this after
	// `ApplyAction("ask")` so the awaiting transition does not race with
	// payload_patch capture. No-op when runtimeID is empty.
	StopAgent(runtimeID string)
}

type TaskStore interface {
	CreateTask(task *orchestrator.Task) error
	GetTask(id string) (*orchestrator.Task, error)
	ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error)
	UpdateTask(task *orchestrator.Task) error
	DeleteTask(id string) error
	FindTaskByRemote(remoteID string) (*orchestrator.Task, error)
	FindTaskByRef(ref, parentID string) (*orchestrator.Task, error)
	// ListChildren returns direct children (one level only) of the given parent
	// task, ordered by created_at ASC. Returns an empty slice (not nil) when the
	// task has no children. Used by finalizeTerminal to sweep boid/<id8> branches
	// once a supervisor reaches a terminal state.
	ListChildren(parentID string) ([]*orchestrator.Task, error)
}

type ActionStore interface {
	CreateAction(action *orchestrator.Action) error
	ListActionsByTask(taskID string) ([]*orchestrator.Action, error)
}

// TaskTriageStore provides access to the cross-project-issue-triage Phase 1
// sidecar (docs/plans/cross-project-issue-triage.md 実測c). Deliberately
// separate from TaskStore: TaskStore's orchestrator.Task is the API DTO
// (marshaled to JSON with no conversion layer), so keeping triage-specific
// fields out of it means they don't auto-expose in every task API response.
type TaskTriageStore interface {
	UpsertTaskTriage(tt *orchestrator.TaskTriage) error
	GetTaskTriage(taskID string) (*orchestrator.TaskTriage, error)
	DeleteTaskTriage(taskID string) error
	// ParkedFrom derives which status (triaged/ready) a parked task was
	// parked from, from the actions log (not a stored column — 決定13).
	ParkedFrom(taskID string) (orchestrator.TaskStatus, error)
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

type JobStore interface {
	GetJob(id string) (*Job, error)
	ListJobsByTask(taskID string) ([]*Job, error)
	UpdateJob(job *Job) error
}

// GlobalJobStore supports cross-task job listing with context (task title, project name).
type GlobalJobStore interface {
	ListJobsWithContext(filter JobListFilter) ([]JobWithContext, error)
}

type TxStore interface {
	TaskStore
	ActionStore
	TaskTriageStore
	JobStore
}

type Transactor interface {
	WithinTx(func(TxStore) error) error
}

type GCStore interface {
	GC(olderThan time.Duration, dryRun bool) (*orchestrator.GCResult, error)
}

type GCService interface {
	Run(olderThan time.Duration, dryRun bool) (*orchestrator.GCResult, error)
}

type DeviceGCStore interface {
	DeleteRevokedDevices(ctx context.Context, dryRun bool) (int64, error)
}

// JobLogReader reads the transcript log for a given runtime.
type JobLogReader interface {
	ReadJobLog(runtimeID string) ([]byte, error)
	StatJobLog(runtimeID string) (size int64, mtime time.Time, err error)
}

// SessionDispatcher launches a session job (claude / codex / opencode under
// a HarnessAdapter) and returns the runtime job id.
type SessionDispatcher interface {
	StartSession(ctx context.Context, req StartSessionRequest) (*StartSessionResult, error)
}

// ExecDispatcher launches a JobKindExec job (arbitrary argv, shell harness,
// no HarnessAdapter agent) through Runner.Dispatch() and returns the runtime
// job id. Implemented by internal/server's sessionDispatcherAdapter.
type ExecDispatcher interface {
	StartExec(ctx context.Context, req StartExecRequest) (*StartExecResult, error)
}
