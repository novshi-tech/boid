package orchestrator

// docs/plans/ingestion-identity.md PR-3 (B-3): 読み口 — BoidOpActionList
// (`boid action list`) の store 実装。
//
// I-9 (差分 reconcile を続ける) を選んだ設計の帰結として、workspace の tick
// スクリプトが「押す前に現況と比べる」ときの比較先が daemon の読み戻しになる。
// `ListActionsByTask` (store.go) は per-task なので、それだけを口にすると
// tick のたびに triage task の数だけ brokered op が飛ぶ (「B-3: per-task だ
// けだと tick が O(N) になる」節)。ここは **workspace スコープの一括読み**を
// 既定にする — 1 回の呼び出しで、そのスコープに属する全 task の actions を
// created_at 昇順で返す。
//
// カーソルは (created_at, id) の組。理由は EncodeActionCursor のコメントを
// 参照 — actions.id は UUID (作成順との相関が無い) なので、id 単独では
// 「同じ created_at を持つ複数行」を安定に区別できない。
//
// 読み口の設計上の限界 (PR-1/PR-2 レビューで判明、実装時に意識すること):
// captured task の作成 (task_create / BoidOpTaskResolveOrCapture の新規作成
// パス) は action を 1 本も残さない (task_create も同様で一貫はしている)。
// つまり「いつ誰がこの task を起こしたか」は action_list からは読み戻せず、
// workspace の tick スクリプトが「新しい task が増えた」ことを検出する手段は
// この op には無い — 別途 card_list (status=parked。card-model-cleanup PR-3
// で task_triage_list/captured から rename・洗い替え済み) を併用する必要が
// ある。action_list は「既存 task に何が起きたか」の読み口であって、
// 「task の存在そのものの変化」の読み口ではない。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

// DefaultActionListLimit / MaxActionListLimit bound a single BoidOpActionList
// call's ROW COUNT — not its response size. This is the direct response to
// the PR-1/PR-2 review finding "wire レベルにサイズ上限がない…返り値側にも
// limit が要る — 一括読み は大量のデータを返しうる": since action_list is a
// brand-new read surface added BY this PR, its own default/max limit is
// where that concern is closed for THIS op (see the PR's report for the
// separate judgment call on the pre-existing, broker-wide input-side gap,
// which this PR does not touch).
//
// MaxActionListLimit (1000) × each action's own payload cap (MaxContentBytes
// = 64KiB, content_size.go) puts the mathematical worst case at ~64MB —
// nowhere near "bounded" in an absolute sense. This is deliberately NOT a
// tight response-size bound; it is a row-count cap chosen against real
// payload sizes (khi's actual attrs blobs measured max 22,572B, nowhere
// close to the 64KiB cap — 実測, 2026-08-19), which keep a 1000-row response
// small in practice today. Neither constant is a measured constant the way
// MaxContentBytes itself is — there is no equivalent real-workspace
// measurement of "how many actions land per tick" yet (khi does not call
// this op today; it is this PR's own new consumer). Revisit (tighten the
// row cap, or add a real response-byte budget) once a workspace is actually
// driving this op in production and the gap between "small in practice" and
// "bounded in the worst case" starts to matter — 12 節 B-3's own framing
// ("問題になったら…決める") applies here too.
const (
	DefaultActionListLimit = 200
	MaxActionListLimit     = 1000
)

// ClampActionListLimit resolves a caller-requested limit (<=0 meaning "use
// the default") against the two constants above. Always returns a value in
// [1, MaxActionListLimit] — a caller can request fewer, but never more.
func ClampActionListLimit(requested int) int {
	if requested <= 0 {
		return DefaultActionListLimit
	}
	if requested > MaxActionListLimit {
		return MaxActionListLimit
	}
	return requested
}

// ErrActionListUnscoped is returned by ListActionsSince when the filter
// names neither a project scope (ProjectIDs) nor a WorkspaceID. Broker +
// executor scoping (mirroring BoidOpTaskList/BoidOpCardList exactly)
// is the primary enforcement point — this is defense in depth at the query
// itself, cheap to add and directly closes the "brokered scoping bug" class
// PR-4/PR-5 review flagged three separate times (executor-only scoping with
// the broker passing a request through unscoped).
var ErrActionListUnscoped = errors.New("action list: no project or workspace scope given")

// ActionListFilter scopes and paginates ListActionsSince (B-3's "workspace
// スコープの一括読み").
type ActionListFilter struct {
	// ProjectIDs scopes to a set of projects via a single SQL IN(...) — used
	// both for an explicit single --project-id (a 1-element slice) and for
	// the "neither project nor workspace given" fallback
	// (TokenContext.AllowedProjectIDs, resolved by the caller — boid_executor.go
	// — before this filter is built). A single query rather than
	// one-call-per-project (the loop BoidOpTaskList/BoidOpCardList's
	// executor uses over AllowedProjectIDs) is deliberate: correctly merging
	// cursor-paginated results across N separately-limited per-project
	// queries needs an N-way merge (each source must contribute up to
	// `limit` candidates, the union re-sorted and re-truncated, and the next
	// cursor derived per-source) — real complexity a single IN(...) query
	// the SQL engine already sorts in one pass avoids entirely.
	ProjectIDs []string
	// WorkspaceID scopes via project_workspaces, matching ListTasks' own
	// WorkspaceID join (store.go). In practice mutually exclusive with
	// ProjectIDs — the caller picks exactly one shape per call, mirroring
	// BoidOpCardList's own project_id/workspace_id branching — but
	// nothing here forbids combining them (both apply as an AND).
	WorkspaceID string
	// TaskID additionally narrows to one task's actions. Safe to combine
	// with ProjectIDs/WorkspaceID as a plain AND condition: a task outside
	// the scope simply matches zero rows — unlike action_send's separate
	// GetTask+AllowsProject check (which exists because that op has no
	// other scope to AND against), there is no way for TaskID alone to
	// widen what this filter returns.
	TaskID string
	// Since is the OPAQUE wire cursor (EncodeActionCursor's output) marking
	// the exclusive lower bound of an append-only scan — empty means "from
	// the beginning". Decoded internally so callers never need to know the
	// encoding.
	//
	// There is deliberately no "start from now, skip all history" sentinel
	// (a `--since latest` equivalent) alongside this. Opus review
	// (2026-08-19) raised it as a real gap — a script that loses its cursor,
	// or connects for the first time, has to page through the ENTIRE
	// history before it reaches the present. Not adding it now is a
	// judgment call, not an oversight: khi's current action volume is in
	// the hundreds (実測, 2026-08-19), so paging the full history at
	// DefaultActionListLimit=200 is a handful of calls, not a real cost —
	// and a "latest" mode is a genuinely new piece of API surface (wire
	// schema, broker/executor scoping, CLI flag, its own tests), not a
	// one-line fix, so it does not belong bundled into an
	// already-in-flight review-fix pass. Revisit if/when action volume
	// grows enough that "page from the beginning" stops being cheap, or a
	// concrete caller (a fresh workspace's first connect) actually needs
	// it.
	Since string
	// Limit is clamped internally via ClampActionListLimit — any value
	// (including 0 or negative) is accepted.
	Limit int
}

// EncodeActionCursor renders a (created_at, id) pair as the opaque cursor
// string BoidOpActionList's wire contract carries.
//
// id is part of the cursor — not just created_at — because actions.id is a
// random UUID (migration 0001_initial.sql), not a creation-ordered
// sequence: two actions created in the same instant would otherwise be
// indistinguishable by created_at alone, and a cursor that cannot tell them
// apart risks silently skipping or re-delivering one of the pair. Ordering
// by (created_at, id) together, consistently on both the SELECT's ORDER BY
// and the cursor's WHERE threshold, is the standard keyset-pagination
// technique for exactly this "ties on the primary sort key" case.
//
// The real-world source of same-instant ties is
// internal/dispatcher/store.go's markStaleTasksAborted: it takes ONE
// now := time.Now().UTC() OUTSIDE its per-task loop and inserts N action
// rows sharing that exact value (the daemon-restart bulk-abort path) — NOT
// TaskWorkflowService.persistFiredEvents (internal/api/workflow_action.go),
// which calls CreateAction once per event inside the loop and so gets a
// fresh time.Now() per row (measured: three rows in one transaction landed
// with distinct nanosecond timestamps, never tied). Getting this citation
// right matters: if a future reader "verifies" that ties don't happen by
// checking persistFiredEvents alone, they will conclude id is unnecessary
// and drop it — see
// TestListActionsSince_TiedCreatedAt_PageBoundary_NoDuplicatesNoGaps
// (action_list_test.go) for the reproduction against the actual tie
// source.
func EncodeActionCursor(createdAt time.Time, id string) string {
	return createdAt.UTC().Format(time.RFC3339Nano) + "|" + id
}

// DecodeActionCursor parses EncodeActionCursor's output. An empty string
// decodes to the zero Time and empty id ("from the beginning") without
// error. A non-empty but malformed cursor is a hard error — J-10's general
// posture (machine-readable contracts error loudly on bad input rather than
// silently reinterpreting it) applies here too: silently treating a
// corrupted cursor as "from the beginning" would quietly re-deliver a
// script's entire history instead of surfacing the mistake.
func DecodeActionCursor(cursor string) (createdAt time.Time, id string, err error) {
	if cursor == "" {
		return time.Time{}, "", nil
	}
	idx := strings.LastIndex(cursor, "|")
	if idx < 0 {
		return time.Time{}, "", fmt.Errorf("action list: malformed cursor %q", cursor)
	}
	tsPart, idPart := cursor[:idx], cursor[idx+1:]
	if idPart == "" {
		return time.Time{}, "", fmt.Errorf("action list: malformed cursor %q", cursor)
	}
	ts, perr := time.Parse(time.RFC3339Nano, tsPart)
	if perr != nil {
		return time.Time{}, "", fmt.Errorf("action list: malformed cursor %q: %w", cursor, perr)
	}
	return ts, idPart, nil
}

// ListActionsSince is BoidOpActionList's store implementation (B-3): a
// SINGLE append-only, workspace-scoped read across every task's actions,
// replacing the per-task-only ListActionsByTask (store.go) for this use
// case — see this file's package doc comment / the design doc's "B-3:
// per-task だけだと tick が O(N) になる" for why a per-task loop is the wrong
// shape here.
//
// Returns the matching actions oldest-first (ORDER BY created_at, id — the
// same order the cursor advances in) and the NEXT cursor: the last returned
// row's cursor, or filter.Since unchanged when no rows matched (so a caller
// that polls and gets nothing new just keeps resending the same cursor next
// time, rather than the cursor going stale or resetting to the beginning).
func ListActionsSince(dbtx db.DBTX, filter ActionListFilter) ([]*Action, string, error) {
	if len(filter.ProjectIDs) == 0 && filter.WorkspaceID == "" {
		return nil, "", ErrActionListUnscoped
	}
	sinceCreatedAt, sinceID, err := DecodeActionCursor(filter.Since)
	if err != nil {
		return nil, "", err
	}
	limit := ClampActionListLimit(filter.Limit)

	var joins []string
	var conditions []string
	var args []any

	if filter.WorkspaceID != "" {
		joins = append(joins, "INNER JOIN project_workspaces pw ON pw.project_id = t.project_id AND pw.workspace_id = ?")
		args = append(args, filter.WorkspaceID)
	}
	if len(filter.ProjectIDs) > 0 {
		placeholders := make([]string, len(filter.ProjectIDs))
		for i, pid := range filter.ProjectIDs {
			placeholders[i] = "?"
			args = append(args, pid)
		}
		conditions = append(conditions, "t.project_id IN ("+strings.Join(placeholders, ", ")+")")
	}
	if filter.TaskID != "" {
		conditions = append(conditions, "a.task_id = ?")
		args = append(args, filter.TaskID)
	}
	if !sinceCreatedAt.IsZero() || sinceID != "" {
		// The keyset-pagination threshold itself: strictly greater than
		// (created_at, id) in tuple order.
		conditions = append(conditions, "(a.created_at > ? OR (a.created_at = ? AND a.id > ?))")
		args = append(args, sinceCreatedAt, sinceCreatedAt, sinceID)
		// Redundant on top of the OR-condition above (logically implied by
		// it: every row it can match already satisfies created_at >=
		// sinceCreatedAt) but NOT redundant for SQLite's query planner:
		// EXPLAIN QUERY PLAN measured (Opus review, 2026-08-19) shows the OR
		// form alone as "SCAN a USING INDEX idx_actions_created_at_id" — a
		// full index-order scan from the beginning — because SQLite cannot
		// derive a range bound from an OR of two AND-clauses. Adding this
		// single plain inequality flips it to "SEARCH a USING INDEX
		// idx_actions_created_at_id (created_at>?)", a real range seek.
		// Without it, a caller reconnecting near the END of a large actions
		// table (e.g. a long-running workspace catching up after downtime)
		// pays a full leading-edge index scan on every single page — see
		// this file's own package doc / the design doc's migration 0042
		// comment for why that "quietly degrades" shape is exactly what the
		// index was added to prevent in the first place.
		conditions = append(conditions, "a.created_at >= ?")
		args = append(args, sinceCreatedAt)
	}

	query := `SELECT a.id, a.task_id, a.type, a.payload, a.from_status, a.to_status, a.created_at, a.actor
		FROM actions a
		JOIN tasks t ON t.id = a.task_id`
	for _, j := range joins {
		query += " " + j
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY a.created_at ASC, a.id ASC LIMIT ?"
	args = append(args, limit)

	rows, err := dbtx.Query(query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list actions since: %w", err)
	}
	defer rows.Close()

	var actions []*Action
	for rows.Next() {
		var a Action
		var payload, fromStatus, toStatus string
		if err := rows.Scan(&a.ID, &a.TaskID, &a.Type, &payload, &fromStatus, &toStatus, &a.CreatedAt, &a.Actor); err != nil {
			return nil, "", fmt.Errorf("list actions since: scan: %w", err)
		}
		a.Payload = json.RawMessage(payload)
		a.FromStatus = TaskStatus(fromStatus)
		a.ToStatus = TaskStatus(toStatus)
		actions = append(actions, &a)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("list actions since: rows: %w", err)
	}

	nextCursor := filter.Since
	if len(actions) > 0 {
		last := actions[len(actions)-1]
		nextCursor = EncodeActionCursor(last.CreatedAt, last.ID)
	}
	return actions, nextCursor, nil
}
