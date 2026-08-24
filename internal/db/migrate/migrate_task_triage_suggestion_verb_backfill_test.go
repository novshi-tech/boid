package migrate

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
)

// backfillOnlySQL extracts 0044's backfill UPDATE statements (everything
// after the ALTER TABLE) directly from the real migration file — reading it
// through the SAME embed.FS the package uses, so the test can never drift
// from the actual migration text. This exists purely to work around a
// testing wrinkle: unlike 0040 (pure UPDATEs, safely re-runnable), 0044
// leads with a non-idempotent `ALTER TABLE ... ADD COLUMN`, so the
// delete-schema_migrations-row-then-reapply trick 0040's own test uses
// would fail on a second run ("duplicate column name"). Re-executing just
// the backfill half against fixture rows inserted AFTER the column already
// exists is equivalent to "a fresh daemon runs 0044 once against a DB that
// already has these tasks/sidecars" — the real cutover scenario — without
// needing to fight the ALTER TABLE's one-shot nature.
func backfillOnlySQL(t *testing.T) string {
	t.Helper()
	raw, err := schemaFS.ReadFile("migrations/0044_add_task_triage_suggestion_verb.sql")
	if err != nil {
		t.Fatalf("read 0044 migration file: %v", err)
	}
	parts := strings.SplitN(string(raw), ";", 2)
	if len(parts) != 2 {
		t.Fatalf("0044 migration file has no ALTER TABLE/backfill split point: %s", raw)
	}
	if !strings.Contains(parts[0], "ALTER TABLE") {
		t.Fatalf("expected the first statement to be the ALTER TABLE, got: %s", parts[0])
	}
	return parts[1]
}

// 0044 promotes suggestion_verb out of the opaque detail blob into a real
// column (docs/plans/suggestion-as-state-transition-impl.md §4.1). PR #988
// review, MEDIUM 2: the backfill must NOT resurrect a suggestion recorded on
// a card that was already terminated (done/dropped/aborted) BEFORE this
// migration ran — the cutover runbook (docs/plans/
// suggestion-as-state-transition-impl.md §6) has nose terminate every
// pre-cutover card on the OLD daemon (step 2) before the new daemon (which
// carries this migration) is deployed (step 4). The old daemon's direct
// drop/done actions do not strip detail.attrs.suggestion (that strip is new
// in PR-1/PR-2), so a card terminated with a still-pending suggestion in its
// blob would otherwise get suggestion_verb promoted on first boot of the new
// daemon and — since queue_next's predicate is now status-agnostic
// (suggestion_verb != ”, store.go) — sit in the Queue tab forever (until a
// Reject or 30-day GC), directly contradicting design doc §3.6's "the queue
// only shows what the machine still needs a decision on".
func TestApply_0044_BackfillExcludesTerminalCards(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

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
	insertTask("parked-card", "parked")
	insertTask("working-card", "working")
	insertTask("done-card", "done")
	insertTask("dropped-card", "dropped")
	insertTask("aborted-card", "aborted")

	insertSidecar := func(taskID string) {
		t.Helper()
		if _, err := d.Conn.Exec(
			`INSERT INTO task_triage (task_id, detail) VALUES (?, ?)`,
			taskID, `{"attrs":{"suggestion":{"verb":"drop","reason":"stale, pre-cutover"}}}`,
		); err != nil {
			t.Fatalf("insert sidecar %s: %v", taskID, err)
		}
	}
	insertSidecar("parked-card")
	insertSidecar("working-card")
	insertSidecar("done-card")
	insertSidecar("dropped-card")
	insertSidecar("aborted-card")

	// Re-run 0044's backfill (only — the ALTER TABLE already ran as part of
	// the full Apply() above and cannot run twice) against these
	// just-inserted fixture rows, exactly as it would run against
	// already-existing rows on a fresh daemon's first boot.
	if _, err := d.Conn.Exec(backfillOnlySQL(t)); err != nil {
		t.Fatalf("re-run 0044 backfill: %v", err)
	}

	assertVerb := func(taskID, want string) {
		t.Helper()
		var got string
		if err := d.Conn.QueryRow(`SELECT suggestion_verb FROM task_triage WHERE task_id = ?`, taskID).Scan(&got); err != nil {
			t.Fatalf("read suggestion_verb %s: %v", taskID, err)
		}
		if got != want {
			t.Errorf("task %s: suggestion_verb = %q, want %q", taskID, got, want)
		}
	}
	// Live cards: backfill promotes the blob's verb as designed.
	assertVerb("parked-card", "drop")
	assertVerb("working-card", "drop")
	// Terminal cards: the backfill must NOT resurrect a stale suggestion —
	// these cards were already terminated before the new daemon (and this
	// migration) ever ran.
	assertVerb("done-card", "")
	assertVerb("dropped-card", "")
	assertVerb("aborted-card", "")
}
