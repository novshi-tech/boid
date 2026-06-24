package dispatcher

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/db"
)

type jobScanner interface {
	Scan(dest ...any) error
}

func CreateJob(dbtx db.DBTX, j *Job) error {
	if j.ID == "" {
		j.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	j.CreatedAt = now
	j.UpdatedAt = now
	if j.Status == "" {
		j.Status = JobStatusRunning
	}
	if j.Role == "" {
		j.Role = "hook"
	}

	cols, err := inspectJobColumns(dbtx)
	if err != nil {
		return fmt.Errorf("inspect jobs columns: %w", err)
	}

	var taskID any
	if j.TaskID != "" {
		taskID = j.TaskID
	}
	columns := []string{"id", "task_id", "project_id"}
	args := []any{j.ID, taskID, j.ProjectID}

	if cols.hasHookID {
		columns = append(columns, "hook_id")
		args = append(args, j.HandlerID)
	}
	columns = append(columns, "handler_id", "role", "status")
	args = append(args, j.HandlerID, j.Role, j.Status)

	if cols.hasRuntimeID {
		columns = append(columns, "runtime_id")
		args = append(args, j.RuntimeID)
	}
	if cols.hasInteractive {
		columns = append(columns, "interactive")
		args = append(args, boolToInt(j.Interactive))
	}
	if cols.hasTTY {
		columns = append(columns, "tty")
		args = append(args, boolToInt(j.TTY))
	}
	if cols.hasExecutionState {
		columns = append(columns, "execution_state")
		args = append(args, j.ExecutionState)
	}
	if cols.hasDisplayName {
		columns = append(columns, "display_name")
		args = append(args, j.DisplayName)
	}

	columns = append(columns, "created_at", "updated_at")
	args = append(args, j.CreatedAt, j.UpdatedAt)

	query := fmt.Sprintf(
		`INSERT INTO jobs (%s) VALUES (%s)`,
		joinColumns(columns),
		placeholders(len(columns)),
	)

	_, err = dbtx.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("insert job: %w", err)
	}
	return nil
}

func GetJob(dbtx db.DBTX, id string) (*Job, error) {
	selectSQL, err := jobSelectSQL(dbtx, `WHERE id = ?`)
	if err != nil {
		return nil, fmt.Errorf("build get job query: %w", err)
	}
	row := dbtx.QueryRow(selectSQL, id)
	return scanJob(row)
}

func ListJobsByTask(dbtx db.DBTX, taskID string) ([]*Job, error) {
	selectSQL, err := jobSelectSQL(dbtx, `WHERE task_id = ? ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("build list jobs query: %w", err)
	}
	rows, err := dbtx.Query(selectSQL, taskID)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// JobFilter specifies optional filters for listing jobs globally.
type JobFilter struct {
	Status       string
	Interactive  *bool // nil = no filter
	TasklessOnly bool  // true = only jobs where task_id IS NULL
}

// ListJobsFiltered returns jobs across all tasks matching the given filter.
func ListJobsFiltered(dbtx db.DBTX, filter JobFilter) ([]*Job, error) {
	cols, err := inspectJobColumns(dbtx)
	if err != nil {
		return nil, fmt.Errorf("inspect jobs columns: %w", err)
	}

	var conditions []string
	var args []any
	if filter.Status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.Interactive != nil && cols.hasInteractive {
		conditions = append(conditions, "interactive = ?")
		args = append(args, boolToInt(*filter.Interactive))
	}
	if filter.TasklessOnly {
		conditions = append(conditions, "task_id IS NULL")
	}

	suffix := "ORDER BY created_at"
	if len(conditions) > 0 {
		suffix = "WHERE " + strings.Join(conditions, " AND ") + " ORDER BY created_at"
	}

	selectSQL, err := jobSelectSQL(dbtx, suffix)
	if err != nil {
		return nil, fmt.Errorf("build list jobs filtered query: %w", err)
	}
	rows, err := dbtx.Query(selectSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs filtered: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// MarkStaleJobsFailed marks all running jobs as failed.
// Call this on server startup to clean up jobs left in running state from a previous crash or restart.
func MarkStaleJobsFailed(dbtx db.DBTX) error {
	_, err := dbtx.Exec(
		`UPDATE jobs SET status = ?, exit_code = -1, updated_at = ? WHERE status = ?`,
		string(JobStatusFailed), time.Now().UTC(), string(JobStatusRunning),
	)
	return err
}

// MarkStaleExecutingTasksAborted transitions all tasks in "executing" status to
// "aborted" and records a daemon_shutdown abort action for each. Call this on
// server startup after MarkStaleJobsFailed. Returns the number of tasks transitioned.
func MarkStaleExecutingTasksAborted(conn *sql.DB) (int, error) {
	return markStaleTasksAborted(conn, "executing")
}

// MarkStaleAwaitingTasksAborted does the same for tasks left in "awaiting"
// status from a previous crash or restart. After a restart no agent is parked
// in the (purely in-memory) BlockingAskRegistry, so every awaiting task is a
// zombie with no live agent behind it — reclaim it. It carries the same
// daemon_shutdown code as the executing path, so the startup auto-reopen sweep
// (FindDaemonShutdownAbortedTasks) restarts it and the agent re-asks if needed.
func MarkStaleAwaitingTasksAborted(conn *sql.DB) (int, error) {
	return markStaleTasksAborted(conn, "awaiting")
}

// markStaleTasksAborted aborts every task currently in fromStatus, recording a
// daemon_shutdown abort action (from_status = fromStatus) for each.
func markStaleTasksAborted(conn *sql.DB, fromStatus string) (int, error) {
	var count int
	err := db.InTxDB(conn, func(tx db.DBTX) error {
		rows, err := tx.Query(`SELECT id FROM tasks WHERE status = ?`, fromStatus)
		if err != nil {
			return fmt.Errorf("query %s tasks: %w", fromStatus, err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("scan task id: %w", err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate %s tasks: %w", fromStatus, err)
		}

		const abortPayload = `{"code":"daemon_shutdown","message":"daemon が再起動されたため中断されました。 起動時に自動 reopen されます。"}`
		now := time.Now().UTC()
		for _, id := range ids {
			if _, err := tx.Exec(
				`INSERT INTO actions (id, task_id, type, payload, from_status, to_status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				uuid.New().String(), id, "abort", abortPayload, fromStatus, "aborted", now,
			); err != nil {
				return fmt.Errorf("insert abort action for task %s: %w", id, err)
			}
			if _, err := tx.Exec(
				`UPDATE tasks SET status = 'aborted', updated_at = ? WHERE id = ?`,
				now, id,
			); err != nil {
				return fmt.Errorf("update task status for %s: %w", id, err)
			}
		}
		count = len(ids)
		return nil
	})
	return count, err
}

// FindDaemonShutdownAbortedTasks returns IDs of tasks currently in aborted
// status whose most recent aborted-transition action carries
// payload.code == "daemon_shutdown". Use this on daemon startup to
// auto-reopen tasks that were interrupted by the previous shutdown.
//
// "Most recent" means the latest action with to_status='aborted' for that
// task (ordered by created_at desc). If a task was aborted by
// daemon_shutdown and then aborted again later for another reason, the
// later code wins and the task is NOT returned — matching the intuition
// that only freshly-shutdown tasks deserve auto-reopen.
func FindDaemonShutdownAbortedTasks(conn *sql.DB) ([]string, error) {
	rows, err := conn.Query(`SELECT id FROM tasks WHERE status = 'aborted'`)
	if err != nil {
		return nil, fmt.Errorf("query aborted tasks: %w", err)
	}
	var abortedIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan aborted id: %w", err)
		}
		abortedIDs = append(abortedIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate aborted tasks: %w", err)
	}

	var result []string
	for _, id := range abortedIDs {
		var payload []byte
		err := conn.QueryRow(
			`SELECT payload FROM actions WHERE task_id = ? AND to_status = 'aborted' ORDER BY created_at DESC LIMIT 1`,
			id,
		).Scan(&payload)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("query latest abort action for %s: %w", id, err)
		}
		var p struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			continue
		}
		if p.Code == "daemon_shutdown" {
			result = append(result, id)
		}
	}
	return result, nil
}

func UpdateJob(dbtx db.DBTX, j *Job) error {
	j.UpdatedAt = time.Now().UTC()

	cols, err := inspectJobColumns(dbtx)
	if err != nil {
		return fmt.Errorf("inspect jobs columns: %w", err)
	}

	assignments := []string{
		"status = ?",
		"exit_code = ?",
		"output = ?",
	}
	args := []any{j.Status, j.ExitCode, j.Output}

	if cols.hasRuntimeID {
		assignments = append(assignments, "runtime_id = ?")
		args = append(args, j.RuntimeID)
	}
	if cols.hasInteractive {
		assignments = append(assignments, "interactive = ?")
		args = append(args, boolToInt(j.Interactive))
	}
	if cols.hasTTY {
		assignments = append(assignments, "tty = ?")
		args = append(args, boolToInt(j.TTY))
	}
	if cols.hasExecutionState {
		assignments = append(assignments, "execution_state = ?")
		args = append(args, j.ExecutionState)
	}
	if cols.hasDisplayName {
		assignments = append(assignments, "display_name = ?")
		args = append(args, j.DisplayName)
	}

	assignments = append(assignments, "updated_at = ?")
	args = append(args, j.UpdatedAt, j.ID)

	_, err = dbtx.Exec(
		fmt.Sprintf(`UPDATE jobs SET %s WHERE id = ?`, joinAssignments(assignments)),
		args...,
	)
	return err
}

func scanJob(s jobScanner) (*Job, error) {
	var j Job
	var taskID sql.NullString
	var exitCode sql.NullInt64
	var interactive, tty sql.NullInt64
	var executionState, displayName sql.NullString
	if err := s.Scan(&j.ID, &taskID, &j.ProjectID, &j.HandlerID, &j.Role, &j.RuntimeID, &interactive, &tty, &j.Status, &exitCode, &j.Output, &executionState, &displayName, &j.CreatedAt, &j.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("job not found")
		}
		return nil, fmt.Errorf("scan job: %w", err)
	}
	j.TaskID = taskID.String
	if exitCode.Valid {
		j.ExitCode = int(exitCode.Int64)
	}
	j.Interactive = interactive.Valid && interactive.Int64 != 0
	j.TTY = tty.Valid && tty.Int64 != 0
	if executionState.Valid {
		j.ExecutionState = executionState.String
	}
	if displayName.Valid {
		j.DisplayName = displayName.String
	}
	return &j, nil
}

func jobSelectSQL(dbtx db.DBTX, suffix string) (string, error) {
	cols, err := inspectJobColumns(dbtx)
	if err != nil {
		return "", err
	}

	handlerExpr := "handler_id"
	if cols.hasHookID {
		handlerExpr = "COALESCE(NULLIF(handler_id, ''), hook_id)"
	}
	runtimeExpr := `'' AS runtime_id`
	if cols.hasRuntimeID {
		runtimeExpr = "runtime_id"
	}
	interactiveExpr := "0 AS interactive"
	if cols.hasInteractive {
		interactiveExpr = "interactive"
	}
	ttyExpr := "0 AS tty"
	if cols.hasTTY {
		ttyExpr = "tty"
	}
	executionStateExpr := `'' AS execution_state`
	if cols.hasExecutionState {
		executionStateExpr = "execution_state"
	}
	displayNameExpr := `'' AS display_name`
	if cols.hasDisplayName {
		displayNameExpr = "display_name"
	}

	return fmt.Sprintf(
		`SELECT id, task_id, project_id, %s AS handler_id, role, %s, %s, %s, status, exit_code, output, %s, %s, created_at, updated_at FROM jobs %s`,
		handlerExpr,
		runtimeExpr,
		interactiveExpr,
		ttyExpr,
		executionStateExpr,
		displayNameExpr,
		suffix,
	), nil
}

type jobColumns struct {
	hasHookID         bool
	hasRuntimeID      bool
	hasInteractive    bool
	hasTTY            bool
	hasExecutionState bool
	hasDisplayName    bool
}

func inspectJobColumns(dbtx db.DBTX) (jobColumns, error) {
	hasHookID, err := jobColumnExists(dbtx, "hook_id")
	if err != nil {
		return jobColumns{}, fmt.Errorf("detect jobs.hook_id: %w", err)
	}
	hasRuntimeID, err := jobColumnExists(dbtx, "runtime_id")
	if err != nil {
		return jobColumns{}, fmt.Errorf("detect jobs.runtime_id: %w", err)
	}
	hasInteractive, err := jobColumnExists(dbtx, "interactive")
	if err != nil {
		return jobColumns{}, fmt.Errorf("detect jobs.interactive: %w", err)
	}
	hasTTY, err := jobColumnExists(dbtx, "tty")
	if err != nil {
		return jobColumns{}, fmt.Errorf("detect jobs.tty: %w", err)
	}
	hasExecutionState, err := jobColumnExists(dbtx, "execution_state")
	if err != nil {
		return jobColumns{}, fmt.Errorf("detect jobs.execution_state: %w", err)
	}
	hasDisplayName, err := jobColumnExists(dbtx, "display_name")
	if err != nil {
		return jobColumns{}, fmt.Errorf("detect jobs.display_name: %w", err)
	}
	return jobColumns{
		hasHookID:         hasHookID,
		hasRuntimeID:      hasRuntimeID,
		hasInteractive:    hasInteractive,
		hasTTY:            hasTTY,
		hasExecutionState: hasExecutionState,
		hasDisplayName:    hasDisplayName,
	}, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func joinColumns(columns []string) string {
	if len(columns) == 0 {
		return ""
	}
	return joinWithSeparator(columns, ", ")
}

func joinAssignments(assignments []string) string {
	if len(assignments) == 0 {
		return ""
	}
	return joinWithSeparator(assignments, ", ")
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	values := make([]string, n)
	for i := range values {
		values[i] = "?"
	}
	return joinWithSeparator(values, ", ")
}

func joinWithSeparator(values []string, sep string) string {
	if len(values) == 0 {
		return ""
	}
	out := values[0]
	for _, value := range values[1:] {
		out += sep + value
	}
	return out
}

func jobColumnExists(dbtx db.DBTX, column string) (bool, error) {
	rows, err := dbtx.Query(`PRAGMA table_info(jobs)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			typeName  string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typeName, &notNull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
