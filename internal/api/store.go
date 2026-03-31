package api

import (
	"context"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type MetaStore interface {
	Get(id string) (*orchestrator.ProjectMeta, bool)
}

type TransitionResolver interface {
	Resolve(meta *orchestrator.ProjectMeta, behavior string) (*orchestrator.StateMachine, error)
}

type DispatchCoordinator interface {
	DispatchAndAdvance(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, behavior *orchestrator.TaskBehavior, sm *orchestrator.StateMachine) (*orchestrator.DispatchResult, error)
}

type JobLifecycle interface {
	CompleteJob(jobID string, result dispatcher.JobCompletionResult)
	UnregisterJob(jobID string)
	CleanupTaskWindow(taskID string)
}

type WorktreeCleaner interface {
	CleanupForTask(taskID, projectDir, newStatus string) error
}

type ProjectService interface {
	CreateProject(workDir string) (*orchestrator.Project, error)
	ListProjects(workspaceID string) ([]*orchestrator.Project, error)
	GetProject(id string) (*orchestrator.Project, error)
	DeleteProject(id string) error
	ReloadProjects() (*ProjectReloadResult, error)
}

type TaskService interface {
	CreateTask(req CreateTaskRequest) (*orchestrator.Task, error)
	ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error)
	GetTask(id string) (*orchestrator.Task, error)
}

type WebService interface {
	ListTasks(status string) ([]*orchestrator.Task, error)
	GetTaskDetail(id string) (*TaskDetailView, error)
	ListProjects() ([]*orchestrator.Project, error)
}

type WorkflowService interface {
	ApplyAction(ctx context.Context, taskID string, req ApplyActionRequest) (*ActionApplication, error)
	CompleteJob(ctx context.Context, jobID string, req JobDoneRequest) (*dispatcher.Job, error)
}

type TaskStore interface {
	CreateTask(task *orchestrator.Task) error
	GetTask(id string) (*orchestrator.Task, error)
	ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error)
	UpdateTask(task *orchestrator.Task) error
}

type ActionStore interface {
	CreateAction(action *orchestrator.Action) error
	ListActionsByTask(taskID string) ([]*orchestrator.Action, error)
}

type ProjectRepository interface {
	CreateProject(project *orchestrator.Project) error
	GetProject(id string) (*orchestrator.Project, error)
	ListProjects() ([]*orchestrator.Project, error)
	DeleteProject(id string) error
}

type JobStore interface {
	GetJob(id string) (*dispatcher.Job, error)
	ListJobsByTask(taskID string) ([]*dispatcher.Job, error)
	UpdateJob(job *dispatcher.Job) error
}

type TxStore interface {
	TaskStore
	ActionStore
	JobStore
}

type Transactor interface {
	WithinTx(func(TxStore) error) error
}
