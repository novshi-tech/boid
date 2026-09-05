package orchestrator

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

type TaskRepository struct {
	db db.DBTX
	// metaResolver backs CreateAction's internal-signal ingest decision. nil
	// disables ingest entirely — CreateAction still writes the action row as
	// before. Set via SetMetaProjectResolver post-construction rather than a
	// NewTaskRepository parameter, since most call sites never use it.
	metaResolver MetaProjectResolver
}

func NewTaskRepository(db db.DBTX) *TaskRepository {
	return &TaskRepository{db: db}
}

// SetMetaProjectResolver wires the metaproject lookup CreateAction's ingest
// step needs — see the metaResolver field's own doc comment.
func (r *TaskRepository) SetMetaProjectResolver(resolver MetaProjectResolver) {
	r.metaResolver = resolver
}

func (r *TaskRepository) CreateTask(task *Task) error {
	return CreateTask(r.db, task)
}

func (r *TaskRepository) GetTask(id string) (*Task, error) {
	return GetTask(r.db, id)
}

// GetTaskStatus satisfies api.TaskStatusReader — the narrow read
// api.TaskAppService.WaitTaskTerminal polls with. See GetTaskStatus (store.go)
// for why it is not GetTask.
func (r *TaskRepository) GetTaskStatus(id string) (TaskStatus, error) {
	return GetTaskStatus(r.db, id)
}

func (r *TaskRepository) ListTasks(filter TaskFilter) ([]*Task, error) {
	return ListTasks(r.db, filter)
}

func (r *TaskRepository) UpdateTask(task *Task) error {
	return UpdateTask(r.db, task)
}

// TouchTaskUpdatedAt satisfies api.TaskUpdatedAtToucher.
func (r *TaskRepository) TouchTaskUpdatedAt(id string) error {
	return TouchTaskUpdatedAt(r.db, id)
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

func (r *TaskRepository) FindTaskByRef(ref, parentID, projectID string) (*Task, error) {
	return FindTaskByRef(r.db, ref, parentID, projectID)
}

func (r *TaskRepository) ListChildren(parentID string) ([]*Task, error) {
	return ListChildren(r.db, parentID)
}

// CreateAction persists action, then — within the SAME transaction —
// ingests it into the target card's workspace inbox when eligible. Same
// dual-mode shape as DeleteTask above: nests inside an already-open tx when
// r.db is one, or opens its own spanning transaction when r.db is a raw
// *sql.DB, so the INSERT and the ingest can never commit independently.
func (r *TaskRepository) CreateAction(ctx context.Context, action *Action) error {
	conn, ok := r.db.(*sql.DB)
	if !ok {
		return CreateAction(ctx, r.db, action, r.metaResolver)
	}
	return db.InTxDB(conn, func(tx db.DBTX) error {
		return CreateAction(ctx, tx, action, r.metaResolver)
	})
}

func (r *TaskRepository) ListActionsByTask(taskID string) ([]*Action, error) {
	return ListActionsByTask(r.db, taskID)
}

// ListActionsSince is the workspace-scoped action_list read.
func (r *TaskRepository) ListActionsSince(filter ActionListFilter) ([]*Action, string, error) {
	return ListActionsSince(r.db, filter)
}

func (r *TaskRepository) UpsertTaskTriage(tt *CardAttrs) error {
	return UpsertTaskTriage(r.db, tt)
}

func (r *TaskRepository) GetTaskTriage(taskID string) (*CardAttrs, error) {
	return GetTaskTriage(r.db, taskID)
}

func (r *TaskRepository) ListTaskTriageByTaskIDs(taskIDs []string) (map[string]*CardAttrs, error) {
	return ListTaskTriageByTaskIDs(r.db, taskIDs)
}

func (r *TaskRepository) DeleteTaskTriage(taskID string) error {
	return DeleteTaskTriage(r.db, taskID)
}

func (r *TaskRepository) ParkedFrom(taskID string) (TaskStatus, error) {
	return ParkedFrom(r.db, taskID)
}

// LinkIdentity / UnlinkIdentity / UnlinkAllForTask / ResolveIdentity /
// ListIdentitiesByTask are thin wrappers over task_identity.go's identity index.
func (r *TaskRepository) LinkIdentity(projectID, identity, taskID string) error {
	return LinkIdentity(r.db, projectID, identity, taskID)
}

func (r *TaskRepository) UnlinkIdentity(projectID, identity string) error {
	return UnlinkIdentity(r.db, projectID, identity)
}

func (r *TaskRepository) UnlinkAllForTask(taskID string) error {
	return UnlinkAllForTask(r.db, taskID)
}

func (r *TaskRepository) ResolveIdentity(projectID, identity string) (*Task, error) {
	return ResolveIdentity(r.db, projectID, identity)
}

func (r *TaskRepository) ListIdentitiesByTask(taskID string) ([]string, error) {
	return ListIdentitiesByTask(r.db, taskID)
}

// CreateTriggerRun / CompleteTriggerRun / ListInFlightTriggerRuns /
// LatestTriggerRun are thin wrappers over trigger_run.go's trigger_runs ledger.
func (r *TaskRepository) CreateTriggerRun(run *TriggerRun) error {
	return CreateTriggerRun(r.db, run)
}

func (r *TaskRepository) CompleteTriggerRun(id string, finishedAt time.Time, exitCode int) error {
	return CompleteTriggerRun(r.db, id, finishedAt, exitCode)
}

func (r *TaskRepository) ListInFlightTriggerRuns() ([]*TriggerRun, error) {
	return ListInFlightTriggerRuns(r.db)
}

func (r *TaskRepository) LatestTriggerRun(projectID, triggerName string) (*TriggerRun, error) {
	return LatestTriggerRun(r.db, projectID, triggerName)
}

// SetTriggerRunJobID / DeleteTriggerRun back the insert-then-dispatch split
// — see trigger_run.go's own doc comments.
func (r *TaskRepository) SetTriggerRunJobID(id, jobID string) error {
	return SetTriggerRunJobID(r.db, id, jobID)
}

func (r *TaskRepository) DeleteTriggerRun(id string) error {
	return DeleteTriggerRun(r.db, id)
}

// IngestSignals / GetSignalCursor / ListSignals / ClaimSignals / AckSignals /
// HasPendingSignals wrap signal_store.go's inbox store.
//
// IngestSignals and ClaimSignals each need their SQL statements to run as
// one transaction, so — unlike every other wrapper on this type — they
// follow DeleteTask's pattern above rather than being one-line delegators:
// when r.db is a raw *sql.DB, they open their own spanning transaction.
func (r *TaskRepository) IngestSignals(workspaceID, service, connector string, rows []SignalIngestRow) error {
	conn, ok := r.db.(*sql.DB)
	if !ok {
		return IngestSignals(r.db, workspaceID, service, connector, rows)
	}
	return db.InTxDB(conn, func(tx db.DBTX) error {
		return IngestSignals(tx, workspaceID, service, connector, rows)
	})
}

func (r *TaskRepository) GetSignalCursor(workspaceID, service, connector string) (string, error) {
	return GetSignalCursor(r.db, workspaceID, service, connector)
}

func (r *TaskRepository) ListSignals(filter SignalFilter) ([]*Signal, error) {
	return ListSignals(r.db, filter)
}

func (r *TaskRepository) ClaimSignals(workspaceID string, limit, maxAttempts int) ([]*Signal, error) {
	conn, ok := r.db.(*sql.DB)
	if !ok {
		return ClaimSignals(r.db, workspaceID, limit, maxAttempts)
	}
	var claimed []*Signal
	err := db.InTxDB(conn, func(tx db.DBTX) error {
		c, err := ClaimSignals(tx, workspaceID, limit, maxAttempts)
		if err != nil {
			return err
		}
		claimed = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

func (r *TaskRepository) ClaimSignalIDs(workspaceID string, ids []string) error {
	return ClaimSignalIDs(r.db, workspaceID, ids)
}

func (r *TaskRepository) AckSignals(workspaceID string, ids []string) error {
	return AckSignals(r.db, workspaceID, ids)
}

func (r *TaskRepository) HasPendingSignals(workspaceID string, maxAttempts int) (bool, error) {
	return HasPendingSignals(r.db, workspaceID, maxAttempts)
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

// AssignWorkspaceIfExists atomically checks-then-assigns. See the
// package-level function's doc comment. r.db must be a *sql.DB — every
// production wiring path constructs ProjectRepository with the daemon's
// single *sql.DB handle.
func (r *ProjectRepository) AssignWorkspaceIfExists(projectID, workspaceID string) error {
	conn, ok := r.db.(*sql.DB)
	if !ok {
		return fmt.Errorf("AssignWorkspaceIfExists: repository is not backed by a *sql.DB (got %T)", r.db)
	}
	return AssignWorkspaceIfExists(conn, projectID, workspaceID)
}

func (r *ProjectRepository) ListWorkspaces() ([]*WorkspaceSummary, error) {
	return ListWorkspaces(r.db)
}

// ListProjectWorkspaceReferences returns project_workspaces membership
// directly (see the package-level function's doc comment for why this
// differs from ListWorkspaces and who still needs it).
func (r *ProjectRepository) ListProjectWorkspaceReferences() ([]*WorkspaceSummary, error) {
	return ListProjectWorkspaceReferences(r.db)
}

// GetWorkspaceSummary returns a single workspace's summary (project count +
// revision). See the package-level function's doc comment for the
// os.ErrNotExist contract.
func (r *ProjectRepository) GetWorkspaceSummary(slug string) (*WorkspaceSummary, error) {
	return GetWorkspaceSummary(r.db, slug)
}

// WorkspaceExists reports whether slug refers to an existing workspaces table row.
func (r *ProjectRepository) WorkspaceExists(slug string) (bool, error) {
	return WorkspaceExists(r.db, slug)
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
	conn           *sql.DB
	runtimesDir    string
	transcriptsDir string
	sandboxTmpDir  string
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

// WithTranscriptsDir enables disk-level cleanup of the persistent
// transcript/diagnostics root (`<dir>/<runtime_id>`, holding transcript.log
// and diagnostics.json), keyed by the same runtime_id as WithRuntimesDir but
// a separate (persistent-volume) directory tree. Empty disables this
// cleanup, so those files accumulate forever.
func (s *TaskGCStore) WithTranscriptsDir(dir string) *TaskGCStore {
	s.transcriptsDir = dir
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
	if (s.runtimesDir != "" || s.transcriptsDir != "") && !dryRun {
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
		r, err := GCTasks(dbtx, terminalTaskStatusStrings(), olderThan, dryRun)
		if err != nil {
			return err
		}
		result = r
		// trigger_runs has no other retention — purge it in the same
		// transaction and schedule rather than a separate GC pass.
		n, err := GCTriggerRuns(dbtx, olderThan, dryRun)
		if err != nil {
			return err
		}
		result.TriggerRuns = n
		// Signal inbox GC rides the same transaction and schedule.
		sn, err := GCSignals(dbtx, olderThan, dryRun)
		if err != nil {
			return err
		}
		result.Signals = sn
		return nil
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

// cleanRuntimes deletes runtime directories for GC target jobs: the
// runtime_id-keyed sandbox scaffolding dir (`<RuntimesDir>/<runtime_id>`),
// its sibling in the persistent transcript root (`<TranscriptsDir>/
// <runtime_id>`), and the job.id-keyed git-gateway clone workspace dir
// (`<RuntimesDir>/<job.id>/workspace`) — the two use different directory
// naming schemes, so both must be checked. Covers both task-bound jobs
// (GC'd via the owning task's terminal status) and task-less ad-hoc jobs
// (GC'd by their own terminal status and updated_at). Errors are logged as
// warnings; failures do not block subsequent DB deletion. Returns the
// number of directories successfully deleted.
func (s *TaskGCStore) cleanRuntimes(olderThan time.Duration) int {
	// Uses the same terminal set as GCTasks (terminalStatusSQLList).
	query := `
		SELECT j.id, j.runtime_id
		FROM jobs j
		LEFT JOIN tasks t ON t.id = j.task_id
		WHERE (
		  (j.task_id IS NOT NULL AND t.status IN (` + terminalStatusSQLList + `))
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
		// is still readable. Only the runtime_id-keyed sandbox dir can hold
		// docker state.
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
			if s.runtimesDir != "" {
				remove(filepath.Join(s.runtimesDir, j.runtimeID), true)
			}
			// Persistent transcript-root sibling: never holds docker state.
			if s.transcriptsDir != "" {
				remove(filepath.Join(s.transcriptsDir, j.runtimeID), false)
			}
		}
		// job.id-keyed dir: houses the git-gateway clone workspace, only
		// created by clone-mode dispatch, so a no-op when absent.
		if s.runtimesDir != "" {
			remove(filepath.Join(s.runtimesDir, j.id), false)
		}
	}
	return count
}

// cleanTaskAttachments deletes the per-task data directory
// (`<attachmentsRoot>/tasks/<id>`) for tasks that have been in a terminal
// state for olderThan. The full per-task directory is removed, not just the
// attachments/ subdir, so future sibling data is also covered. Errors are
// logged as warnings; failures do not block subsequent DB deletion.
func (s *TaskGCStore) cleanTaskAttachments(olderThan time.Duration) {
	if s.attachmentsRoot == "" {
		return
	}
	query := `
		SELECT t.id
		FROM tasks t
		WHERE t.status IN (` + terminalStatusSQLList + `)`
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
