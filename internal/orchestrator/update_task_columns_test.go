package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestUpdateTask_PersistsRemoteIDProjectIDAutoStart pins a silent no-op bug:
// UpdateTask's UPDATE ... SET column list was missing remote_id, project_id
// and auto_start (present in CreateTask's INSERT, present in scanTask's
// SELECT, but dropped from the UPDATE). Every caller that mutates
// task.RemoteID / task.ProjectID / task.AutoStart in memory and then calls
// UpdateTask (api.TaskAppService.UpdateTask being the concrete case that
// surfaced this: khi-task-collector's self-heal loop patched remote_id
// forever without it ever sticking) got a 200/nil-error response while the
// row underneath silently kept its old value. See UpdateTask's own doc
// comment for why ref/behavior are deliberately NOT in this list.
func TestUpdateTask_PersistsRemoteIDProjectIDAutoStart(t *testing.T) {
	d := createTestProject(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-2", WorkDir: "/tmp/proj-2"}); err != nil {
		t.Fatalf("create second project: %v", err)
	}

	task := &orchestrator.Task{
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "proj-1",
		Title:     "t",
		RemoteID:  "OLD-1",
		Exec: &orchestrator.ExecAttrs{
			Behavior:  "dev",
			AutoStart: false,
		},
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	task.RemoteID = "ROOKPF-306"
	task.ProjectID = "proj-2"
	task.Exec.AutoStart = true
	if err := orchestrator.UpdateTask(d.Conn, task); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	// Re-fetch through a FRESH GetTask call (separate DB round trip from the
	// mutated in-memory `task` above) — this is the actual shape of the
	// reported bug: the in-memory struct looks updated no matter what the
	// SQL does, only a re-read exposes whether it was persisted.
	got, err := orchestrator.GetTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.RemoteID != "ROOKPF-306" {
		t.Errorf("RemoteID after re-fetch = %q, want %q (UpdateTask must persist remote_id)", got.RemoteID, "ROOKPF-306")
	}
	if got.ProjectID != "proj-2" {
		t.Errorf("ProjectID after re-fetch = %q, want %q (UpdateTask must persist project_id)", got.ProjectID, "proj-2")
	}
	if !got.Exec.AutoStart {
		t.Error("AutoStart after re-fetch = false, want true (UpdateTask must persist auto_start)")
	}
}

// TestUpdateTask_DoesNotPersistRefOrBehavior pins the flip side: ref and
// behavior are intentionally NOT in UpdateTask's column list (no API-level
// UpdateTaskRequest field exists to change either — see
// docs/plans/ingestion-identity.md 決定16 for ref, and CreateTaskRequest vs.
// UpdateTaskRequest in internal/apiwire/task.go for behavior). Mutating them
// in-memory and calling UpdateTask must be a no-op, not a write — this test
// guards against a future edit accidentally wiring one of them into the SQL
// without also deciding whether it needs a status guard first.
func TestUpdateTask_DoesNotPersistRefOrBehavior(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "proj-1",
		Title:     "t",
		Ref:       "original-ref",
		Exec: &orchestrator.ExecAttrs{
			Behavior: "dev",
		},
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	task.Ref = "changed-ref"
	task.Exec.Behavior = "changed-behavior"
	if err := orchestrator.UpdateTask(d.Conn, task); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	got, err := orchestrator.GetTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Ref != "original-ref" {
		t.Errorf("Ref after UpdateTask = %q, want unchanged %q (ref is an immutable dedup key)", got.Ref, "original-ref")
	}
	if got.Exec.Behavior != "dev" {
		t.Errorf("Behavior after UpdateTask = %q, want unchanged %q (behavior has no update port)", got.Exec.Behavior, "dev")
	}
}
