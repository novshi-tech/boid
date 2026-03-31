package orchestrator

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/model"
)

type TaskFilter struct {
	Status    string
	ProjectID string
}

func CreateTask(dbtx db.DBTX, t *model.Task) error {
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

	_, err := dbtx.Exec(
		`INSERT INTO tasks (id, project_id, remote_id, datasource_id, title, description, status, behavior, payload, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.RemoteID, t.DataSourceID, t.Title, t.Description, t.Status, t.Behavior, string(t.Payload), t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	return nil
}

func GetTask(dbtx db.DBTX, id string) (*model.Task, error) {
	row := dbtx.QueryRow(
		`SELECT id, project_id, remote_id, datasource_id, title, description, status, behavior, payload, created_at, updated_at FROM tasks WHERE id = ?`, id,
	)
	t, err := scanTask(row)
	if err != nil && len(id) >= 8 {
		// Try prefix match
		row = dbtx.QueryRow(
			`SELECT id, project_id, remote_id, datasource_id, title, description, status, behavior, payload, created_at, updated_at FROM tasks WHERE id LIKE ?`, id+"%",
		)
		return scanTask(row)
	}
	return t, err
}

func ListTasks(dbtx db.DBTX, filter TaskFilter) ([]*model.Task, error) {
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

	rows, err := dbtx.Query(query, args...)
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

func UpdateTask(dbtx db.DBTX, t *model.Task) error {
	t.UpdatedAt = time.Now().UTC()
	_, err := dbtx.Exec(
		`UPDATE tasks SET status = ?, payload = ?, updated_at = ? WHERE id = ?`,
		t.Status, string(t.Payload), t.UpdatedAt, t.ID,
	)
	return err
}

func CreateAction(dbtx db.DBTX, a *model.Action) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	a.CreatedAt = time.Now().UTC()
	if len(a.Payload) == 0 {
		a.Payload = json.RawMessage("{}")
	}

	_, err := dbtx.Exec(
		`INSERT INTO actions (id, task_id, type, payload, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		a.ID, a.TaskID, a.Type, string(a.Payload), a.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert action: %w", err)
	}
	return nil
}

func ListActionsByTask(dbtx db.DBTX, taskID string) ([]*model.Action, error) {
	rows, err := dbtx.Query(
		`SELECT id, task_id, type, payload, created_at FROM actions WHERE task_id = ? ORDER BY created_at`, taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("list actions: %w", err)
	}
	defer rows.Close()

	var actions []*model.Action
	for rows.Next() {
		var a model.Action
		var payload string
		if err := rows.Scan(&a.ID, &a.TaskID, &a.Type, &payload, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan action: %w", err)
		}
		a.Payload = json.RawMessage(payload)
		actions = append(actions, &a)
	}
	return actions, rows.Err()
}

type taskScanner interface {
	Scan(dest ...any) error
}

func scanTask(s taskScanner) (*model.Task, error) {
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
