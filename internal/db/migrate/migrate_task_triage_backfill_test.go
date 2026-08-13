package migrate

import (
	"testing"

	"github.com/novshi-tech/boid/internal/db"
)

// 0040 backfills the task_triage sidecar so "has a row" is a reliable "is a
// triage task" test (docs/plans/cross-project-issue-triage.md Phase 1 PR-5a),
// and converges the urgency/kind that pre-PR-5a writes left inside the opaque
// detail blob. Both halves have real branching, so they are pinned against a
// real database rather than trusted by reading the SQL.
func TestApply_0040_BackfillsTaskTriageRows(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	// Migrate up to (but not including) 0040 by applying everything, then
	// simulating the pre-0040 world: delete the rows 0040 would have created
	// and re-run just that migration's statements via a second Apply. Simpler
	// and equally faithful: apply everything, insert pre-0040-shaped rows, and
	// assert Apply is idempotent enough to converge them on a second run.
	if err := Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	if _, err := d.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('p1', '/tmp/p1')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	insertTask := func(id, status string) {
		t.Helper()
		if _, err := d.Conn.Exec(
			`INSERT INTO tasks (id, project_id, title, status, behavior) VALUES (?, 'p1', 't', ?, 'dev')`,
			id, status,
		); err != nil {
			t.Fatalf("insert task %s: %v", id, err)
		}
	}
	insertTask("card", "triaged")
	insertTask("ordinary", "executing")
	insertTask("legacy", "working")

	// A pre-PR-5a sidecar: urgency/kind live in the blob, columns are empty.
	if _, err := d.Conn.Exec(
		`INSERT INTO task_triage (task_id, detail) VALUES ('legacy', ?)`,
		`{"attrs":{"urgency":"today","kind":"issue","summary":"keep me"}}`,
	); err != nil {
		t.Fatalf("insert legacy sidecar: %v", err)
	}

	// Re-run 0040's statements the way a fresh daemon would.
	if _, err := d.Conn.Exec(`DELETE FROM schema_migrations WHERE version = '0040_backfill_task_triage_rows'`); err != nil {
		t.Fatalf("reset 0040: %v", err)
	}
	if err := Apply(d.Conn); err != nil {
		t.Fatalf("re-apply migrations: %v", err)
	}

	// (1) the triage-status task gained a row; the ordinary one did not.
	assertRow := func(taskID string, want bool) {
		t.Helper()
		var n int
		if err := d.Conn.QueryRow(`SELECT COUNT(*) FROM task_triage WHERE task_id = ?`, taskID).Scan(&n); err != nil {
			t.Fatalf("count sidecar %s: %v", taskID, err)
		}
		if (n > 0) != want {
			t.Fatalf("task %s: sidecar rows = %d, want present=%v", taskID, n, want)
		}
	}
	assertRow("card", true)
	assertRow("ordinary", false)

	// (2) the legacy blob's urgency/kind were promoted to the columns and
	// removed from the blob, while the untouched keys survived.
	var urgency, kind, detail string
	if err := d.Conn.QueryRow(
		`SELECT urgency, kind, detail FROM task_triage WHERE task_id = 'legacy'`,
	).Scan(&urgency, &kind, &detail); err != nil {
		t.Fatalf("read legacy sidecar: %v", err)
	}
	if urgency != "today" || kind != "issue" {
		t.Fatalf("urgency=%q kind=%q, want promoted from the blob", urgency, kind)
	}
	var stillThere int
	if err := d.Conn.QueryRow(
		`SELECT (json_extract(detail,'$.attrs.urgency') IS NOT NULL)
		      + (json_extract(detail,'$.attrs.kind') IS NOT NULL)
		 FROM task_triage WHERE task_id = 'legacy'`,
	).Scan(&stillThere); err != nil {
		t.Fatalf("check blob: %v", err)
	}
	if stillThere != 0 {
		t.Fatalf("promoted keys still in the blob (drift): %s", detail)
	}
	var summary string
	if err := d.Conn.QueryRow(
		`SELECT json_extract(detail,'$.attrs.summary') FROM task_triage WHERE task_id = 'legacy'`,
	).Scan(&summary); err != nil {
		t.Fatalf("check summary: %v", err)
	}
	if summary != "keep me" {
		t.Fatalf("summary = %q, want the untouched keys preserved", summary)
	}
}
