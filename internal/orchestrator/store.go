package orchestrator

import (
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator/refname"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ErrTaskNotFound is returned by scanTask (and propagated by GetTask,
// FindTaskByRemote, FindTaskByRef) when no matching task row exists. Callers
// should check for it with errors.Is rather than matching on error strings.
var ErrTaskNotFound = errors.New("task not found")

// ParentIDSentinelRoot is a sentinel value for CreateTaskRequest.ParentID that
// explicitly requests root-task creation. When this value is detected at an
// entry point (sandbox, CLI, HTTP API), auto-populate is skipped and the
// stored parent_id is left empty. Use this when a root task must be created
// from inside a child context where auto-populate would otherwise attach the
// current task as parent.
const ParentIDSentinelRoot = "-"

func isUUID(s string) bool {
	return uuidPattern.MatchString(s)
}

type TaskFilter struct {
	Status      string
	ProjectID   string
	Behavior    string
	WorkspaceID string
	Title       string
	ParentID    *string
}

// taskSelectCols は tasks テーブルの基本カラム一覧（テーブル別名 t を使用）。
const taskSelectCols = `t.id, t.project_id, t.remote_id, t.title, t.description,` +
	` t.status, t.behavior, t.traits, t.readonly, t.worktree,` +
	` t.branch_prefix, t.base_branch, t.payload, t.instructions, t.auto_start,` +
	` t.ref, t.parent_id, t.created_at, t.updated_at`

// taskChildCountCols は子タスク数を集計するサブクエリカラム群（テーブル別名 t を前提）。
const taskChildCountCols = `` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status = 'done'),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status = 'aborted'),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status NOT IN ('done', 'aborted'))`

func CreateTask(dbtx db.DBTX, t *Task) error {
	// Get-or-create: when ref and parent are both set, return the existing task
	// instead of inserting a duplicate. This makes create idempotent across
	// supervisor resume cycles, where the same child-create call may be replayed.
	if t.Ref != "" && t.ParentID != "" {
		existing, err := FindTaskByRef(dbtx, t.Ref, t.ParentID)
		if err != nil {
			return fmt.Errorf("find existing ref: %w", err)
		}
		if existing != nil {
			*t = *existing
			return nil
		}
	}

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
	// Auto-generate ref when ref is empty and a parent scope is provided.
	if t.Ref == "" && t.ParentID != "" {
		ref, err := generateUniqueRef(dbtx, t.ParentID)
		if err != nil {
			return fmt.Errorf("generate ref: %w", err)
		}
		t.Ref = ref
	}
	traitsJSON, err := marshalTraits(t.Traits)
	if err != nil {
		return fmt.Errorf("marshal traits: %w", err)
	}
	instructionsJSON, err := marshalInstructions(t.Instructions)
	if err != nil {
		return fmt.Errorf("marshal instructions: %w", err)
	}

	_, err = dbtx.Exec(
		`INSERT INTO tasks (id, project_id, remote_id, title, description, status, behavior, traits, readonly, worktree, branch_prefix, base_branch, payload, instructions, auto_start, ref, parent_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.RemoteID, t.Title, t.Description, t.Status, t.Behavior, traitsJSON, t.Readonly, t.Worktree, t.BranchPrefix, t.BaseBranch, string(t.Payload), instructionsJSON, t.AutoStart, t.Ref, t.ParentID, t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		// Concurrent create: if another goroutine just inserted the same (ref, parent_id),
		// fall back to the existing task rather than returning an error.
		if t.Ref != "" && t.ParentID != "" && strings.Contains(err.Error(), "UNIQUE constraint failed") {
			existing, findErr := FindTaskByRef(dbtx, t.Ref, t.ParentID)
			if findErr == nil && existing != nil {
				*t = *existing
				return nil
			}
		}
		return fmt.Errorf("insert task: %w", err)
	}
	return nil
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
	return t, nil
}

func ListTasks(dbtx db.DBTX, filter TaskFilter) ([]*Task, error) {
	var conditions []string
	var args []any
	var joins []string
	var ctePrefix string

	// "open" は特殊フィルタ: 自身が open 状態 OR open な子を持つ（ヘッダー救済）OR open な祖先を持つ（子孫救済）
	if filter.Status == "open" {
		ctePrefix = `WITH RECURSIVE open_descendants(id) AS (` +
			`SELECT id FROM tasks WHERE status NOT IN ('done','aborted') ` +
			`UNION ` +
			`SELECT c.id FROM tasks c JOIN open_descendants od ON c.parent_id = od.id` +
			`) `
		conditions = append(conditions, `(t.status NOT IN ('done', 'aborted') OR `+
			`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status NOT IN ('done', 'aborted')) > 0 OR `+
			`t.id IN (SELECT id FROM open_descendants))`)
	} else if filter.Status == "closed" {
		conditions = append(conditions, "t.status IN ('done', 'aborted')")
	} else if filter.Status != "" {
		conditions = append(conditions, "t.status = ?")
		args = append(args, filter.Status)
	}
	if filter.ProjectID != "" {
		conditions = append(conditions, "t.project_id = ?")
		args = append(args, filter.ProjectID)
	}
	if filter.Behavior != "" {
		conditions = append(conditions, "t.behavior = ?")
		args = append(args, filter.Behavior)
	}
	if filter.WorkspaceID != "" {
		joins = append(joins, "INNER JOIN project_workspaces pw ON pw.project_id = t.project_id AND pw.workspace_id = ?")
		args = append([]any{filter.WorkspaceID}, args...)
	}
	if filter.Title != "" {
		conditions = append(conditions, "LOWER(t.title) LIKE ?")
		args = append(args, "%"+strings.ToLower(filter.Title)+"%")
	}
	if filter.ParentID != nil {
		conditions = append(conditions, "t.parent_id = ?")
		args = append(args, *filter.ParentID)
	}
	query := ctePrefix + `SELECT ` + taskSelectCols + `, ` + taskChildCountCols + ` FROM tasks t`
	for _, j := range joins {
		query += " " + j
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	if filter.Status == "closed" {
		query += " ORDER BY t.updated_at DESC"
	} else {
		query += " ORDER BY t.created_at DESC"
	}

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
	return tasks, nil
}

func UpdateTask(dbtx db.DBTX, t *Task) error {
	t.UpdatedAt = time.Now().UTC()
	traitsJSON, err := marshalTraits(t.Traits)
	if err != nil {
		return fmt.Errorf("marshal traits: %w", err)
	}
	instructionsJSON, err := marshalInstructions(t.Instructions)
	if err != nil {
		return fmt.Errorf("marshal instructions: %w", err)
	}
	_, err = dbtx.Exec(
		`UPDATE tasks SET title = ?, description = ?, status = ?, traits = ?, readonly = ?, worktree = ?, branch_prefix = ?, base_branch = ?, payload = ?, instructions = ?, parent_id = ?, updated_at = ? WHERE id = ?`,
		t.Title, t.Description, t.Status, traitsJSON, t.Readonly, t.Worktree, t.BranchPrefix, t.BaseBranch, string(t.Payload), instructionsJSON, t.ParentID, t.UpdatedAt, t.ID,
	)
	if err != nil {
		return err
	}
	return nil
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
	Tasks      int64
	Jobs       int64
	Actions    int64
	Worktrees  int64
	Runtimes   int64
	SandboxTmp int64 // leaked /tmp/boid-* sandbox artifacts removed
	Devices    int64 // revoked web devices deleted
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

	// Task-less jobs (ad-hoc `boid agent`/`boid exec` sessions with no
	// task_id, e.g. `boid agent claude -p <project>`) have no task row to
	// join against, so they are GC'd separately by their own terminal
	// status (jobs.status) and age (jobs.updated_at) instead of a task's.
	// actions/worktrees don't need this: both have a NOT NULL task_id FK,
	// so every row is task-bound and already covered above.
	var tasklessCond string
	var tasklessArgs []any
	if olderThan > 0 {
		tasklessCond = `task_id IS NULL AND status IN ('completed', 'failed') AND updated_at < ?`
		tasklessArgs = []any{time.Now().UTC().Add(-olderThan)}
	} else {
		tasklessCond = `task_id IS NULL AND status IN ('completed', 'failed')`
	}

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

		var tasklessJobs int64
		row = dbtx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE `+tasklessCond, tasklessArgs...)
		if err := row.Scan(&tasklessJobs); err != nil {
			return nil, fmt.Errorf("count taskless jobs: %w", err)
		}
		result.Jobs += tasklessJobs

		var tasklessRuntimes int64
		row = dbtx.QueryRow(`SELECT COUNT(DISTINCT runtime_id) FROM jobs WHERE runtime_id != '' AND `+tasklessCond, tasklessArgs...)
		if err := row.Scan(&tasklessRuntimes); err != nil {
			return nil, fmt.Errorf("count taskless runtimes: %w", err)
		}
		result.Runtimes += tasklessRuntimes
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

	res, err := dbtx.Exec(`DELETE FROM jobs WHERE `+tasklessCond, tasklessArgs...)
	if err != nil {
		return nil, fmt.Errorf("delete taskless jobs: %w", err)
	}
	n, _ := res.RowsAffected()
	result.Jobs += n

	res, err = dbtx.Exec(`DELETE FROM tasks WHERE `+taskCond, condArgs...)
	if err != nil {
		return nil, fmt.Errorf("delete tasks: %w", err)
	}
	result.Tasks, _ = res.RowsAffected()
	return result, nil
}

// FindTaskByRemote returns the most recently created task (by created_at DESC, id DESC)
// matching the given remote_id, or nil if no matching task is found.
func FindTaskByRemote(dbtx db.DBTX, remoteID string) (*Task, error) {
	row := dbtx.QueryRow(
		`SELECT `+taskSelectCols+`, `+taskChildCountCols+` FROM tasks t WHERE t.remote_id = ? ORDER BY t.created_at DESC, t.id DESC LIMIT 1`,
		remoteID,
	)
	t, err := scanTask(row)
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
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
			if errors.Is(err, ErrTaskNotFound) {
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
		if errors.Is(err, ErrTaskNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

// ListChildren returns all direct children of the given parent task, ordered
// by created_at ASC (oldest first). Returns an empty slice if the task has no
// children — never nil. parentID must be non-empty; passing "" returns an
// empty result (root tasks have no parent record, so they can't be queried as
// children either).
func ListChildren(dbtx db.DBTX, parentID string) ([]*Task, error) {
	if parentID == "" {
		return nil, nil
	}
	rows, err := dbtx.Query(
		`SELECT `+taskSelectCols+`, `+taskChildCountCols+` FROM tasks t WHERE t.parent_id = ? ORDER BY t.created_at ASC`,
		parentID,
	)
	if err != nil {
		return nil, fmt.Errorf("list children: %w", err)
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
	return tasks, nil
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
	var instructionsJSON string
	var traitsJSON string
	if err := s.Scan(
		&t.ID, &t.ProjectID, &t.RemoteID, &t.Title, &t.Description,
		&t.Status, &t.Behavior, &traitsJSON, &t.Readonly, &t.Worktree,
		&t.BranchPrefix, &t.BaseBranch, &payload, &instructionsJSON, &t.AutoStart,
		&t.Ref, &t.ParentID, &t.CreatedAt, &t.UpdatedAt,
		&t.TotalChildCount, &t.DoneChildCount, &t.AbortedChildCount, &t.OpenChildCount,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("scan task: %w", err)
	}
	t.Payload = json.RawMessage(payload)
	traits, err := unmarshalTraits(traitsJSON)
	if err != nil {
		return nil, fmt.Errorf("unmarshal traits: %w", err)
	}
	t.Traits = traits
	instructions, err := unmarshalInstructions(instructionsJSON)
	if err != nil {
		return nil, fmt.Errorf("unmarshal instructions: %w", err)
	}
	t.Instructions = instructions
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

func marshalInstructions(instructions Instructions) (string, error) {
	if len(instructions) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal([]Instruction(instructions))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unmarshalInstructions reads either the new array form or the legacy map form
// (`{"main": {...}}`) for backward compatibility with rows persisted before
// the state-machine simplification.
func unmarshalInstructions(s string) (Instructions, error) {
	if s == "" || s == "[]" || s == "{}" {
		return nil, nil
	}
	var instructions Instructions
	if err := json.Unmarshal([]byte(s), &instructions); err != nil {
		return nil, err
	}
	if len(instructions) == 0 {
		return nil, nil
	}
	return instructions, nil
}

// generateUniqueRef generates a unique ref for the given parent scope.
// It retries up to 5 times with fresh adjective_noun candidates, then falls back
// to appending a 4-character random suffix to guarantee uniqueness.
func generateUniqueRef(dbtx db.DBTX, parentID string) (string, error) {
	const maxRetries = 5
	const suffixLen = 4

	rng := newRNG()
	for i := 0; i < maxRetries; i++ {
		candidate := refname.Generate(rng)
		exists, err := refExistsInParent(dbtx, candidate, parentID)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
	// Fallback: append a short random suffix to ensure uniqueness.
	return refname.Generate(rng) + "_" + randomAlpha(rng, suffixLen), nil
}

// refExistsInParent checks whether a ref already exists within the given parent scope.
func refExistsInParent(dbtx db.DBTX, ref, parentID string) (bool, error) {
	row := dbtx.QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE ref = ? AND parent_id = ?`, ref, parentID,
	)
	var count int
	if err := row.Scan(&count); err != nil {
		return false, fmt.Errorf("check ref existence: %w", err)
	}
	return count > 0, nil
}

// newRNG creates a new random source seeded from crypto/rand.
func newRNG() *rand.Rand {
	var s1, s2 uint64
	if err := binary.Read(cryptorand.Reader, binary.LittleEndian, &s1); err != nil {
		s1 = uint64(time.Now().UnixNano())
	}
	if err := binary.Read(cryptorand.Reader, binary.LittleEndian, &s2); err != nil {
		s2 = uint64(time.Now().UnixNano() >> 17)
	}
	return rand.New(rand.NewPCG(s1, s2))
}

// randomAlpha returns a random lowercase alphanumeric string of length n.
func randomAlpha(rng *rand.Rand, n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rng.IntN(len(chars))]
	}
	return string(b)
}

