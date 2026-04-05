package dispatcher

import "github.com/novshi-tech/boid/internal/db"

type JobRepository struct {
	db db.DBTX
}

func NewJobRepository(db db.DBTX) *JobRepository {
	return &JobRepository{db: db}
}

func (r *JobRepository) CreateJob(job *Job) error {
	return CreateJob(r.db, job)
}

func (r *JobRepository) GetJob(id string) (*Job, error) {
	return GetJob(r.db, id)
}

func (r *JobRepository) ListJobsByTask(taskID string) ([]*Job, error) {
	return ListJobsByTask(r.db, taskID)
}

func (r *JobRepository) ListJobsFiltered(filter JobFilter) ([]*Job, error) {
	return ListJobsFiltered(r.db, filter)
}

func (r *JobRepository) UpdateJob(job *Job) error {
	return UpdateJob(r.db, job)
}

type WorktreeRepository struct {
	db db.DBTX
}

func NewWorktreeRepository(db db.DBTX) *WorktreeRepository {
	return &WorktreeRepository{db: db}
}

func (r *WorktreeRepository) CreateWorktree(worktree *Worktree) error {
	return CreateWorktree(r.db, worktree)
}

func (r *WorktreeRepository) GetWorktreeByTask(taskID string) (*Worktree, error) {
	return GetWorktreeByTask(r.db, taskID)
}

func (r *WorktreeRepository) MarkWorktreeCleaned(taskID string) error {
	return MarkWorktreeCleaned(r.db, taskID)
}

func (r *WorktreeRepository) ListActiveWorktrees() ([]*Worktree, error) {
	return ListActiveWorktrees(r.db)
}
