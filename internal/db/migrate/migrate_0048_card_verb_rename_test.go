package migrate

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
)

// apply0048 inserts card-verb-rename test fixtures at the post-0047,
// pre-0048 schema shape, then applies 0048 alone. Mirrors applyThrough's own
// loop (migrate_0045_card_sti_test.go) rather than reusing it directly,
// since the fixture insert must happen strictly BETWEEN 0047 and 0048.
func apply0048(t *testing.T, conn *sql.DB) {
	t.Helper()
	for _, m := range allMigrations() {
		if m.version == "0048_card_verb_rename" {
			if err := applyMigration(conn, m); err != nil {
				t.Fatalf("apply 0048: %v", err)
			}
			return
		}
	}
	t.Fatal("0048_card_verb_rename not found in allMigrations()")
}

// TestApply_0048_RenamesSuggestionVerbColumn pins card-next-step-and-
// timeline.md §8: a card row's promoted suggestion_verb column carrying the
// retired spelling is rewritten to the current name; an already-current
// value, and an execution row's own (unrelated) columns, are untouched.
func TestApply_0048_RenamesSuggestionVerbColumn(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	applyThrough(t, d.Conn, "0047_add_tasks_idempotency_key")

	if _, err := d.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('p1', '/tmp/p1')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	insertCard := func(id, verb string) {
		t.Helper()
		if _, err := d.Conn.Exec(
			`INSERT INTO tasks (id, type, project_id, title, status, kind, urgency, wake_task_id, suggestion_verb, detail)
			 VALUES (?, 'card', 'p1', 't', 'working', '', '', '', ?, '{}')`,
			id, verb,
		); err != nil {
			t.Fatalf("insert card %s: %v", id, err)
		}
	}
	insertCard("legacy-working", "working")
	insertCard("legacy-done", "done")
	insertCard("already-start", "start")
	insertCard("no-suggestion", "")

	apply0048(t, d.Conn)

	want := map[string]string{
		"legacy-working": "start",
		"legacy-done":    "complete",
		"already-start":  "start",
		"no-suggestion":  "",
	}
	for id, wantVerb := range want {
		var got string
		if err := d.Conn.QueryRow(`SELECT suggestion_verb FROM tasks WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("query %s: %v", id, err)
		}
		if got != wantVerb {
			t.Errorf("task %s: suggestion_verb = %q, want %q", id, got, wantVerb)
		}
	}
}

// TestApply_0048_RenamesDetailSuggestionVerb pins the JSON-blob half: both
// the attrs.suggestion.verb path (the one attrs_set actually writes,
// internal/orchestrator/card.go's DetailSuggestion doc comment) and the
// top-level suggestion.verb fallback path are rewritten.
func TestApply_0048_RenamesDetailSuggestionVerb(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	applyThrough(t, d.Conn, "0047_add_tasks_idempotency_key")

	if _, err := d.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('p1', '/tmp/p1')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	insertCard := func(id string, detail string) {
		t.Helper()
		if _, err := d.Conn.Exec(
			`INSERT INTO tasks (id, type, project_id, title, status, kind, urgency, wake_task_id, suggestion_verb, detail)
			 VALUES (?, 'card', 'p1', 't', 'working', '', '', '', 'working', ?)`,
			id, detail,
		); err != nil {
			t.Fatalf("insert card %s: %v", id, err)
		}
	}
	insertCard("attrs-path", `{"attrs":{"suggestion":{"verb":"working","reason":"why"},"kept":"value"}}`)
	insertCard("top-level-path", `{"suggestion":{"verb":"done","reason":"why"}}`)

	apply0048(t, d.Conn)

	var attrsDetail string
	if err := d.Conn.QueryRow(`SELECT detail FROM tasks WHERE id = 'attrs-path'`).Scan(&attrsDetail); err != nil {
		t.Fatalf("query attrs-path: %v", err)
	}
	var m struct {
		Attrs struct {
			Suggestion struct {
				Verb   string `json:"verb"`
				Reason string `json:"reason"`
			} `json:"suggestion"`
			Kept string `json:"kept"`
		} `json:"attrs"`
	}
	if err := json.Unmarshal([]byte(attrsDetail), &m); err != nil {
		t.Fatalf("unmarshal attrs-path detail: %v", err)
	}
	if m.Attrs.Suggestion.Verb != "start" {
		t.Errorf("attrs.suggestion.verb = %q, want start", m.Attrs.Suggestion.Verb)
	}
	if m.Attrs.Suggestion.Reason != "why" {
		t.Errorf("attrs.suggestion.reason = %q, want preserved", m.Attrs.Suggestion.Reason)
	}
	if m.Attrs.Kept != "value" {
		t.Errorf("unrelated detail key dropped: %+v", m)
	}

	var topDetail string
	if err := d.Conn.QueryRow(`SELECT detail FROM tasks WHERE id = 'top-level-path'`).Scan(&topDetail); err != nil {
		t.Fatalf("query top-level-path: %v", err)
	}
	var m2 struct {
		Suggestion struct {
			Verb string `json:"verb"`
		} `json:"suggestion"`
	}
	if err := json.Unmarshal([]byte(topDetail), &m2); err != nil {
		t.Fatalf("unmarshal top-level-path detail: %v", err)
	}
	if m2.Suggestion.Verb != "complete" {
		t.Errorf("suggestion.verb (top-level) = %q, want complete", m2.Suggestion.Verb)
	}
}

// TestApply_0048_ExecutionRowsUntouched pins that this data backfill is
// scoped to type='card' only — an execution row's own status/behavior
// vocabulary (which happens to include the unrelated words "start"/"done")
// must never be touched by this migration.
func TestApply_0048_ExecutionRowsUntouched(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	applyThrough(t, d.Conn, "0047_add_tasks_idempotency_key")

	if _, err := d.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('p1', '/tmp/p1')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := d.Conn.Exec(
		`INSERT INTO tasks (id, type, project_id, title, status, behavior, traits, readonly, branch_prefix, base_branch, payload, instructions, auto_start)
		 VALUES ('exec-1', 'execution', 'p1', 't', 'executing', 'dev', '[]', 0, '', '', '{}', '[]', 0)`,
	); err != nil {
		t.Fatalf("insert execution task: %v", err)
	}

	apply0048(t, d.Conn)

	var status string
	if err := d.Conn.QueryRow(`SELECT status FROM tasks WHERE id = 'exec-1'`).Scan(&status); err != nil {
		t.Fatalf("query exec-1: %v", err)
	}
	if status != "executing" {
		t.Errorf("execution row's own status = %q, want untouched (executing)", status)
	}
}
