package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/model"
)

func (d *DB) CreateJob(j *model.Job) error {
	if j.ID == "" {
		j.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	j.CreatedAt = now
	j.UpdatedAt = now
	if j.Status == "" {
		j.Status = model.JobStatusRunning
	}

	if j.Role == "" {
		j.Role = "hook"
	}

	_, err := d.Conn.Exec(
		`INSERT INTO jobs (id, task_id, project_id, handler_id, role, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.TaskID, j.ProjectID, j.HandlerID, j.Role, j.Status, j.CreatedAt, j.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert job: %w", err)
	}
	return nil
}

func (d *DB) GetJob(id string) (*model.Job, error) {
	row := d.Conn.QueryRow(
		`SELECT id, task_id, project_id, handler_id, role, status, exit_code, output, created_at, updated_at FROM jobs WHERE id = ?`, id,
	)
	return scanJob(row)
}

func (d *DB) ListJobsByTask(taskID string) ([]*model.Job, error) {
	rows, err := d.Conn.Query(
		`SELECT id, task_id, project_id, handler_id, role, status, exit_code, output, created_at, updated_at FROM jobs WHERE task_id = ? ORDER BY created_at`, taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*model.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (d *DB) UpdateJob(j *model.Job) error {
	j.UpdatedAt = time.Now().UTC()
	_, err := d.Conn.Exec(
		`UPDATE jobs SET status = ?, exit_code = ?, output = ?, updated_at = ? WHERE id = ?`,
		j.Status, j.ExitCode, j.Output, j.UpdatedAt, j.ID,
	)
	return err
}

func scanJob(s scanner) (*model.Job, error) {
	var j model.Job
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
