package api

import (
	"context"
	"syscall"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

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
