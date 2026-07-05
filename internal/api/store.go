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

type WorktreeCleaner interface {
	CleanupForTask(taskID, projectDir, newStatus string) error
	// SweepChildBranches deletes the boid/<id8> branches associated with the
	// given child task IDs. Called by finalizeTerminal once a supervisor task
	// reaches a terminal state — its children's branches were retained through
	// CleanupForTask so the supervisor could merge them into the base branch;
	// now that the supervisor is done, the merged refs are safe to drop.
	// Branches that don't exist (already deleted, never created) are silently
	// skipped. Non-boid/* branches (root-task base branches) are never deleted.
	SweepChildBranches(projectDir string, taskIDs []string) error
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
	// "codex", "opencode", or "shell" — the shell harness drops the user
	// into an interactive bash inside the project sandbox (`boid agent shell`).
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
