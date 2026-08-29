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

// terminalStatusSQLList is derived once from IsTerminalStatus (model.go)
// rather than hardcoded a second time here, so the open/closed/queue filter
// SQL below can never drift from that helper as new statuses are added
// (Opus指摘#9 応用: one source of truth).
//
// card-model-cleanup PR-2 (migration 0045) retired preExecutionStatusSQLList
// (IsPreExecutionStatus itself no longer exists — see model.go): the
// predicate it backed, "is this task in a not-yet-active card status", is
// now answered directly by the tasks.type column wherever a query needs it
// (see ListTasks's "triage" branch below), not by enumerating a status set.
var (
	terminalStatusSQLList = sqlStatusInList(filterTaskStatuses(IsTerminalStatus))
	// notOpenSelfStatusSQLList = terminal ∪ {parked} — used by the self-clause
	// of the "open" filter (項1, sqlOpenSelf): a fresh/resting card must not
	// pollute the default open view on its own, distinct from the
	// child-rescue/descendant clauses which keep it visible when it has live
	// (executing) children.
	//
	// card-model-cleanup PR-2 (migration 0045) removed captured/triaged/ready
	// from the status vocabulary entirely — they are no longer valid
	// tasks.status values at all (the CHECK constraint rejects them), not
	// merely excluded from this list. parked is the only non-terminal card
	// status that belongs here: working is deliberately NOT included (see
	// TaskStatusWorking's own doc comment in model.go) — a working card has
	// already had a decision applied to it and belongs in Open the same way
	// an executing task does. captured's old carve-out (docs/plans/
	// ingestion-identity.md PR-2, B-2, 「captured の可視化」) is moot now that
	// captured itself no longer exists.
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
	// ActiveOnly narrows the result to non-terminal tasks (docs/plans/
	// webui-detail-list-redesign.md §3.5's 「アクティブのみ (非終端)」トグル).
	// This is a SEPARATE axis from Status — composes with any Status value
	// (including "", meaning "no status predicate at all") rather than being
	// yet another special Status keyword, because "non-terminal" must include
	// TaskStatusParked (unlike the legacy "open" Status keyword's self-clause,
	// which deliberately excludes parked — see notOpenSelfStatusSQLList's own
	// doc comment). Ignored (no-op) when false.
	ActiveOnly bool
	// Limit/Offset implement the list page's pagination (§3.5 / §5 論点4: LIMIT/
	// OFFSET is enough at the current scale; keyset can follow if it stops
	// being enough). Limit <= 0 means "no LIMIT clause" (unbounded, the
	// pre-PR-4 default every non-web caller still gets). Offset > 0 with
	// Limit <= 0 still applies (via SQLite's `LIMIT -1 OFFSET ?`) rather than
	// being silently dropped.
	Limit  int
	Offset int
}

// taskSelectCols は tasks テーブルの全カラム一覧（テーブル別名 t を使用）。
//
// card-model-cleanup PR-2 (migration 0045, docs/plans/card-model-cleanup.md
// §3.4) 以降、tasks は Single Table Inheritance: card 専用列 (kind/urgency/
// wake_at/wake_task_id/suggestion_verb/detail) と execution 専用列
// (behavior/traits/readonly/branch_prefix/base_branch/payload/instructions/
// auto_start) が同じテーブルに同居し、自分の type でない側の列は常に NULL
// (CHECK 制約が保証)。scanTask がこの列順を前提に nullable スキャンするので、
// 変更する場合は両方を同時に直すこと。
//
// tasks.worktree DB 列は branch-policy-simplification Phase 2 で Task 構造体
// から外れたが、既存 DB との互換のため列自体は残す (NOT NULL DEFAULT FALSE、
// migration 0007)。INSERT / UPDATE / SELECT からは列参照を落とし、書き込みは
// 列 default に任せる。
const taskSelectCols = `t.id, t.type, t.project_id, t.remote_id, t.title, t.description, t.status,` +
	` t.behavior, t.traits, t.readonly, t.branch_prefix, t.base_branch, t.payload, t.instructions, t.auto_start,` +
	` t.kind, t.urgency, t.wake_at, t.wake_task_id, t.suggestion_verb, t.detail,` +
	` t.ref, t.parent_id, t.idempotency_key, t.created_at, t.updated_at`

// validateTaskTypeConsistency enforces design doc §3.5's invariant: Card is
// non-nil iff Type == TaskTypeCard, Exec is non-nil iff Type ==
// TaskTypeExecution. Called by CreateTask (task-creation side) and scanTask
// (DB-scan side) — Q17 of docs/plans/card-model-cleanup.md §10 asks for the
// check to exist on BOTH sides: the DB's own CHECK constraint (migration
// 0045) already prevents a row from violating this, but scanTask's copy
// catches a hand-built row from a test fixture or a future direct SQL write
// that bypasses CreateTask/UpdateTask, and CreateTask's copy fails fast
// before ever reaching the DB.
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

// rejectIdempotencyKeyTypeMismatch guards CreateTask's idempotency_key
// get-or-create (PR #1012 review, Opus L3): the pre-insert lookup and the
// post-INSERT-conflict fallback both run BEFORE validateTaskTypeConsistency
// would otherwise catch a shape problem, and BEFORE the caller-requested
// Task (t) is overwritten with the existing row — so without this check, a
// TaskTypeCard create whose idempotency_key collides with an existing
// TaskTypeExecution row (or vice versa) would silently hand back a task of
// the WRONG type: `t.Type` ends up existing.Type, `t.Exec`/`t.Card` end up
// whichever the existing row actually has, and a caller that (reasonably)
// assumed its own requested type would get e.g. a nil Exec on what it
// thinks is an execution task. This can only happen when two DIFFERENT
// call sites reuse the same (project_id, parent_id, idempotency_key) for
// semantically different tasks — a caller bug, but one that must surface
// as an error, not a silently wrong-shaped success.
func rejectIdempotencyKeyTypeMismatch(requested, existing *Task) error {
	if requested.Type != "" && existing.Type != requested.Type {
		return fmt.Errorf(
			"idempotency_key %q (project_id=%s, parent_id=%s) already used by a %s task (id=%s); this create requested a %s task",
			requested.IdempotencyKey, requested.ProjectID, requested.ParentID, existing.Type, existing.ID, requested.Type,
		)
	}
	return nil
}

// taskChildCountCols は子タスク数を集計するサブクエリカラム群（テーブル別名 t を前提）。
//
// open_child_count (最後の1本) は pre-execution な子を引き続きカウントする —
// 逆輸入1で「open/specced な子は task_triage.detail.children の JSON に留め、
// dispatch 時にのみ task 化する」設計になったため、pre-execution な子タスクが
// 実在して親の done 主張を塞ぐケースはそもそも発生しない。ここを緩めると
// verifyDoneClaim の詐称防止ガードを無意味に弱めるだけになる (Opus指摘#6)。
// terminal (done/aborted/dropped) だけを除外する。
// awaiting カウント (最後の1本) は docs/plans/webui-detail-list-redesign.md
// PR-4 (§3.5) 追加分: 一覧の行ロールアップで「⚠ N」(質問持ちの子の存在) を
// 親の行に上げるための集計。他の3本と同じく直接の子のみを数える（孫は数えない
// — §3.3-2 の子一覧統合と同じく1階層のみが対象）。
var taskChildCountCols = `` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status = 'done'),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status = 'aborted'),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status NOT IN (` + terminalStatusSQLList + `)),` +
	`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status = 'awaiting')`

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

	// Get-or-create: idempotency_key-based (docs/plans/
	// signal-ingest-detailed-design.md §8, migration 0047). Independent of
	// the Ref check above — a task may carry either, both, or neither.
	// Unlike Ref, idempotency_key has no external-identity meaning (no
	// UUID-shortcut lookup, no link/drop semantics — see the field's own doc
	// comment on Task). Scoped by (ProjectID, ParentID) — same 3-column scope
	// migration 0037 uses for Ref, and for the identical reason: scoping by
	// ProjectID alone let two DIFFERENT parents in the same project silently
	// collide on a reused key, with the second parent's create call handing
	// back the FIRST parent's child (PR #1012 review, Opus M3 — see
	// Task.IdempotencyKey's own doc comment for the full story).
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
		// Type-specific defaults: "pending" is not a legal card status (and
		// "parked" is not a legal execution status) under migration 0045's
		// CHECK constraint, so the old type-agnostic default-to-pending
		// would fail a fresh card outright.
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

	// idempotency_key is genuinely nullable (migration 0047's own comment):
	// an empty Go string must bind SQL NULL, not the empty string, so the
	// partial unique index's `WHERE idempotency_key IS NOT NULL` never sees a
	// keyless row.
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
		// unique index (migration 0047) instead of ref's. Independent check —
		// see the pre-insert idempotency_key block above for why this doesn't
		// fold into the Ref branch.
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

// GetTaskStatus reads ONLY a task's status, by exact id.
//
// Split out from GetTask because GetTask is not a cheap read: taskSelectCols is
// followed by taskChildCountCols, five correlated `SELECT COUNT(*) FROM tasks c
// WHERE c.parent_id = t.id` subqueries, and tasks(parent_id) is deliberately
// unindexed (docs: the index was judged not to pay for itself) — so each call
// costs five scans of the tasks table. That is fine for a one-shot lookup and
// wrong for something polled once a second per waiter, on a pool that is a
// single connection (internal/db/db.go's SetMaxOpenConns(1)) shared by every
// dispatch, sweep and web request.
//
// No prefix fallback, unlike GetTask: this is for callers that already resolved
// an id and are re-reading it, so a partial id here means the caller skipped
// the resolve rather than that it wants a lookup.
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

	// "open" は特殊フィルタ: 自身が open 状態 OR open な子を持つ（ヘッダー救済）。
	//
	// docs/plans/webui-detail-list-redesign.md PR-4 (§3.6): 再帰 CTE
	// (open_descendants/open_ancestors、複数階層の祖先救済・子孫救済) は
	// 一覧のツリー表示 (task_tree.templ, BuildTreeItems) 撤廃に伴い削除した —
	// 存在理由がツリー表示の孤児防止だけだったため (§2.2 の記述)。残るのは
	// self clause と 1 階層の子救済 (直接の子だけを見る非相関サブクエリ) の
	// みで、CLI/API から直接 "open" を指定する既存の呼び手 (Web UI 自身は
	// もう "open" を送らない — 既定は無フィルタ + ActiveOnly トグル) との
	// 後方互換のために残してある。
	//
	// NOT IN は役割ごとに使う集合が異なる (Opus指摘#7):
	//   - 子救済 (taskChildCountCols 型の直接の子カウント) は「terminal で
	//     なければ open」— pre-execution な子を含めたまま。進行中の
	//     triage task (executing な子を持つ triaged 親など) を隠さないため。
	//   - 自身が open かの self clause だけは「terminal でも pre-execution でも
	//     なければ open」— pre-execution な task 単体は従来の open ビューを
	//     汚さない (項1 本来の目的)。
	if filter.Status == "open" {
		conditions = append(conditions, `(t.status NOT IN (`+notOpenSelfStatusSQLList+`) OR `+
			`(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status NOT IN (`+terminalStatusSQLList+`)) > 0)`)
	} else if filter.Status == "closed" {
		conditions = append(conditions, "t.status IN ("+terminalStatusSQLList+")")
	} else if filter.Status == "cards_live" || filter.Status == "triage" {
		// Phase 1 PR-5a; card-model-cleanup PR-2 (migration 0045) restates the
		// predicate on tasks.type instead of enumerating a status set: 「今生
		// きている card」= every card that hasn't reached a terminal status
		// yet (parked ∪ working). ListCards's default filter (`boid card
		// list` with no status — the exact call khi's open_triage_task_ids,
		// app/trigger.py, makes), so this predicate directly decides which
		// cards khi's signal detection considers on every sweep
		// (docs/plans/suggestion-as-state-transition-impl.md §4.1's khi 対象軸
		// note).
		//
		// docs/plans/webui-detail-list-redesign.md PR-4 (§3.6): renamed from
		// "triage" to "cards_live" — the predicate content is UNCHANGED
		// (a status name, not a filter name, since card machine v2 has no
		// "triaged" status at all — see the design doc's own note on why the
		// old name confused readers). "triage" is accepted as a compatibility
		// alias for any EXPLICIT caller (no known external one exists as of
		// this PR — see the design doc's own audit), so a stale bookmark or
		// script keeps working. card_read.go's internal default-fill (an
		// unset status defaulting to this predicate, the path khi's
		// no-status call actually takes) has been updated to use the
		// canonical "cards_live" name directly, since that is boid's own
		// code, not an external caller needing the alias.
		//
		// done is DELIBERATELY excluded (a decision made here, not just
		// inherited): non-boid signal sources (Slack/Jira/Bitbucket) do not
		// drop a done card from their own re-detection (khi's detect.py
		// NO_RECORD_STATUSES is dropped/aborted only), so a source reopening
		// a done card is already handled outside this filter. Including done
		// here would mean every done card stays a signal candidate until
		// 30-day GC sweeps it — unbounded work for no additional coverage.
		conditions = append(conditions, "t.type = 'card' AND t.status IN ('parked', 'working')")
	} else if filter.Status != "" {
		// docs/plans/webui-detail-list-redesign.md PR-4 (§3.6, §5 論点3):
		// "queue_next" membership (the old `suggestion_verb != ''` branch) is
		// REMOVED — the Queue tab it backed is gone (タブ4枚撤廃), and no
		// external caller sends it (grep confirmed by the design doc's own
		// audit; the Web UI's own default fill was "open", never
		// "queue_next"). It, and every other exact status string, now falls
		// through to this single generic `t.status = ?` literal-equality
		// branch. "queue_next" is not a real tasks.status value (the CHECK
		// constraint would reject it), so that comparison can never match a
		// row — an explicit `?status=queue_next` call deterministically
		// returns an EMPTY list, not an error. This is the decided answer to
		// 論点3's "status=queue_next を明示指定したときの応答": empty, not 400
		// — pinned by TestListTasks_QueueNext_ReturnsEmpty.
		conditions = append(conditions, "t.status = ?")
		args = append(args, filter.Status)
	}
	// ActiveOnly (docs/plans/webui-detail-list-redesign.md §3.5's 「アクティブ
	// のみ (非終端)」トグル) is a second, independent axis from Status above —
	// composes via AND with whatever Status already narrowed to (including no
	// narrowing at all, filter.Status == ""). Deliberately just "not
	// terminal", not notOpenSelfStatusSQLList's terminal∪parked: a parked
	// card IS active (it hasn't reached done/aborted/dropped), unlike the
	// legacy "open" Status keyword's self-clause, which carves parked out
	// because the (now-removed) Parked tab owned it instead.
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
	// docs/plans/webui-detail-list-redesign.md PR-4 (§3.5, §3.2): every view
	// sorts by updated_at DESC now (tie-break: id ASC, deterministic) — the
	// per-branch ORDER BY this used to carry (created_at DESC by default,
	// updated_at DESC only for "closed", a bespoke urgency-then-created_at
	// order for "queue_next") is gone along with "queue_next" itself. A
	// suggestion attached to a card, or any status transition, bumps
	// updated_at (PR-3, TouchTaskUpdatedAt/UpdateTask) — so this single ORDER
	// BY is what makes "a card just got a suggestion" surface at the top of
	// every view without a dedicated queue ordering.
	query += " ORDER BY t.updated_at DESC, t.id ASC"
	// LIMIT/OFFSET (§3.5, §5 論点4): Limit<=0 means unbounded (every non-web
	// caller today). Offset>0 with Limit<=0 still applies via SQLite's
	// LIMIT -1 (unbounded) OFFSET ? rather than being silently dropped.
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

// UpdateTask persists every mutable Task column. Compare against CreateTask's
// INSERT column list before adding a new field to Task: a column missing
// here while present there is a silent no-op — the caller gets a nil error
// (and, if it copies the request field onto the in-memory *Task first, a
// response that *looks* updated) while the row underneath keeps its old
// value. This exact class of bug shipped for remote_id/project_id/
// auto_start (fixed together; see update_task_columns_test.go /
// task_update_persist_test.go) — found via khi-task-collector's self-heal
// loop patching remote_id every cycle forever without it ever sticking.
//
// Two INSERT columns are deliberately absent from this UPDATE, not dropped:
//   - ref: an immutable dedup key by design (docs/plans/
//     ingestion-identity.md 決定16 — "後から変えられない dedup キー").
//     CreateTask is the only writer; there is no
//     UpdateTaskRequest.Ref field at all (internal/apiwire/task.go), so
//     there is no API-level "口" that could even attempt to change it.
//   - behavior: set once at CreateTask time and never exposed on
//     UpdateTaskRequest either (only CreateTaskRequest has Behavior) — no
//     update port exists, so there is nothing to silently drop.
//
// remote_id and auto_start have no status guard here — callers rely on
// being able to set them regardless of task status (khi patches remote_id
// on working/parked tasks; a caller may flip auto_start after the task
// already left pending). project_id, by contrast, IS gated by
// IsPreDispatchEditableStatus one layer up in
// api.TaskAppService.UpdateTask — this function has no status of its own to
// check, it just persists whatever the caller decided was valid to set.
//
// card-model-cleanup PR-2 (migration 0045): this SET clause deliberately
// does NOT touch kind/urgency/wake_at/wake_task_id/suggestion_verb/detail
// (the CardAttrs columns) even though they now live on this same tasks row.
// Those are UpsertTaskTriage's exclusive write path (workflow_card.go's
// park/attrs_set/child_* side effects), which does its own read-modify-write
// scoped to exactly those columns; folding them into this function's blanket
// rewrite would introduce a lost-update race that did NOT exist before the
// STI merge (task_triage was a genuinely separate table/row with its own
// writer before this PR — a concurrent GetTask-then-UpdateTask on the core
// columns could never clobber a concurrent attrs_set). The exec columns
// (traits/readonly/branch_prefix/base_branch/payload/instructions/
// auto_start) keep the SAME blanket-rewrite treatment they always had
// (UpdateTask was already their sole non-locked writer pre-refactor); for a
// card, t.Exec is nil so execInsertValues binds SQL NULL to all of them —
// already their permanent value on a card row (CHECK-enforced), so this is
// a no-op, not a new write path.
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

// TouchTaskUpdatedAt bumps id's updated_at column to now — a single-column
// statement that carries NO status, deliberately narrower than UpdateTask's
// blanket rewrite (docs/plans/webui-detail-list-redesign.md §3.2, PR-3). It
// exists so a non-transitioning side effect (attrs_set's suggestion
// attachment, child_closed's parent self-record) can record "something
// worth surfacing changed here" in the SAME transaction as its own write,
// without reintroducing the stale-snapshot race UpdateTask(t) risks for
// those actions — see workflow_action.go's skipTaskUpdate doc comment for
// why those actions never call UpdateTask with a caller-held Task value.
func TouchTaskUpdatedAt(dbtx db.DBTX, id string) error {
	_, err := dbtx.Exec(`UPDATE tasks SET updated_at = ? WHERE id = ?`, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("touch task updated_at: %w", err)
	}
	return nil
}

// CreateAction inserts a, then — within the SAME statement's transaction —
// ingests it into its target card's workspace inbox when eligible
// (docs/plans/boid-internal-signal-inbox.md §4.5). resolver may be nil (no
// metaproject lookup wired — e.g. most existing tests that don't care about
// signals at all): IngestActionSignal treats that identically to "this
// workspace declares no metaproject", a quiet no-op that leaves every other
// behavior of this function untouched (Q5's "1 ビットも変わらない").
//
// ctx carries the write's origin project via WriterProjectIDFromContext
// (signal_ingest_bridge.go) when this write came from inside a sandbox
// (internal/server/boid_executor.go's ExecuteBoidBuiltin is the sole place
// that ever sets it) — used only for the actor-axis self-reference check,
// never for anything this function itself does with a.
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

	// Best-effort, deliberately not returned as an error (§6.2): a failed
	// ingest must not roll back the action that already committed above —
	// see IngestActionSignal's own doc comment for why (Q11).
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
	// TriggerRuns is the count of finished trigger_runs rows GCTriggerRuns
	// deleted (N-2, Opus review) — trigger_runs otherwise has no retention
	// at all and grows unbounded (khi's 2 triggers alone project to
	// ~105,000 rows/year).
	TriggerRuns int64
	// Signals is the count of signals rows GCSignals deleted
	// (docs/plans/signal-ingest-detailed-design.md §2/§9: "inbox の GC:
	// acked 30 日で既存 GC tx に相乗り") — both acked rows older than the
	// cutoff and unacked (including dead-lettered) rows whose received_at
	// is older than the cutoff, so a permanently-dead signal doesn't linger
	// forever just because nothing ever acks it.
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

// GCTriggerRuns deletes FINISHED trigger_runs rows older than olderThan
// (N-2, Opus review): with no retention at all, this table grows unbounded
// (`every: 10m` × 1 trigger = 144 rows/day = ~52,560 rows/year; khi's 2
// triggers alone project to ~105,000 rows/year). olderThan=0 disables the
// time filter, matching GCTasks' own convention (every finished row is
// eligible). In-flight rows (finished_at IS NULL) are NEVER touched
// regardless of age — they are single-flight's own source of truth, not
// stale history (the same reason GCTasks never deletes a non-terminal
// task).
//
// Called from TaskGCStore.GC in the SAME transaction as GCTasks, so
// `boid gc` / POST /api/gc purges trigger_runs on the existing 30-day
// schedule without a separate GC pass.
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

// FindTaskByIdempotencyKey returns the task matching the given idempotency
// key within the given (projectID, parentID) scope, or nil if no matching
// task is found. See migration 0047 and Task.IdempotencyKey's doc comment
// for the field's contract (docs/plans/signal-ingest-detailed-design.md §8)
// and for why parentID is part of the scope (PR #1012 review, Opus M3: a
// project-only scope let two different parents silently collide on a reused
// key).
//
// Unlike FindTaskByRef, there is no UUID-shaped-string special case here:
// idempotency_key is never treated as a stand-in for a task ID, only as an
// opaque workspace-internal stable key. The equality comparison below also
// never needs an explicit `IS NOT NULL` guard — SQL's NULL = 'x' is neither
// true nor false, so a keyless (NULL) row can never match a non-empty
// idempotencyKey argument, and this function itself short-circuits before
// querying when idempotencyKey is empty.
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

// scanTask reads one tasks row (STI: card-model-cleanup PR-2, migration
// 0045) into a tagged-union Task. Every subtype-specific column is scanned
// into a nullable sql.Null* target regardless of the row's actual type — the
// DB's CHECK constraint guarantees the "other" type's columns are NULL, but
// scanTask cannot rely on that alone (a hand-built fixture row in a test, or
// a future direct SQL write, could violate it) — see
// validateTaskTypeConsistency's own doc comment for why this function calls
// it too, not just CreateTask.
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
