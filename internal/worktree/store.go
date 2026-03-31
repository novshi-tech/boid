package worktree

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/db"
)

type worktreeScanner interface {
	Scan(dest ...any) error
}

func CreateWorktree(dbtx db.DBTX, w *Worktree) error {
	if w.ID == "" {
		w.ID = uuid.New().String()
	}
	w.CreatedAt = time.Now().UTC()

	_, err := dbtx.Exec(
		`INSERT INTO worktrees (id, task_id, project_id, path, branch, base_branch, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.TaskID, w.ProjectID, w.Path, w.Branch, w.BaseBranch, w.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert worktree: %w", err)
	}
	return nil
}

func GetWorktreeByTask(dbtx db.DBTX, taskID string) (*Worktree, error) {
	row := dbtx.QueryRow(
		`SELECT id, task_id, project_id, path, branch, base_branch, created_at, cleaned_at
		 FROM worktrees WHERE task_id = ?`, taskID,
	)
	return scanWorktree(row)
}

func MarkWorktreeCleaned(dbtx db.DBTX, taskID string) error {
	now := time.Now().UTC()
	res, err := dbtx.Exec(
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

func ListActiveWorktrees(dbtx db.DBTX) ([]*Worktree, error) {
	rows, err := dbtx.Query(
		`SELECT id, task_id, project_id, path, branch, base_branch, created_at, cleaned_at
		 FROM worktrees WHERE cleaned_at IS NULL ORDER BY created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("list active worktrees: %w", err)
	}
	defer rows.Close()

	var wts []*Worktree
	for rows.Next() {
		w, err := scanWorktree(rows)
		if err != nil {
			return nil, err
		}
		wts = append(wts, w)
	}
	return wts, rows.Err()
}

func scanWorktree(s worktreeScanner) (*Worktree, error) {
	var w Worktree
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
