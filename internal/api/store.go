package api

import (
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

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
