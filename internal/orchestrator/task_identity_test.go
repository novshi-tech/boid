package orchestrator_test

// docs/plans/ingestion-identity.md PR-1 (B-1): identity 索引の store 層テスト。
// 「検証」節 (PR-1) を直接 pin する:
//   - 同じ (project_id, identity) を別 task へ link → ErrIdentityConflict
//   - 同じ task への再 link は成功 (冪等)
//   - 別 project なら同じ identity 文字列を link できる (I-3)
//   - drop 相当の一括解放 (UnlinkAllForTask) → 同じキーで再び link できる
//   - task 行の削除 (GC) で binding が cascade で消える

import (
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestLinkIdentity_ConflictOnDifferentTask_ButIdempotentOnSameTask(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	taskA := &orchestrator.Task{ProjectID: "proj-1", Title: "A", Behavior: "dev", Payload: []byte(`{}`)}
	if err := orchestrator.CreateTask(d.Conn, taskA); err != nil {
		t.Fatalf("create task A: %v", err)
	}
	taskB := &orchestrator.Task{ProjectID: "proj-1", Title: "B", Behavior: "dev", Payload: []byte(`{}`)}
	if err := orchestrator.CreateTask(d.Conn, taskB); err != nil {
		t.Fatalf("create task B: %v", err)
	}

	if err := orchestrator.LinkIdentity(d.Conn, "proj-1", "jira:ROOKPF-289", taskA.ID); err != nil {
		t.Fatalf("first link: %v", err)
	}

	// Re-linking the SAME (project, identity) to the SAME task must succeed
	// (idempotent) — a workspace resend must not be treated as a conflict.
	if err := orchestrator.LinkIdentity(d.Conn, "proj-1", "jira:ROOKPF-289", taskA.ID); err != nil {
		t.Fatalf("idempotent re-link to same task: %v", err)
	}

	// Linking the SAME (project, identity) to a DIFFERENT task must be
	// rejected outright (silent newest-wins is exactly the bug this
	// unique-index-backed store closes — see match_card.py's
	// build_key_index() note in the design doc).
	err := orchestrator.LinkIdentity(d.Conn, "proj-1", "jira:ROOKPF-289", taskB.ID)
	if !errors.Is(err, orchestrator.ErrIdentityConflict) {
		t.Fatalf("link to different task: err = %v, want ErrIdentityConflict", err)
	}

	// The conflicting link must not have moved the binding.
	got, err := orchestrator.ResolveIdentity(d.Conn, "proj-1", "jira:ROOKPF-289")
	if err != nil {
		t.Fatalf("resolve after rejected conflict: %v", err)
	}
	if got.ID != taskA.ID {
		t.Fatalf("resolved task = %s, want %s (conflict must not move the binding)", got.ID, taskA.ID)
	}
}

func TestLinkIdentity_SameIdentityDifferentProjects(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project 1: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-2", WorkDir: "/tmp/proj-2"}); err != nil {
		t.Fatalf("create project 2: %v", err)
	}
	task1 := &orchestrator.Task{ProjectID: "proj-1", Title: "T1", Behavior: "dev", Payload: []byte(`{}`)}
	if err := orchestrator.CreateTask(d.Conn, task1); err != nil {
		t.Fatalf("create task1: %v", err)
	}
	task2 := &orchestrator.Task{ProjectID: "proj-2", Title: "T2", Behavior: "dev", Payload: []byte(`{}`)}
	if err := orchestrator.CreateTask(d.Conn, task2); err != nil {
		t.Fatalf("create task2: %v", err)
	}

	// I-3: identity scoping is per-project, so the SAME identity string may
	// be linked to a DIFFERENT task in a DIFFERENT project without conflict.
	if err := orchestrator.LinkIdentity(d.Conn, "proj-1", "jira:SAME-1", task1.ID); err != nil {
		t.Fatalf("link in proj-1: %v", err)
	}
	if err := orchestrator.LinkIdentity(d.Conn, "proj-2", "jira:SAME-1", task2.ID); err != nil {
		t.Fatalf("link in proj-2: %v", err)
	}

	got1, err := orchestrator.ResolveIdentity(d.Conn, "proj-1", "jira:SAME-1")
	if err != nil {
		t.Fatalf("resolve proj-1: %v", err)
	}
	if got1.ID != task1.ID {
		t.Fatalf("proj-1 resolved to %s, want %s", got1.ID, task1.ID)
	}

	got2, err := orchestrator.ResolveIdentity(d.Conn, "proj-2", "jira:SAME-1")
	if err != nil {
		t.Fatalf("resolve proj-2: %v", err)
	}
	if got2.ID != task2.ID {
		t.Fatalf("proj-2 resolved to %s, want %s", got2.ID, task2.ID)
	}
}

func TestResolveIdentity_NotFound(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	_, err := orchestrator.ResolveIdentity(d.Conn, "proj-1", "jira:UNKNOWN-1")
	if !errors.Is(err, orchestrator.ErrTaskNotFound) {
		t.Fatalf("err = %v, want ErrTaskNotFound", err)
	}
}

func TestUnlinkIdentity_ThenRelinkToDifferentTask(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	taskA := &orchestrator.Task{ProjectID: "proj-1", Title: "A", Behavior: "dev", Payload: []byte(`{}`)}
	if err := orchestrator.CreateTask(d.Conn, taskA); err != nil {
		t.Fatalf("create task A: %v", err)
	}
	taskB := &orchestrator.Task{ProjectID: "proj-1", Title: "B", Behavior: "dev", Payload: []byte(`{}`)}
	if err := orchestrator.CreateTask(d.Conn, taskB); err != nil {
		t.Fatalf("create task B: %v", err)
	}

	if err := orchestrator.LinkIdentity(d.Conn, "proj-1", "jira:X-1", taskA.ID); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := orchestrator.UnlinkIdentity(d.Conn, "proj-1", "jira:X-1"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	// Freed key can now be linked to a DIFFERENT task without conflict.
	if err := orchestrator.LinkIdentity(d.Conn, "proj-1", "jira:X-1", taskB.ID); err != nil {
		t.Fatalf("re-link after unlink: %v", err)
	}
	got, err := orchestrator.ResolveIdentity(d.Conn, "proj-1", "jira:X-1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.ID != taskB.ID {
		t.Fatalf("resolved task = %s, want %s", got.ID, taskB.ID)
	}
}

func TestUnlinkAllForTask_ReleasesEveryIdentity(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "T", Behavior: "dev", Payload: []byte(`{}`)}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	other := &orchestrator.Task{ProjectID: "proj-1", Title: "Other", Behavior: "dev", Payload: []byte(`{}`)}
	if err := orchestrator.CreateTask(d.Conn, other); err != nil {
		t.Fatalf("create other task: %v", err)
	}

	// A task can hold multiple identities.
	if err := orchestrator.LinkIdentity(d.Conn, "proj-1", "jira:M-1", task.ID); err != nil {
		t.Fatalf("link 1: %v", err)
	}
	if err := orchestrator.LinkIdentity(d.Conn, "proj-1", "slack:987.654", task.ID); err != nil {
		t.Fatalf("link 2: %v", err)
	}
	names, err := orchestrator.ListIdentitiesByTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("list before unlink: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("identities before unlink = %v, want 2 entries", names)
	}

	if err := orchestrator.UnlinkAllForTask(d.Conn, task.ID); err != nil {
		t.Fatalf("unlink all: %v", err)
	}

	names, err = orchestrator.ListIdentitiesByTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("list after unlink: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("identities after unlink = %v, want none", names)
	}

	// Freed keys can be relinked to a DIFFERENT task (I-6's whole point).
	if err := orchestrator.LinkIdentity(d.Conn, "proj-1", "jira:M-1", other.ID); err != nil {
		t.Fatalf("relink freed key to different task: %v", err)
	}
}

func TestTaskDelete_CascadesIdentityBindings(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "T", Behavior: "dev", Payload: []byte(`{}`)}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := orchestrator.LinkIdentity(d.Conn, "proj-1", "jira:GC-1", task.ID); err != nil {
		t.Fatalf("link: %v", err)
	}

	if _, err := d.Conn.Exec(`DELETE FROM tasks WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("delete task: %v", err)
	}

	// FK ON DELETE CASCADE must have removed the binding row along with the
	// task — the same guarantee task_triage relies on for GC (30 day terminal
	// task sweep).
	names, err := orchestrator.ListIdentitiesByTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("list after task delete: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("identities after cascade delete = %v, want none", names)
	}
	if _, err := orchestrator.ResolveIdentity(d.Conn, "proj-1", "jira:GC-1"); !errors.Is(err, orchestrator.ErrTaskNotFound) {
		t.Fatalf("resolve after cascade delete: err = %v, want ErrTaskNotFound", err)
	}
}

func TestLinkIdentity_RejectsEmptyIdentity(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "T", Behavior: "dev", Payload: []byte(`{}`)}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := orchestrator.LinkIdentity(d.Conn, "proj-1", "", task.ID); err == nil {
		t.Fatal("expected error linking an empty identity string")
	}
}
