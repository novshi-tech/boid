package api

import (
	"context"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

type MetaStore interface {
	Get(id string) (*orchestrator.ProjectMeta, bool)
}

type DispatchCoordinator interface {
	DispatchAndAdvance(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) (*orchestrator.DispatchResult, error)
	DispatchEntryGates(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta) (*orchestrator.EntryGateResult, error)
}

type JobLifecycle interface {
	CompleteJob(jobID string, result JobCompletion)
	UnregisterJob(jobID string)
	CleanupTaskWindow(taskID string)
}

type BrokerRegistry interface {
	RegisterBrokerCommands(commands map[string]orchestrator.HostCommandSpec, builtinPolicies map[string]sandbox.BuiltinPolicy, projectID string) (*BrokerRegisterResponse, error)
}

type WorktreeCleaner interface {
	CleanupForTask(taskID, projectDir, newStatus string) error
}

type ProjectService interface {
	CreateProject(workDir string) (*orchestrator.Project, error)
	ListProjects(workspaceID string) ([]*orchestrator.Project, error)
	ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error)
	GetProject(id string) (*orchestrator.Project, error)
	SetProjectWorkspace(id, workspaceID string) (*orchestrator.Project, error)
	DeleteProject(id string) error
	ReloadProjects() (*ProjectReloadResult, error)
}

type TaskService interface {
	CreateTask(req CreateTaskRequest) (*orchestrator.Task, error)
	ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error)
	GetTask(id string) (*orchestrator.Task, error)
	GetTaskDetail(id string) (*TaskDetailView, error)
	UpdateTask(id string, req UpdateTaskRequest) (*orchestrator.Task, error)
	DeleteTask(id string, force bool) error
	ImportTasks(reqs []CreateTaskRequest) (*ImportResult, error)
	DuplicateTask(sourceID string, autoStart bool) (*orchestrator.Task, error)
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
	ListTasks(status string) ([]*orchestrator.Task, error)
	GetTaskDetail(id string) (*TaskDetailView, error)
	ListProjects() ([]*orchestrator.Project, error)
	ApplyAction(taskID string, actionType string) error
	DuplicateTask(id string) (string, error)
	ListJobs(status string) ([]JobWithContext, error)
	GetJob(id string) (*JobWithContext, error)
}

type WorkflowService interface {
	ApplyAction(ctx context.Context, taskID string, req ApplyActionRequest) (*ActionApplication, error)
	CompleteJob(ctx context.Context, jobID string, req JobDoneRequest) (*Job, error)
	TriggerDependents(ctx context.Context, taskID string)
}

type TaskStore interface {
	CreateTask(task *orchestrator.Task) error
	GetTask(id string) (*orchestrator.Task, error)
	ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error)
	UpdateTask(task *orchestrator.Task) error
	DeleteTask(id string) error
	FindTaskByRemote(remoteID, datasourceID string) (*orchestrator.Task, error)
	FindTaskByRef(ref, parentID string) (*orchestrator.Task, error)
	FindDependentTasks(taskID string) ([]*orchestrator.Task, error)
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
	GC(olderThan time.Duration, dryRun bool, ephemeral *bool) (*orchestrator.GCResult, error)
}

type GCService interface {
	Run(olderThan time.Duration, dryRun bool, ephemeral *bool) (*orchestrator.GCResult, error)
}
