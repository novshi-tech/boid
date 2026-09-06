package api

// Pins two Opus-review fixes to the launcher task-continuation attach:
//   - a launcher-supplied ref/idempotency_key hitting CreateTask's own
//     get-or-create early-return must still attach to its card_requests
//     row (P1) — otherwise the slot stays "launching" forever.
//   - a card-type create (initial_status=parked) must never consume a
//     card_requests slot (P2) — that guard lives in boid_executor.go, so
//     this file only pins the general-purpose attach helper's own
//     behavior via the ref/idempotency paths.

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type fakeCardRequestTaskLinker struct {
	calls []struct {
		task      *orchestrator.Task
		requestID string
	}
	err error
}

func (f *fakeCardRequestTaskLinker) CreateTaskLinkedToCardRequest(t *orchestrator.Task, requestID string) error {
	f.calls = append(f.calls, struct {
		task      *orchestrator.Task
		requestID string
	}{t, requestID})
	return f.err
}

func TestCreateTask_RefHit_StillAttachesCardRequest(t *testing.T) {
	existing := &orchestrator.Task{ID: "task-1", ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Ref: "issue-123"}
	store := &stubTaskStore{
		refTasks: map[string]*orchestrator.Task{"issue-123:": existing},
	}
	linker := &fakeCardRequestTaskLinker{}
	svc := &TaskAppService{
		Tasks:             store,
		Meta:              stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		CardRequestLinker: linker,
	}

	got, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:     "proj-1",
		Title:         "retry from launcher",
		Behavior:      "dev",
		Ref:           "issue-123",
		CardRequestID: "req-1",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if got.ID != existing.ID {
		t.Fatalf("got.ID = %q, want the existing task %q", got.ID, existing.ID)
	}
	if len(linker.calls) != 1 {
		t.Fatalf("CreateTaskLinkedToCardRequest calls = %d, want 1 — a ref hit must still attach", len(linker.calls))
	}
	if linker.calls[0].requestID != "req-1" || linker.calls[0].task.ID != existing.ID {
		t.Errorf("call = %+v, want request req-1 attached to existing task", linker.calls[0])
	}
}

func TestCreateTask_IdempotencyKeyHit_StillAttachesCardRequest(t *testing.T) {
	existing := &orchestrator.Task{ID: "task-1", ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, IdempotencyKey: "key-1"}
	store := &stubTaskStore{
		idempotencyTasks: map[string]*orchestrator.Task{"proj-1::key-1": existing},
	}
	linker := &fakeCardRequestTaskLinker{}
	svc := &TaskAppService{
		Tasks:             store,
		Meta:              stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		CardRequestLinker: linker,
	}

	got, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:      "proj-1",
		Title:          "retry from launcher",
		Behavior:       "dev",
		IdempotencyKey: "key-1",
		CardRequestID:  "req-1",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if got.ID != existing.ID {
		t.Fatalf("got.ID = %q, want the existing task %q", got.ID, existing.ID)
	}
	if len(linker.calls) != 1 {
		t.Fatalf("CreateTaskLinkedToCardRequest calls = %d, want 1 — an idempotency-key hit must still attach", len(linker.calls))
	}
}

// TestCreateTask_NoCardRequestID_LinkerNeverCalled pins that an ordinary
// create (no card_request context) never touches CardRequestLinker at all.
func TestCreateTask_NoCardRequestID_LinkerNeverCalled(t *testing.T) {
	existing := &orchestrator.Task{ID: "task-1", ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Ref: "issue-123"}
	store := &stubTaskStore{
		refTasks: map[string]*orchestrator.Task{"issue-123:": existing},
	}
	linker := &fakeCardRequestTaskLinker{}
	svc := &TaskAppService{
		Tasks:             store,
		Meta:              stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		CardRequestLinker: linker,
	}

	if _, err := svc.CreateTask(CreateTaskRequest{ProjectID: "proj-1", Title: "ordinary", Behavior: "dev", Ref: "issue-123"}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if len(linker.calls) != 0 {
		t.Fatalf("CreateTaskLinkedToCardRequest calls = %d, want 0 for an ordinary create", len(linker.calls))
	}
}
