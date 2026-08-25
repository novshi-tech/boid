package components

import (
	"bytes"
	"context"
	"regexp"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// newTestDB opens an in-memory, migrated sqlite DB. This mirrors
// testutil.NewTestDB, but is duplicated here rather than importing
// testutil: testutil (as a whole package) pulls in internal/server, which
// imports internal/api, which imports this very package (web/templates/
// components) — importing testutil from here would be an import cycle.
func newTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d
}

// TestStatusTabActive_EmptyStatusTreatedAsOpen pins the defensive
// normalization statusTabActive's doc comment claims: an empty
// filter.Status (should not happen once web.go's TaskList normalizes it,
// but this function must not assume that) is treated as "open".
func TestStatusTabActive_EmptyStatusTreatedAsOpen(t *testing.T) {
	if !statusTabActive("", "open") {
		t.Error(`statusTabActive("", "open") = false, want true`)
	}
	if statusTabActive("", "parked") {
		t.Error(`statusTabActive("", "parked") = true, want false`)
	}
}

// TestStatusTabActive_UnknownStatusMatchesNothing is the BD-8 bug this
// function replaced: any status that isn't "closed" or "queue_next" used
// to fall into an else-branch that always marked Open active. An unknown
// or not-yet-added status value (e.g. "parked" before this tab existed)
// must not match any tab.
func TestStatusTabActive_UnknownStatusMatchesNothing(t *testing.T) {
	for _, tab := range []string{"open", "closed", "queue_next", "parked"} {
		if statusTabActive("some-future-status", tab) {
			t.Errorf(`statusTabActive("some-future-status", %q) = true, want false`, tab)
		}
	}
}

func TestStatusTabActive_ExactMatch(t *testing.T) {
	for _, tab := range []string{"open", "closed", "queue_next", "parked"} {
		if !statusTabActive(tab, tab) {
			t.Errorf("statusTabActive(%q, %q) = false, want true", tab, tab)
		}
	}
}

// parkedTabHxValsStatusRE extracts the status value the Parked tab button
// actually sends via hx-vals, from templ's rendered (HTML-escaped) output —
// e.g. `hx-vals="{&#34;status&#34;:&#34;parked&#34;}" ... >Parked</button>`.
var parkedTabHxValsStatusRE = regexp.MustCompile(`hx-vals="\{&#34;status&#34;:&#34;([a-z_]+)&#34;\}"[^>]*>Parked</button>`)

// extractParkedTabStatus renders the real TaskFilters template and reads the
// literal status string the Parked tab button sends. It must come from the
// rendered output, not be re-typed here — otherwise a test comparing it to
// another hand-typed "parked" string would protect nothing (both ends could
// drift together, or either end alone, without the test noticing).
func extractParkedTabStatus(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	if err := TaskFilters(orchestrator.TaskFilter{Status: "open"}, nil, nil).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render TaskFilters: %v", err)
	}
	m := parkedTabHxValsStatusRE.FindStringSubmatch(buf.String())
	if m == nil {
		t.Fatalf("could not find Parked tab's hx-vals status in rendered HTML: %s", buf.String())
	}
	return m[1]
}

// TestParkedTab_StatusString_MatchesStorePredicateAndFiltersCorrectly is the
// wiring-seams #28 regression (BD-8 post-merge review): filters.templ's
// `statusTab(filter, "parked", "Parked")` call and store.go's ListTasks
// status predicate are two ends of the same seam, connected only by a
// string literal — there is no shared constant, and store.go has no
// "parked"-specific branch (it falls through to the generic
// `else if filter.Status != "" { ... t.status = ? }` case, see store.go).
// TestStatusTabActive_* above only pins the *active-tab-highlighting* half
// (statusTabActive); it never touches the real store. This test connects
// both real ends: it extracts the literal the UI actually sends (not a
// hardcoded copy), asserts that literal matches the orchestrator.
// TaskStatusParked constant, and then feeds that exact literal into the
// real store.ListTasks predicate against a real DB to confirm it returns
// parked tasks only. If either end's literal drifts (e.g. store.go grows a
// dedicated "parked" branch with a typo, or filters.templ's literal
// changes), this test breaks.
func TestParkedTab_StatusString_MatchesStorePredicateAndFiltersCorrectly(t *testing.T) {
	sentStatus := extractParkedTabStatus(t)
	if sentStatus != string(orchestrator.TaskStatusParked) {
		t.Fatalf("Parked tab sends status %q, want %q (orchestrator.TaskStatusParked)", sentStatus, orchestrator.TaskStatusParked)
	}

	d := newTestDB(t)
	proj := &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}
	if err := orchestrator.CreateProject(d.Conn, proj); err != nil {
		t.Fatalf("create project: %v", err)
	}

	// card-model-cleanup PR-2 (design doc §3.3): captured/triaged/ready no
	// longer exist as statuses at all — migration 0045's CHECK constraint
	// rejects them at the DB level, so the exact superset scenario the
	// original Opus review flagged (2026-08-18 — a future dedicated "parked"
	// branch written to match queue's old pre-execution status set instead
	// of an exact match) is now structurally impossible, not merely
	// untested. This still exercises the same exact-match guard across every
	// status that CAN exist today — both Card statuses (parked/working/done/
	// dropped) and Execution statuses (pending/executing/awaiting/done/
	// aborted) — so a future superset bug (e.g. a "parked" branch that also
	// matches "working") would still be caught.
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusWorking,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusDropped,
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
		orchestrator.TaskStatusAborted,
	}
	ids := map[orchestrator.TaskStatus]string{}
	for _, s := range statuses {
		task := &orchestrator.Task{ProjectID: "proj-1", Title: "task-" + string(s), Status: s}
		switch s {
		case orchestrator.TaskStatusParked, orchestrator.TaskStatusWorking, orchestrator.TaskStatusDropped:
			// Card-only statuses (migration 0045's CHECK constraint).
			task.Type = orchestrator.TaskTypeCard
			task.Card = &orchestrator.CardAttrs{}
		default:
			// "done" is shared by both types (design doc §3.3); either type
			// exercises this test's exact-match assertion equally, so pick
			// Execution here alongside the genuinely Execution-only statuses.
			task.Type = orchestrator.TaskTypeExecution
			task.Exec = &orchestrator.ExecAttrs{Behavior: "dev"}
		}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("create %s: %v", s, err)
		}
		ids[s] = task.ID
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: sentStatus})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 1 || got[0].ID != ids[orchestrator.TaskStatusParked] {
		gotIDs := make([]string, len(got))
		for i, tk := range got {
			gotIDs[i] = tk.ID + ":" + string(tk.Status)
		}
		t.Fatalf("ListTasks(Status=%q) = %v, want exactly [%s] (the parked task)", sentStatus, gotIDs, ids[orchestrator.TaskStatusParked])
	}
}
