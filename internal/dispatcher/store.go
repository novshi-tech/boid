package dispatcher

import (
	"database/sql"
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

	columns := []string{"id", "task_id", "project_id"}
	args := []any{j.ID, j.TaskID, j.ProjectID}

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
	Status      string
	Interactive *bool // nil = no filter
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
	var exitCode sql.NullInt64
	var interactive, tty sql.NullInt64
	var executionState sql.NullString
	if err := s.Scan(&j.ID, &j.TaskID, &j.ProjectID, &j.HandlerID, &j.Role, &j.RuntimeID, &interactive, &tty, &j.Status, &exitCode, &j.Output, &executionState, &j.CreatedAt, &j.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("job not found")
		}
		return nil, fmt.Errorf("scan job: %w", err)
	}
	if exitCode.Valid {
		j.ExitCode = int(exitCode.Int64)
	}
	j.Interactive = interactive.Valid && interactive.Int64 != 0
	j.TTY = tty.Valid && tty.Int64 != 0
	if executionState.Valid {
		j.ExecutionState = executionState.String
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

	return fmt.Sprintf(
		`SELECT id, task_id, project_id, %s AS handler_id, role, %s, %s, %s, status, exit_code, output, %s, created_at, updated_at FROM jobs %s`,
		handlerExpr,
		runtimeExpr,
		interactiveExpr,
		ttyExpr,
		executionStateExpr,
		suffix,
	), nil
}

type jobColumns struct {
	hasHookID          bool
	hasRuntimeID       bool
	hasInteractive     bool
	hasTTY             bool
	hasExecutionState  bool
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
	return jobColumns{
		hasHookID:         hasHookID,
		hasRuntimeID:      hasRuntimeID,
		hasInteractive:    hasInteractive,
		hasTTY:            hasTTY,
		hasExecutionState: hasExecutionState,
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
