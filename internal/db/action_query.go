package db

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/model"
)

func (d *DB) CreateAction(a *model.Action) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	a.CreatedAt = time.Now().UTC()
	if len(a.Payload) == 0 {
		a.Payload = json.RawMessage("{}")
	}

	_, err := d.Conn.Exec(
		`INSERT INTO actions (id, task_id, type, payload, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		a.ID, a.TaskID, a.Type, string(a.Payload), a.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert action: %w", err)
	}
	return nil
}

func (d *DB) ListActionsByTask(taskID string) ([]*model.Action, error) {
	rows, err := d.Conn.Query(
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
