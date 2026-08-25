package orchestrator_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// newTestDB opens an in-memory DB via db.Open (not a bare sql.Open) so
// PRAGMA foreign_keys=ON is set — required by other tables' FKs (e.g.
// identity bindings) even though card-model-cleanup PR-2 folded the old
// task_triage sidecar's own ON DELETE CASCADE into a plain same-row delete
// (see TestTaskTriage_DeleteAndCascadeOnTaskDelete's updated doc comment).
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return d.Conn
}

// createTestTask inserts a CARD row directly (type='card') — every function
// in card.go this file exercises (UpsertTaskTriage/GetTaskTriage/
// ListTaskTriageByTaskIDs/DeleteTaskTriage/ParkedFrom) reads/writes a card's
// kind/urgency/wake_at/wake_task_id/suggestion_verb/detail columns on the
// unified tasks row (WHERE type = 'card') since card-model-cleanup PR-2
// (migration 0045) folded the old task_triage sidecar table into tasks
// itself. status must be one of the four card statuses (parked/working/
// done/dropped) — migration 0045's CHECK constraint on tasks.status now
// rejects anything else for a type='card' row.
func createTestTask(t *testing.T, dbtx interface {
	Exec(string, ...any) (sql.Result, error)
}, id string, status orchestrator.TaskStatus) {
	t.Helper()
	now := time.Now().UTC()
	_, err := dbtx.Exec(
		`INSERT INTO projects (id, work_dir, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		"proj-"+id, "/tmp/"+id, now, now,
	)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	_, err = dbtx.Exec(
		`INSERT INTO tasks (id, type, project_id, title, status, kind, urgency, wake_task_id, suggestion_verb, detail, created_at, updated_at)
		 VALUES (?, 'card', ?, ?, ?, '', '', '', '', '{}', ?, ?)`,
		id, "proj-"+id, "task "+id, string(status), now, now,
	)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
}

func TestTaskTriage_UpsertAndGet(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusParked)

	wakeAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	tt := &orchestrator.CardAttrs{
		TaskID:  "t1",
		Kind:    "issue",
		Urgency: "today",
		WakeAt:  &wakeAt,
		Detail:  []byte(`{"summary":"test"}`),
	}
	if err := orchestrator.UpsertTaskTriage(conn, tt); err != nil {
		t.Fatalf("UpsertTaskTriage: %v", err)
	}

	got, err := orchestrator.GetTaskTriage(conn, "t1")
	if err != nil {
		t.Fatalf("GetTaskTriage: %v", err)
	}
	if got.Kind != "issue" || got.Urgency != "today" {
		t.Fatalf("got %+v", got)
	}
	if got.WakeAt == nil || !got.WakeAt.Equal(wakeAt) {
		t.Fatalf("WakeAt = %v, want %v", got.WakeAt, wakeAt)
	}
	if string(got.Detail) != `{"summary":"test"}` {
		t.Fatalf("Detail = %s", got.Detail)
	}
}

// TestTaskTriage_SuggestionVerb_RoundTrips pins suggestion_verb's promotion
// to a real task_triage column (docs/plans/suggestion-as-state-transition-impl.md
// §4.1, migration 0044): UpsertTaskTriage/GetTaskTriage must round-trip it
// exactly like kind/urgency do — the queue predicate (store.go's
// "queue_next" branch) reads this column directly, so a write that silently
// dropped it would make every suggestion invisible to the queue.
func TestTaskTriage_SuggestionVerb_RoundTrips(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusParked)

	tt := &orchestrator.CardAttrs{TaskID: "t1", SuggestionVerb: "go"}
	if err := orchestrator.UpsertTaskTriage(conn, tt); err != nil {
		t.Fatalf("UpsertTaskTriage: %v", err)
	}

	got, err := orchestrator.GetTaskTriage(conn, "t1")
	if err != nil {
		t.Fatalf("GetTaskTriage: %v", err)
	}
	if got.SuggestionVerb != "go" {
		t.Fatalf("SuggestionVerb = %q, want %q", got.SuggestionVerb, "go")
	}
}

// TestTaskTriage_SuggestionVerb_DefaultsToEmpty pins the column's "no
// suggestion" representation: an empty string, the same TEXT NOT NULL
// DEFAULT ” convention urgency/kind already use (NOT a nullable column —
// see migration 0044's own doc comment for why this PR chose that over the
// brief's literal "IS NOT NULL" wording).
func TestTaskTriage_SuggestionVerb_DefaultsToEmpty(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusParked)

	if err := orchestrator.UpsertTaskTriage(conn, &orchestrator.CardAttrs{TaskID: "t1"}); err != nil {
		t.Fatalf("UpsertTaskTriage: %v", err)
	}
	got, err := orchestrator.GetTaskTriage(conn, "t1")
	if err != nil {
		t.Fatalf("GetTaskTriage: %v", err)
	}
	if got.SuggestionVerb != "" {
		t.Fatalf("SuggestionVerb = %q, want empty", got.SuggestionVerb)
	}
}

// TestListTaskTriageByTaskIDs_IncludesSuggestionVerb pins the batch-fetch
// path (the one the Queue/Parked Web UI views and khi's ListCards actually
// use) alongside the single-row GetTaskTriage coverage above — a column
// present in one query and forgotten in the other would silently blank the
// verb badge specifically on list views.
func TestListTaskTriageByTaskIDs_IncludesSuggestionVerb(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusParked)
	if err := orchestrator.UpsertTaskTriage(conn, &orchestrator.CardAttrs{TaskID: "t1", SuggestionVerb: "park"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := orchestrator.ListTaskTriageByTaskIDs(conn, []string{"t1"})
	if err != nil {
		t.Fatalf("ListTaskTriageByTaskIDs: %v", err)
	}
	if got["t1"] == nil || got["t1"].SuggestionVerb != "park" {
		t.Fatalf("got[t1] = %+v, want SuggestionVerb=park", got["t1"])
	}
}

func TestTaskTriage_UpsertUpdatesExistingRow(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusParked)

	if err := orchestrator.UpsertTaskTriage(conn, &orchestrator.CardAttrs{TaskID: "t1", Urgency: "week"}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := orchestrator.UpsertTaskTriage(conn, &orchestrator.CardAttrs{TaskID: "t1", Urgency: "now"}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, err := orchestrator.GetTaskTriage(conn, "t1")
	if err != nil {
		t.Fatalf("GetTaskTriage: %v", err)
	}
	if got.Urgency != "now" {
		t.Fatalf("Urgency = %q, want %q (upsert should update, not duplicate)", got.Urgency, "now")
	}
}

func TestTaskTriage_GetNotFound(t *testing.T) {
	conn := newTestDB(t)
	if _, err := orchestrator.GetTaskTriage(conn, "missing"); err == nil {
		t.Fatal("expected error for missing task_triage row")
	}
}

// createExecutionTestTask inserts a plain EXECUTION row (type='execution')
// — the card-model-cleanup PR-2 counterpart to createTestTask's card-only
// insert, needed by tests that must exercise "this task is not a card at
// all" (a card row can no longer exist without its CardAttrs columns, since
// they live on the same row — there is no more "rowless card" state to
// construct; the only way to get a genuinely absent CardAttrs is a
// different task TYPE entirely).
func createExecutionTestTask(t *testing.T, dbtx interface {
	Exec(string, ...any) (sql.Result, error)
}, id string) {
	t.Helper()
	now := time.Now().UTC()
	_, err := dbtx.Exec(
		`INSERT INTO projects (id, work_dir, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		"proj-"+id, "/tmp/"+id, now, now,
	)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	_, err = dbtx.Exec(
		`INSERT INTO tasks (id, type, project_id, title, status, behavior, traits, readonly, branch_prefix, base_branch, payload, instructions, auto_start, created_at, updated_at)
		 VALUES (?, 'execution', ?, ?, 'pending', '', '[]', FALSE, '', '', '{}', '[]', FALSE, ?, ?)`,
		id, "proj-"+id, "task "+id, now, now,
	)
	if err != nil {
		t.Fatalf("insert execution task: %v", err)
	}
}

// TestListTaskTriageByTaskIDs_ReturnsRequestedRowsOnly is the store-side
// pin for BD-8 残件1's batch fetch: mixed presence (a card, a card with no
// attrs ever set, an execution task that is not a card at all, and an id
// that doesn't exist) in a single call. card-model-cleanup PR-2: a card
// always "has" its CardAttrs columns (they're part of the same row), so
// "no task_triage row" is no longer expressible for a card — the case this
// test now exercises for "must be absent from the map" is t3, an
// EXECUTION task (never a card to begin with).
func TestListTaskTriageByTaskIDs_ReturnsRequestedRowsOnly(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusParked)
	createTestTask(t, conn, "t2", orchestrator.TaskStatusParked)
	createExecutionTestTask(t, conn, "t3") // not a card at all

	if err := orchestrator.UpsertTaskTriage(conn, &orchestrator.CardAttrs{TaskID: "t1", Urgency: "now"}); err != nil {
		t.Fatalf("upsert t1: %v", err)
	}
	if err := orchestrator.UpsertTaskTriage(conn, &orchestrator.CardAttrs{TaskID: "t2", Urgency: "today"}); err != nil {
		t.Fatalf("upsert t2: %v", err)
	}

	got, err := orchestrator.ListTaskTriageByTaskIDs(conn, []string{"t1", "t2", "t3", "does-not-exist-at-all"})
	if err != nil {
		t.Fatalf("ListTaskTriageByTaskIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2, got %+v", len(got), got)
	}
	if got["t1"] == nil || got["t1"].Urgency != "now" {
		t.Errorf("got[t1] = %+v, want Urgency=now", got["t1"])
	}
	if got["t2"] == nil || got["t2"].Urgency != "today" {
		t.Errorf("got[t2] = %+v, want Urgency=today", got["t2"])
	}
	if _, ok := got["t3"]; ok {
		t.Error(`"t3" is an execution task, not a card, and must be absent from the map`)
	}
	if _, ok := got["does-not-exist-at-all"]; ok {
		t.Error(`"does-not-exist-at-all" must be absent from the map`)
	}
}

// TestListTaskTriageByTaskIDs_EmptyInput covers the degenerate call (e.g.
// triageByTaskID called with zero tasks) — must return an empty map, not
// error or panic on an empty IN (...) clause.
func TestListTaskTriageByTaskIDs_EmptyInput(t *testing.T) {
	conn := newTestDB(t)
	got, err := orchestrator.ListTaskTriageByTaskIDs(conn, nil)
	if err != nil {
		t.Fatalf("ListTaskTriageByTaskIDs(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0", len(got))
	}
}

// TestListTaskTriageByTaskIDs_ChunksAcrossInClauseLimit is the chunking
// regression: requesting more task IDs than fit in a single `IN (...)`
// clause (taskTriageInClauseChunkSize) must still return every matching row
// — no silent drop at the chunk boundary, no duplicate rows across chunks.
func TestListTaskTriageByTaskIDs_ChunksAcrossInClauseLimit(t *testing.T) {
	conn := newTestDB(t)
	const n = 550 // > the 500-row chunk size, so this exercises 2 chunks
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("chunk-t%04d", i)
		ids[i] = id
		createTestTask(t, conn, id, orchestrator.TaskStatusParked)
		if err := orchestrator.UpsertTaskTriage(conn, &orchestrator.CardAttrs{TaskID: id, Urgency: "week"}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}

	got, err := orchestrator.ListTaskTriageByTaskIDs(conn, ids)
	if err != nil {
		t.Fatalf("ListTaskTriageByTaskIDs: %v", err)
	}
	if len(got) != n {
		t.Fatalf("len(got) = %d, want %d (a chunk boundary must not drop or duplicate rows)", len(got), n)
	}
	for _, id := range ids {
		if got[id] == nil {
			t.Fatalf("missing row for %s", id)
		}
	}
}

// TestListTaskTriageByTaskIDs_OneRowScanErrorDoesNotSinkOthers is the Opus
// review finding (2026-08-18): an earlier version of this function
// returned early (discarding everything, including rows from earlier
// chunks) the moment any single row failed to Scan. A malformed wake_at
// value — written by something other than UpsertTaskTriage's own
// nullableTime path, e.g. a manual `UPDATE` — is a real, if narrow, way for
// exactly one row's Scan to fail independent of the query itself
// succeeding. That must not cost every other row in the batch its
// enrichment (決定9: a whole tab silently losing every suggestion/urgency
// badge because of one bad row is the exact failure class this PR exists
// to close).
func TestListTaskTriageByTaskIDs_OneRowScanErrorDoesNotSinkOthers(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "good1", orchestrator.TaskStatusParked)
	createTestTask(t, conn, "bad", orchestrator.TaskStatusParked)
	createTestTask(t, conn, "good2", orchestrator.TaskStatusParked)
	for _, id := range []string{"good1", "bad", "good2"} {
		if err := orchestrator.UpsertTaskTriage(conn, &orchestrator.CardAttrs{TaskID: id, Urgency: "now"}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	// Bypass UpsertTaskTriage's nullableTime marshaling with a raw,
	// non-parseable wake_at — this is what makes rows.Scan fail for this
	// one row specifically, independent of the query itself. card-model-
	// cleanup PR-2: wake_at now lives directly on the tasks row (WHERE
	// type='card'), not a separate task_triage table.
	if _, err := conn.Exec(`UPDATE tasks SET wake_at = 'not-a-real-timestamp' WHERE id = 'bad' AND type = 'card'`); err != nil {
		t.Fatalf("raw update to induce a malformed wake_at: %v", err)
	}

	got, err := orchestrator.ListTaskTriageByTaskIDs(conn, []string{"good1", "bad", "good2"})
	if err == nil {
		t.Error("expected a non-nil error surfaced from the malformed row's Scan failure")
	}
	if got["good1"] == nil || got["good1"].Urgency != "now" {
		t.Errorf(`got["good1"] = %+v, want a populated row (must survive "bad"'s Scan failure)`, got["good1"])
	}
	if got["good2"] == nil || got["good2"].Urgency != "now" {
		t.Errorf(`got["good2"] = %+v, want a populated row (must survive "bad"'s Scan failure)`, got["good2"])
	}
	if _, ok := got["bad"]; ok {
		t.Error(`got["bad"] should be absent (its row failed to Scan), not a zero-value or partial entry`)
	}
}

func TestTaskTriage_DeleteAndCascadeOnTaskDelete(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusParked)
	if err := orchestrator.UpsertTaskTriage(conn, &orchestrator.CardAttrs{TaskID: "t1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// card-model-cleanup PR-2: the card attrs columns now live directly on
	// the tasks row itself (no more separate task_triage sidecar with its
	// own ON DELETE CASCADE FK — Opus指摘#14's original concern), so
	// deleting the task row trivially takes its card attrs with it. Still
	// worth pinning: this is the behavior GC's bare `DELETE FROM tasks`
	// relies on.
	if _, err := conn.Exec(`DELETE FROM tasks WHERE id = ?`, "t1"); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	if _, err := orchestrator.GetTaskTriage(conn, "t1"); err == nil {
		t.Fatal("expected card attrs to be gone along with its deleted task")
	}
}

// TestTaskTriage_ExplicitDelete pins card-model-cleanup PR-2's redefinition
// of DeleteTaskTriage (see its own doc comment in orchestrator/card.go):
// since card attrs now live directly on the tasks row (no separate sidecar
// row to literally delete), "delete" can only mean resetting the columns
// back to their empty defaults — the row itself, and its type='card', stay
// put. GetTaskTriage must therefore still SUCCEED after an explicit delete,
// returning a card with empty CardAttrs, not sql.ErrNoRows.
func TestTaskTriage_ExplicitDelete(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusParked)
	if err := orchestrator.UpsertTaskTriage(conn, &orchestrator.CardAttrs{TaskID: "t1", Urgency: "now", Kind: "issue"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := orchestrator.DeleteTaskTriage(conn, "t1"); err != nil {
		t.Fatalf("DeleteTaskTriage: %v", err)
	}
	got, err := orchestrator.GetTaskTriage(conn, "t1")
	if err != nil {
		t.Fatalf("GetTaskTriage after explicit delete: %v (task row/type='card' must survive; only the columns reset)", err)
	}
	if got.Urgency != "" || got.Kind != "" {
		t.Errorf("card attrs after explicit delete = %+v, want empty (Urgency/Kind reset)", got)
	}
}

// ParkedFrom は actions ログ (from_status) から park 直前の状態を導出する。
// task_triage に parked_from 列を重複保存しない設計 (Opus指摘#1、決定13準拠)。
func TestParkedFrom_DerivesFromLatestParkAction(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusParked)

	// card-model-cleanup PR-2: machine_card.go's park rule now has exactly
	// one edge (working → parked) — the old three-way triaged/ready/working
	// origin is gone along with those statuses themselves. "working" is the
	// only currently-valid FromStatus a real park action ever carries.
	if err := orchestrator.CreateAction(conn, &orchestrator.Action{
		TaskID: "t1", Type: "park", FromStatus: orchestrator.TaskStatusWorking, ToStatus: orchestrator.TaskStatusParked,
	}); err != nil {
		t.Fatalf("create park action: %v", err)
	}

	from, err := orchestrator.ParkedFrom(conn, "t1")
	if err != nil {
		t.Fatalf("ParkedFrom: %v", err)
	}
	if from != orchestrator.TaskStatusWorking {
		t.Fatalf("ParkedFrom = %q, want %q", from, orchestrator.TaskStatusWorking)
	}
}

func TestParkedFrom_UsesMostRecentParkAction(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusParked)

	// card-model-cleanup PR-2: park now only ever fires from "working" in
	// production (machine_card.go's single park edge) — the old three-way
	// triaged/ready/working origin this test used to model no longer exists
	// (those statuses are gone). The FIRST park action below uses a
	// different-but-still-valid FromStatus (done) purely as a distinguishable
	// marker so this test can tell "used the most recent row" apart from
	// "used any row" — it does not model a realistic card lifecycle. The
	// LAST (most recent) park action uses "working", the only FromStatus a
	// real park action carries today.
	actions := []struct {
		typ  string
		from orchestrator.TaskStatus
		to   orchestrator.TaskStatus
	}{
		{"park", orchestrator.TaskStatusDone, orchestrator.TaskStatusParked}, // older, distinguishable marker only
		{"reopen", orchestrator.TaskStatusParked, orchestrator.TaskStatusWorking},
		{"park", orchestrator.TaskStatusWorking, orchestrator.TaskStatusParked}, // most recent, realistic
	}
	for i, a := range actions {
		time.Sleep(time.Millisecond) // ensure created_at ordering is stable
		if err := orchestrator.CreateAction(conn, &orchestrator.Action{
			TaskID: "t1", Type: a.typ, FromStatus: a.from, ToStatus: a.to,
		}); err != nil {
			t.Fatalf("create action %d: %v", i, err)
		}
	}

	from, err := orchestrator.ParkedFrom(conn, "t1")
	if err != nil {
		t.Fatalf("ParkedFrom: %v", err)
	}
	if from != orchestrator.TaskStatusWorking {
		t.Fatalf("ParkedFrom = %q, want %q (most recent park action)", from, orchestrator.TaskStatusWorking)
	}
}

func TestParkedFrom_NoParkAction(t *testing.T) {
	conn := newTestDB(t)
	createTestTask(t, conn, "t1", orchestrator.TaskStatusParked)
	if _, err := orchestrator.ParkedFrom(conn, "t1"); err == nil {
		t.Fatal("expected error when no park action exists")
	}
}
