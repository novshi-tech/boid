package migrate

import (
	"database/sql"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
)

// applyThrough applies allMigrations() (migrate.go) up to and including
// stopAtVersion. Unlike Apply() — which always runs the whole chain — this
// lets a test build a database in the SHAPE migration 0045 expects to find
// on input (task_triage table present, legacy tasks columns, pre-cutover
// statuses allowed) before exercising 0045 itself. Mirrors Apply()'s own
// apply loop minus the schema-ceiling check (irrelevant for a fresh
// in-memory DB with nothing pre-recorded in schema_migrations).
func applyThrough(t *testing.T, conn *sql.DB, stopAtVersion string) {
	t.Helper()
	if err := ensureSchemaMigrationsTable(conn); err != nil {
		t.Fatalf("ensure schema_migrations table: %v", err)
	}
	for _, m := range allMigrations() {
		if err := applyMigration(conn, m); err != nil {
			t.Fatalf("apply migration %s: %v", m.version, err)
		}
		if m.version == stopAtVersion {
			return
		}
	}
	t.Fatalf("version %q not found in allMigrations()", stopAtVersion)
}

const lastPre0045Version = "0044_add_task_triage_suggestion_verb"

// pre0045Fixture is one row inserted into the pre-migration-0045 tasks (and,
// optionally, task_triage) tables.
type pre0045Fixture struct {
	id       string
	status   string
	behavior string
	// hasSidecar, when true, also inserts a task_triage row for id.
	hasSidecar             bool
	kind, urgency, wakeAt  string
	wakeTaskID, suggestion string
	detail                 string
}

func insertPre0045Task(t *testing.T, conn *sql.DB, f pre0045Fixture) {
	t.Helper()
	if _, err := conn.Exec(
		`INSERT INTO tasks (id, project_id, title, status, behavior) VALUES (?, 'p1', ?, ?, ?)`,
		f.id, f.id, f.status, f.behavior,
	); err != nil {
		t.Fatalf("insert pre-0045 task %s: %v", f.id, err)
	}
	if f.hasSidecar {
		detail := f.detail
		if detail == "" {
			detail = "{}"
		}
		var wakeAt any
		if f.wakeAt != "" {
			wakeAt = f.wakeAt
		}
		if _, err := conn.Exec(
			`INSERT INTO task_triage (task_id, kind, urgency, wake_at, wake_task_id, suggestion_verb, detail) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			f.id, f.kind, f.urgency, wakeAt, f.wakeTaskID, f.suggestion, detail,
		); err != nil {
			t.Fatalf("insert pre-0045 sidecar %s: %v", f.id, err)
		}
	}
}

// setupPre0045Fixtures builds the "混在 DB" fixture card-model-cleanup.md §8
// asks the migration test to cover: a card with a sidecar row, a rowless
// card (no sidecar row, card-lifecycle status), a legacy-status card (with
// and without a sidecar row), and an ordinary execution task — then applies
// migration 0045 and returns the connection for assertions.
func setupPre0045Fixtures(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	applyThrough(t, d.Conn, lastPre0045Version)

	if _, err := d.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('p1', '/tmp/p1')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	// 1. Card with a sidecar row, live status.
	insertPre0045Task(t, d.Conn, pre0045Fixture{
		id: "card-with-row", status: "parked", behavior: "",
		hasSidecar: true, kind: "issue", urgency: "now", suggestion: "go",
		detail: `{"summary":"has a sidecar"}`,
	})
	// 2. Rowless card: no task_triage row at all, but a card-lifecycle status
	// (the SeedTaskTriage best-effort-seeding gap this migration's type
	// judgment step 2 exists to rescue).
	insertPre0045Task(t, d.Conn, pre0045Fixture{
		id: "rowless-working-card", status: "working", behavior: "",
	})
	// 3. Legacy-status card WITH a sidecar row: washed over to parked, its
	// sidecar content preserved.
	insertPre0045Task(t, d.Conn, pre0045Fixture{
		id: "legacy-triaged-with-row", status: "triaged", behavior: "",
		hasSidecar: true, kind: "signal", urgency: "week",
	})
	// 4. Legacy-status card WITHOUT a sidecar row: washed over to parked via
	// the rowless-card fallback, empty CardAttrs.
	insertPre0045Task(t, d.Conn, pre0045Fixture{
		id: "legacy-captured-rowless", status: "captured", behavior: "",
	})
	// 5. Dropped card, rowless (dropped is card-lifecycle per
	// isCardLifecycleStatus's pre-PR-2 reasoning, not washed — dropped is a
	// terminal card status, not one of the 3 legacy ones).
	insertPre0045Task(t, d.Conn, pre0045Fixture{
		id: "dropped-card-rowless", status: "dropped", behavior: "",
	})
	// 6. Done card WITH a sidecar row: the row is the ONLY signal that
	// distinguishes this from an ordinary done execution task (done/aborted
	// are deliberately excluded from the rowless-card status fallback).
	insertPre0045Task(t, d.Conn, pre0045Fixture{
		id: "done-card-with-row", status: "done", behavior: "",
		hasSidecar: true, kind: "issue",
	})
	// 7. Ordinary execution task, no sidecar row, executing.
	insertPre0045Task(t, d.Conn, pre0045Fixture{
		id: "exec-executing", status: "executing", behavior: "executor",
	})
	// 8. Ordinary execution task, done, no sidecar row (must NOT be
	// misclassified as a card just because done is shared vocabulary).
	insertPre0045Task(t, d.Conn, pre0045Fixture{
		id: "exec-done", status: "done", behavior: "executor",
	})

	if err := applyMigration(d.Conn, mustFindMigration(t, "0045_card_sti_migration")); err != nil {
		t.Fatalf("apply 0045: %v", err)
	}
	return d.Conn
}

func mustFindMigration(t *testing.T, version string) migration {
	t.Helper()
	for _, m := range allMigrations() {
		if m.version == version {
			return m
		}
	}
	t.Fatalf("migration %q not found", version)
	return migration{}
}

func queryTaskRow(t *testing.T, conn *sql.DB, id string) (taskType, status string, behavior, kind, urgency, suggestionVerb sql.NullString) {
	t.Helper()
	row := conn.QueryRow(`SELECT type, status, behavior, kind, urgency, suggestion_verb FROM tasks WHERE id = ?`, id)
	if err := row.Scan(&taskType, &status, &behavior, &kind, &urgency, &suggestionVerb); err != nil {
		t.Fatalf("query task %s: %v", id, err)
	}
	return
}

func TestApply_0045_TypeJudgment(t *testing.T) {
	conn := setupPre0045Fixtures(t)

	cases := []struct {
		id         string
		wantType   string
		wantStatus string
	}{
		{"card-with-row", "card", "parked"},
		{"rowless-working-card", "card", "working"},
		{"legacy-triaged-with-row", "card", "parked"},
		{"legacy-captured-rowless", "card", "parked"},
		{"dropped-card-rowless", "card", "dropped"},
		{"done-card-with-row", "card", "done"},
		{"exec-executing", "execution", "executing"},
		{"exec-done", "execution", "done"},
	}
	for _, c := range cases {
		taskType, status, _, _, _, _ := queryTaskRow(t, conn, c.id)
		if taskType != c.wantType {
			t.Errorf("%s: type = %q, want %q", c.id, taskType, c.wantType)
		}
		if status != c.wantStatus {
			t.Errorf("%s: status = %q, want %q", c.id, status, c.wantStatus)
		}
	}
}

func TestApply_0045_SidecarContentMigratedToCardColumns(t *testing.T) {
	conn := setupPre0045Fixtures(t)

	_, _, behavior, kind, urgency, verb := queryTaskRow(t, conn, "card-with-row")
	if behavior.Valid {
		t.Errorf("card-with-row: behavior should be NULL, got %v", behavior)
	}
	if kind.String != "issue" || urgency.String != "now" || verb.String != "go" {
		t.Errorf("card-with-row: kind=%q urgency=%q verb=%q, want issue/now/go", kind.String, urgency.String, verb.String)
	}
	var detail string
	if err := conn.QueryRow(`SELECT detail FROM tasks WHERE id = 'card-with-row'`).Scan(&detail); err != nil {
		t.Fatalf("read detail: %v", err)
	}
	if detail != `{"summary":"has a sidecar"}` {
		t.Errorf("detail = %q, want the sidecar's original blob preserved", detail)
	}
}

func TestApply_0045_RowlessCardGetsEmptyCardAttrs(t *testing.T) {
	conn := setupPre0045Fixtures(t)

	_, _, _, kind, urgency, verb := queryTaskRow(t, conn, "rowless-working-card")
	if !kind.Valid || kind.String != "" || !urgency.Valid || urgency.String != "" || !verb.Valid || verb.String != "" {
		t.Errorf("rowless-working-card: kind/urgency/verb = %q/%q/%q, want empty-but-non-null (NOT NULL for a card row)", kind.String, urgency.String, verb.String)
	}
}

func TestApply_0045_ExecutionRowsKeepBehaviorAndNullCardColumns(t *testing.T) {
	conn := setupPre0045Fixtures(t)

	_, _, behavior, kind, urgency, verb := queryTaskRow(t, conn, "exec-executing")
	if behavior.String != "executor" {
		t.Errorf("exec-executing: behavior = %q, want executor", behavior.String)
	}
	if kind.Valid || urgency.Valid || verb.Valid {
		t.Errorf("exec-executing: card columns should be NULL, got kind=%v urgency=%v verb=%v", kind, urgency, verb)
	}
}

// TestApply_0045_ActionsHistoryUntouched pins §6.2-3 / Q9: the status
// 洗い替え only rewrites tasks.status, never actions.from_status/to_status —
// action rows are a台帳 (audit ledger), not something migrations rewrite.
func TestApply_0045_ActionsHistoryUntouched(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	applyThrough(t, d.Conn, lastPre0045Version)
	if _, err := d.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('p1', '/tmp/p1')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	insertPre0045Task(t, d.Conn, pre0045Fixture{id: "t1", status: "triaged", behavior: ""})
	if _, err := d.Conn.Exec(
		`INSERT INTO actions (id, task_id, type, from_status, to_status) VALUES ('a1', 't1', 'triage', 'captured', 'triaged')`,
	); err != nil {
		t.Fatalf("insert action: %v", err)
	}

	if err := applyMigration(d.Conn, mustFindMigration(t, "0045_card_sti_migration")); err != nil {
		t.Fatalf("apply 0045: %v", err)
	}

	var from, to string
	if err := d.Conn.QueryRow(`SELECT from_status, to_status FROM actions WHERE id = 'a1'`).Scan(&from, &to); err != nil {
		t.Fatalf("read action: %v", err)
	}
	if from != "captured" || to != "triaged" {
		t.Errorf("action from_status/to_status = %q/%q, want the original legacy values untouched (captured/triaged)", from, to)
	}
	// The task row itself IS washed over.
	_, status, _, _, _, _ := queryTaskRow(t, d.Conn, "t1")
	if status != "parked" {
		t.Errorf("task status = %q, want parked (washed over)", status)
	}
}

// TestApply_0045_TaskTriageTableDropped pins Q6/Q7: task_triage no longer
// exists after 0045 runs.
func TestApply_0045_TaskTriageTableDropped(t *testing.T) {
	conn := setupPre0045Fixtures(t)
	exists, err := tableExists(conn, "task_triage")
	if err != nil {
		t.Fatalf("check task_triage existence: %v", err)
	}
	if exists {
		t.Error("task_triage table still exists after migration 0045")
	}
}

// TestApply_0045_CheckConstraintsRejectCrossTypeWrites pins Q10/Q11: the new
// CHECK constraint actually enforces "opposite type's fields are NULL" and
// "own type's fields (behavior excepted) are NOT NULL" — not just as
// documentation but as a real DB-level guard.
func TestApply_0045_CheckConstraintsRejectCrossTypeWrites(t *testing.T) {
	conn := setupPre0045Fixtures(t)

	// A card row may not carry a non-NULL behavior.
	if _, err := conn.Exec(`UPDATE tasks SET behavior = 'x' WHERE id = 'card-with-row'`); err == nil {
		t.Error("expected CHECK violation setting behavior on a card row, got nil error")
	}
	// An execution row may not carry a non-NULL kind.
	if _, err := conn.Exec(`UPDATE tasks SET kind = 'issue' WHERE id = 'exec-executing'`); err == nil {
		t.Error("expected CHECK violation setting kind on an execution row, got nil error")
	}
	// A card row's status must be in the card vocabulary.
	if _, err := conn.Exec(`UPDATE tasks SET status = 'executing' WHERE id = 'card-with-row'`); err == nil {
		t.Error("expected CHECK violation setting a card row's status to an execution-only value, got nil error")
	}
	// behavior = '' (empty, not NULL) is legal on an execution row (Q11: the
	// column constraint is IS NOT NULL, not a non-empty-string check).
	if _, err := conn.Exec(`UPDATE tasks SET behavior = '' WHERE id = 'exec-executing'`); err != nil {
		t.Errorf("empty-string behavior on an execution row should be legal (no-yaml project), got: %v", err)
	}
}

// TestApply_0045_TaskIdentitiesAndJobsSurviveTableRebuild pins the row
// survival of the two tables that reference tasks(id) but are NOT part of
// migration 0045's own table-rebuild: task_identities (the only table with
// an ON DELETE CASCADE FK to tasks — added in 0041) and jobs (a plain
// NO ACTION FK, added in 0001, still nullable per 0021).
//
// This is the exact seam disableForeignKeys (migrate.go) exists to cross
// safely: 0045 does `PRAGMA foreign_keys=OFF` (outside the tx, see
// migrate.go's doc comment on the modernc.org/sqlite DROP-TABLE-under-FK
// quirk), then `DROP TABLE tasks` + `ALTER TABLE tasks_new RENAME TO tasks`
// inside it. Neither task_identities nor jobs is touched by 0045's own SQL
// at all — their rows are never re-INSERTed, only left in place while the
// table they reference is swapped out from under them. Row loss (a
// CASCADE-on-drop silently firing before the rename lands) or row
// corruption (a stale reference surviving pointed at nothing) would be the
// quietest possible way this migration could break in production, since
// neither symptom raises an error — this test is the check no other test
// in this file provides (they all either read back tasks/task_triage
// columns directly or check task_triage's own existence).
func TestApply_0045_TaskIdentitiesAndJobsSurviveTableRebuild(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	applyThrough(t, d.Conn, lastPre0045Version)
	if _, err := d.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('p1', '/tmp/p1')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	insertPre0045Task(t, d.Conn, pre0045Fixture{
		id: "card-with-refs", status: "working", behavior: "",
	})
	if _, err := d.Conn.Exec(
		`INSERT INTO task_identities (identity, project_id, task_id, created_at) VALUES ('JIRA-1', 'p1', 'card-with-refs', datetime('now'))`,
	); err != nil {
		t.Fatalf("insert task_identities row: %v", err)
	}
	if _, err := d.Conn.Exec(
		`INSERT INTO jobs (id, task_id, project_id, handler_id, status, output) VALUES ('job-1', 'card-with-refs', 'p1', 'dev', 'exited', 'hello')`,
	); err != nil {
		t.Fatalf("insert jobs row: %v", err)
	}

	if err := applyMigration(d.Conn, mustFindMigration(t, "0045_card_sti_migration")); err != nil {
		t.Fatalf("apply 0045: %v", err)
	}

	// The referenced task itself must still exist under the same id (sanity
	// check that the fixture and the FK target line up) — queryTaskRow
	// itself fatals the test if the row is missing.
	queryTaskRow(t, d.Conn, "card-with-refs")

	var identityTaskID, identityProjectID string
	if err := d.Conn.QueryRow(
		`SELECT task_id, project_id FROM task_identities WHERE identity = 'JIRA-1'`,
	).Scan(&identityTaskID, &identityProjectID); err != nil {
		t.Fatalf("task_identities row lost across migration 0045: %v", err)
	}
	if identityTaskID != "card-with-refs" || identityProjectID != "p1" {
		t.Errorf("task_identities row = task_id=%q project_id=%q, want task_id=card-with-refs project_id=p1", identityTaskID, identityProjectID)
	}

	var jobTaskID, jobHandlerID, jobOutput string
	if err := d.Conn.QueryRow(
		`SELECT task_id, handler_id, output FROM jobs WHERE id = 'job-1'`,
	).Scan(&jobTaskID, &jobHandlerID, &jobOutput); err != nil {
		t.Fatalf("jobs row lost across migration 0045: %v", err)
	}
	if jobTaskID != "card-with-refs" || jobHandlerID != "dev" || jobOutput != "hello" {
		t.Errorf("jobs row = task_id=%q handler_id=%q output=%q, want task_id=card-with-refs handler_id=dev output=hello", jobTaskID, jobHandlerID, jobOutput)
	}

	// task_identities' ON DELETE CASCADE FK must still be wired to the NEW
	// tasks table post-rebuild, not silently dropped by the DROP TABLE
	// tasks — deleting the task should still cascade-delete the identity
	// row, exactly as it would have pre-migration. jobs.task_id is a plain
	// NO ACTION FK (not CASCADE, unchanged by 0045 — jobs' own schema isn't
	// touched), so it must be cleared first or the DELETE below would fail
	// on jobs' own FK rather than exercising task_identities' CASCADE at
	// all; that NO ACTION behavior is itself unrelated to what 0045 changed
	// and isn't what this assertion is checking.
	if _, err := d.Conn.Exec(`DELETE FROM jobs WHERE id = 'job-1'`); err != nil {
		t.Fatalf("delete job (to isolate the task_identities CASCADE check from jobs' own NO ACTION FK): %v", err)
	}
	if _, err := d.Conn.Exec(`DELETE FROM tasks WHERE id = 'card-with-refs'`); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	var remaining int
	if err := d.Conn.QueryRow(`SELECT COUNT(*) FROM task_identities WHERE identity = 'JIRA-1'`).Scan(&remaining); err != nil {
		t.Fatalf("count task_identities after task delete: %v", err)
	}
	if remaining != 0 {
		t.Errorf("task_identities row survived deleting its referenced task (CASCADE FK not wired to the post-0045 tasks table); remaining = %d, want 0", remaining)
	}
}
