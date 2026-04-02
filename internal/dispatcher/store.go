package dispatcher

import (
	"database/sql"
	"fmt"
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

	hasHookID, err := jobColumnExists(dbtx, "hook_id")
	if err != nil {
		return fmt.Errorf("detect jobs.hook_id: %w", err)
	}

	var query string
	var args []any
	if hasHookID {
		query = `INSERT INTO jobs (id, task_id, project_id, hook_id, handler_id, role, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
		args = []any{j.ID, j.TaskID, j.ProjectID, j.HandlerID, j.HandlerID, j.Role, j.Status, j.CreatedAt, j.UpdatedAt}
	} else {
		query = `INSERT INTO jobs (id, task_id, project_id, handler_id, role, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
		args = []any{j.ID, j.TaskID, j.ProjectID, j.HandlerID, j.Role, j.Status, j.CreatedAt, j.UpdatedAt}
	}

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

func UpdateJob(dbtx db.DBTX, j *Job) error {
	j.UpdatedAt = time.Now().UTC()
	_, err := dbtx.Exec(
		`UPDATE jobs SET status = ?, exit_code = ?, output = ?, updated_at = ? WHERE id = ?`,
		j.Status, j.ExitCode, j.Output, j.UpdatedAt, j.ID,
	)
	return err
}

func scanJob(s jobScanner) (*Job, error) {
	var j Job
	var exitCode sql.NullInt64
	if err := s.Scan(&j.ID, &j.TaskID, &j.ProjectID, &j.HandlerID, &j.Role, &j.Status, &exitCode, &j.Output, &j.CreatedAt, &j.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("job not found")
		}
		return nil, fmt.Errorf("scan job: %w", err)
	}
	if exitCode.Valid {
		j.ExitCode = int(exitCode.Int64)
	}
	return &j, nil
}

func jobSelectSQL(dbtx db.DBTX, suffix string) (string, error) {
	hasHookID, err := jobColumnExists(dbtx, "hook_id")
	if err != nil {
		return "", err
	}
	if hasHookID {
		return `SELECT id, task_id, project_id, COALESCE(NULLIF(handler_id, ''), hook_id) AS handler_id, role, status, exit_code, output, created_at, updated_at FROM jobs ` + suffix, nil
	}
	return `SELECT id, task_id, project_id, handler_id, role, status, exit_code, output, created_at, updated_at FROM jobs ` + suffix, nil
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
