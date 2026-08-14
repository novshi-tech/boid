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

// terminalStatusSQLList / preExecutionStatusSQLList are derived once from
// IsTerminalStatus / IsPreExecutionStatus (model.go) rather than hardcoded a
// second time here, so the open/closed/queue filter SQL below can never drift
// from those helpers as new statuses are added (Opus指摘#9 応用: one source of
// truth).
var (
	terminalStatusSQLList     = sqlStatusInList(filterTaskStatuses(IsTerminalStatus))
	preExecutionStatusSQLList = sqlStatusInList(filterTaskStatuses(IsPreExecutionStatus))
	// notOpenSelfStatusSQLList = terminal ∪ pre-execution — used by the
	// self-clause of the "open" filter (項1, sqlOpenSelf): pre-execution
	// tasks must not pollute the default open view on their own, distinct
	// from the child-rescue/descendant clauses which keep pre-execution
	// visible when they have live (executing) children.
	notOpenSelfStatusSQLList = sqlStatusInList(append(
		append([]TaskStatus{}, filterTaskStatuses(IsTerminalStatus)...),
		filterTaskStatuses(IsPreExecutionStatus)...,
	))
	// notOpenAncestorGateStatusSQLList = terminal ∪ {parked} — used only by the
	// open_descendants CTE's seed (子孫救済, 項3). "working → parked"
	// (machine.go) explicitly parks a card aside while its already-dispatched
	// child keeps running; that live child stays visible on its own status
	// (self clause) without help from this gate. Once the child terminates,
	// this gate must stop pulling it (or its done/aborted siblings) into the
	// open view just because the parent is still "parked" — parked means
	// "set aside", unlike executing/triaged which represent an actually live
	// thread. Other pre-execution statuses (captured/triaged/ready) are
	// deliberately NOT added here: per 逆輸入1, pre-execution tasks don't
	// normally have real child Task rows yet, so widening the gate further
	// would be scope creep.
	notOpenAncestorGateStatusSQLList = sqlStatusInList(append(
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
}

// taskSelectCols は tasks テーブルの基本カラム一覧（テーブル別名 t を使用）。
//
// tasks.worktree DB 列は branch-policy-simplification Phase 2 で Task 構造体
// から外れたが、既存 DB との互換のため列自体は残す (NOT NULL DEFAULT FALSE、
// migration 0007)。INSERT / UPDATE / SELECT からは列参照を落とし、書き込みは
// 列 default に任せる。
const taskSelectCols = `t.id, t.project_id, t.remote_id, t.title, t.description,` +
	` t.status, t.behavior, t.traits, t.readonly,` +
	` t.branch_prefix, t.base_branch, t.payload, t.instructions, t.auto_start,` +
	` t.ref, t.parent_id, t.created_at, t.updated_at`

// taskChildCountCols は子タスク数を集計するサブクエリカラム群（テーブル別名 t を前提）。
//
// open_child_count (最後の1本) は pre-execution な子を引き続きカウントする —
// 逆輸入1で「open/specced な子は task_triage.detail.children の JSON に留め、
// dispatch 時にのみ task 化する」設計になったため、pre-execution な子タスクが
// 実在して親の done 主張を塞ぐケースはそもそも発生しない。ここを緩めると
// verifyDoneClaim の詐称防止ガードを無意味に弱めるだけになる (Opus指摘#6)。
// terminal (done/aborted/dropped) だけを除外する。
var taskChildCountCols = `` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status = 'done'),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status = 'aborted'),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status NOT IN (` + terminalStatusSQLList + `))`

func CreateTask(dbtx db.DBTX, t *Task) error {
	// Get-or-create: when ref is set, return the existing task (scoped by
	// ParentID, "" for a root task, AND ProjectID) instead of inserting a
	// duplicate. This makes create idempotent across supervisor resume
	// cycles (child-create replay) AND, since Phase 1 PR-4 (docs/plans/
	// cross-project-issue-triage.md 論点7), across a root-level ingestion
	// push replay. ProjectID scoping was added by migration 0037 (codex
	// review Blocker fix): idx_tasks_ref_parent (migration 0010) alone was
	// unique on (ref, parent_id) only, so once root tasks (parent_id = "" for
	// every workspace) became dedup-eligible, two different workspaces using
	// the same source ref would collide and the second one would silently
	// receive the FIRST workspace's task back — see FindTaskByRef's doc
	// comment for the full story.
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
		`INSERT INTO tasks (id, project_id, remote_id, title, description, status, behavior, traits, readonly, branch_prefix, base_branch, payload, instructions, auto_start, ref, parent_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.RemoteID, t.Title, t.Description, t.Status, t.Behavior, traitsJSON, t.Readonly, t.BranchPrefix, t.BaseBranch, string(t.Payload), instructionsJSON, t.AutoStart, t.Ref, t.ParentID, t.CreatedAt, t.UpdatedAt,
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

	// "open" は特殊フィルタ: 自身が open 状態 OR open な子を持つ（ヘッダー救済）OR
	// open な祖先を持つ（子孫救済）OR open な子孫を持つ（祖先救済、複数階層）
	//
	// NOT IN は役割ごとに使う集合が異なる (Opus指摘#7):
	//   - 子救済 (taskChildCountCols 型の直接の子カウント) と祖先救済 CTE
	//     (open_ancestors) は「terminal でなければ open」— pre-execution な子・
	//     子孫を含めたまま。進行中の triage task (executing な子を持つ triaged
	//     親など) を隠さないため。
	//   - 子孫救済 CTE (open_descendants) の祖先ゲートだけは
	//     notOpenAncestorGateStatusSQLList (terminal ∪ parked) を使う —
	//     parked は「意図的に脇に置かれた」状態であり、executing/triaged と
	//     違って「進行中の thread」ではないため、parked な祖先だけで
	//     done/aborted な子孫まで救済してしまうと working→parked にした直後に
	//     もう終わっている子タスクが Open タブに居座り続けるバグになる
	//     (working→parked は machine.go の設計上、まだ生きている dispatched
	//     child は self clause だけで開いたままになる)。
	//   - 自身が open かの self clause だけは「terminal でも pre-execution でも
	//     なければ open」— pre-execution な task 単体は従来の open ビューを
	//     汚さない (項1 本来の目的)。
	if filter.Status == "open" {
		// open_descendants は「非 terminal かつ非 parked な祖先を持つ子孫」だけを
		// 表す — base case を「非 terminal な task 自身」ではなく「非 terminal
		// な task の直接の子」にすることで、自分自身が seed に紛れ込まない形に
		// している。当初は自分自身を seed にしていたため、親を持つ
		// pre-execution task が (親の状態に関係なく) 常に自己マッチして
		// self clause の pre-execution 除外をすり抜けるバグがあった —
		// done な親を持つ childless triaged task が open に漏れるケースで
		// 発覚 (codex レビューで指摘、実装時の parent_id != '' ガードだけでは
		// 不十分だった)。この形なら「祖先救済」の意味そのまま: 自分の親
		// (またはその先祖) が非 terminal かつ非 parked である場合にのみ子孫と
		// してマッチする。
		//
		// open_ancestors は逆方向 (祖先救済、複数階層) — 「terminal/parked な
		// 親でも、その部分木のどこかに非 terminal な子孫が居るなら、その子孫
		// までのパス上の祖先は全部見せる」。既存の子救済 (直接の子だけ見る
		// taskChildCountCols 相当の条件) は 1 階層しか救済できないため、
		// 「working→parked にした親 → 完了済みの子 → まだ生きている孫」の
		// ような 3 階層のケースだと、孫は self clause で表示されるのに、その
		// 直接の親 (完了済みの子) が Open タブから消えて孫だけ宙に浮く
		// (「親がいないのに子だけ表示される」) UI 上の孤児表示バグになる。
		// open_ancestors は base case (直接の子が非 terminal) から親側へ再帰的に
		// 遡ることで、その部分木全体を一貫して Open タブに表示する。
		ctePrefix = `WITH RECURSIVE open_descendants(id) AS (` +
			`SELECT c.id FROM tasks c JOIN tasks p ON c.parent_id = p.id ` +
			`WHERE p.status NOT IN (` + notOpenAncestorGateStatusSQLList + `) ` +
			`UNION ` +
			`SELECT c.id FROM tasks c JOIN open_descendants od ON c.parent_id = od.id` +
			`), open_ancestors(id) AS (` +
			`SELECT p.id FROM tasks c JOIN tasks p ON c.parent_id = p.id ` +
			`WHERE c.status NOT IN (` + terminalStatusSQLList + `) ` +
			`UNION ` +
			`SELECT p.id FROM tasks c JOIN tasks p ON c.parent_id = p.id ` +
			`JOIN open_ancestors oa ON oa.id = c.id` +
			`) `
		conditions = append(conditions, `(t.status NOT IN (`+notOpenSelfStatusSQLList+`) OR `+
			`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status NOT IN (`+terminalStatusSQLList+`)) > 0 OR `+
			`t.id IN (SELECT id FROM open_descendants) OR `+
			`t.id IN (SELECT id FROM open_ancestors))`)
	} else if filter.Status == "closed" {
		conditions = append(conditions, "t.status IN ("+terminalStatusSQLList+")")
	} else if filter.Status == "queue" {
		// PR-3 の Web UI queue タブが束ねる予定の集合を backend に先出し
		// (Opus指摘#8): PR-1〜PR-3 の間も `?status=queue` で pre-execution
		// な triage task が見えるようにする安価な保険。
		//
		// PR-3 では意図的にこの意味を変えない (TestListTasks_Queue_
		// ReturnsExactlyPreExecutionSet がこの広い superset を pin 済み)。
		// 実際の Web UI queue タブ・queue の決定論的評価 (決定12) の対象は
		// 下の "queue_next" — urgency を伴う狭い述語 (queue 節 rule 2/3) —
		// を新設して使う。
		conditions = append(conditions, "t.status IN ("+preExecutionStatusSQLList+")")
	} else if filter.Status == "triage" {
		// Phase 1 PR-5a: 「今生きている triage task」= pre-execution ∪ working。
		// ListTriage の既定フィルタ (無指定でのフルスキャン防止) であり、
		// 「queue」(pre-execution のみ) では working の card が読めず、無条件では
		// 全 task 行を走査してしまうため、この 1 本を足す。done/dropped は
		// 明示 status で引く。
		conditions = append(conditions, "t.status IN ("+preExecutionStatusSQLList+", 'working')")
	} else if filter.Status == "queue_next" {
		// queue の決定論的評価 節 rule 2 (queue 所属): state ∈ {ready, triaged}
		// かつ urgency ∈ {now, today, week}。captured は UC-4 の専用確認
		// セクション行き (QueueEligible の doc comment参照)、someday/空 urgency
		// は棚卸し (UC-5) でのみ動く (決定9)。INNER JOIN なので task_triage
		// 行が無い ready/triaged task (urgency 未設定) は自然に除外される。
		joins = append(joins, "INNER JOIN task_triage tt ON tt.task_id = t.id")
		conditions = append(conditions, "t.status IN ('ready','triaged') AND tt.urgency IN ('now','today','week')")
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
	} else if filter.Status == "queue_next" {
		// queue の決定論的評価 節 rule 3 (並び順、全順序): urgency (now > today
		// > week) → state (ready が先) → created_at 昇順 (古いものを腐らせない)
		// → id。 orchestrator.UrgencyRank / StateRank express the same rule as
		// pure Go functions (queue.go) for unit testing; this CASE expression
		// must stay in lockstep with them.
		query += " ORDER BY " +
			"CASE tt.urgency WHEN 'now' THEN 0 WHEN 'today' THEN 1 WHEN 'week' THEN 2 ELSE 3 END, " +
			"CASE t.status WHEN 'ready' THEN 0 ELSE 1 END, " +
			"t.created_at ASC, t.id ASC"
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
		`UPDATE tasks SET title = ?, description = ?, status = ?, traits = ?, readonly = ?, branch_prefix = ?, base_branch = ?, payload = ?, instructions = ?, parent_id = ?, updated_at = ? WHERE id = ?`,
		t.Title, t.Description, t.Status, traitsJSON, t.Readonly, t.BranchPrefix, t.BaseBranch, string(t.Payload), instructionsJSON, t.ParentID, t.UpdatedAt, t.ID,
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
		`INSERT INTO actions (id, task_id, type, payload, from_status, to_status, created_at, actor)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.TaskID, a.Type, string(a.Payload), string(a.FromStatus), string(a.ToStatus), a.CreatedAt, a.Actor,
	)
	if err != nil {
		return fmt.Errorf("insert action: %w", err)
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

	// Task-less jobs (ad-hoc `boid agent`/`boid exec` sessions with no
	// task_id, e.g. `boid agent claude -p <project>`) have no task row to
	// join against, so they are GC'd separately by their own terminal
	// status (jobs.status) and age (jobs.updated_at) instead of a task's.
	// actions doesn't need this: it has a NOT NULL task_id FK, so every row
	// is task-bound and already covered above.
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
// (parentID, projectID) scope, or nil if no matching task is found.
// If ref is a UUID, the task is looked up by id directly (backward
// compatibility), but the result is still validated against BOTH parentID
// and projectID before being returned — treating a scope mismatch the same
// as "not found" (nil, nil), not an error.
//
// projectID scoping was added in Phase 1 PR-4 (docs/plans/
// cross-project-issue-triage.md 論点7, codex review Blocker fix): once
// root tasks (parentID == "") became dedup-eligible, EVERY workspace's root
// tasks share parent_id = "", so scoping by parent_id alone let a caller in
// workspace B silently receive workspace A's task back just by reusing the
// same source ref string (or, for the UUID branch, by supplying any task ID
// it happened to know) — a cross-workspace task leak. Callers MUST pass the
// project they are creating/looking within; there is no "unscoped" call left
// (idx_tasks_ref_parent_project, migration 0037, enforces the same scope at
// the DB level for the ref+parent_id uniqueness itself).
func FindTaskByRef(dbtx db.DBTX, ref, parentID, projectID string) (*Task, error) {
	if ref == "" {
		return nil, nil
	}
	// UUID refs are looked up by task id first, for backward compatibility
	// (some callers pass a real task ID as Ref, expecting an id-based
	// fetch). If that doesn't yield an in-scope match, fall through to the
	// ordinary ref-column scoped lookup below (codex review round 2 Major
	// fix): an external source_ref can coincidentally BE UUID-shaped (Phase
	// 1 PR-4 ingestion, 論点7 — plausible for some ticketing systems' issue
	// ids), in which case it was never meant as a task-ID lookup at all. The
	// id-based branch alone made resending such a ref non-idempotent: a
	// created task's own (fresh, auto-generated) ID is a DIFFERENT UUID than
	// the string stored in its `ref` column, so a retry's id-lookup would
	// always miss, hit the unique index on re-insert, and then miss AGAIN on
	// the error-fallback retry — surfacing as a hard error instead of
	// returning the existing task.
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

func scanTask(s taskScanner) (*Task, error) {
	var t Task
	var payload string
	var instructionsJSON string
	var traitsJSON string
	if err := s.Scan(
		&t.ID, &t.ProjectID, &t.RemoteID, &t.Title, &t.Description,
		&t.Status, &t.Behavior, &traitsJSON, &t.Readonly,
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
