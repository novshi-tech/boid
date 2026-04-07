package orchestrator

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/db"
)

type TaskFilter struct {
	Status    string
	ProjectID string
}

func CreateTask(dbtx db.DBTX, t *Task) error {
	if t.ID == "" {
		t.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = TaskStatusPending
	}
	if len(t.Payload) == 0 {
		t.Payload = json.RawMessage("{}")
	}
	traitsJSON, err := marshalTraits(t.Traits)
	if err != nil {
		return fmt.Errorf("marshal traits: %w", err)
	}

	_, err = dbtx.Exec(
		`INSERT INTO tasks (id, project_id, remote_id, datasource_id, title, description, status, behavior, transition, traits, readonly, worktree, branch_prefix, base_branch, payload, auto_start, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.RemoteID, t.DataSourceID, t.Title, t.Description, t.Status, t.Behavior, t.Transition, traitsJSON, t.Readonly, t.Worktree, t.BranchPrefix, t.BaseBranch, string(t.Payload), t.AutoStart, t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	return nil
}

func GetTask(dbtx db.DBTX, id string) (*Task, error) {
	row := dbtx.QueryRow(
		`SELECT id, project_id, remote_id, datasource_id, title, description, status, behavior, transition, traits, readonly, worktree, branch_prefix, base_branch, payload, auto_start, created_at, updated_at FROM tasks WHERE id = ?`, id,
	)
	t, err := scanTask(row)
	if err != nil && len(id) >= 8 {
		// Try prefix match
		row = dbtx.QueryRow(
			`SELECT id, project_id, remote_id, datasource_id, title, description, status, behavior, transition, traits, readonly, worktree, branch_prefix, base_branch, payload, auto_start, created_at, updated_at FROM tasks WHERE id LIKE ?`, id+"%",
		)
		return scanTask(row)
	}
	return t, err
}

func ListTasks(dbtx db.DBTX, filter TaskFilter) ([]*Task, error) {
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

	query := `SELECT id, project_id, remote_id, datasource_id, title, description, status, behavior, transition, traits, readonly, worktree, branch_prefix, base_branch, payload, auto_start, created_at, updated_at FROM tasks`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY created_at DESC"

	rows, err := dbtx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func UpdateTask(dbtx db.DBTX, t *Task) error {
	t.UpdatedAt = time.Now().UTC()
	traitsJSON, err := marshalTraits(t.Traits)
	if err != nil {
		return fmt.Errorf("marshal traits: %w", err)
	}
	_, err = dbtx.Exec(
		`UPDATE tasks SET title = ?, description = ?, status = ?, transition = ?, traits = ?, readonly = ?, worktree = ?, branch_prefix = ?, base_branch = ?, payload = ?, updated_at = ? WHERE id = ?`,
		t.Title, t.Description, t.Status, t.Transition, traitsJSON, t.Readonly, t.Worktree, t.BranchPrefix, t.BaseBranch, string(t.Payload), t.UpdatedAt, t.ID,
	)
	return err
}

func CreateAction(dbtx db.DBTX, a *Action) error {
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

func ListActionsByTask(dbtx db.DBTX, taskID string) ([]*Action, error) {
	rows, err := dbtx.Query(
		`SELECT id, task_id, type, payload, created_at FROM actions WHERE task_id = ? ORDER BY created_at`, taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("list actions: %w", err)
	}
	defer rows.Close()

	var actions []*Action
	for rows.Next() {
		var a Action
		var payload string
		if err := rows.Scan(&a.ID, &a.TaskID, &a.Type, &payload, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan action: %w", err)
		}
		a.Payload = json.RawMessage(payload)
		actions = append(actions, &a)
	}
	return actions, rows.Err()
}

// GCResult holds the count of records affected by GC.
type GCResult struct {
	Tasks     int64
	Jobs      int64
	Actions   int64
	Worktrees int64
}

// GCTasks deletes terminal tasks older than olderThan and their related data
// (actions, jobs, worktrees). If dryRun is true, counts only without deleting.
// olderThan=0 disables the time filter (all matching tasks are affected).
// Must be called within a transaction for atomicity.
func GCTasks(dbtx db.DBTX, statuses []string, olderThan time.Duration, dryRun bool) (*GCResult, error) {
	if len(statuses) == 0 {
		return &GCResult{}, nil
	}

	ph := make([]string, len(statuses))
	for i := range statuses {
		ph[i] = "?"
	}
	placeholders := strings.Join(ph, ", ")

	var taskCond string
	var condArgs []any
	if olderThan > 0 {
		taskCond = `status IN (` + placeholders + `) AND (strftime('%s','now') - strftime('%s',updated_at)) > ?`
		condArgs = make([]any, len(statuses)+1)
		for i, s := range statuses {
			condArgs[i] = s
		}
		condArgs[len(statuses)] = int64(olderThan.Seconds())
	} else {
		taskCond = `status IN (` + placeholders + `)`
		condArgs = make([]any, len(statuses))
		for i, s := range statuses {
			condArgs[i] = s
		}
	}

	subquery := `SELECT id FROM tasks WHERE ` + taskCond
	result := &GCResult{}

	if dryRun {
		row := dbtx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE `+taskCond, condArgs...)
		if err := row.Scan(&result.Tasks); err != nil {
			return nil, fmt.Errorf("count tasks: %w", err)
		}
		for _, table := range []string{"actions", "jobs", "worktrees"} {
			row := dbtx.QueryRow(
				`SELECT COUNT(*) FROM `+table+` WHERE task_id IN (`+subquery+`)`,
				condArgs...,
			)
			var n int64
			if err := row.Scan(&n); err != nil {
				return nil, fmt.Errorf("count %s: %w", table, err)
			}
			switch table {
			case "actions":
				result.Actions = n
			case "jobs":
				result.Jobs = n
			case "worktrees":
				result.Worktrees = n
			}
		}
		return result, nil
	}

	for _, table := range []string{"actions", "jobs", "worktrees"} {
		res, err := dbtx.Exec(
			`DELETE FROM `+table+` WHERE task_id IN (`+subquery+`)`,
			condArgs...,
		)
		if err != nil {
			return nil, fmt.Errorf("delete %s: %w", table, err)
		}
		n, _ := res.RowsAffected()
		switch table {
		case "actions":
			result.Actions = n
		case "jobs":
			result.Jobs = n
		case "worktrees":
			result.Worktrees = n
		}
	}

	res, err := dbtx.Exec(`DELETE FROM tasks WHERE `+taskCond, condArgs...)
	if err != nil {
		return nil, fmt.Errorf("delete tasks: %w", err)
	}
	result.Tasks, _ = res.RowsAffected()
	return result, nil
}

// FindTaskByRemote returns the task matching the given remote_id and datasource_id,
// or nil if no matching task is found.
func FindTaskByRemote(dbtx db.DBTX, remoteID, datasourceID string) (*Task, error) {
	row := dbtx.QueryRow(
		`SELECT id, project_id, remote_id, datasource_id, title, description, status, behavior, transition, traits, readonly, worktree, branch_prefix, base_branch, payload, auto_start, created_at, updated_at FROM tasks WHERE remote_id = ? AND datasource_id = ?`,
		remoteID, datasourceID,
	)
	t, err := scanTask(row)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

func DeleteTask(dbtx db.DBTX, id string) error {
	if _, err := GetTask(dbtx, id); err != nil {
		return err
	}
	for _, table := range []string{"actions", "jobs", "worktrees"} {
		if _, err := dbtx.Exec(`DELETE FROM `+table+` WHERE task_id = ?`, id); err != nil {
			return fmt.Errorf("delete %s: %w", table, err)
		}
	}
	if _, err := dbtx.Exec(`DELETE FROM tasks WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	return nil
}

type taskScanner interface {
	Scan(dest ...any) error
}

func scanTask(s taskScanner) (*Task, error) {
	var t Task
	var payload string
	var traitsJSON string
	if err := s.Scan(&t.ID, &t.ProjectID, &t.RemoteID, &t.DataSourceID, &t.Title, &t.Description, &t.Status, &t.Behavior, &t.Transition, &traitsJSON, &t.Readonly, &t.Worktree, &t.BranchPrefix, &t.BaseBranch, &payload, &t.AutoStart, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("task not found")
		}
		return nil, fmt.Errorf("scan task: %w", err)
	}
	t.Payload = json.RawMessage(payload)
	traits, err := unmarshalTraits(traitsJSON)
	if err != nil {
		return nil, fmt.Errorf("unmarshal traits: %w", err)
	}
	t.Traits = traits
	return &t, nil
}

func marshalTraits(traits []string) (string, error) {
	if traits == nil {
		return "[]", nil
	}
	b, err := json.Marshal(traits)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalTraits(s string) ([]string, error) {
	if s == "" || s == "[]" {
		return nil, nil
	}
	var traits []string
	if err := json.Unmarshal([]byte(s), &traits); err != nil {
		return nil, err
	}
	return traits, nil
}
