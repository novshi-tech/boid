package orchestrator

import (
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
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

// SetProjectUpstreamURL updates a project's captured upstream_url. See the
// package-level function for the underlying statement.
func (r *ProjectRepository) SetProjectUpstreamURL(id, upstreamURL string) error {
	return SetProjectUpstreamURL(r.db, id, upstreamURL)
}

// AssignDefaultWorkspaceToUnlinked inserts a project_workspaces row pointing
// at workspaceID for every project that does not yet have one. Returns the
// number of rows inserted. See the package-level function for the underlying
// statement.
func (r *ProjectRepository) AssignDefaultWorkspaceToUnlinked(workspaceID string) (int, error) {
	return AssignDefaultWorkspaceToUnlinked(r.db, workspaceID)
}

// TaskGCStore handles GC of tasks and their related data.
type TaskGCStore struct {
	conn          *sql.DB
	runtimesDir   string
	sandboxTmpDir string
	// attachmentsRoot, when non-empty, is the data-home directory under which
	// per-task attachments live at `<root>/tasks/<id>/attachments`. GC
	// removes the per-task directory for tasks that have been in a terminal
	// state for olderThan. Empty disables this cleanup.
	attachmentsRoot string
	// RuntimeReaper, when set, is called with each runtime directory path
	// before os.RemoveAll removes it. Use this to Reap docker resources that
	// may still be alive in the upstream daemon (safety net for jobs whose
	// cleanupSandboxAfterWait did not complete, e.g. daemon restart).
	RuntimeReaper func(runtimeDir string) error
}

func NewTaskGCStore(conn *sql.DB) *TaskGCStore {
	return &TaskGCStore{conn: conn}
}

// WithRuntimesDir enables disk-level cleanup of per-sandbox runtime
// directories (`<dir>/<runtime_id>` and the git-gateway clone workspace dir
// `<dir>/<job_id>/workspace`) for GC target jobs. Empty disables runtime
// cleanup.
func (s *TaskGCStore) WithRuntimesDir(dir string) *TaskGCStore {
	s.runtimesDir = dir
	return s
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

// WithAttachmentsRoot enables disk-level cleanup of the per-task attachments
// directory tree rooted at `<dir>/tasks/<id>/attachments`. dir is the
// data-home (matches dataHomeFor in wire.go). Empty disables the cleanup.
func (s *TaskGCStore) WithAttachmentsRoot(dir string) *TaskGCStore {
	s.attachmentsRoot = dir
	return s
}

func (s *TaskGCStore) GC(olderThan time.Duration, dryRun bool) (*GCResult, error) {
	runtimesDeleted := 0
	if s.runtimesDir != "" && !dryRun {
		runtimesDeleted = s.cleanRuntimes(olderThan)
	}
	sandboxTmpDeleted := 0
	if s.sandboxTmpDir != "" && !dryRun {
		sandboxTmpDeleted = cleanSandboxTmp(s.sandboxTmpDir, olderThan)
	}
	if s.attachmentsRoot != "" && !dryRun {
		s.cleanTaskAttachments(olderThan)
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

// cleanRuntimes deletes runtime directories for GC target jobs: both the
// runtime_id-keyed sandbox scaffolding dir (`<RuntimesDir>/<runtime_id>`)
// and the job.id-keyed git-gateway clone workspace dir
// (`<RuntimesDir>/<job.id>/workspace`, see dispatcher.Runner.Dispatch's
// clone-mode path — the two use different directory naming schemes, so both
// must be checked). Covers task-bound jobs (GC'd via the owning task's
// terminal status, as before) and task-less jobs — ad-hoc `boid agent
// claude -p`/`boid exec` sessions with no task_id, which the previous INNER
// JOIN on tasks silently excluded forever regardless of the job's own
// status or age. Task-less jobs are instead GC'd by their own terminal
// status (completed/failed) and updated_at.
// Errors are logged as warnings; failures do not block subsequent DB deletion.
// Returns the number of runtime directories successfully deleted.
func (s *TaskGCStore) cleanRuntimes(olderThan time.Duration) int {
	query := `
		SELECT j.id, j.runtime_id
		FROM jobs j
		LEFT JOIN tasks t ON t.id = j.task_id
		WHERE (
		  (j.task_id IS NOT NULL AND t.status IN ('done', 'aborted'))
		  OR
		  (j.task_id IS NULL AND j.status IN ('completed', 'failed'))
		)`
	var args []any
	if olderThan > 0 {
		query += ` AND COALESCE(t.updated_at, j.updated_at) < ?`
		args = append(args, time.Now().UTC().Add(-olderThan))
	}

	rows, err := s.conn.Query(query, args...)
	if err != nil {
		slog.Warn("gc runtimes: query failed", "error", err)
		return 0
	}
	defer rows.Close()

	type gcJob struct {
		id        string
		runtimeID string
	}
	var jobs []gcJob
	for rows.Next() {
		var j gcJob
		if err := rows.Scan(&j.id, &j.runtimeID); err != nil {
			slog.Warn("gc runtimes: scan failed", "error", err)
			return 0
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("gc runtimes: rows error", "error", err)
		return 0
	}

	count := 0
	seen := make(map[string]bool)
	remove := func(dir string, reap bool) {
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return
		}
		// Reap docker resources before removing the directory so the ledger
		// is still readable (safety net for jobs whose cleanupSandboxAfterWait
		// didn't complete, e.g. after a daemon restart). Only the
		// runtime_id-keyed sandbox dir can hold docker state.
		if reap && s.RuntimeReaper != nil {
			if err := s.RuntimeReaper(dir); err != nil {
				slog.Warn("gc docker reap failed", "dir", dir, "error", err)
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("gc runtimes: remove failed", "dir", dir, "error", err)
			return
		}
		slog.Info("gc runtime removed", "dir", dir)
		count++
	}

	for _, j := range jobs {
		if j.runtimeID != "" {
			remove(filepath.Join(s.runtimesDir, j.runtimeID), true)
		}
		// job.id-keyed dir: houses the git-gateway clone workspace. Not
		// every job creates one (only clone-mode dispatch does), so this is
		// a no-op when absent.
		remove(filepath.Join(s.runtimesDir, j.id), false)
	}
	return count
}

// cleanTaskAttachments deletes the per-task data directory
// (`<attachmentsRoot>/tasks/<id>`) for tasks that have been in a terminal
// state for olderThan. The full per-task directory is removed — not just the
// attachments/ subdir — so any sibling data we add to that tree in the
// future is also covered. Errors are logged as warnings; failures do not
// block subsequent DB deletion. Mirrors the cleanRuntimes pattern.
//
// **In-flight safety**: the SELECT explicitly filters `t.status IN ('done',
// 'aborted')`, so attachments for executing/awaiting tasks (whose
// directories are live-bound into a running sandbox) are never touched.
func (s *TaskGCStore) cleanTaskAttachments(olderThan time.Duration) {
	if s.attachmentsRoot == "" {
		return
	}
	query := `
		SELECT t.id
		FROM tasks t
		WHERE t.status IN ('done', 'aborted')`
	var args []any
	if olderThan > 0 {
		query += ` AND t.updated_at < ?`
		args = append(args, time.Now().UTC().Add(-olderThan))
	}

	rows, err := s.conn.Query(query, args...)
	if err != nil {
		slog.Warn("gc attachments: query failed", "error", err)
		return
	}
	defer rows.Close()

	var taskIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			slog.Warn("gc attachments: scan failed", "error", err)
			return
		}
		taskIDs = append(taskIDs, id)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("gc attachments: rows error", "error", err)
		return
	}

	for _, id := range taskIDs {
		dir := filepath.Join(s.attachmentsRoot, "tasks", id)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("gc attachments: remove failed", "task_id", id, "error", err)
			continue
		}
		slog.Info("gc attachments removed", "task_id", id)
	}
}

