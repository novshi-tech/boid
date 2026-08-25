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
	// AnswerSuggestion backs docs/plans/ingestion-identity.md PR-3 (B-3+B-4,
	// J-6)'s accept/reject buttons on the task_triage suggestion card — the
	// "既に開いている穴" this PR closes (決定14 moved judgment to the Web UI,
	// but the Web UI had no path to RECORD a reject, so re-suggestion was
	// never suppressed). A dedicated method rather than routing through
	// ApplyAction (like ReopenTask above): the generic
	// WebService.ApplyAction(taskID, actionType) signature has no payload
	// parameter, and `answered` needs one ({answer, verb, basis}).
	AnswerSuggestion(taskID string, req AnswerSuggestionRequest) error
	ListHooksForStatus(taskID, status string) ([]orchestrator.Hook, error)
	ReplayHook(ctx context.Context, taskID string, req ReplayHookRequest) (*ReplayHookResult, error)
	GetProjectByID(id string) (*orchestrator.Project, error)
}

type WorkflowService interface {
	ApplyAction(ctx context.Context, taskID string, req ApplyActionRequest) (*ActionApplication, error)
	// GetCard / ListCards are the task_triage read surface (card_read.go,
	// Phase 1 PR-5a). Part of WorkflowService because the brokered ops that
	// expose them to the sandbox reach the daemon through this same interface.
	GetCard(taskID string) (*CardView, error)
	ListCards(filter orchestrator.TaskFilter) ([]*CardView, error)
	CompleteJob(ctx context.Context, jobID string, req JobDoneRequest) (*Job, error)
	// StopAgent asks the agent backing runtimeID to terminate gracefully,
	// without tearing down the surrounding runner-inner-child. The broker's
	// `boid job done` call still fires normally, preserving any payload
	// patch the agent wrote up to that point. NotifyTask calls this after
	// `ApplyAction("ask")` so the awaiting transition does not race with
	// payload_patch capture. No-op when runtimeID is empty.
	StopAgent(runtimeID string)
}

// TaskCreator is the slice of TaskAppService that TaskWorkflowService.Dispatch
// needs to task-ify a triage task's specced children (docs/plans/
// cross-project-issue-triage.md Phase 1 PR-2, 逆輸入1). Implemented by
// TaskAppService and wired in after TaskAppService itself is constructed
// (wire.go creates TaskWorkflowService first, TaskAppService second, so this
// mirrors the same late-binding idiom the reverse direction already uses —
// see WorkflowService/TaskAppService.Workflow above) to avoid a literal
// struct-construction cycle between the two services.
type TaskCreator interface {
	CreateTask(req CreateTaskRequest) (*orchestrator.Task, error)
}

type TaskStore interface {
	CreateTask(task *orchestrator.Task) error
	GetTask(id string) (*orchestrator.Task, error)
	ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error)
	UpdateTask(task *orchestrator.Task) error
	DeleteTask(id string) error
	FindTaskByRemote(remoteID string) (*orchestrator.Task, error)
	// FindTaskByRef looks up an existing task by ref, scoped to BOTH
	// parentID ("" for a root task) and projectID (Phase 1 PR-4, codex review
	// Blocker fix — see orchestrator.FindTaskByRef's doc comment for why
	// projectID scoping is required, not optional).
	FindTaskByRef(ref, parentID, projectID string) (*orchestrator.Task, error)
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

// CardStore provides access to the cross-project-issue-triage Phase 1
// sidecar (docs/plans/cross-project-issue-triage.md 実測c). Deliberately
// separate from TaskStore: TaskStore's orchestrator.Task is the API DTO
// (marshaled to JSON with no conversion layer), so keeping triage-specific
// fields out of it means they don't auto-expose in every task API response.
type CardStore interface {
	UpsertTaskTriage(tt *orchestrator.CardAttrs) error
	// SeedTaskTriage creates an empty row only when none exists (INSERT ...
	// ON CONFLICT DO NOTHING). Distinct from UpsertTaskTriage precisely
	// because it cannot overwrite: it asserts membership ("this is a triage
	// task") and nothing else.
	SeedTaskTriage(taskID string) error
	GetTaskTriage(taskID string) (*orchestrator.CardAttrs, error)
	// ListTaskTriageByTaskIDs batch-fetches sidecar rows for a set of task
	// IDs in O(chunks) queries instead of O(N) GetTaskTriage calls (BD-8
	// 残件1) — see orchestrator.ListTaskTriageByTaskIDs's doc comment for
	// the chunking rationale and the "batch replaces per-row error
	// tolerance" tradeoff. A taskID with no sidecar row is simply absent
	// from the returned map, never an error.
	ListTaskTriageByTaskIDs(taskIDs []string) (map[string]*orchestrator.CardAttrs, error)
	DeleteTaskTriage(taskID string) error
	// ParkedFrom derives which status (triaged/ready/working) a parked task
	// was parked from, from the actions log (not a stored column — 決定13).
	ParkedFrom(taskID string) (orchestrator.TaskStatus, error)
}

// TaskIdentityStore provides access to docs/plans/ingestion-identity.md
// PR-1 (B-1)'s identity index (task_identities table): external key ->
// task, scoped per project (I-1/I-2/I-3). Deliberately separate from
// TaskStore for the same reason CardStore is: identity bindings are
// not part of orchestrator.Task's own JSON shape.
type TaskIdentityStore interface {
	// LinkIdentity binds identity to taskID within projectID's scope.
	// Re-linking the same task is idempotent; linking a DIFFERENT task
	// already bound to identity returns orchestrator.ErrIdentityConflict.
	LinkIdentity(projectID, identity, taskID string) error
	// UnlinkIdentity removes one binding, if any (idempotent — not found is
	// not an error).
	UnlinkIdentity(projectID, identity string) error
	// UnlinkAllForTask releases every identity bound to taskID (I-6: called
	// from the drop side effect — see TaskWorkflowService.ApplyAction).
	UnlinkAllForTask(taskID string) error
	// ResolveIdentity looks up the task bound to (projectID, identity).
	// Returns orchestrator.ErrTaskNotFound when no binding exists.
	ResolveIdentity(projectID, identity string) (*orchestrator.Task, error)
	// ListIdentitiesByTask returns every identity bound to taskID.
	ListIdentitiesByTask(taskID string) ([]string, error)
}

// ActionListStore provides the workspace-scoped read backing docs/plans/
// ingestion-identity.md PR-3 (B-3)'s BoidOpActionList. Deliberately separate
// from ActionStore (ListActionsByTask, per-task, embedded in TxStore) for the
// same reason CardStore/TaskIdentityStore are their own interfaces:
// this is a non-transactional READ used outside WithinTx — the same
// "TaskWorkflowService.TaskTriage CardStore" narrowing pattern
// (workflow.go), not TxStore.
type ActionListStore interface {
	// ListActionsSince returns the actions matching filter (oldest first)
	// plus the next cursor. See orchestrator.ListActionsSince's own doc
	// comment for the pagination/scoping contract.
	ListActionsSince(filter orchestrator.ActionListFilter) ([]*orchestrator.Action, string, error)
}

// TriggerRunStore backs docs/plans/ingestion-identity.md PR-4 (B-5)'s
// trigger_runs ledger read/writes — narrowed from *orchestrator.
// TaskRepository the same way ActionListStore/CardStore are (workflow.go).
// Deliberately non-transactional (unlike TxStore's WithinTx-scoped methods):
// each call is a standalone statement, matching this table's actual access
// pattern in trigger_loop.go (no create+link atomicity requirement the way
// PR-2's ResolveOrCapture has — a trigger_runs row's create and its later
// complete are two separate ticks by construction, so there is nothing to
// wrap in one transaction).
type TriggerRunStore interface {
	CreateTriggerRun(run *orchestrator.TriggerRun) error
	CompleteTriggerRun(id string, finishedAt time.Time, exitCode int) error
	ListInFlightTriggerRuns() ([]*orchestrator.TriggerRun, error)
	LatestTriggerRun(projectID, triggerName string) (*orchestrator.TriggerRun, error)
	// SetTriggerRunJobID / DeleteTriggerRun back the Opus review Blocker 1
	// insert-then-dispatch split — see orchestrator.CreateTriggerRun's own
	// doc comment for the full sequence.
	SetTriggerRunJobID(id, jobID string) error
	DeleteTriggerRun(id string) error
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
	CardStore
	JobStore
	// TaskIdentityStore is embedded so the drop side effect (I-6:
	// TaskWorkflowService.ApplyAction's WithinTx switch) can release a
	// dropped task's identity bindings atomically with the drop transition
	// itself, the same way park/attrs_set/child_added/child_specced apply
	// their own sidecar side effects inside the same transaction.
	TaskIdentityStore
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
