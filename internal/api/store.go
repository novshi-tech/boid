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

// ReplayHookResult is the output of a hook replay.
type ReplayHookResult struct {
	Task        *orchestrator.Task        `json:"task"`
	FiredEvents []orchestrator.FiredEvent `json:"fired_events,omitempty"`
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
	// RemoveWorkspace deletes slug (DELETE /api/workspaces/{slug}).
	// *StatusError{400} for the reserved default slug, {404} unknown slug.
	RemoveWorkspace(slug string) error

	// ExportWorkspace returns slug's raw yaml body (the marshaled
	// WorkspaceMeta, deliberately with no top-level "slug" key — the slug is
	// already known from the URL this backs) and its current revision (GET
	// /api/workspaces/{slug}/export, docs/plans/workspace-db-consolidation.md
	// PR5 Step A). *StatusError{404} when slug is unknown.
	ExportWorkspace(slug string) (yamlBytes []byte, revision string, err error)
	// ImportWorkspace inserts (mode="create-only") or upserts
	// (mode="replace") slug's workspace meta from an import body (POST
	// /api/workspaces/import?mode=..., PR5 Step B/C). Unlike
	// CreateWorkspace/UpdateWorkspace, mode="replace" is a true upsert: it
	// creates slug if absent and overwrites it wholesale (last-write-wins,
	// no If-Match) if present. *StatusError{409} for mode="create-only"
	// against an existing slug, {400} for an invalid slug, an unrecognized
	// mode value, or an unknown host_commands reference.
	ImportWorkspace(slug string, meta *orchestrator.WorkspaceMeta, mode string) (*WorkspaceDetail, error)
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
	// Home reports the workspace home directory's on-disk size (docs/plans/
	// home-workspace-volume.md Phase 4 PR5). Populated only by GET
	// /api/workspaces/{slug} (WorkspaceHandler.Show) — Create/Update/Import
	// leave it nil, since computing it means walking a directory tree and
	// none of those callers need it. nil (omitted from the JSON body) when
	// the handler was not wired with a RuntimesDir to resolve it from.
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

type ImportError struct {
	Line     int    `json:"line"`
	RemoteID string `json:"remote_id"`
	Error    string `json:"error"`
}

type ImportResult struct {
	Created int           `json:"created"`
	Skipped int           `json:"skipped"`
	Errors  []ImportError `json:"errors"`
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

// StartSessionRequest is the body of POST /api/sessions and
// POST /api/projects/{id}/sessions. Phase 3-d (PR1) introduced the session
// concept as a first-class JobKind so the WebUI [New Session] dialog and
// the `boid agent` CLI share one daemon entry point.
type StartSessionRequest struct {
	// ProjectID names the project whose traits the session inherits. For the
	// project-scoped route (`/api/projects/{id}/sessions`) it is taken from
	// the URL and the body field is ignored.
	ProjectID string `json:"project_id"`

	// HarnessType selects the agent adapter. Must be one of "claude",
	// "codex", or "opencode". The historical "shell" session variant was
	// retired after the git gateway cutover — `boid exec -p <project> -- bash`
	// runs the shell adapter through the same Runner.Dispatch() with an
	// interactive PTY, so there is no session use case left for it.
	// sessionDispatcherAdapter.StartSession rejects any other value at the
	// API boundary.
	HarnessType string `json:"harness_type"`

	// Instruction is the optional bootstrap prompt for the first turn. Empty
	// leaves the harness to pick its default (no positional for session mode
	// on claude, since /boid-task is meaningless without a task.yaml).
	Instruction string `json:"instruction,omitempty"`

	// Readonly, when true, mounts the project workspace read-only. Sessions
	// default to writable (developer ergonomics > fail-safety).
	Readonly bool `json:"readonly,omitempty"`

	// Model overrides the harness binary's default model selection.
	Model string `json:"model,omitempty"`

	// DisplayName is the human-readable session label persisted to
	// jobs.display_name. Empty falls back to "<harness> session".
	DisplayName string `json:"display_name,omitempty"`
}

// StartSessionResult is the response shape for POST /api/sessions and the
// project-scoped variant.
type StartSessionResult struct {
	JobID     string `json:"job_id"`
	AttachURL string `json:"attach_url"`
}

// SessionDispatcher launches a session job (claude / codex / opencode under
// a HarnessAdapter) and returns the runtime job id.
type SessionDispatcher interface {
	StartSession(ctx context.Context, req StartSessionRequest) (*StartSessionResult, error)
}

// StartExecRequest is the body of POST /api/projects/{id}/exec. `boid exec`
// used to be a client-side-only path (the CLI built its own SandboxRuntimeInfo
// and syscall.Exec'd straight into the sandbox launcher), which is exactly why
// it never picked up the git gateway cutover's Dispatch()-only wiring
// (registerGatewayToken / GatewayURL / GatewayCloneURL) — see
// docs/plans/git-gateway-cutover.md. This request type is the daemon-side
// entry point that routes exec through the same Runner.Dispatch() path as
// every session, so any future dispatch-time wiring lands on both by
// construction instead of needing a second, easy-to-forget call site.
//
// Unlike sessions (fixed harness_type, agent-driven argv), exec runs an
// arbitrary user-supplied argv with no HarnessAdapter agent — see
// dispatcher.BuildExecJobSpec, which forces HarnessType="shell" underneath.
type StartExecRequest struct {
	// ProjectID is taken from the URL for the project-scoped route; there is
	// no top-level /api/exec (every exec is inherently project-scoped —
	// `boid exec -p <ref> -- argv...`), so the handler always fills this in
	// from chi.URLParam before it reaches the dispatcher.
	ProjectID string `json:"project_id"`

	// Argv is the literal program + arguments to run inside the sandbox.
	// Required, non-empty.
	Argv []string `json:"argv"`

	// Readonly, when true, mounts the project workspace read-only. Exec
	// defaults to writable, matching the CLI's --readonly flag default.
	Readonly bool `json:"readonly,omitempty"`

	// Interactive requests a PTY-backed sandbox. The CLI computes this from
	// isatty(stdin) && isatty(stdout) (see cmd/exec.go) — both, not stdin
	// alone, because a PTY is only correct when the whole terminal round-trip
	// is real; `boid exec -- cmd | grep pattern` must NOT get a PTY even
	// though its own stdin is a real terminal, or the PTY's line-discipline
	// framing would corrupt the piped bytes grep receives. false selects the
	// plain-pipe transport (see runtime_local_linux.go's non-interactive
	// branch and its StdinForward stdin-piping addition).
	Interactive bool `json:"interactive,omitempty"`

	// DisplayName is the human-readable label persisted to jobs.display_name.
	// Empty falls back to argv[0] (dispatcher.BuildExecJobSpec).
	DisplayName string `json:"display_name,omitempty"`
}

// StartExecResult is the response shape for POST /api/projects/{id}/exec.
type StartExecResult struct {
	JobID     string `json:"job_id"`
	AttachURL string `json:"attach_url"`
}

// ExecDispatcher launches a JobKindExec job (arbitrary argv, shell harness,
// no HarnessAdapter agent) through Runner.Dispatch() and returns the runtime
// job id. Implemented by internal/server's sessionDispatcherAdapter.
type ExecDispatcher interface {
	StartExec(ctx context.Context, req StartExecRequest) (*StartExecResult, error)
}
