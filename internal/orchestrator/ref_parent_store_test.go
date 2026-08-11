package orchestrator_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestCreateTask_RefAndParentID_Persisted(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task with ref",
		Behavior:  "dev",
		Ref:       "task-a",
		ParentID:  "parent-uuid-1234",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := orchestrator.GetTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Ref != "task-a" {
		t.Errorf("Ref = %q, want %q", got.Ref, "task-a")
	}
	if got.ParentID != "parent-uuid-1234" {
		t.Errorf("ParentID = %q, want %q", got.ParentID, "parent-uuid-1234")
	}
}

func TestCreateTask_EmptyRef_NoUniqueConstraint(t *testing.T) {
	d := createTestProject(t)

	// Multiple tasks with empty ref should not conflict
	for i := 0; i < 3; i++ {
		task := &orchestrator.Task{
			ProjectID: "proj-1",
			Title:     "Task without ref",
			Behavior:  "dev",
			// Ref:      "" (zero value, no ref)
			ParentID: "same-parent",
		}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("CreateTask[%d]: %v", i, err)
		}
	}
}

// TestCreateTask_SameRefSameParent_GetOrCreate verifies that creating a second
// task with the same (ref, parent_id) returns the existing task instead of
// failing with a unique constraint error (get-or-create semantics).
func TestCreateTask_SameRefSameParent_GetOrCreate(t *testing.T) {
	d := createTestProject(t)

	t1 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task A",
		Behavior:  "dev",
		Ref:       "step-1",
		ParentID:  "parent-uuid-abc",
	}
	if err := orchestrator.CreateTask(d.Conn, t1); err != nil {
		t.Fatalf("first CreateTask: %v", err)
	}

	t2 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task B (same ref — should get existing)",
		Behavior:  "dev",
		Ref:       "step-1",
		ParentID:  "parent-uuid-abc",
	}
	if err := orchestrator.CreateTask(d.Conn, t2); err != nil {
		t.Fatalf("second CreateTask with same ref: %v (want get-or-create, not error)", err)
	}
	if t2.ID != t1.ID {
		t.Errorf("second CreateTask returned id=%q, want existing id=%q", t2.ID, t1.ID)
	}
}

// TestCreateTask_SameRef_RootTasks_GetOrCreate pins Phase 1 PR-4 論点7: the
// get-or-create dedup, previously limited to ParentID != "" (children only),
// now also applies to root tasks (ParentID == ""). This is what makes an
// ingestion push (`task_create --initial-status triaged --ref BGO-214`)
// idempotent across a khi crash between the create response and khi
// recording the returned task_id — a resend with the same ref must return
// the SAME task, not a duplicate card.
func TestCreateTask_SameRef_RootTasks_GetOrCreate(t *testing.T) {
	d := createTestProject(t)

	t1 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "BGO-214: fix the thing",
		Behavior:  "dev",
		Ref:       "BGO-214",
		// ParentID intentionally empty: root task (a card).
	}
	if err := orchestrator.CreateTask(d.Conn, t1); err != nil {
		t.Fatalf("first CreateTask: %v", err)
	}

	t2 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "BGO-214: fix the thing (resend)",
		Behavior:  "dev",
		Ref:       "BGO-214",
	}
	if err := orchestrator.CreateTask(d.Conn, t2); err != nil {
		t.Fatalf("second (resend) CreateTask with same ref: %v (want get-or-create, not error)", err)
	}
	if t2.ID != t1.ID {
		t.Errorf("resend CreateTask returned id=%q, want existing id=%q", t2.ID, t1.ID)
	}

	// A different ref must still create a distinct root task.
	t3 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "BGO-215: a different card",
		Behavior:  "dev",
		Ref:       "BGO-215",
	}
	if err := orchestrator.CreateTask(d.Conn, t3); err != nil {
		t.Fatalf("third CreateTask (different ref): %v", err)
	}
	if t3.ID == t1.ID {
		t.Errorf("different ref unexpectedly returned the same task id %q", t3.ID)
	}
}

// TestCreateTask_SameRef_UUIDShaped_RootTask_GetOrCreate pins the codex
// review round 2 Major fix: an external source_ref that happens to BE
// UUID-shaped must still dedup idempotently on resend. Before the fix,
// FindTaskByRef's UUID branch treated the ref exclusively as a task-ID
// lookup, which always misses (the created task's own auto-generated ID is
// a DIFFERENT uuid than the string stored in its `ref` column) — a resend
// would hit the unique index on re-insert and then STILL miss on the
// error-fallback re-lookup, surfacing as a hard error instead of returning
// the existing task.
func TestCreateTask_SameRef_UUIDShaped_RootTask_GetOrCreate(t *testing.T) {
	d := createTestProject(t)
	uuidRef := uuid.New().String()

	t1 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "card with a UUID-shaped source ref",
		Behavior:  "dev",
		Ref:       uuidRef,
	}
	if err := orchestrator.CreateTask(d.Conn, t1); err != nil {
		t.Fatalf("first CreateTask: %v", err)
	}
	if t1.ID == uuidRef {
		t.Fatalf("test fixture invariant broken: task's own auto-generated ID coincidentally equals its ref %q", uuidRef)
	}

	t2 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "resend after a crash",
		Behavior:  "dev",
		Ref:       uuidRef,
	}
	if err := orchestrator.CreateTask(d.Conn, t2); err != nil {
		t.Fatalf("resend CreateTask with the same UUID-shaped ref: %v (want get-or-create, not error)", err)
	}
	if t2.ID != t1.ID {
		t.Fatalf("resend CreateTask returned id=%q, want existing id=%q", t2.ID, t1.ID)
	}
}

// TestCreateTask_SameRef_DifferentProject_RootTasks_NoCollision pins Phase 1
// PR-4's codex review Blocker fix: two root tasks in DIFFERENT projects using
// the SAME source ref (parent_id="" for both) must NOT collide — before
// migration 0037 scoped idx_tasks_ref_parent_project by project_id too, the
// second project's create would silently return the FIRST project's task
// (a cross-workspace leak), since every workspace's root tasks previously
// shared the bare (ref, parent_id="") key.
func TestCreateTask_SameRef_DifferentProject_RootTasks_NoCollision(t *testing.T) {
	d := createTestProject(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-2", WorkDir: "/tmp/proj2"}); err != nil {
		t.Fatalf("create second project: %v", err)
	}

	t1 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "workspace A's BGO-214",
		Behavior:  "dev",
		Ref:       "BGO-214",
	}
	if err := orchestrator.CreateTask(d.Conn, t1); err != nil {
		t.Fatalf("first CreateTask: %v", err)
	}

	t2 := &orchestrator.Task{
		ProjectID: "proj-2",
		Title:     "workspace B's unrelated BGO-214",
		Behavior:  "dev",
		Ref:       "BGO-214",
	}
	if err := orchestrator.CreateTask(d.Conn, t2); err != nil {
		t.Fatalf("second CreateTask (different project, same ref): %v (must not collide)", err)
	}
	if t2.ID == t1.ID {
		t.Fatalf("cross-project ref collision: project proj-2's create returned proj-1's task %q", t1.ID)
	}
	if t2.ProjectID != "proj-2" {
		t.Fatalf("t2.ProjectID = %q, want proj-2", t2.ProjectID)
	}
}

func TestCreateTask_SameRefDifferentParent_OK(t *testing.T) {
	d := createTestProject(t)

	t1 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task A",
		Behavior:  "dev",
		Ref:       "step-1",
		ParentID:  "parent-uuid-aaa",
	}
	if err := orchestrator.CreateTask(d.Conn, t1); err != nil {
		t.Fatalf("first CreateTask: %v", err)
	}

	t2 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task B",
		Behavior:  "dev",
		Ref:       "step-1",
		ParentID:  "parent-uuid-bbb",
	}
	if err := orchestrator.CreateTask(d.Conn, t2); err != nil {
		t.Fatalf("second CreateTask (different parent): %v", err)
	}
}

func TestListTasks_RefAndParentID_Persisted(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task with ref",
		Behavior:  "dev",
		Ref:       "my-ref",
		ParentID:  "my-parent",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Ref != "my-ref" {
		t.Errorf("Ref = %q, want %q", tasks[0].Ref, "my-ref")
	}
	if tasks[0].ParentID != "my-parent" {
		t.Errorf("ParentID = %q, want %q", tasks[0].ParentID, "my-parent")
	}
}

func TestFindTaskByRef_Found(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Ref task",
		Behavior:  "dev",
		Ref:       "step-2",
		ParentID:  "parent-xyz",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := orchestrator.FindTaskByRef(d.Conn, "step-2", "parent-xyz", "proj-1")
	if err != nil {
		t.Fatalf("FindTaskByRef: %v", err)
	}
	if got == nil {
		t.Fatal("FindTaskByRef returned nil, want task")
	}
	if got.ID != task.ID {
		t.Errorf("ID = %q, want %q", got.ID, task.ID)
	}
	if got.Ref != "step-2" {
		t.Errorf("Ref = %q, want %q", got.Ref, "step-2")
	}
}

func TestFindTaskByRef_NotFound_ReturnsNil(t *testing.T) {
	d := createTestProject(t)

	got, err := orchestrator.FindTaskByRef(d.Conn, "nonexistent", "parent-xyz", "proj-1")
	if err != nil {
		t.Fatalf("FindTaskByRef: %v", err)
	}
	if got != nil {
		t.Fatalf("FindTaskByRef returned %+v, want nil", got)
	}
}

func TestFindTaskByRef_UUID_LooksUpByID(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task without ref",
		Behavior:  "dev",
		// Ref is empty — only referenceable by UUID
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Look up by the task's UUID (not by ref name)
	got, err := orchestrator.FindTaskByRef(d.Conn, task.ID, "", "proj-1")
	if err != nil {
		t.Fatalf("FindTaskByRef(uuid): %v", err)
	}
	if got == nil {
		t.Fatal("FindTaskByRef(uuid) returned nil, want task")
	}
	if got.ID != task.ID {
		t.Errorf("ID = %q, want %q", got.ID, task.ID)
	}
}

func TestFindTaskByRef_UUID_NotFound_ReturnsNil(t *testing.T) {
	d := createTestProject(t)

	// Non-existent UUID
	got, err := orchestrator.FindTaskByRef(d.Conn, "00000000-0000-0000-0000-000000000000", "", "proj-1")
	if err != nil {
		t.Fatalf("FindTaskByRef(nonexistent uuid): %v", err)
	}
	if got != nil {
		t.Fatalf("FindTaskByRef returned %+v, want nil for nonexistent UUID", got)
	}
}

// TestFindTaskByRef_UUID_WrongProject_ReturnsNil pins Phase 1 PR-4's codex
// review Blocker fix: the UUID-ref lookup branch used to ignore project
// scoping entirely, so a caller in a DIFFERENT project than the target task
// could fetch it just by knowing (or guessing) its UUID. Scope mismatch must
// now behave exactly like "not found".
func TestFindTaskByRef_UUID_WrongProject_ReturnsNil(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task without ref",
		Behavior:  "dev",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := orchestrator.FindTaskByRef(d.Conn, task.ID, "", "proj-outside")
	if err != nil {
		t.Fatalf("FindTaskByRef(uuid, wrong project): %v", err)
	}
	if got != nil {
		t.Fatalf("FindTaskByRef returned task for wrong project, want nil")
	}
}

func TestFindTaskByRef_WrongParent_ReturnsNil(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Ref task",
		Behavior:  "dev",
		Ref:       "step-3",
		ParentID:  "parent-correct",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := orchestrator.FindTaskByRef(d.Conn, "step-3", "parent-wrong", "proj-1")
	if err != nil {
		t.Fatalf("FindTaskByRef: %v", err)
	}
	if got != nil {
		t.Fatalf("FindTaskByRef returned task for wrong parent, want nil")
	}
}
