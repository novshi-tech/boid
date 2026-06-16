package orchestrator

import (
	"database/sql"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func (r *TaskRepository) FindTaskByRemote(remoteID string) (*Task, error) {
	return FindTaskByRemote(r.db, remoteID)
}

func (r *TaskRepository) FindTaskByRef(ref, parentID string) (*Task, error) {
	return FindTaskByRef(r.db, ref, parentID)
}

func (r *TaskRepository) FindDependentTasks(_ string) ([]*Task, error) {
	return nil, nil
}

func (r *TaskRepository) ListChildren(parentID string) ([]*Task, error) {
	return ListChildren(r.db, parentID)
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
	conn              *sql.DB
	resolveProjectDir func(projectID string) (string, error)
	gitBin            string
	runtimesDir       string
	sandboxTmpDir     string
	// RuntimeReaper, when set, is called with each runtime directory path
	// before os.RemoveAll removes it. Use this to Reap docker resources that
	// may still be alive in the upstream daemon (safety net for jobs whose
	// cleanupSandboxAfterWait did not complete, e.g. daemon restart).
	RuntimeReaper func(runtimeDir string) error
}

func NewTaskGCStore(conn *sql.DB) *TaskGCStore {
	return &TaskGCStore{conn: conn}
}

// NewTaskGCStoreWithWorktree creates a TaskGCStore that also cleans up
// worktree directories on disk before deleting DB records.
// resolveProjectDir returns the project's work directory given its ID.
// gitBin is the path to the git binary; empty string defaults to "git".
// runtimesDir is the path to the runtimes root directory; empty string disables runtime cleanup.
func NewTaskGCStoreWithWorktree(conn *sql.DB, resolveProjectDir func(projectID string) (string, error), gitBin string, runtimesDir string) *TaskGCStore {
	return &TaskGCStore{
		conn:              conn,
		resolveProjectDir: resolveProjectDir,
		gitBin:            gitBin,
		runtimesDir:       runtimesDir,
	}
}

// WithRuntimeReaper sets a callback that is invoked with each runtime directory
// path before it is deleted. This allows the caller to Reap docker resources
// created by sandbox jobs (safety net when cleanupSandboxAfterWait didn't run,
// e.g. after a daemon restart).
func (s *TaskGCStore) WithRuntimeReaper(fn func(runtimeDir string) error) *TaskGCStore {
	s.RuntimeReaper = fn
	return s
}

// WithSandboxTmpDir enables safety-net cleanup of leaked /tmp/boid-* sandbox
// artifacts during GC. Pass the directory to scan (typically "/tmp"); empty
// string disables this cleanup.
func (s *TaskGCStore) WithSandboxTmpDir(dir string) *TaskGCStore {
	s.sandboxTmpDir = dir
	return s
}

func (s *TaskGCStore) gcGitBin() string {
	if s.gitBin != "" {
		return s.gitBin
	}
	return "git"
}

func (s *TaskGCStore) GC(olderThan time.Duration, dryRun bool) (*GCResult, error) {
	runtimesDeleted := 0
	if s.runtimesDir != "" && !dryRun {
		runtimesDeleted = s.cleanRuntimes(olderThan)
	}
	if s.resolveProjectDir != nil && !dryRun {
		s.cleanWorktrees(olderThan)
	}
	sandboxTmpDeleted := 0
	if s.sandboxTmpDir != "" && !dryRun {
		sandboxTmpDeleted = cleanSandboxTmp(s.sandboxTmpDir, olderThan)
	}

	var result *GCResult
	err := db.InTxDB(s.conn, func(dbtx db.DBTX) error {
		r, err := GCTasks(dbtx, []string{"done", "aborted"}, olderThan, dryRun)
		result = r
		return err
	})
	if err != nil {
		return nil, err
	}
	if !dryRun {
		result.Runtimes = int64(runtimesDeleted)
		result.SandboxTmp = int64(sandboxTmpDeleted)
	}
	return result, nil
}

// cleanRuntimes deletes runtime directories for GC target tasks.
// Errors are logged as warnings; failures do not block subsequent DB deletion.
// Returns the number of runtime directories successfully deleted.
func (s *TaskGCStore) cleanRuntimes(olderThan time.Duration) int {
	query := `
		SELECT DISTINCT j.runtime_id
		FROM jobs j
		JOIN tasks t ON t.id = j.task_id
		WHERE j.runtime_id != ''
		  AND t.status IN ('done', 'aborted')`
	var args []any
	if olderThan > 0 {
		query += ` AND t.updated_at < ?`
		args = append(args, time.Now().UTC().Add(-olderThan))
	}

	rows, err := s.conn.Query(query, args...)
	if err != nil {
		slog.Warn("gc runtimes: query failed", "error", err)
		return 0
	}
	defer rows.Close()

	var runtimeIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			slog.Warn("gc runtimes: scan failed", "error", err)
			return 0
		}
		runtimeIDs = append(runtimeIDs, id)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("gc runtimes: rows error", "error", err)
		return 0
	}

	count := 0
	for _, id := range runtimeIDs {
		dir := filepath.Join(s.runtimesDir, id)
		// Reap docker resources before removing the directory so the ledger
		// is still readable (safety net for jobs whose cleanupSandboxAfterWait
		// didn't complete, e.g. after a daemon restart).
		if s.RuntimeReaper != nil {
			if err := s.RuntimeReaper(dir); err != nil {
				slog.Warn("gc docker reap failed", "runtime_id", id, "error", err)
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("gc runtimes: remove failed", "runtime_id", id, "error", err)
			continue
		}
		slog.Info("gc runtime removed", "runtime_id", id)
		count++
	}
	return count
}

type gcWorktreeRecord struct {
	taskID    string
	projectID string
	path      string
	branch    string
}

// cleanWorktrees performs disk-level cleanup of worktrees for GC target tasks.
// Errors are logged as warnings; failures do not block subsequent DB deletion.
func (s *TaskGCStore) cleanWorktrees(olderThan time.Duration) {
	query := `
		SELECT w.task_id, w.project_id, w.path, w.branch
		FROM worktrees w
		JOIN tasks t ON t.id = w.task_id
		WHERE w.cleaned_at IS NULL
		  AND t.status IN ('done', 'aborted')`
	var args []any
	if olderThan > 0 {
		query += ` AND t.updated_at < ?`
		args = append(args, time.Now().UTC().Add(-olderThan))
	}

	rows, err := s.conn.Query(query, args...)
	if err != nil {
		slog.Warn("gc worktrees: query failed", "error", err)
		return
	}
	defer rows.Close()

	var wts []gcWorktreeRecord
	for rows.Next() {
		var r gcWorktreeRecord
		if err := rows.Scan(&r.taskID, &r.projectID, &r.path, &r.branch); err != nil {
			slog.Warn("gc worktrees: scan failed", "error", err)
			return
		}
		wts = append(wts, r)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("gc worktrees: rows error", "error", err)
		return
	}

	for _, w := range wts {
		projectDir, err := s.resolveProjectDir(w.projectID)
		if err != nil {
			slog.Warn("gc worktrees: resolve project dir failed", "task_id", w.taskID, "project_id", w.projectID, "error", err)
			continue
		}

		cmd := exec.Command(s.gcGitBin(), "-C", projectDir, "worktree", "remove", "--force", w.path)
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Warn("gc worktrees: git worktree remove failed, attempting manual cleanup",
				"task_id", w.taskID, "error", err, "output", strings.TrimSpace(string(out)))
			os.RemoveAll(w.path)
			exec.Command(s.gcGitBin(), "-C", projectDir, "worktree", "prune").Run()
		}

		cmd = exec.Command(s.gcGitBin(), "-C", projectDir, "branch", "-D", w.branch)
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Warn("gc worktrees: git branch -D failed",
				"task_id", w.taskID, "branch", w.branch, "error", err, "output", strings.TrimSpace(string(out)))
		}

		slog.Info("gc worktree removed", "task_id", w.taskID, "path", w.path)
	}
}
