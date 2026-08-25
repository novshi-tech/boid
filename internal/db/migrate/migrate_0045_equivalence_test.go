package migrate

import (
	"sort"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestApply_0045_OpenListEquivalence and its siblings below pin design doc
// §8's "退役前の等価性検証の規律": migration 0045 must not change which
// tasks the "open"/"queue_next" views surface, or how a parent/child tree
// resolves, for the status vocabulary that is actually live post-cutover
// (parked/working/done/dropped for cards; pending/executing/awaiting/done/
// aborted for execution tasks). The three now-removed legacy statuses
// (captured/triaged/ready) are deliberately OUT of scope here: washing them
// over to "parked" (§3.3) is a documented, INTENTIONAL behavior change for
// any pre-cutover row that still carried one (a captured task, specifically,
// used to be carved OUT of the "open" exclusion — docs/plans/
// ingestion-identity.md PR-2, B-2 — and parked is not; see this file's own
// comment further down for the full reasoning) — real data is expected to
// hold zero such rows by the time this migration runs (card machine v2 has
// produced none since it shipped), so proving equivalence FOR them would be
// asserting a change is a non-change, which is not what "equivalence" means
// here. This test's coverage is: build a fixture DB pre-0045 (task_triage
// table, flat tasks columns), compute the "open"/"queue_next" ID sets the
// OLD predicate (as store.go implemented it immediately before this PR —
// see git history on store.go for the literal SQL these mirror) would have
// returned, run migration 0045, then compute the SAME two ID sets via a
// FIXED, frozen reimplementation of the post-0045/pre-PR-4 predicate
// (openTaskIDsAfterMigrationFixedPredicate / queueNextTaskIDsAfterMigrationFixedPredicate,
// below) and assert they match.
//
// docs/plans/webui-detail-list-redesign.md PR-4 (§3.6) deliberately changed
// BOTH the "open" predicate (removed the multi-level open_descendants/
// open_ancestors CTEs) and "queue_next" (removed entirely) — a real,
// intentional, documented product change, not a regression. Before PR-4,
// this test called orchestrator.ListTasks directly for its "after" leg,
// which coupled a historical migration-equivalence check to whatever
// ListTasks does TODAY; PR-4 decoupled it by freezing the "after" SQL here
// instead, so this file stays a pure point-in-time record of migration
// 0045's own correctness, unaffected by later, unrelated predicate changes.
//
// The "before" SQL below is a deliberately narrow reimplementation: only
// enough of store.go's actual notOpenSelfStatusSQLList / open_descendants /
// open_ancestors CTE shape to exercise the parent/child rescue paths this
// fixture needs — not a byte-for-byte copy of the pre-PR-2 query (that
// would just be duplicating store.go, not independently checking it). The
// CTE STRUCTURE itself did not change in this PR (only which status values
// feed the exclusion list did), so a structurally faithful reimplementation
// against the OLD status vocabulary is a meaningful independent check.
func openTaskIDsBeforeMigration(t *testing.T, conn *db.DB) []string {
	t.Helper()
	// Pre-0045 vocabulary: notOpenSelfStatusSQLList = terminal(done,aborted,
	// dropped) ∪ (pre-execution(captured,triaged,parked,ready) minus
	// captured) = done,aborted,dropped,triaged,parked,ready. This fixture
	// uses only parked (of that pre-execution set) since captured/triaged/
	// ready are out of scope (see this file's package doc comment above).
	// notOpenAncestorGateStatusSQLList = terminal ∪ {parked} (unchanged by
	// this PR either side of the migration).
	const notOpenSelf = `('done','aborted','dropped','parked')`
	const notOpenAncestorGate = `('done','aborted','dropped','parked')`
	const terminal = `('done','aborted','dropped')`
	query := `
		WITH RECURSIVE open_descendants(id) AS (
			SELECT c.id FROM tasks c JOIN tasks p ON c.parent_id = p.id
			WHERE p.status NOT IN ` + notOpenAncestorGate + `
			UNION
			SELECT c.id FROM tasks c JOIN open_descendants od ON c.parent_id = od.id
		), open_ancestors(id) AS (
			SELECT p.id FROM tasks c JOIN tasks p ON c.parent_id = p.id
			WHERE c.status NOT IN ` + terminal + `
			UNION
			SELECT p.id FROM tasks c JOIN tasks p ON c.parent_id = p.id
			JOIN open_ancestors oa ON oa.id = c.id
		)
		SELECT t.id FROM tasks t
		WHERE (t.status NOT IN ` + notOpenSelf + ` OR
			(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status NOT IN ` + terminal + `) > 0 OR
			t.id IN (SELECT id FROM open_descendants) OR
			t.id IN (SELECT id FROM open_ancestors))`
	rows, err := conn.Conn.Query(query)
	if err != nil {
		t.Fatalf("open (before) query: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("open (before) scan: %v", err)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func queueNextTaskIDsBeforeMigration(t *testing.T, conn *db.DB) []string {
	t.Helper()
	rows, err := conn.Conn.Query(`
		SELECT t.id FROM tasks t
		INNER JOIN task_triage tt ON tt.task_id = t.id
		WHERE tt.suggestion_verb != ''`)
	if err != nil {
		t.Fatalf("queue_next (before) query: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("queue_next (before) scan: %v", err)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// openTaskIDsAfterMigrationFixedPredicate and queueNextTaskIDsAfterMigrationFixedPredicate
// mirror store.go's ListTasks("open") / ListTasks("queue_next") predicates
// AS THEY STOOD immediately after migration 0045 and before docs/plans/
// webui-detail-list-redesign.md PR-4 (§3.6) simplified/removed them.
//
// This test's job is proving migration 0045 (task_triage → STI folding)
// didn't change "open"/"queue_next" behavior — it is NOT a live regression
// test for whatever orchestrator.ListTasks does today (PR-4's own
// store_test.go pins carry that job, e.g. TestListTasks_Open_
// NoLongerRescuesGreatGrandchild / TestListTasks_QueueNext_ReturnsEmpty).
// PR-4 deliberately changed both predicates (CTE removal, queue_next
// removal) — calling the live orchestrator.ListTasks here would make this
// migration-equivalence test fail on every later, unrelated predicate
// change, which is not what "did migration 0045 preserve behavior" means.
// So the "after" leg is frozen to a literal copy of the post-0045/pre-PR-4
// SQL instead of the live function — this file now stands on its own as a
// historical snapshot, decoupled from ListTasks's ongoing evolution.
func openTaskIDsAfterMigrationFixedPredicate(t *testing.T, conn *db.DB) []string {
	t.Helper()
	const notOpenSelf = `('done','aborted','dropped','parked')`
	const notOpenAncestorGate = `('done','aborted','dropped','parked')`
	const terminal = `('done','aborted','dropped')`
	query := `
		WITH RECURSIVE open_descendants(id) AS (
			SELECT c.id FROM tasks c JOIN tasks p ON c.parent_id = p.id
			WHERE p.status NOT IN ` + notOpenAncestorGate + `
			UNION
			SELECT c.id FROM tasks c JOIN open_descendants od ON c.parent_id = od.id
		), open_ancestors(id) AS (
			SELECT p.id FROM tasks c JOIN tasks p ON c.parent_id = p.id
			WHERE c.status NOT IN ` + terminal + `
			UNION
			SELECT p.id FROM tasks c JOIN tasks p ON c.parent_id = p.id
			JOIN open_ancestors oa ON oa.id = c.id
		)
		SELECT t.id FROM tasks t
		WHERE (t.status NOT IN ` + notOpenSelf + ` OR
			(SELECT COUNT(*) FROM tasks c WHERE c.parent_id = t.id AND c.status NOT IN ` + terminal + `) > 0 OR
			t.id IN (SELECT id FROM open_descendants) OR
			t.id IN (SELECT id FROM open_ancestors))`
	rows, err := conn.Conn.Query(query)
	if err != nil {
		t.Fatalf("open (after, fixed predicate) query: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("open (after, fixed predicate) scan: %v", err)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func queueNextTaskIDsAfterMigrationFixedPredicate(t *testing.T, conn *db.DB) []string {
	t.Helper()
	rows, err := conn.Conn.Query(`SELECT t.id FROM tasks t WHERE t.suggestion_verb != ''`)
	if err != nil {
		t.Fatalf("queue_next (after, fixed predicate) query: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("queue_next (after, fixed predicate) scan: %v", err)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedTaskIDs(tasks []*orchestrator.Task) []string {
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	sort.Strings(ids)
	return ids
}

// buildEquivalenceFixture inserts the mixed card/execution/parent-child
// dataset every equivalence test in this file shares, and returns the open
// DB handle (still pre-0045).
func buildEquivalenceFixture(t *testing.T) *db.DB {
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
	insertTask := func(id, status, behavior, parentID string) {
		t.Helper()
		if _, err := d.Conn.Exec(
			`INSERT INTO tasks (id, project_id, title, status, behavior, parent_id) VALUES (?, 'p1', ?, ?, ?, ?)`,
			id, id, status, behavior, parentID,
		); err != nil {
			t.Fatalf("insert task %s: %v", id, err)
		}
	}

	// Plain execution tasks (no children).
	insertTask("exec-pending", "pending", "executor", "")
	insertTask("exec-executing", "executing", "executor", "")
	insertTask("exec-awaiting", "awaiting", "executor", "")
	insertTask("exec-done", "done", "executor", "")
	insertTask("exec-aborted", "aborted", "executor", "")

	// Plain cards (no children).
	insertTask("card-parked", "parked", "", "")
	insertTask("card-working", "working", "", "")
	insertTask("card-done", "done", "", "")
	insertTask("card-dropped", "dropped", "", "")

	// Rescue case 1 (child rescue, self clause): a done execution parent
	// with a still-executing child must stay in Open via the direct
	// child-count clause.
	insertTask("parent-done-with-live-child", "done", "executor", "")
	insertTask("child-of-done-parent", "executing", "executor", "parent-done-with-live-child")

	// Rescue case 2 (descendant rescue CTE): a parked card grandparent with
	// a done middle child and an executing grandchild — the grandchild must
	// pull both ancestors into Open via open_ancestors, and the grandchild
	// itself must appear via open_descendants seeded from the middle child.
	insertTask("grandparent-parked", "parked", "", "")
	insertTask("middle-done", "done", "executor", "grandparent-parked")
	insertTask("grandchild-executing", "executing", "executor", "middle-done")

	// queue_next fixture: a parked card with a sidecar carrying a
	// suggestion_verb, and one without (must NOT appear in queue_next).
	insertTask("queue-candidate", "parked", "", "")
	if _, err := d.Conn.Exec(
		`INSERT INTO task_triage (task_id, suggestion_verb) VALUES ('queue-candidate', 'go')`,
	); err != nil {
		t.Fatalf("insert sidecar: %v", err)
	}
	insertTask("queue-non-candidate", "working", "", "")
	if _, err := d.Conn.Exec(
		`INSERT INTO task_triage (task_id, suggestion_verb) VALUES ('queue-non-candidate', '')`,
	); err != nil {
		t.Fatalf("insert sidecar: %v", err)
	}

	return d
}

func TestApply_0045_OpenListEquivalence(t *testing.T) {
	d := buildEquivalenceFixture(t)

	before := openTaskIDsBeforeMigration(t, d)

	if err := applyMigration(d.Conn, mustFindMigration(t, "0045_card_sti_migration")); err != nil {
		t.Fatalf("apply 0045: %v", err)
	}

	afterIDs := openTaskIDsAfterMigrationFixedPredicate(t, d)

	if len(before) == 0 {
		t.Fatal("fixture bug: expected a non-empty 'before' open set")
	}
	if !equalStringSlices(before, afterIDs) {
		t.Errorf("open list ID set changed across migration 0045:\n before: %v\n after:  %v", before, afterIDs)
	}
}

func TestApply_0045_QueueNextEquivalence(t *testing.T) {
	d := buildEquivalenceFixture(t)

	before := queueNextTaskIDsBeforeMigration(t, d)

	if err := applyMigration(d.Conn, mustFindMigration(t, "0045_card_sti_migration")); err != nil {
		t.Fatalf("apply 0045: %v", err)
	}

	afterIDs := queueNextTaskIDsAfterMigrationFixedPredicate(t, d)

	want := []string{"queue-candidate"}
	if !equalStringSlices(before, want) {
		t.Fatalf("fixture bug: 'before' queue_next set = %v, want %v", before, want)
	}
	if !equalStringSlices(before, afterIDs) {
		t.Errorf("queue_next ID set changed across migration 0045:\n before: %v\n after:  %v", before, afterIDs)
	}
}

// TestApply_0045_TaskTreeEquivalence pins the third leg of §8's "task tree"
// equivalence claim: parent_id relationships (and therefore ListChildren /
// child-count aggregation) survive the migration untouched — the migration
// never rewrites id/parent_id, only type/status/column-placement, so this is
// mostly a smoke test that the rebuild didn't scramble foreign keys.
func TestApply_0045_TaskTreeEquivalence(t *testing.T) {
	d := buildEquivalenceFixture(t)

	type edge struct{ parent, child string }
	wantEdges := []edge{
		{"parent-done-with-live-child", "child-of-done-parent"},
		{"grandparent-parked", "middle-done"},
		{"middle-done", "grandchild-executing"},
	}

	if err := applyMigration(d.Conn, mustFindMigration(t, "0045_card_sti_migration")); err != nil {
		t.Fatalf("apply 0045: %v", err)
	}

	for _, e := range wantEdges {
		children, err := orchestrator.ListChildren(d.Conn, e.parent)
		if err != nil {
			t.Fatalf("list children of %s: %v", e.parent, err)
		}
		found := false
		for _, c := range children {
			if c.ID == e.child {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("parent %q: children = %v, want to include %q", e.parent, sortedTaskIDs(children), e.child)
		}
	}

	// Child-count aggregation on the done parent must still see its one
	// live (non-terminal) child.
	parent, err := orchestrator.GetTask(d.Conn, "parent-done-with-live-child")
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if parent.OpenChildCount != 1 {
		t.Errorf("parent-done-with-live-child.OpenChildCount = %d, want 1", parent.OpenChildCount)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
