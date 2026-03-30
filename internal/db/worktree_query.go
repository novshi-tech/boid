package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/model"
)

func (d *DB) CreateWorktree(w *model.Worktree) error {
	if w.ID == "" {
		w.ID = uuid.New().String()
	}
	w.CreatedAt = time.Now().UTC()

	_, err := d.Conn.Exec(
		`INSERT INTO worktrees (id, task_id, project_id, path, branch, base_branch, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.TaskID, w.ProjectID, w.Path, w.Branch, w.BaseBranch, w.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert worktree: %w", err)
	}
	return nil
}

func (d *DB) GetWorktreeByTask(taskID string) (*model.Worktree, error) {
	row := d.Conn.QueryRow(
		`SELECT id, task_id, project_id, path, branch, base_branch, created_at, cleaned_at
		 FROM worktrees WHERE task_id = ?`, taskID,
	)
	return scanWorktree(row)
}

func (d *DB) MarkWorktreeCleaned(taskID string) error {
	now := time.Now().UTC()
	res, err := d.Conn.Exec(
		`UPDATE worktrees SET cleaned_at = ? WHERE task_id = ? AND cleaned_at IS NULL`,
		now, taskID,
	)
	if err != nil {
		return fmt.Errorf("mark worktree cleaned: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("no active worktree for task %s", taskID)
	}
	return nil
}

// ListActiveWorktrees returns worktrees that have not been cleaned up.
func (d *DB) ListActiveWorktrees() ([]*model.Worktree, error) {
	rows, err := d.Conn.Query(
		`SELECT id, task_id, project_id, path, branch, base_branch, created_at, cleaned_at
		 FROM worktrees WHERE cleaned_at IS NULL ORDER BY created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("list active worktrees: %w", err)
	}
	defer rows.Close()

	var wts []*model.Worktree
	for rows.Next() {
		w, err := scanWorktree(rows)
		if err != nil {
			return nil, err
		}
		wts = append(wts, w)
	}
	return wts, rows.Err()
}

func scanWorktree(s scanner) (*model.Worktree, error) {
	var w model.Worktree
	var cleanedAt sql.NullTime
	if err := s.Scan(&w.ID, &w.TaskID, &w.ProjectID, &w.Path, &w.Branch, &w.BaseBranch, &w.CreatedAt, &cleanedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan worktree: %w", err)
	}
	if cleanedAt.Valid {
		w.CleanedAt = &cleanedAt.Time
	}
	return &w, nil
}
