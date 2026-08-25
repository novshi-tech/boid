package api

// TestTaskHandlerPatch_RemoteIDOnly_PersistsToRealDB and
// TestTaskHandlerPatch_ProjectIDOnly_PersistsToRealDB close the layering gap
// that let PATCH /api/tasks/{id} stay broken even after PR #974 fixed
// UpdateTask's SQL (task_update_persist_test.go / update_task_columns_test.go
// both pin the fix at the TaskAppService / SQL layer, calling
// TaskAppService.UpdateTask directly). TaskHandler.Patch's own "at least one
// of ..." guard (task.go) was never updated when RemoteID / ProjectID were
// added to UpdateTaskRequest, so a {"remote_id": "..."} -only PATCH body was
// rejected with 400 *before* ever reaching the (already-fixed) service/SQL
// layers underneath — no amount of service- or SQL-layer testing could catch
// a bug that lives entirely in the HTTP handler's own hand-written boolean
// expression. These tests exercise the real net/http handler (not the
// patchTaskService stub task_patch_test.go otherwise uses in this package)
// against a real sqlite DB, reusing task_update_persist_test.go's
// newRealTaskAppService helper (the same "db.Open(\":memory:\") +
// migrate.Apply" idiom — internal/api cannot import testutil here without a
// circular import through internal/server) and task_patch_test.go's
// patchRequest helper.

import (
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestTaskHandlerPatch_RemoteIDOnly_PersistsToRealDB is the HTTP-layer
// counterpart of the exact repro reported against khi production data:
// `echo '{"remote_id":"ROOKPF-307"}' | boid task update <id> --patch-file -`
// returned "at least one of title, description, payload, instructions,
// parent_id, or auto_start is required" (400) instead of persisting.
func TestTaskHandlerPatch_RemoteIDOnly_PersistsToRealDB(t *testing.T) {
	svc, tasks := newRealTaskAppService(t)
	h := &TaskHandler{Service: svc}

	// card-model-cleanup PR-2: "working" is a Card-only status now (design
	// doc §3.3), and a Card cannot carry Behavior — moved to the
	// execution-status equivalent (executing), matching
	// task_update_persist_test.go's identical substitution, since RemoteID
	// editing is gated on neither type nor status.
	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeExecution,
		Title:     "t",
		Status:    orchestrator.TaskStatusExecuting,
		RemoteID:  "OLD-1",
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := tasks.CreateTask(task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	w := patchRequest(t, http.HandlerFunc(h.Patch), task.ID, map[string]any{
		"remote_id": "ROOKPF-307",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	got, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.RemoteID != "ROOKPF-307" {
		t.Fatalf("RemoteID after re-fetch = %q, want %q", got.RemoteID, "ROOKPF-307")
	}
}

// TestTaskHandlerPatch_ProjectIDOnly_PersistsToRealDB is project_id's
// counterpart of the RemoteID test above — project_id was the other field
// PR #974 added to UpdateTaskRequest / the SQL column list without ever
// being added to TaskHandler.Patch's guard.
func TestTaskHandlerPatch_ProjectIDOnly_PersistsToRealDB(t *testing.T) {
	svc, tasks := newRealTaskAppService(t)
	h := &TaskHandler{Service: svc}

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeExecution,
		Title:     "t",
		Status:    orchestrator.TaskStatusPending,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := tasks.CreateTask(task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	w := patchRequest(t, http.HandlerFunc(h.Patch), task.ID, map[string]any{
		"project_id": "proj-2",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	got, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.ProjectID != "proj-2" {
		t.Fatalf("ProjectID after re-fetch = %q, want %q", got.ProjectID, "proj-2")
	}
}

// TestTaskHandlerPatch_ProjectIDOnly_NonPreDispatchStatus_Returns409 pins
// that opening the HTTP-layer guard to project_id does not bypass
// task_service.go's IsPreDispatchEditableStatus check underneath: a
// project_id-only patch against a task that is no longer pre-dispatch must
// still be rejected (409), just no longer with the wrong 400.
func TestTaskHandlerPatch_ProjectIDOnly_NonPreDispatchStatus_Returns409(t *testing.T) {
	svc, tasks := newRealTaskAppService(t)
	h := &TaskHandler{Service: svc}

	// working is a Card-only status (design doc §3.3): this is exactly the
	// "not pre-dispatch" fixture IsPreDispatchEditableStatus needs — a card
	// is only editable while parked, so working correctly represents "no
	// longer editable" for a card the same way it would for the retired
	// flat-model fixture.
	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Type:      orchestrator.TaskTypeCard,
		Title:     "t",
		Status:    orchestrator.TaskStatusWorking,
		Card:      &orchestrator.CardAttrs{},
	}
	if err := tasks.CreateTask(task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	w := patchRequest(t, http.HandlerFunc(h.Patch), task.ID, map[string]any{
		"project_id": "proj-2",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusConflict, w.Body.String())
	}

	got, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.ProjectID != "proj-1" {
		t.Fatalf("ProjectID after re-fetch = %q, want unchanged %q", got.ProjectID, "proj-1")
	}
}
