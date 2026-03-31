package dispatcher

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type jobScanner interface {
	Scan(dest ...any) error
}

func CreateJob(dbtx DBTX, j *Job) error {
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

	_, err := dbtx.Exec(
		`INSERT INTO jobs (id, task_id, project_id, handler_id, role, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.TaskID, j.ProjectID, j.HandlerID, j.Role, j.Status, j.CreatedAt, j.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert job: %w", err)
	}
	return nil
}

func GetJob(dbtx DBTX, id string) (*Job, error) {
	row := dbtx.QueryRow(
		`SELECT id, task_id, project_id, handler_id, role, status, exit_code, output, created_at, updated_at FROM jobs WHERE id = ?`, id,
	)
	return scanJob(row)
}

func ListJobsByTask(dbtx DBTX, taskID string) ([]*Job, error) {
	rows, err := dbtx.Query(
		`SELECT id, task_id, project_id, handler_id, role, status, exit_code, output, created_at, updated_at FROM jobs WHERE task_id = ? ORDER BY created_at`, taskID,
	)
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

func UpdateJob(dbtx DBTX, j *Job) error {
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
