package server

import (
	"database/sql"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type apiTxStore struct {
	tasks   *orchestrator.TaskRepository
	actions *orchestrator.TaskRepository
	jobs    *dispatcher.JobRepository
}

func (s apiTxStore) CreateTask(task *orchestrator.Task) error {
	return s.tasks.CreateTask(task)
}

func (s apiTxStore) GetTask(id string) (*orchestrator.Task, error) {
	return s.tasks.GetTask(id)
}

func (s apiTxStore) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return s.tasks.ListTasks(filter)
}

func (s apiTxStore) UpdateTask(task *orchestrator.Task) error {
	return s.tasks.UpdateTask(task)
}

func (s apiTxStore) CreateAction(action *orchestrator.Action) error {
	return s.actions.CreateAction(action)
}

func (s apiTxStore) ListActionsByTask(taskID string) ([]*orchestrator.Action, error) {
	return s.actions.ListActionsByTask(taskID)
}

func (s apiTxStore) GetJob(id string) (*dispatcher.Job, error) {
	return s.jobs.GetJob(id)
}

func (s apiTxStore) ListJobsByTask(taskID string) ([]*dispatcher.Job, error) {
	return s.jobs.ListJobsByTask(taskID)
}

func (s apiTxStore) UpdateJob(job *dispatcher.Job) error {
	return s.jobs.UpdateJob(job)
}

type apiTransactor struct {
	db *sql.DB
}

func (t apiTransactor) WithinTx(fn func(api.TxStore) error) error {
	return db.InTxDB(t.db, func(tx db.DBTX) error {
		store := apiTxStore{
			tasks:   orchestrator.NewTaskRepository(tx),
			actions: orchestrator.NewTaskRepository(tx),
			jobs:    dispatcher.NewJobRepository(tx),
		}
		return fn(store)
	})
}
