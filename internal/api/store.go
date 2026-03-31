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
