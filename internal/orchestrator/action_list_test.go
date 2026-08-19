package orchestrator_test

// docs/plans/ingestion-identity.md PR-3 (B-3): ListActionsSince's own
// contract, at the store layer (broker/executor-level scoping is covered
// separately by internal/sandbox and internal/server tests — see
// broker_test.go's TestBroker_BoidActionList_* and
// boid_executor_action_list_test.go). Pins:
//
//   - since カーソルが単調に進み、同じ action を 2 度返さない (検証節)
//   - scoping: project 無指定で無スコープに引かない (ErrActionListUnscoped)
//   - ProjectIDs / WorkspaceID both correctly isolate one project's actions
//     from another's — the thing a leaked scope would get wrong
//   - a malformed cursor is a hard error, not silently "from the beginning"

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestListActionsSince_Unscoped_Rejected(t *testing.T) {
	d := testutil.NewTestDB(t)
	_, _, err := orchestrator.ListActionsSince(d.Conn, orchestrator.ActionListFilter{})
	if !errors.Is(err, orchestrator.ErrActionListUnscoped) {
		t.Fatalf("err = %v, want ErrActionListUnscoped", err)
	}
}

// TestListActionsSince_CursorMonotonic_NoDuplicates pins the doc's own
// verification item: paginating with a small Limit and following
// NextCursor visits every action exactly once, in created_at order.
func TestListActionsSince_CursorMonotonic_NoDuplicates(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "T", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	const n = 5
	var created []*orchestrator.Action
	for i := 0; i < n; i++ {
		a := &orchestrator.Action{TaskID: task.ID, Type: "noted", Payload: json.RawMessage(`{"i":` + strconv.Itoa(i) + `}`)}
		if err := orchestrator.CreateAction(d.Conn, a); err != nil {
			t.Fatalf("create action %d: %v", i, err)
		}
		created = append(created, a)
	}

	seen := map[string]bool{}
	var order []string
	cursor := ""
	for page := 0; page < n+2; page++ { // generous upper bound against an infinite-loop bug
		actions, next, err := orchestrator.ListActionsSince(d.Conn, orchestrator.ActionListFilter{
			ProjectIDs: []string{"proj-1"},
			Since:      cursor,
			Limit:      2,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(actions) == 0 {
			if next != cursor {
				t.Fatalf("page %d: empty page changed the cursor: %q -> %q", page, cursor, next)
			}
			break
		}
		for _, a := range actions {
			if seen[a.ID] {
				t.Fatalf("action %s returned twice", a.ID)
			}
			seen[a.ID] = true
			order = append(order, a.ID)
		}
		if next == cursor {
			t.Fatalf("page %d: cursor did not advance despite %d rows returned", page, len(actions))
		}
		cursor = next
	}

	if len(order) != n {
		t.Fatalf("total actions returned across pages = %d, want %d", len(order), n)
	}
	for i, a := range created {
		if order[i] != a.ID {
			t.Errorf("position %d: got action %s, want %s (created_at order)", i, order[i], a.ID)
		}
	}
}

// TestListActionsSince_ProjectScoping_IsolatesOtherProject pins that a
// caller scoped to proj-1 never sees proj-2's actions, even though both
// exist in the same actions table.
func TestListActionsSince_ProjectScoping_IsolatesOtherProject(t *testing.T) {
	d := testutil.NewTestDB(t)
	for _, id := range []string{"proj-1", "proj-2"} {
		if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: id, WorkDir: "/tmp/" + id}); err != nil {
			t.Fatalf("create project %s: %v", id, err)
		}
	}
	task1 := &orchestrator.Task{ProjectID: "proj-1", Title: "T1", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task1); err != nil {
		t.Fatalf("create task1: %v", err)
	}
	task2 := &orchestrator.Task{ProjectID: "proj-2", Title: "T2", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task2); err != nil {
		t.Fatalf("create task2: %v", err)
	}
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{TaskID: task1.ID, Type: "noted"}); err != nil {
		t.Fatalf("create action1: %v", err)
	}
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{TaskID: task2.ID, Type: "noted"}); err != nil {
		t.Fatalf("create action2: %v", err)
	}

	actions, _, err := orchestrator.ListActionsSince(d.Conn, orchestrator.ActionListFilter{ProjectIDs: []string{"proj-1"}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(actions) != 1 || actions[0].TaskID != task1.ID {
		t.Fatalf("actions = %+v, want exactly task1's one action", actions)
	}
}

// TestListActionsSince_WorkspaceScoping_IsolatesOtherWorkspace pins the
// WorkspaceID join branch: two projects with the same identity NAMESPACE
// but different workspace assignment must not bleed into each other.
func TestListActionsSince_WorkspaceScoping_IsolatesOtherWorkspace(t *testing.T) {
	d := testutil.NewTestDB(t)
	for _, id := range []string{"proj-a", "proj-b"} {
		if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: id, WorkDir: "/tmp/" + id}); err != nil {
			t.Fatalf("create project %s: %v", id, err)
		}
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-a", "ws-1"); err != nil {
		t.Fatalf("assign proj-a: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-b", "ws-2"); err != nil {
		t.Fatalf("assign proj-b: %v", err)
	}
	taskA := &orchestrator.Task{ProjectID: "proj-a", Title: "TA", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, taskA); err != nil {
		t.Fatalf("create taskA: %v", err)
	}
	taskB := &orchestrator.Task{ProjectID: "proj-b", Title: "TB", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, taskB); err != nil {
		t.Fatalf("create taskB: %v", err)
	}
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{TaskID: taskA.ID, Type: "noted"}); err != nil {
		t.Fatalf("create actionA: %v", err)
	}
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{TaskID: taskB.ID, Type: "noted"}); err != nil {
		t.Fatalf("create actionB: %v", err)
	}

	actions, _, err := orchestrator.ListActionsSince(d.Conn, orchestrator.ActionListFilter{WorkspaceID: "ws-1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(actions) != 1 || actions[0].TaskID != taskA.ID {
		t.Fatalf("actions = %+v, want exactly ws-1's one action (taskA)", actions)
	}
}

// TestListActionsSince_TaskIDNarrowsWithinScope pins that TaskID combines
// with ProjectIDs as an AND, never widening the scope.
func TestListActionsSince_TaskIDNarrowsWithinScope(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	task1 := &orchestrator.Task{ProjectID: "proj-1", Title: "T1", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task1); err != nil {
		t.Fatalf("create task1: %v", err)
	}
	task2 := &orchestrator.Task{ProjectID: "proj-1", Title: "T2", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task2); err != nil {
		t.Fatalf("create task2: %v", err)
	}
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{TaskID: task1.ID, Type: "noted"}); err != nil {
		t.Fatalf("create action1: %v", err)
	}
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{TaskID: task2.ID, Type: "noted"}); err != nil {
		t.Fatalf("create action2: %v", err)
	}

	actions, _, err := orchestrator.ListActionsSince(d.Conn, orchestrator.ActionListFilter{
		ProjectIDs: []string{"proj-1"},
		TaskID:     task1.ID,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(actions) != 1 || actions[0].TaskID != task1.ID {
		t.Fatalf("actions = %+v, want exactly task1's one action", actions)
	}
}

func TestDecodeActionCursor_Malformed_IsError(t *testing.T) {
	for _, bad := range []string{"garbage", "2026-08-19T00:00:00Z|", "|abc", "not-a-time|abc"} {
		if _, _, err := orchestrator.DecodeActionCursor(bad); err == nil {
			t.Errorf("DecodeActionCursor(%q) = nil error, want an error", bad)
		}
	}
}

func TestListActionsSince_MalformedCursor_Rejected(t *testing.T) {
	d := testutil.NewTestDB(t)
	if _, _, err := orchestrator.ListActionsSince(d.Conn, orchestrator.ActionListFilter{
		ProjectIDs: []string{"proj-1"},
		Since:      "not-a-valid-cursor",
	}); err == nil {
		t.Fatal("expected an error for a malformed cursor")
	}
}

func TestClampActionListLimit(t *testing.T) {
	cases := []struct {
		requested int
		want      int
	}{
		{0, orchestrator.DefaultActionListLimit},
		{-5, orchestrator.DefaultActionListLimit},
		{50, 50},
		{orchestrator.MaxActionListLimit + 1000, orchestrator.MaxActionListLimit},
	}
	for _, c := range cases {
		if got := orchestrator.ClampActionListLimit(c.requested); got != c.want {
			t.Errorf("ClampActionListLimit(%d) = %d, want %d", c.requested, got, c.want)
		}
	}
}

// insertActionAtRaw inserts one actions row with an EXPLICIT created_at,
// bypassing orchestrator.CreateAction (which always overwrites CreatedAt
// with a fresh time.Now().UTC() and so can never produce a tie). This is
// the same raw-INSERT shape internal/dispatcher/store.go's
// markStaleTasksAborted uses in production — the real source of
// same-instant created_at ties (see EncodeActionCursor's doc comment).
func insertActionAtRaw(t *testing.T, conn db.DBTX, id, taskID string, at time.Time) {
	t.Helper()
	if _, err := conn.Exec(
		`INSERT INTO actions (id, task_id, type, payload, from_status, to_status, created_at, actor)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, taskID, "noted", "{}", "", "", at, "daemon",
	); err != nil {
		t.Fatalf("insert action %s at %s: %v", id, at, err)
	}
}

// TestListActionsSince_TiedCreatedAt_PageBoundary_NoDuplicatesNoGaps pins
// Opus review finding #2 (2026-08-19 revisit of PR-3): the "actions.id is
// part of the cursor because two actions can share created_at" claim in
// EncodeActionCursor's doc comment cites
// dispatcher.markStaleTasksAborted's ONE now := time.Now().UTC() shared
// across N inserted rows (daemon-restart bulk abort) — NOT
// TaskWorkflowService.persistFiredEvents, whose per-row CreateAction calls
// each take a fresh timestamp and never actually tie. This test reproduces
// markStaleTasksAborted's exact shape (one shared timestamp, insertion
// order scrambled vs id order — mirroring "SELECT id FROM tasks WHERE
// status = ?" not being id-ordered) directly against the actions table, and
// walks Limit=2 pages across the resulting 6-way tie plus one later,
// untied action. Expected page shape (also the design doc's/review's own
// worked example):
//
//	page=0 cursor=""                     -> [id-a id-b]
//	page=1 cursor=(tiedAt|id-b)          -> [id-c id-d]
//	page=2 cursor=(tiedAt|id-d)          -> [id-e id-f]
//	page=3 cursor=(tiedAt|id-f)          -> [id-z-later]
//	page=4 cursor=(laterAt|id-z-later)   -> []  (cursor held steady)
func TestListActionsSince_TiedCreatedAt_PageBoundary_NoDuplicatesNoGaps(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "T", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	tiedAt := time.Date(2026, 8, 19, 3, 4, 5, 123456789, time.UTC)
	// Insertion order deliberately scrambled vs id order.
	for _, id := range []string{"id-f", "id-b", "id-d", "id-a", "id-e", "id-c"} {
		insertActionAtRaw(t, d.Conn, id, task.ID, tiedAt)
	}
	laterAt := tiedAt.Add(time.Hour)
	insertActionAtRaw(t, d.Conn, "id-z-later", task.ID, laterAt)

	wantPages := [][]string{
		{"id-a", "id-b"},
		{"id-c", "id-d"},
		{"id-e", "id-f"},
		{"id-z-later"},
		{},
	}

	cursor := ""
	seen := map[string]bool{}
	for i, want := range wantPages {
		actions, next, err := orchestrator.ListActionsSince(d.Conn, orchestrator.ActionListFilter{
			ProjectIDs: []string{"proj-1"},
			Since:      cursor,
			Limit:      2,
		})
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		var gotIDs []string
		for _, a := range actions {
			if seen[a.ID] {
				t.Fatalf("page %d: action %s returned twice", i, a.ID)
			}
			seen[a.ID] = true
			gotIDs = append(gotIDs, a.ID)
		}
		var wantNonNil []string
		if len(want) > 0 {
			wantNonNil = want
		}
		if !reflect.DeepEqual(gotIDs, wantNonNil) {
			t.Fatalf("page %d: ids = %v, want %v", i, gotIDs, want)
		}
		if len(want) == 0 {
			if next != cursor {
				t.Fatalf("page %d: empty page changed cursor: %q -> %q", i, cursor, next)
			}
		} else if next == cursor {
			t.Fatalf("page %d: cursor did not advance despite %d rows returned", i, len(actions))
		}
		t.Logf("page=%d cursor=%q -> %v (next=%q)", i, cursor, gotIDs, next)
		cursor = next
	}

	if len(seen) != 7 {
		t.Fatalf("total distinct actions seen across all pages = %d, want 7", len(seen))
	}
}
