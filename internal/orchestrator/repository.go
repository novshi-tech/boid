package orchestrator

import (
	"database/sql"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)


type TaskRepository struct {
	db db.DBTX
}

func NewTaskRepository(db db.DBTX) *TaskRepository {
	return &TaskRepository{db: db}
}

func (r *TaskRepository) CreateTask(task *Task) error {
	return CreateTask(r.db, task)
}

func (r *TaskRepository) GetTask(id string) (*Task, error) {
	return GetTask(r.db, id)
}

func (r *TaskRepository) ListTasks(filter TaskFilter) ([]*Task, error) {
	return ListTasks(r.db, filter)
}

func (r *TaskRepository) UpdateTask(task *Task) error {
	return UpdateTask(r.db, task)
}

func (r *TaskRepository) DeleteTask(id string) error {
	conn, ok := r.db.(*sql.DB)
	if !ok {
		return DeleteTask(r.db, id)
	}
	return db.InTxDB(conn, func(tx db.DBTX) error {
		return DeleteTask(tx, id)
	})
}

func (r *TaskRepository) CreateAction(action *Action) error {
	return CreateAction(r.db, action)
}

func (r *TaskRepository) ListActionsByTask(taskID string) ([]*Action, error) {
	return ListActionsByTask(r.db, taskID)
}

type ProjectRepository struct {
	db db.DBTX
}

func NewProjectRepository(db db.DBTX) *ProjectRepository {
	return &ProjectRepository{db: db}
}

func (r *ProjectRepository) CreateProject(project *Project) error {
	return CreateProject(r.db, project)
}

func (r *ProjectRepository) GetProject(id string) (*Project, error) {
	return GetProject(r.db, id)
}

func (r *ProjectRepository) ListProjects() ([]*Project, error) {
	return ListProjects(r.db)
}

func (r *ProjectRepository) SetProjectWorkspace(projectID, workspaceID string) error {
	return SetProjectWorkspace(r.db, projectID, workspaceID)
}

func (r *ProjectRepository) ListWorkspaces() ([]*WorkspaceSummary, error) {
	return ListWorkspaces(r.db)
}

func (r *ProjectRepository) DeleteProject(id string) error {
	return DeleteProject(r.db, id)
}

// TaskGCStore handles GC of tasks and their related data.
type TaskGCStore struct {
	conn *sql.DB
}

func NewTaskGCStore(conn *sql.DB) *TaskGCStore {
	return &TaskGCStore{conn: conn}
}

func (s *TaskGCStore) GC(olderThan time.Duration, dryRun bool) (*GCResult, error) {
	var result *GCResult
	err := db.InTxDB(s.conn, func(dbtx db.DBTX) error {
		r, err := GCTasks(dbtx, []string{"done", "aborted"}, olderThan, dryRun)
		result = r
		return err
	})
	return result, err
}
