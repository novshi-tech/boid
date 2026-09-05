package orchestrator

// This file implements the store side of BoidOpActionList (`boid action
// list`): a single, workspace-scoped, append-only read across every task's
// actions (rather than a per-task loop over ListActionsByTask, which would
// make a workspace tick script's diff-reconcile O(N) in its task count),
// returned oldest-first with a (created_at, id) keyset cursor — see
// EncodeActionCursor for why id must be part of the cursor.
//
// Design limit: a new task's creation (task_create /
// BoidOpTaskResolveOrCapture) leaves no action row, so "a new task appeared"
// is not observable through this read — callers also need card_list
// (status=parked) to detect that. action_list answers "what happened to an
// existing task", not "did the set of tasks change". See docs/plans/
// ingestion-identity.md for the full design.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

// DefaultActionListLimit / MaxActionListLimit bound a single BoidOpActionList
// call's row count — not its response size. MaxActionListLimit (1000) ×
// each action's own payload cap (MaxContentBytes = 64KiB, content_size.go)
// puts the mathematical worst case at ~64MB, but real payloads are far
// smaller in practice, keeping a 1000-row response small today. Revisit
// (tighten the row cap, or add a real response-byte budget) once a
// workspace is actually driving this op at scale.
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
// executor scoping (mirroring BoidOpTaskList/BoidOpCardList) is the primary
// enforcement point; this is defense in depth at the query itself.
var ErrActionListUnscoped = errors.New("action list: no project or workspace scope given")

// ActionListFilter scopes and paginates ListActionsSince.
type ActionListFilter struct {
	// ProjectIDs scopes to a set of projects via a single SQL IN(...) — used
	// both for an explicit single --project-id and for the "neither project
	// nor workspace given" fallback (TokenContext.AllowedProjectIDs). A
	// single query rather than one-call-per-project is deliberate: merging
	// cursor-paginated results across N separately-limited per-project
	// queries would need a real N-way merge, which a single IN(...) query
	// avoids entirely.
	ProjectIDs []string
	// WorkspaceID scopes via project_workspaces, matching ListTasks' own
	// WorkspaceID join (store.go). In practice mutually exclusive with
	// ProjectIDs, but nothing here forbids combining them (both apply as an AND).
	WorkspaceID string
	// TaskID additionally narrows to one task's actions. Safe to combine
	// with ProjectIDs/WorkspaceID as a plain AND condition: a task outside
	// the scope simply matches zero rows.
	TaskID string
	// Since is the opaque wire cursor (EncodeActionCursor's output) marking
	// the exclusive lower bound of an append-only scan — empty means "from
	// the beginning". Decoded internally so callers never need to know the
	// encoding. There is deliberately no "start from now, skip all history"
	// sentinel: at today's action volume, paging the full history from
	// scratch is a handful of calls, not a real cost.
	Since string
	// Limit is clamped internally via ClampActionListLimit — any value
	// (including 0 or negative) is accepted.
	Limit int
}

// EncodeActionCursor renders a (created_at, id) pair as the opaque cursor
// string BoidOpActionList's wire contract carries.
//
// id is part of the cursor — not just created_at — because actions.id is a
// random UUID, not a creation-ordered sequence: two actions created in the
// same instant are otherwise indistinguishable by created_at alone, risking
// a silently skipped or re-delivered row. Ordering by (created_at, id)
// together, on both the SELECT's ORDER BY and the cursor's WHERE threshold,
// is the standard keyset-pagination technique for this tied-key case.
//
// Real same-instant ties come from internal/dispatcher/store.go's
// markStaleTasksAborted, which takes one now := time.Now().UTC() outside its
// per-task loop and inserts N action rows sharing that exact value. See
// TestListActionsSince_TiedCreatedAt_PageBoundary_NoDuplicatesNoGaps for the
// reproduction.
func EncodeActionCursor(createdAt time.Time, id string) string {
	return createdAt.UTC().Format(time.RFC3339Nano) + "|" + id
}

// DecodeActionCursor parses EncodeActionCursor's output. An empty string
// decodes to the zero Time and empty id ("from the beginning") without
// error. A non-empty but malformed cursor is a hard error: silently treating
// a corrupted cursor as "from the beginning" would quietly re-deliver a
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

// ListActionsSince is BoidOpActionList's store implementation: a single
// append-only, workspace-scoped read across every task's actions, replacing
// the per-task-only ListActionsByTask (store.go) for this use case — see
// this file's package doc comment for why a per-task loop is the wrong
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
		// it) but not redundant for SQLite's query planner: SQLite cannot
		// derive a range bound from an OR of two AND-clauses, so without
		// this plain inequality it falls back to a full index-order scan
		// from the beginning instead of a real range seek on
		// idx_actions_created_at_id.
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
