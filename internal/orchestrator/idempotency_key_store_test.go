package orchestrator_test

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestCreateTask_SameIdempotencyKey_GetOrCreate pins docs/plans/
// signal-ingest-detailed-design.md §8: a second CreateTask call with the same
// (project_id, idempotency_key) must not insert a duplicate row — it must
// return the FIRST task instead (get-or-create, exit-0 semantics), mirroring
// Ref's own get-or-create pattern (ref_parent_store_test.go).
func TestCreateTask_SameIdempotencyKey_GetOrCreate(t *testing.T) {
	d := createTestProject(t)

	t1 := &orchestrator.Task{
		ProjectID:      "proj-1",
		Type:           orchestrator.TaskTypeExecution,
		Title:          "First attempt",
		IdempotencyKey: "parent-card-1:child-gen-1",
		Exec: &orchestrator.ExecAttrs{
			Behavior: "dev",
		},
	}
	if err := orchestrator.CreateTask(d.Conn, t1); err != nil {
		t.Fatalf("first CreateTask: %v", err)
	}

	t2 := &orchestrator.Task{
		ProjectID:      "proj-1",
		Type:           orchestrator.TaskTypeExecution,
		Title:          "Retry (should get existing)",
		IdempotencyKey: "parent-card-1:child-gen-1",
		Exec: &orchestrator.ExecAttrs{
			Behavior: "dev",
		},
	}
	if err := orchestrator.CreateTask(d.Conn, t2); err != nil {
		t.Fatalf("second CreateTask with same idempotency key: %v (want get-or-create, not error)", err)
	}
	if t2.ID != t1.ID {
		t.Errorf("second CreateTask returned id=%q, want existing id=%q", t2.ID, t1.ID)
	}
	if t2.Title != "First attempt" {
		t.Errorf("second CreateTask returned title=%q, want the FIRST task's title (existing row, not the retry's)", t2.Title)
	}

	// Exactly one row must exist — not two.
	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListTasks returned %d tasks, want exactly 1 (no duplicate insert)", len(got))
	}
}

// TestCreateTask_SameIdempotencyKey_DifferentProject_NoCollision pins that
// idempotency_key is scoped by project_id (matching the partial unique index
// `(project_id, idempotency_key) WHERE idempotency_key IS NOT NULL` from
// migration 0047) — two different projects reusing the same internal key must
// each get their own task, not collide.
func TestCreateTask_SameIdempotencyKey_DifferentProject_NoCollision(t *testing.T) {
	d := createTestProject(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-2", WorkDir: "/tmp/proj-2"}); err != nil {
		t.Fatalf("create project proj-2: %v", err)
	}

	t1 := &orchestrator.Task{
		ProjectID:      "proj-1",
		Type:           orchestrator.TaskTypeExecution,
		Title:          "Project 1 task",
		IdempotencyKey: "shared-key",
		Exec:           &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := orchestrator.CreateTask(d.Conn, t1); err != nil {
		t.Fatalf("CreateTask proj-1: %v", err)
	}

	t2 := &orchestrator.Task{
		ProjectID:      "proj-2",
		Type:           orchestrator.TaskTypeExecution,
		Title:          "Project 2 task",
		IdempotencyKey: "shared-key",
		Exec:           &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := orchestrator.CreateTask(d.Conn, t2); err != nil {
		t.Fatalf("CreateTask proj-2 with same key as proj-1: %v (want independent creation, not collision)", err)
	}
	if t2.ID == t1.ID {
		t.Fatalf("proj-2 task got the SAME id as proj-1's task (%q) — idempotency_key must be project-scoped", t1.ID)
	}
}

// TestCreateTask_SameIdempotencyKey_DifferentParent_NoCollision is the M3
// regression guard (PR #1012 review, Opus): scoping idempotency_key by
// ProjectID alone let two DIFFERENT parent tasks in the SAME project, both
// reusing a plain key like "child-gen-1", silently collide — the second
// parent's create call succeeded (no error) but handed back the FIRST
// parent's child, which is exactly the "wrong id, no error" failure mode
// this feature exists to prevent (just aimed at the wrong parent). Confirmed
// reproducible before migration 0047 was rescoped to
// (project_id, parent_id, idempotency_key): parent-b's ListChildren stayed
// empty while parent-a silently gained an extra child with the wrong title.
func TestCreateTask_SameIdempotencyKey_DifferentParent_NoCollision(t *testing.T) {
	d := createTestProject(t)

	// Both parents pre-exist as ordinary tasks so ListChildren has something
	// real to scope against below.
	parentA := &orchestrator.Task{ID: "parent-a", ProjectID: "proj-1", Type: orchestrator.TaskTypeCard, Card: &orchestrator.CardAttrs{}}
	parentB := &orchestrator.Task{ID: "parent-b", ProjectID: "proj-1", Type: orchestrator.TaskTypeCard, Card: &orchestrator.CardAttrs{}}
	if err := orchestrator.CreateTask(d.Conn, parentA); err != nil {
		t.Fatalf("create parent-a: %v", err)
	}
	if err := orchestrator.CreateTask(d.Conn, parentB); err != nil {
		t.Fatalf("create parent-b: %v", err)
	}

	childA := &orchestrator.Task{
		ProjectID:      "proj-1",
		Type:           orchestrator.TaskTypeExecution,
		Title:          "parent-a's child",
		ParentID:       "parent-a",
		Ref:            "child-a-ref",
		IdempotencyKey: "child-gen-1",
		Exec:           &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := orchestrator.CreateTask(d.Conn, childA); err != nil {
		t.Fatalf("CreateTask under parent-a: %v", err)
	}

	childB := &orchestrator.Task{
		ProjectID:      "proj-1",
		Type:           orchestrator.TaskTypeExecution,
		Title:          "parent-b's child",
		ParentID:       "parent-b",
		Ref:            "child-b-ref", // different ref: this collision is specific to idempotency_key
		IdempotencyKey: "child-gen-1", // SAME key as childA, but a DIFFERENT parent
		Exec:           &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := orchestrator.CreateTask(d.Conn, childB); err != nil {
		t.Fatalf("CreateTask under parent-b with same idempotency key as parent-a's child: %v (want independent creation, not collision)", err)
	}
	if childB.ID == childA.ID {
		t.Fatalf("parent-b's child got the SAME id as parent-a's child (%q) — idempotency_key must be parent-scoped, not just project-scoped", childA.ID)
	}
	if childB.Title != "parent-b's child" {
		t.Errorf("childB.Title = %q, want its OWN title (got parent-a's child back instead of creating its own)", childB.Title)
	}

	bChildren, err := orchestrator.ListChildren(d.Conn, "parent-b")
	if err != nil {
		t.Fatalf("ListChildren(parent-b): %v", err)
	}
	if len(bChildren) != 1 || bChildren[0].ID != childB.ID {
		t.Errorf("ListChildren(parent-b) = %v, want exactly [childB] (%q)", bChildren, childB.ID)
	}

	aChildren, err := orchestrator.ListChildren(d.Conn, "parent-a")
	if err != nil {
		t.Fatalf("ListChildren(parent-a): %v", err)
	}
	if len(aChildren) != 1 || aChildren[0].ID != childA.ID {
		t.Errorf("ListChildren(parent-a) = %v, want exactly [childA] (%q) — must not have gained parent-b's attempt", aChildren, childA.ID)
	}
}

// TestCreateTask_SameIdempotencyKey_TypeMismatch_ReturnsError is the L3
// regression guard (PR #1012 review, Opus): a Card create whose
// idempotency_key collides with an EXISTING Execution row under the same
// (project_id, parent_id) — or vice versa — must fail loudly, not silently
// hand back the wrong-typed task. Without this check, the dedup hit
// overwrites the caller's *Task with the existing row's full shape
// (including its Type), so a caller that built an Execution task with a
// non-nil Exec would get back a Card with Exec == nil, before
// validateTaskTypeConsistency ever runs (the dedup hit returns early,
// bypassing that check entirely).
func TestCreateTask_SameIdempotencyKey_TypeMismatch_ReturnsError(t *testing.T) {
	d := createTestProject(t)

	card := &orchestrator.Task{
		ProjectID:      "proj-1",
		Type:           orchestrator.TaskTypeCard,
		Title:          "existing card",
		IdempotencyKey: "shared-key",
		Card:           &orchestrator.CardAttrs{},
	}
	if err := orchestrator.CreateTask(d.Conn, card); err != nil {
		t.Fatalf("create card: %v", err)
	}

	exec := &orchestrator.Task{
		ProjectID:      "proj-1",
		Type:           orchestrator.TaskTypeExecution,
		Title:          "conflicting execution create",
		IdempotencyKey: "shared-key",
		Exec:           &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	err := orchestrator.CreateTask(d.Conn, exec)
	if err == nil {
		t.Fatalf("CreateTask succeeded (returned task %+v) for a type-mismatched idempotency_key collision, want an error", exec)
	}

	// Exactly the original card must exist — the mismatched attempt must not
	// have inserted a second (wrongly-typed) row either.
	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 1 || got[0].ID != card.ID {
		t.Fatalf("ListTasks = %v, want exactly the original card (%q)", got, card.ID)
	}
}

// TestCreateTask_EmptyIdempotencyKey_AlwaysCreatesNew is the regression guard:
// omitting idempotency_key must not change existing behavior at all — every
// call creates a fresh task, exactly like before this feature existed
// (mirrors TestCreateTask_EmptyRef_NoUniqueConstraint).
func TestCreateTask_EmptyIdempotencyKey_AlwaysCreatesNew(t *testing.T) {
	d := createTestProject(t)

	ids := make(map[string]bool)
	for i := 0; i < 3; i++ {
		task := &orchestrator.Task{
			ProjectID: "proj-1",
			Type:      orchestrator.TaskTypeExecution,
			Title:     "Task without idempotency key",
			// IdempotencyKey: "" (zero value, no key)
			Exec: &orchestrator.ExecAttrs{Behavior: "dev"},
		}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("CreateTask[%d]: %v", i, err)
		}
		if ids[task.ID] {
			t.Fatalf("CreateTask[%d] reused id %q — every no-key create must be distinct", i, task.ID)
		}
		ids[task.ID] = true
	}
	if len(ids) != 3 {
		t.Fatalf("created %d distinct tasks, want 3", len(ids))
	}
}

// TestCreateTask_SameIdempotencyKey_ConcurrentCreates_OnlyOneRowInserted is
// the true-concurrency regression guard: N goroutines racing CreateTask with
// the SAME (project_id, idempotency_key) must not all insert — exactly one
// row must exist afterward, and every goroutine must come back with that
// SAME task id (no goroutine may observe a hard error). The partial unique
// index (migration 0047) is what makes the loser's INSERT fail, and
// CreateTask's post-insert-conflict fallback (store.go) is what turns that
// failure into "return the winner's row" instead of surfacing the DB error —
// see TestWorkspaceRepository_UpdateIfRevisionMatches_ConcurrentPUTsOnlyOneWins
// (workspace_repository_test.go) for the same shape of test against a
// different feature.
func TestCreateTask_SameIdempotencyKey_ConcurrentCreates_OnlyOneRowInserted(t *testing.T) {
	d := createTestProject(t)

	const n = 8
	var wg sync.WaitGroup
	errCount := int32(0)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			task := &orchestrator.Task{
				ProjectID:      "proj-1",
				Type:           orchestrator.TaskTypeExecution,
				Title:          "concurrent create",
				IdempotencyKey: "race-key",
				Exec:           &orchestrator.ExecAttrs{Behavior: "dev"},
			}
			if err := orchestrator.CreateTask(d.Conn, task); err != nil {
				atomic.AddInt32(&errCount, 1)
				t.Errorf("goroutine %d: CreateTask: %v", i, err)
				return
			}
			ids[i] = task.ID
		}(i)
	}
	wg.Wait()

	if errCount != 0 {
		t.Fatalf("%d/%d concurrent CreateTask calls errored, want 0", errCount, n)
	}

	first := ids[0]
	if first == "" {
		t.Fatal("goroutine 0 recorded no id")
	}
	for i, id := range ids {
		if id != first {
			t.Errorf("goroutine %d returned id=%q, want all goroutines to agree on %q", i, id, first)
		}
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListTasks returned %d tasks after %d concurrent creates, want exactly 1", len(got), n)
	}
}
