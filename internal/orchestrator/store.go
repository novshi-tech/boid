package orchestrator

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator/refname"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// sqlStatusInList renders a SQL "'a','b','c'" fragment from TaskStatus values.
// Values are drawn only from KnownTaskStatuses() (a fixed enum), never from
// user input, so string concatenation here is safe.
func sqlStatusInList(statuses []TaskStatus) string {
	parts := make([]string, len(statuses))
	for i, s := range statuses {
		parts[i] = "'" + string(s) + "'"
	}
	return strings.Join(parts, ",")
}

// terminalStatusSQLList is derived once from IsTerminalStatus (model.go) so
// the status filters below can't drift from it as statuses are added.
var (
	terminalStatusSQLList = sqlStatusInList(filterTaskStatuses(IsTerminalStatus))
	// notOpenSelfStatusSQLList = terminal ∪ {parked}: the self-clause of the
	// "open" filter. working is deliberately NOT included — a working card
	// belongs in Open the same way an executing task does.
	notOpenSelfStatusSQLList = sqlStatusInList(append(
		append([]TaskStatus{}, filterTaskStatuses(IsTerminalStatus)...),
		TaskStatusParked,
	))
)

func filterTaskStatuses(pred func(TaskStatus) bool) []TaskStatus {
	var out []TaskStatus
	for _, s := range KnownTaskStatuses() {
		if pred(s) {
			out = append(out, s)
		}
	}
	return out
}

// terminalTaskStatusStrings returns the terminal status set (done/aborted/
// dropped, via IsTerminalStatus) as []string for GCTasks, which takes raw
// status strings rather than TaskStatus.
func terminalTaskStatusStrings() []string {
	statuses := filterTaskStatuses(IsTerminalStatus)
	out := make([]string, len(statuses))
	for i, s := range statuses {
		out[i] = string(s)
	}
	return out
}

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
	// ActiveOnly narrows the result to non-terminal tasks (includes parked).
	// Composes with Status via AND; no-op when false.
	ActiveOnly bool
	// Limit<=0 means unbounded. Offset still applies (via SQLite's
	// `LIMIT -1 OFFSET ?`) even when Limit<=0.
	Limit  int
	Offset int
}

// taskSelectCols is the full tasks column list (alias t). tasks is a single
// table for both card and execution rows (the unused type's columns are
// NULL); scanTask depends on this exact column order — keep both in sync.
const taskSelectCols = `t.id, t.type, t.project_id, t.remote_id, t.title, t.description, t.status,` +
	` t.behavior, t.traits, t.readonly, t.branch_prefix, t.base_branch, t.payload, t.instructions, t.auto_start,` +
	` t.kind, t.urgency, t.wake_at, t.wake_task_id, t.suggestion_verb, t.detail,` +
	` t.ref, t.parent_id, t.idempotency_key, t.created_at, t.updated_at`

// validateTaskTypeConsistency enforces that Card is non-nil iff Type ==
// TaskTypeCard, and Exec is non-nil iff Type == TaskTypeExecution. Called by
// both CreateTask and scanTask so a hand-built row bypassing them is caught too.
func validateTaskTypeConsistency(t *Task) error {
	switch t.Type {
	case TaskTypeCard:
		if t.Card == nil {
			return fmt.Errorf("task type is %q but Card is nil", t.Type)
		}
		if t.Exec != nil {
			return fmt.Errorf("task type is %q but Exec is set", t.Type)
		}
	case TaskTypeExecution:
		if t.Exec == nil {
			return fmt.Errorf("task type is %q but Exec is nil", t.Type)
		}
		if t.Card != nil {
			return fmt.Errorf("task type is %q but Card is set", t.Type)
		}
	default:
		return fmt.Errorf("unknown task type %q (want %q or %q)", t.Type, TaskTypeCard, TaskTypeExecution)
	}
	return nil
}

// rejectIdempotencyKeyTypeMismatch guards CreateTask's idempotency-key
// get-or-create: if an existing row sharing the key has a different Type
// than requested, error out instead of silently returning a wrong-shaped task.
func rejectIdempotencyKeyTypeMismatch(requested, existing *Task) error {
	if requested.Type != "" && existing.Type != requested.Type {
		return fmt.Errorf(
			"idempotency_key %q (project_id=%s, parent_id=%s) already used by a %s task (id=%s); this create requested a %s task",
			requested.IdempotencyKey, requested.ProjectID, requested.ParentID, existing.Type, existing.ID, requested.Type,
		)
	}
	return nil
}

// taskChildCountCols aggregates direct-child counts per status (alias t):
// total, done, aborted, open (non-terminal), and awaiting. Direct children
// only — grandchildren are not counted.
var taskChildCountCols = `` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status = 'done'),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status = 'aborted'),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status NOT IN (` + terminalStatusSQLList + `)),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status = 'awaiting')`

func CreateTask(dbtx db.DBTX, t *Task) error {
	// Get-or-create: when ref is set, return the existing task (scoped by
	// ParentID and ProjectID) instead of inserting a duplicate — makes
	// create idempotent across replays. See FindTaskByRef for scope details.
	if t.Ref != "" {
		existing, err := FindTaskByRef(dbtx, t.Ref, t.ParentID, t.ProjectID)
		if err != nil {
			return fmt.Errorf("find existing ref: %w", err)
		}
		if existing != nil {
			*t = *existing
			return nil
		}
	}

	// Get-or-create: idempotency_key-based, independent of the Ref check
	// above (a task may carry either, both, or neither). Scoped by
	// (ProjectID, ParentID) — see Task.IdempotencyKey's doc comment.
	if t.IdempotencyKey != "" {
		existing, err := FindTaskByIdempotencyKey(dbtx, t.ProjectID, t.ParentID, t.IdempotencyKey)
		if err != nil {
			return fmt.Errorf("find existing idempotency key: %w", err)
		}
		if existing != nil {
			if err := rejectIdempotencyKeyTypeMismatch(t, existing); err != nil {
				return err
			}
			*t = *existing
			return nil
		}
	}

	if err := validateTaskTypeConsistency(t); err != nil {
		return fmt.Errorf("create task: %w", err)
	}

	if t.ID == "" {
		t.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.Status == "" {
		// Type-specific defaults: "pending" and "parked" are each valid for
		// only one task type under the tasks CHECK constraint.
		switch t.Type {
		case TaskTypeCard:
			t.Status = TaskStatusParked
		default:
			t.Status = TaskStatusPending
		}
	}
	if t.Exec != nil && len(t.Exec.Payload) == 0 {
		t.Exec.Payload = json.RawMessage("{}")
	}
	// Auto-generate ref when ref is empty and a parent scope is provided.
	if t.Ref == "" && t.ParentID != "" {
		ref, err := generateUniqueRef(dbtx, t.ParentID)
		if err != nil {
			return fmt.Errorf("generate ref: %w", err)
		}
		t.Ref = ref
	}

	execCols, err := execInsertValues(t.Exec)
	if err != nil {
		return fmt.Errorf("marshal exec attrs: %w", err)
	}
	cardCols := cardInsertValues(t.Card)

	// An empty Go string must bind SQL NULL, not "", so the partial unique
	// index's `WHERE idempotency_key IS NOT NULL` never sees a keyless row.
	var idempotencyKey any
	if t.IdempotencyKey != "" {
		idempotencyKey = t.IdempotencyKey
	}

	_, err = dbtx.Exec(
		`INSERT INTO tasks (
			id, type, project_id, remote_id, title, description, status,
			behavior, traits, readonly, branch_prefix, base_branch, payload, instructions, auto_start,
			kind, urgency, wake_at, wake_task_id, suggestion_verb, detail,
			ref, parent_id, idempotency_key, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, string(t.Type), t.ProjectID, t.RemoteID, t.Title, t.Description, string(t.Status),
		execCols.behavior, execCols.traits, execCols.readonly, execCols.branchPrefix, execCols.baseBranch, execCols.payload, execCols.instructions, execCols.autoStart,
		cardCols.kind, cardCols.urgency, cardCols.wakeAt, cardCols.wakeTaskID, cardCols.suggestionVerb, cardCols.detail,
		t.Ref, t.ParentID, idempotencyKey, t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		// Concurrent create: if another goroutine just inserted the same (ref, parent_id),
		// fall back to the existing task rather than returning an error.
		if t.Ref != "" && strings.Contains(err.Error(), "UNIQUE constraint failed") {
			existing, findErr := FindTaskByRef(dbtx, t.Ref, t.ParentID, t.ProjectID)
			if findErr == nil && existing != nil {
				*t = *existing
				return nil
			}
		}
		// Concurrent create: same race, for idempotency_key's own partial
		// unique index instead of ref's.
		if t.IdempotencyKey != "" && strings.Contains(err.Error(), "UNIQUE constraint failed") {
			existing, findErr := FindTaskByIdempotencyKey(dbtx, t.ProjectID, t.ParentID, t.IdempotencyKey)
			if findErr == nil && existing != nil {
				if mismatchErr := rejectIdempotencyKeyTypeMismatch(t, existing); mismatchErr != nil {
					return mismatchErr
				}
				*t = *existing
				return nil
			}
		}
		return fmt.Errorf("insert task: %w", err)
	}
	return nil
}

// execInsertCols is the nullable SQL-bindable projection of an ExecAttrs (or
// its absence). Every field is `any` so a nil ExecAttrs binds SQL NULL to
// every execution column in one place, rather than repeating a "was Exec
// nil" check at each of CreateTask/UpdateTask's argument positions.
type execInsertCols struct {
	behavior, traits, branchPrefix, baseBranch, payload, instructions any
	readonly, autoStart                                               any
}

func execInsertValues(e *ExecAttrs) (execInsertCols, error) {
	if e == nil {
		return execInsertCols{}, nil
	}
	traitsJSON, err := marshalTraits(e.Traits)
	if err != nil {
		return execInsertCols{}, fmt.Errorf("marshal traits: %w", err)
	}
	instructionsJSON, err := marshalInstructions(e.Instructions)
	if err != nil {
		return execInsertCols{}, fmt.Errorf("marshal instructions: %w", err)
	}
	return execInsertCols{
		behavior:     e.Behavior,
		traits:       traitsJSON,
		readonly:     e.Readonly,
		branchPrefix: e.BranchPrefix,
		baseBranch:   e.BaseBranch,
		payload:      string(e.Payload),
		instructions: instructionsJSON,
		autoStart:    e.AutoStart,
	}, nil
}

// cardInsertCols is execInsertCols' card-side counterpart.
type cardInsertCols struct {
	kind, urgency, wakeAt, wakeTaskID, suggestionVerb, detail any
}

func cardInsertValues(c *CardAttrs) cardInsertCols {
	if c == nil {
		return cardInsertCols{}
	}
	detail := c.Detail
	if len(detail) == 0 {
		detail = json.RawMessage("{}")
	}
	return cardInsertCols{
		kind:           c.Kind,
		urgency:        c.Urgency,
		wakeAt:         nullableTime(c.WakeAt),
		wakeTaskID:     c.WakeTaskID,
		suggestionVerb: c.SuggestionVerb,
		detail:         string(detail),
	}
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

// GetTaskStatus reads only a task's status, by exact id — cheaper than
// GetTask (whose child-count subqueries scan tasks.parent_id, which is
// unindexed), for callers that poll tightly. No prefix fallback.
func GetTaskStatus(dbtx db.DBTX, id string) (TaskStatus, error) {
	var status string
	err := dbtx.QueryRow(`SELECT status FROM tasks WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrTaskNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get task status: %w", err)
	}
	return TaskStatus(status), nil
}

func ListTasks(dbtx db.DBTX, filter TaskFilter) ([]*Task, error) {
	var conditions []string
	var args []any
	var joins []string

	// "open": self is open (not terminal, not parked), OR has a direct child
	// that is not terminal.
	if filter.Status == "open" {
		conditions = append(conditions, `(t.status NOT IN (`+notOpenSelfStatusSQLList+`) OR `+
			`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status NOT IN (`+terminalStatusSQLList+`)) > 0)`)
	} else if filter.Status == "closed" {
		conditions = append(conditions, "t.status IN ("+terminalStatusSQLList+")")
	} else if filter.Status == "cards_live" || filter.Status == "triage" {
		// "cards_live": card tasks not yet in a terminal status (parked ∪
		// working). done is deliberately excluded. "triage" is a
		// compatibility alias for the same predicate.
		conditions = append(conditions, "t.type = 'card' AND t.status IN ('parked', 'working')")
	} else if filter.Status != "" {
		// Any other exact status string. A value that isn't a real
		// tasks.status (e.g. the retired "queue_next") just matches zero
		// rows rather than erroring — see TestListTasks_QueueNext_ReturnsEmpty.
		conditions = append(conditions, "t.status = ?")
		args = append(args, filter.Status)
	}
	// ActiveOnly is a second, independent axis from Status — composes via AND.
	// Deliberately just "not terminal" (parked counts as active), unlike the
	// "open" self-clause above which excludes parked.
	if filter.ActiveOnly {
		conditions = append(conditions, "t.status NOT IN ("+terminalStatusSQLList+")")
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
	query := `SELECT ` + taskSelectCols + `, ` + taskChildCountCols + ` FROM tasks t`
	for _, j := range joins {
		query += " " + j
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	// Every view sorts by updated_at DESC (tie-break id ASC), so any status
	// transition or new suggestion (which bump updated_at) surfaces at top.
	query += " ORDER BY t.updated_at DESC, t.id ASC"
	// Limit<=0 means unbounded; Offset still applies via SQLite's LIMIT -1.
	if filter.Limit > 0 || filter.Offset > 0 {
		limit := filter.Limit
		if limit <= 0 {
			limit = -1
		}
		query += " LIMIT ?"
		args = append(args, limit)
		if filter.Offset > 0 {
			query += " OFFSET ?"
			args = append(args, filter.Offset)
		}
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

// UpdateTask persists every mutable Task column. Keep this in sync with
// CreateTask's INSERT column list: a column missing here is a silent no-op.
//
// ref and behavior are immutable after creation and intentionally excluded.
// The CardAttrs columns (kind/urgency/wake_at/wake_task_id/suggestion_verb/
// detail) are also excluded — UpsertTaskTriage owns their read-modify-write
// exclusively; folding them in here would race with it.
func UpdateTask(dbtx db.DBTX, t *Task) error {
	if err := validateTaskTypeConsistency(t); err != nil {
		return fmt.Errorf("update task: %w", err)
	}
	t.UpdatedAt = time.Now().UTC()
	execCols, err := execInsertValues(t.Exec)
	if err != nil {
		return fmt.Errorf("marshal exec attrs: %w", err)
	}
	_, err = dbtx.Exec(
		`UPDATE tasks SET project_id = ?, remote_id = ?, title = ?, description = ?, status = ?, traits = ?, readonly = ?, branch_prefix = ?, base_branch = ?, payload = ?, instructions = ?, auto_start = ?, parent_id = ?, updated_at = ? WHERE id = ?`,
		t.ProjectID, t.RemoteID, t.Title, t.Description, string(t.Status), execCols.traits, execCols.readonly, execCols.branchPrefix, execCols.baseBranch, execCols.payload, execCols.instructions, execCols.autoStart, t.ParentID, t.UpdatedAt, t.ID,
	)
	if err != nil {
		return err
	}
	return nil
}

// TouchTaskUpdatedAt bumps id's updated_at column to now, without the
// stale-snapshot race a full UpdateTask(t) risks for non-transitioning side
// effects. See workflow_action.go's skipTaskUpdate doc comment.
func TouchTaskUpdatedAt(dbtx db.DBTX, id string) error {
	_, err := dbtx.Exec(`UPDATE tasks SET updated_at = ? WHERE id = ?`, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("touch task updated_at: %w", err)
	}
	return nil
}

// CreateAction inserts a, then ingests it into its target card's workspace
// inbox when eligible. resolver may be nil (no metaproject lookup wired),
// treated as a quiet no-op. ctx may carry the write's origin project via
// WriterProjectIDFromContext, used only for the actor-axis self-reference check.
func CreateAction(ctx context.Context, dbtx db.DBTX, a *Action, resolver MetaProjectResolver) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	a.CreatedAt = time.Now().UTC()
	if len(a.Payload) == 0 {
		a.Payload = json.RawMessage("{}")
	}

	_, err := dbtx.Exec(
		`INSERT INTO actions (id, task_id, type, payload, from_status, to_status, created_at, actor)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.TaskID, a.Type, string(a.Payload), string(a.FromStatus), string(a.ToStatus), a.CreatedAt, a.Actor,
	)
	if err != nil {
		return fmt.Errorf("insert action: %w", err)
	}

	// Best-effort: a failed ingest must not roll back the action already
	// committed above — see IngestActionSignal's own doc comment.
	if ingestErr := IngestActionSignal(ctx, dbtx, a, resolver); ingestErr != nil {
		slog.Warn("internal signal ingest failed; action was still recorded",
			"action_id", a.ID, "task_id", a.TaskID, "type", a.Type, "error", ingestErr)
	}

	return nil
}

func ListActionsByTask(dbtx db.DBTX, taskID string) ([]*Action, error) {
	rows, err := dbtx.Query(
		`SELECT id, task_id, type, payload, from_status, to_status, created_at, actor FROM actions WHERE task_id = ? ORDER BY created_at`, taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("list actions: %w", err)
	}
	defer rows.Close()

	var actions []*Action
	for rows.Next() {
		var a Action
		var payload, fromStatus, toStatus string
		if err := rows.Scan(&a.ID, &a.TaskID, &a.Type, &payload, &fromStatus, &toStatus, &a.CreatedAt, &a.Actor); err != nil {
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
	Runtimes   int64
	SandboxTmp int64 // leaked /tmp/boid-* sandbox artifacts removed
	Devices    int64 // revoked web devices deleted
	// TriggerRuns is the count of finished trigger_runs rows GCTriggerRuns deleted.
	TriggerRuns int64
	// Signals is the count of signals rows GCSignals deleted: both acked
	// rows and unacked (including dead-lettered) rows older than the cutoff.
	Signals int64
}

// GCTasks deletes terminal tasks older than olderThan and their related data
// (actions, jobs). If dryRun is true, counts only without deleting.
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

	// Task-less jobs (ad-hoc sessions with no task_id) have no task row to
	// join against, so they are GC'd separately by their own terminal
	// status and age.
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
		for _, table := range []string{"actions", "jobs"} {
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

	for _, table := range []string{"actions", "jobs"} {
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

// GCTriggerRuns deletes finished trigger_runs rows older than olderThan.
// olderThan=0 disables the time filter. In-flight rows (finished_at IS NULL)
// are never touched regardless of age.
func GCTriggerRuns(dbtx db.DBTX, olderThan time.Duration, dryRun bool) (int64, error) {
	cond := `finished_at IS NOT NULL`
	var args []any
	if olderThan > 0 {
		cond += ` AND finished_at < ?`
		args = []any{time.Now().UTC().Add(-olderThan)}
	}

	if dryRun {
		var n int64
		row := dbtx.QueryRow(`SELECT COUNT(*) FROM trigger_runs WHERE `+cond, args...)
		if err := row.Scan(&n); err != nil {
			return 0, fmt.Errorf("count trigger_runs: %w", err)
		}
		return n, nil
	}

	res, err := dbtx.Exec(`DELETE FROM trigger_runs WHERE `+cond, args...)
	if err != nil {
		return 0, fmt.Errorf("delete trigger_runs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete trigger_runs: rows affected: %w", err)
	}
	return n, nil
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

// FindTaskByRef returns the task matching the given ref within the given
// (parentID, projectID) scope, or nil if no matching task is found. If ref
// is a UUID, the task is looked up by id directly (backward compatibility),
// but the result is still validated against BOTH parentID and projectID
// before being returned — a scope mismatch is treated as "not found", not an error.
func FindTaskByRef(dbtx db.DBTX, ref, parentID, projectID string) (*Task, error) {
	if ref == "" {
		return nil, nil
	}
	// UUID refs are looked up by task id first, for backward compatibility.
	// If that doesn't yield an in-scope match, fall through to the ordinary
	// ref-column scoped lookup below — an external source_ref can
	// coincidentally be UUID-shaped without being a task-ID reference.
	if isUUID(ref) {
		t, err := GetTask(dbtx, ref)
		if err != nil && !errors.Is(err, ErrTaskNotFound) {
			return nil, err
		}
		if err == nil && t.ParentID == parentID && t.ProjectID == projectID {
			return t, nil
		}
		// Not found by id, or found but out of scope — fall through to the
		// ref-column query below rather than returning nil here.
	}
	row := dbtx.QueryRow(
		`SELECT `+taskSelectCols+`, `+taskChildCountCols+` FROM tasks t WHERE t.ref = ? AND t.parent_id = ? AND t.project_id = ?`,
		ref, parentID, projectID,
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

// FindTaskByIdempotencyKey returns the task matching the given idempotency
// key within the given (projectID, parentID) scope, or nil if no matching
// task is found. See Task.IdempotencyKey's doc comment for the field's
// contract. Unlike FindTaskByRef, there is no UUID-shaped-string special
// case: idempotency_key is never treated as a stand-in for a task ID.
func FindTaskByIdempotencyKey(dbtx db.DBTX, projectID, parentID, idempotencyKey string) (*Task, error) {
	if idempotencyKey == "" {
		return nil, nil
	}
	row := dbtx.QueryRow(
		`SELECT `+taskSelectCols+`, `+taskChildCountCols+` FROM tasks t WHERE t.project_id = ? AND t.parent_id = ? AND t.idempotency_key = ?`,
		projectID, parentID, idempotencyKey,
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
	for _, table := range []string{"actions", "jobs"} {
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

// scanTask reads one tasks row into a tagged-union Task. Every subtype-
// specific column is scanned into a nullable sql.Null* target regardless of
// the row's actual type, since scanTask cannot assume the DB's CHECK
// constraint on the "other" type's columns holds for every row it sees.
func scanTask(s taskScanner) (*Task, error) {
	var t Task
	var taskType string
	var status string

	var behavior, traitsJSON, branchPrefix, baseBranch, payload, instructionsJSON sql.NullString
	var readonly, autoStart sql.NullBool

	var kind, urgency, wakeTaskID, suggestionVerb, detail sql.NullString
	var wakeAt sql.NullTime
	var idempotencyKey sql.NullString

	if err := s.Scan(
		&t.ID, &taskType, &t.ProjectID, &t.RemoteID, &t.Title, &t.Description, &status,
		&behavior, &traitsJSON, &readonly, &branchPrefix, &baseBranch, &payload, &instructionsJSON, &autoStart,
		&kind, &urgency, &wakeAt, &wakeTaskID, &suggestionVerb, &detail,
		&t.Ref, &t.ParentID, &idempotencyKey, &t.CreatedAt, &t.UpdatedAt,
		&t.TotalChildCount, &t.DoneChildCount, &t.AbortedChildCount, &t.OpenChildCount, &t.AwaitingChildCount,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("scan task: %w", err)
	}
	t.Type = TaskType(taskType)
	t.Status = TaskStatus(status)
	t.IdempotencyKey = idempotencyKey.String

	switch t.Type {
	case TaskTypeExecution:
		traits, err := unmarshalTraits(traitsJSON.String)
		if err != nil {
			return nil, fmt.Errorf("unmarshal traits: %w", err)
		}
		instructions, err := unmarshalInstructions(instructionsJSON.String)
		if err != nil {
			return nil, fmt.Errorf("unmarshal instructions: %w", err)
		}
		t.Exec = &ExecAttrs{
			Behavior:     behavior.String,
			Traits:       traits,
			Readonly:     readonly.Bool,
			BranchPrefix: branchPrefix.String,
			BaseBranch:   baseBranch.String,
			Payload:      json.RawMessage(payload.String),
			Instructions: instructions,
			AutoStart:    autoStart.Bool,
		}
	case TaskTypeCard:
		card := &CardAttrs{
			TaskID:         t.ID,
			Kind:           kind.String,
			Urgency:        urgency.String,
			WakeTaskID:     wakeTaskID.String,
			SuggestionVerb: suggestionVerb.String,
			Detail:         json.RawMessage(detail.String),
		}
		if wakeAt.Valid {
			w := wakeAt.Time
			card.WakeAt = &w
		}
		t.Card = card
	}
	if err := validateTaskTypeConsistency(&t); err != nil {
		return nil, fmt.Errorf("scan task %q: %w", t.ID, err)
	}
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
