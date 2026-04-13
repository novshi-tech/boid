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

// taskSelectCols は tasks テーブルの基本カラム一覧（テーブル別名 t を使用）。
const taskSelectCols = `t.id, t.project_id, t.remote_id, t.datasource_id, t.title, t.description,` +
	` t.status, t.behavior, t.traits, t.readonly, t.worktree,` +
	` t.branch_prefix, t.base_branch, t.payload, t.auto_start, t.depends_on_payload,` +
	` t.ref, t.parent_id, t.created_at, t.updated_at`

// taskChildCountCols は子タスク数を集計するサブクエリカラム群（テーブル別名 t を前提）。
const taskChildCountCols = `` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status = 'done'),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status = 'aborted'),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status NOT IN ('done', 'aborted'))`

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

	if len(t.DependsOn) > 0 {
		if err := detectCyclicDependency(dbtx, t.ID, t.DependsOn); err != nil {
			return err
		}
	}

	_, err = dbtx.Exec(
		`INSERT INTO tasks (id, project_id, remote_id, datasource_id, title, description, status, behavior, traits, readonly, worktree, branch_prefix, base_branch, payload, auto_start, depends_on_payload, ref, parent_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.RemoteID, t.DataSourceID, t.Title, t.Description, t.Status, t.Behavior, traitsJSON, t.Readonly, t.Worktree, t.BranchPrefix, t.BaseBranch, string(t.Payload), t.AutoStart, t.DependsOnPayload, t.Ref, t.ParentID, t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	for _, depID := range t.DependsOn {
		if _, err := dbtx.Exec(
			`INSERT INTO task_dependencies (task_id, depends_on) VALUES (?, ?)`,
			t.ID, depID,
		); err != nil {
			return fmt.Errorf("insert task dependency %s: %w", depID, err)
		}
	}
	return nil
}

// detectCyclicDependency は BFS で依存グラフを走査し、newTaskID が
// dependsOn の推移的依存に含まれるかを検出する。
func detectCyclicDependency(dbtx db.DBTX, newTaskID string, dependsOn []string) error {
	visited := make(map[string]bool)
	queue := make([]string, len(dependsOn))
	copy(queue, dependsOn)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current == newTaskID {
			return fmt.Errorf("circular dependency detected: task %s", newTaskID)
		}
		if visited[current] {
			continue
		}
		visited[current] = true
		deps, err := loadDependencyIDs(dbtx, current)
		if err != nil {
			return fmt.Errorf("detect cycle: %w", err)
		}
		queue = append(queue, deps...)
	}
	return nil
}

func loadDependencyIDs(dbtx db.DBTX, taskID string) ([]string, error) {
	rows, err := dbtx.Query(
		`SELECT depends_on FROM task_dependencies WHERE task_id = ?`, taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("load dependency ids: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan dependency id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FindDependentTasks は taskID に依存している pending 状態のタスクを返す。
func FindDependentTasks(dbtx db.DBTX, taskID string) ([]*Task, error) {
	rows, err := dbtx.Query(`
		SELECT DISTINCT `+taskSelectCols+`, `+taskChildCountCols+`
		FROM tasks t
		INNER JOIN task_dependencies td ON t.id = td.task_id
		WHERE td.depends_on = ? AND t.status = ?
		ORDER BY t.created_at`,
		taskID, string(TaskStatusPending),
	)
	if err != nil {
		return nil, fmt.Errorf("find dependent tasks: %w", err)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if err := loadTaskDependencies(dbtx, t); err != nil {
			return nil, err
		}
	}
	return tasks, nil
}

func loadTaskDependencies(dbtx db.DBTX, t *Task) error {
	rows, err := dbtx.Query(
		`SELECT depends_on FROM task_dependencies WHERE task_id = ? ORDER BY depends_on`, t.ID,
	)
	if err != nil {
		return fmt.Errorf("load task dependencies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var dep string
		if err := rows.Scan(&dep); err != nil {
			return fmt.Errorf("scan task dependency: %w", err)
		}
		t.DependsOn = append(t.DependsOn, dep)
	}
	return rows.Err()
}

func GetTask(dbtx db.DBTX, id string) (*Task, error) {
	row := dbtx.QueryRow(
		`SELECT `+taskSelectCols+`, `+taskChildCountCols+` FROM tasks t WHERE t.id = ?`, id,
	)
	t, err := scanTask(row)
	if err != nil && len(id) >= 8 {
		// Try prefix match
		row = dbtx.QueryRow(
			`SELECT `+taskSelectCols+`, `+taskChildCountCols+` FROM tasks t WHERE t.id LIKE ?`, id+"%",
		)
		t, err = scanTask(row)
	}
	if err != nil {
		return nil, err
	}
	if err := loadTaskDependencies(dbtx, t); err != nil {
		return nil, err
	}
	return t, nil
}

func ListTasks(dbtx db.DBTX, filter TaskFilter) ([]*Task, error) {
	var conditions []string
	var args []any

	// "open" は特殊フィルタ: 自身が open 状態 OR 閉じているが open な子を持つ（グループヘッダー救済）
	if filter.Status == "open" {
		conditions = append(conditions, `(t.status NOT IN ('done', 'aborted') OR `+
			`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status NOT IN ('done', 'aborted')) > 0)`)
	} else if filter.Status != "" {
		conditions = append(conditions, "t.status = ?")
		args = append(args, filter.Status)
	}
	if filter.ProjectID != "" {
		conditions = append(conditions, "t.project_id = ?")
		args = append(args, filter.ProjectID)
	}

	query := `SELECT ` + taskSelectCols + `, ` + taskChildCountCols + ` FROM tasks t`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY t.created_at DESC"

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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if err := loadTaskDependencies(dbtx, t); err != nil {
			return nil, err
		}
	}
	return tasks, nil
}

func UpdateTask(dbtx db.DBTX, t *Task) error {
	t.UpdatedAt = time.Now().UTC()
	traitsJSON, err := marshalTraits(t.Traits)
	if err != nil {
		return fmt.Errorf("marshal traits: %w", err)
	}
	_, err = dbtx.Exec(
		`UPDATE tasks SET title = ?, description = ?, status = ?, traits = ?, readonly = ?, worktree = ?, branch_prefix = ?, base_branch = ?, payload = ?, updated_at = ? WHERE id = ?`,
		t.Title, t.Description, t.Status, traitsJSON, t.Readonly, t.Worktree, t.BranchPrefix, t.BaseBranch, string(t.Payload), t.UpdatedAt, t.ID,
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
		`INSERT INTO actions (id, task_id, type, payload, from_status, to_status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.TaskID, a.Type, string(a.Payload), string(a.FromStatus), string(a.ToStatus), a.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert action: %w", err)
	}
	return nil
}

func ListActionsByTask(dbtx db.DBTX, taskID string) ([]*Action, error) {
	rows, err := dbtx.Query(
		`SELECT id, task_id, type, payload, from_status, to_status, created_at FROM actions WHERE task_id = ? ORDER BY created_at`, taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("list actions: %w", err)
	}
	defer rows.Close()

	var actions []*Action
	for rows.Next() {
		var a Action
		var payload, fromStatus, toStatus string
		if err := rows.Scan(&a.ID, &a.TaskID, &a.Type, &payload, &fromStatus, &toStatus, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan action: %w", err)
		}
		a.Payload = json.RawMessage(payload)
		a.FromStatus = TaskStatus(fromStatus)
		a.ToStatus = TaskStatus(toStatus)
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
	Runtimes  int64
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
		taskCond = `status IN (` + placeholders + `) AND updated_at < ?`
		condArgs = make([]any, len(statuses)+1)
		for i, s := range statuses {
			condArgs[i] = s
		}
		condArgs[len(statuses)] = time.Now().UTC().Add(-olderThan)
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
		row = dbtx.QueryRow(
			`SELECT COUNT(DISTINCT runtime_id) FROM jobs WHERE runtime_id != '' AND task_id IN (`+subquery+`)`,
			condArgs...,
		)
		if err := row.Scan(&result.Runtimes); err != nil {
			return nil, fmt.Errorf("count runtimes: %w", err)
		}
		return result, nil
	}

	depArgs := append(condArgs, condArgs...)
	if _, err := dbtx.Exec(
		`DELETE FROM task_dependencies WHERE task_id IN (`+subquery+`) OR depends_on IN (`+subquery+`)`,
		depArgs...,
	); err != nil {
		return nil, fmt.Errorf("delete task_dependencies: %w", err)
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
		`SELECT `+taskSelectCols+`, `+taskChildCountCols+` FROM tasks t WHERE t.remote_id = ? AND t.datasource_id = ?`,
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

// FindTaskByRef returns the task matching the given ref within the given parent scope,
// or nil if no matching task is found.
// If ref is a UUID, the task is looked up by id directly (backward compatibility).
func FindTaskByRef(dbtx db.DBTX, ref, parentID string) (*Task, error) {
	if ref == "" {
		return nil, nil
	}
	// UUID refs are looked up by task id for backward compatibility.
	if isUUID(ref) {
		t, err := GetTask(dbtx, ref)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				return nil, nil
			}
			return nil, err
		}
		return t, nil
	}
	row := dbtx.QueryRow(
		`SELECT `+taskSelectCols+`, `+taskChildCountCols+` FROM tasks t WHERE t.ref = ? AND t.parent_id = ?`,
		ref, parentID,
	)
	t, err := scanTask(row)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, nil
		}
		return nil, err
	}
	if err := loadTaskDependencies(dbtx, t); err != nil {
		return nil, err
	}
	return t, nil
}

func DeleteTask(dbtx db.DBTX, id string) error {
	if _, err := GetTask(dbtx, id); err != nil {
		return err
	}
	if _, err := dbtx.Exec(
		`DELETE FROM task_dependencies WHERE task_id = ? OR depends_on = ?`, id, id,
	); err != nil {
		return fmt.Errorf("delete task_dependencies: %w", err)
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
	if err := s.Scan(
		&t.ID, &t.ProjectID, &t.RemoteID, &t.DataSourceID, &t.Title, &t.Description,
		&t.Status, &t.Behavior, &traitsJSON, &t.Readonly, &t.Worktree,
		&t.BranchPrefix, &t.BaseBranch, &payload, &t.AutoStart, &t.DependsOnPayload,
		&t.Ref, &t.ParentID, &t.CreatedAt, &t.UpdatedAt,
		&t.TotalChildCount, &t.DoneChildCount, &t.AbortedChildCount, &t.OpenChildCount,
	); err != nil {
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

