package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/model"
)

type TaskFilter struct {
	Status    string
	ProjectID string
}

func (d *DB) CreateTask(t *model.Task) error {
	if t.ID == "" {
		t.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = model.TaskStatusPending
	}
	if len(t.Payload) == 0 {
		t.Payload = json.RawMessage("{}")
	}

	_, err := d.Conn.Exec(
		`INSERT INTO tasks (id, project_id, remote_id, datasource_id, title, description, status, behavior, payload, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.RemoteID, t.DataSourceID, t.Title, t.Description, t.Status, t.Behavior, string(t.Payload), t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	return nil
}

func (d *DB) GetTask(id string) (*model.Task, error) {
	row := d.Conn.QueryRow(
		`SELECT id, project_id, remote_id, datasource_id, title, description, status, behavior, payload, created_at, updated_at FROM tasks WHERE id = ?`, id,
	)
	t, err := scanTask(row)
	if err != nil && len(id) >= 8 {
		// Try prefix match
		row = d.Conn.QueryRow(
			`SELECT id, project_id, remote_id, datasource_id, title, description, status, behavior, payload, created_at, updated_at FROM tasks WHERE id LIKE ?`, id+"%",
		)
		return scanTask(row)
	}
	return t, err
}

func (d *DB) ListTasks(filter TaskFilter) ([]*model.Task, error) {
	var conditions []string
	var args []any

	if filter.Status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.ProjectID != "" {
		conditions = append(conditions, "project_id = ?")
		args = append(args, filter.ProjectID)
	}

	query := `SELECT id, project_id, remote_id, datasource_id, title, description, status, behavior, payload, created_at, updated_at FROM tasks`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY created_at DESC"

	rows, err := d.Conn.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []*model.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (d *DB) UpdateTask(t *model.Task) error {
	t.UpdatedAt = time.Now().UTC()
	_, err := d.Conn.Exec(
		`UPDATE tasks SET status = ?, payload = ?, updated_at = ? WHERE id = ?`,
		t.Status, string(t.Payload), t.UpdatedAt, t.ID,
	)
	return err
}

func scanTask(s scanner) (*model.Task, error) {
	var t model.Task
	var payload string
	if err := s.Scan(&t.ID, &t.ProjectID, &t.RemoteID, &t.DataSourceID, &t.Title, &t.Description, &t.Status, &t.Behavior, &payload, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("task not found")
		}
		return nil, fmt.Errorf("scan task: %w", err)
	}
	t.Payload = json.RawMessage(payload)
	return &t, nil
}
