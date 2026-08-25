package orchestrator_test

import (
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestListTasks_OrderBy_UpdatedAtDescIDTiebreak pins docs/plans/
// webui-detail-list-redesign.md PR-4's ORDER BY unification (§3.5): every
// view (default no-status, "closed", "cards_live", an exact status literal)
// sorts by updated_at DESC with id as a deterministic tie-break — not
// created_at, and not the bespoke per-branch orderings PR-4 removed. A task
// whose updated_at was bumped after creation (PR-3's TouchTaskUpdatedAt/
// UpdateTask) must surface above an older-but-more-recently-created task,
// proving updated_at (not created_at) actually drives the sort.
func TestListTasks_OrderBy_UpdatedAtDescIDTiebreak(t *testing.T) {
	d := createTestProject(t)

	older := &orchestrator.Task{ProjectID: "proj-1", Title: "created first", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, older); err != nil {
		t.Fatalf("create older: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	newer := &orchestrator.Task{ProjectID: "proj-1", Title: "created second", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, newer); err != nil {
		t.Fatalf("create newer: %v", err)
	}
	time.Sleep(2 * time.Millisecond)

	// Touch the OLDER task's updated_at so it becomes the most-recently-
	// updated row despite being created first — this is what a suggestion
	// attaching (or any status transition) does per PR-3.
	if err := orchestrator.TouchTaskUpdatedAt(d.Conn, older.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d tasks, want 2", len(got))
	}
	if got[0].ID != older.ID {
		t.Errorf("first row = %q (%s), want %q (touched most recently, so updated_at DESC puts it first, not created_at DESC)", got[0].Title, got[0].ID, older.ID)
	}
	if got[1].ID != newer.ID {
		t.Errorf("second row = %q (%s), want %q", got[1].Title, got[1].ID, newer.ID)
	}
}

// TestListTasks_OrderBy_IDTiebreakOnEqualUpdatedAt pins the deterministic
// tie-break half: two rows with the identical updated_at value (same
// millisecond) must still come back in a stable, repeatable order (id ASC),
// not whatever order SQLite happens to return ties in.
func TestListTasks_OrderBy_IDTiebreakOnEqualUpdatedAt(t *testing.T) {
	d := createTestProject(t)

	a := &orchestrator.Task{ID: "aaaaaaaa-0000-0000-0000-000000000001", ProjectID: "proj-1", Title: "a", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	b := &orchestrator.Task{ID: "bbbbbbbb-0000-0000-0000-000000000002", ProjectID: "proj-1", Title: "b", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, b); err != nil { // create b first
		t.Fatalf("create b: %v", err)
	}
	if err := orchestrator.CreateTask(d.Conn, a); err != nil { // then a
		t.Fatalf("create a: %v", err)
	}
	// Force identical updated_at on both rows so the tie-break is the only
	// thing deciding order.
	same := time.Now().UTC()
	if _, err := d.Conn.Exec(`UPDATE tasks SET updated_at = ? WHERE id IN (?, ?)`, same, a.ID, b.ID); err != nil {
		t.Fatalf("force equal updated_at: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID {
		gotIDs := make([]string, len(got))
		for i, tk := range got {
			gotIDs[i] = tk.ID
		}
		t.Fatalf("got %v, want [%s, %s] (id ASC tie-break)", gotIDs, a.ID, b.ID)
	}
}

// TestListTasks_ActiveOnly_ExcludesTerminalIncludesParked pins the
// "アクティブのみ" toggle (§3.5): TaskFilter.ActiveOnly excludes only
// terminal statuses (done/aborted/dropped) — unlike the legacy "open" Status
// keyword's self-clause, a parked card counts as active (it hasn't reached a
// terminal status), and it composes with an unset Status (no other
// narrowing at all).
func TestListTasks_ActiveOnly_ExcludesTerminalIncludesParked(t *testing.T) {
	d := createTestProject(t)
	fixtures := []struct {
		task     *orchestrator.Task
		included bool
	}{
		{&orchestrator.Task{ProjectID: "proj-1", Title: "parked", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}, true},
		{&orchestrator.Task{ProjectID: "proj-1", Title: "working", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}, true},
		{&orchestrator.Task{ProjectID: "proj-1", Title: "executing", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}, true},
		{&orchestrator.Task{ProjectID: "proj-1", Title: "done", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}, false},
		{&orchestrator.Task{ProjectID: "proj-1", Title: "aborted", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusAborted, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}, false},
		{&orchestrator.Task{ProjectID: "proj-1", Title: "dropped", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusDropped, Card: &orchestrator.CardAttrs{}}, false},
	}
	for _, f := range fixtures {
		if err := orchestrator.CreateTask(d.Conn, f.task); err != nil {
			t.Fatalf("create %s: %v", f.task.Title, err)
		}
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{ActiveOnly: true})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, tk := range got {
		gotIDs[tk.ID] = true
	}
	for _, f := range fixtures {
		if f.included && !gotIDs[f.task.ID] {
			t.Errorf("%s (status=%s) missing from ActiveOnly, want included", f.task.Title, f.task.Status)
		}
		if !f.included && gotIDs[f.task.ID] {
			t.Errorf("%s (status=%s) present in ActiveOnly, want excluded (terminal)", f.task.Title, f.task.Status)
		}
	}
}

// TestListTasks_ActiveOnly_False_IsANoOp pins that ActiveOnly's zero value
// (false) changes nothing — the default "全状態表示" view (§3.5).
func TestListTasks_ActiveOnly_False_IsANoOp(t *testing.T) {
	d := createTestProject(t)
	done := &orchestrator.Task{ProjectID: "proj-1", Title: "done", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, done); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{ActiveOnly: false})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	found := false
	for _, tk := range got {
		if tk.ID == done.ID {
			found = true
		}
	}
	if !found {
		t.Error("a terminal task must still appear when ActiveOnly is false (default full-state view)")
	}
}

// ---- Pagination (§3.5, §5 論点4: LIMIT/OFFSET) ----

// TestListTasks_Pagination_FirstPage pins the basic LIMIT behavior: a
// page-size LIMIT with no OFFSET returns exactly that many rows, the
// most-recently-updated first.
func TestListTasks_Pagination_FirstPage(t *testing.T) {
	d := createTestProject(t)
	var all []*orchestrator.Task
	for i := 0; i < 5; i++ {
		task := &orchestrator.Task{ProjectID: "proj-1", Title: "task", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("create task %d: %v", i, err)
		}
		all = append(all, task)
		time.Sleep(time.Millisecond)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Limit: 2})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d tasks, want 2 (LIMIT 2)", len(got))
	}
	// Most recently created == most recently updated_at here (no touches),
	// so page 1 must be the LAST two created, newest first.
	want := []string{all[4].ID, all[3].ID}
	if got[0].ID != want[0] || got[1].ID != want[1] {
		t.Errorf("got [%s, %s], want %v", got[0].ID, got[1].ID, want)
	}
}

// TestListTasks_Pagination_SecondPage pins OFFSET composing with LIMIT: page
// 2 picks up exactly where page 1 left off, no overlap and no gap.
func TestListTasks_Pagination_SecondPage(t *testing.T) {
	d := createTestProject(t)
	var all []*orchestrator.Task
	for i := 0; i < 5; i++ {
		task := &orchestrator.Task{ProjectID: "proj-1", Title: "task", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("create task %d: %v", i, err)
		}
		all = append(all, task)
		time.Sleep(time.Millisecond)
	}

	page1, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("ListTasks page1: %v", err)
	}
	page2, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("ListTasks page2: %v", err)
	}
	if len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("page1=%d page2=%d, want 2 each", len(page1), len(page2))
	}
	want := []string{all[2].ID, all[1].ID}
	if page2[0].ID != want[0] || page2[1].ID != want[1] {
		t.Errorf("page2 = [%s, %s], want %v", page2[0].ID, page2[1].ID, want)
	}
	// No overlap between page1 and page2.
	for _, p1 := range page1 {
		for _, p2 := range page2 {
			if p1.ID == p2.ID {
				t.Errorf("task %s appears in both page1 and page2", p1.ID)
			}
		}
	}
}

// TestListTasks_Pagination_LastPagePartial pins the final-page boundary: an
// offset that leaves fewer rows than Limit returns just the remainder, not
// an error and not a short-by-one/off-by-one slice.
func TestListTasks_Pagination_LastPagePartial(t *testing.T) {
	d := createTestProject(t)
	var all []*orchestrator.Task
	for i := 0; i < 5; i++ {
		task := &orchestrator.Task{ProjectID: "proj-1", Title: "task", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("create task %d: %v", i, err)
		}
		all = append(all, task)
		time.Sleep(time.Millisecond)
	}

	// 5 rows total, page size 2, offset 4 → only 1 row left (the oldest).
	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Limit: 2, Offset: 4})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d tasks, want 1 (last partial page)", len(got))
	}
	if got[0].ID != all[0].ID {
		t.Errorf("got %s, want %s (the oldest/first-created row)", got[0].ID, all[0].ID)
	}
}

// TestListTasks_Pagination_OffsetBeyondDataset_ReturnsEmpty pins the
// past-the-end boundary: an offset larger than the total row count returns
// an empty (not nil-panicking, not erroring) result.
func TestListTasks_Pagination_OffsetBeyondDataset_ReturnsEmpty(t *testing.T) {
	d := createTestProject(t)
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "only one", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Limit: 50, Offset: 100})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d tasks, want 0 (offset beyond dataset)", len(got))
	}
}

// TestListTasks_Pagination_EmptyDataset pins the zero-row boundary: Limit/
// Offset against an empty table returns an empty (non-nil-panicking) slice.
func TestListTasks_Pagination_EmptyDataset(t *testing.T) {
	d := createTestProject(t)
	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Limit: 50, Offset: 0})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d tasks, want 0 (empty dataset)", len(got))
	}
}

// TestListTasks_Pagination_OffsetWithoutLimit pins the "Offset>0, Limit<=0"
// edge case (store.go's own doc comment): it must not be silently dropped —
// SQLite's `LIMIT -1 OFFSET ?` still applies the offset with no cap.
func TestListTasks_Pagination_OffsetWithoutLimit(t *testing.T) {
	d := createTestProject(t)
	var all []*orchestrator.Task
	for i := 0; i < 3; i++ {
		task := &orchestrator.Task{ProjectID: "proj-1", Title: "task", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("create task %d: %v", i, err)
		}
		all = append(all, task)
		time.Sleep(time.Millisecond)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Offset: 1})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d tasks, want 2 (3 total, offset 1, no limit)", len(got))
	}
	if got[0].ID != all[1].ID || got[1].ID != all[0].ID {
		t.Errorf("got [%s, %s], want [%s, %s]", got[0].ID, got[1].ID, all[1].ID, all[0].ID)
	}
}

// TestChildCount_AwaitingChildCount pins the new awaiting-child rollup
// column (§3.5's "⚠ N" list-row rollup): a direct awaiting child is counted
// separately from other non-terminal children, and terminal/other-status
// children are excluded.
func TestChildCount_AwaitingChildCount(t *testing.T) {
	d := createTestProject(t)
	parent := &orchestrator.Task{ProjectID: "proj-1", Title: "Parent", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	children := []*orchestrator.Task{
		{ProjectID: "proj-1", Title: "awaiting-1", ParentID: parent.ID, Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusAwaiting, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}},
		{ProjectID: "proj-1", Title: "awaiting-2", ParentID: parent.ID, Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusAwaiting, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}},
		{ProjectID: "proj-1", Title: "executing", ParentID: parent.ID, Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}},
		{ProjectID: "proj-1", Title: "done", ParentID: parent.ID, Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}},
	}
	for _, c := range children {
		if err := orchestrator.CreateTask(d.Conn, c); err != nil {
			t.Fatalf("create child %s: %v", c.Title, err)
		}
	}

	got, err := orchestrator.GetTask(d.Conn, parent.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AwaitingChildCount != 2 {
		t.Errorf("AwaitingChildCount = %d, want 2", got.AwaitingChildCount)
	}
	if got.TotalChildCount != 4 {
		t.Errorf("TotalChildCount = %d, want 4", got.TotalChildCount)
	}
}
