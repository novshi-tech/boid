package db

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/model"
)

func (tx *Tx) UpdateTask(t *model.Task) error {
	t.UpdatedAt = time.Now().UTC()
	_, err := tx.tx.Exec(
		`UPDATE tasks SET status = ?, payload = ?, updated_at = ? WHERE id = ?`,
		t.Status, string(t.Payload), t.UpdatedAt, t.ID,
	)
	return err
}

func (tx *Tx) CreateAction(a *model.Action) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	a.CreatedAt = time.Now().UTC()
	if len(a.Payload) == 0 {
		a.Payload = json.RawMessage("{}")
	}

	_, err := tx.tx.Exec(
		`INSERT INTO actions (id, task_id, type, payload, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		a.ID, a.TaskID, a.Type, string(a.Payload), a.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert action: %w", err)
	}
	return nil
}

func (tx *Tx) UpdateJob(j *model.Job) error {
	j.UpdatedAt = time.Now().UTC()
	_, err := tx.tx.Exec(
		`UPDATE jobs SET status = ?, exit_code = ?, output = ?, updated_at = ? WHERE id = ?`,
		j.Status, j.ExitCode, j.Output, j.UpdatedAt, j.ID,
	)
	return err
}
