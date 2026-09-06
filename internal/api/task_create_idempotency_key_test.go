package api

// TestTaskHandlerCreate_IdempotencyKey_* exercise the host API entry point
// (POST /api/tasks, TaskHandler.Create) against a REAL sqlite DB (the same
// newRealTaskAppService idiom task_update_persist_test.go/
// task_patch_persist_test.go already use) — this is the layer both `boid
// task create --idempotency-key` (cmd/task.go) and the sandboxed
// `task_create` builtin op (internal/server/boid_executor.go) ultimately
// reach, so pinning correctness here backs all three of docs/plans/
// signal-ingest-detailed-design.md §8's "CLI・sandbox op・host API の3経路
// すべて" entry points at once — the CLI/sandbox-op-specific tests (cmd/
// task_test.go, internal/server/boid_executor_task_create_idempotency_key_test.go)
// only need to check that each entry point puts idempotency_key on the wire
// correctly, since they both terminate in exactly the TaskAppService.CreateTask
// call this file exercises directly.
//
// The actual get-or-create enforcement lives in orchestrator.CreateTask
// (internal/orchestrator/idempotency_key_store_test.go covers it, including
// the concurrent-create race) — these tests instead pin that the HTTP/service
// layer wires req.IdempotencyKey through to the Task row untouched, and that
// a real round trip through the full stack returns the SAME task id on
// retry.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func createTaskRequest(t *testing.T, handler http.Handler, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// TestTaskHandlerCreate_IdempotencyKey_RetryReturnsExistingTask is the
// end-to-end host-API pin for §8's "衝突時は既存 task の id を返して exit 0"
// — a second POST /api/tasks with the same (project_id, idempotency_key)
// must come back 201 (not 409/500) with the FIRST task's id, and must not
// insert a second row.
func TestTaskHandlerCreate_IdempotencyKey_RetryReturnsExistingTask(t *testing.T) {
	svc, tasks := newRealTaskAppService(t)
	h := &TaskHandler{Service: svc}

	w1 := createTaskRequest(t, http.HandlerFunc(h.Create), map[string]any{
		"project_id":      "proj-1",
		"title":           "first attempt",
		"initial_status":  "parked",
		"idempotency_key": "card-1:child-gen-1",
	})
	if w1.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want %d: body=%s", w1.Code, http.StatusCreated, w1.Body.String())
	}
	var first orchestrator.Task
	if err := json.Unmarshal(w1.Body.Bytes(), &first); err != nil {
		t.Fatalf("unmarshal first response: %v", err)
	}
	if first.ID == "" {
		t.Fatal("first create: task id is empty")
	}

	w2 := createTaskRequest(t, http.HandlerFunc(h.Create), map[string]any{
		"project_id":      "proj-1",
		"title":           "retry (should return existing)",
		"initial_status":  "parked",
		"idempotency_key": "card-1:child-gen-1",
	})
	if w2.Code != http.StatusCreated {
		t.Fatalf("retry create status = %d, want %d: body=%s", w2.Code, http.StatusCreated, w2.Body.String())
	}
	var second orchestrator.Task
	if err := json.Unmarshal(w2.Body.Bytes(), &second); err != nil {
		t.Fatalf("unmarshal retry response: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("retry create returned id=%q, want existing id=%q", second.ID, first.ID)
	}

	got, err := tasks.ListTasks(orchestrator.TaskFilter{ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListTasks returned %d tasks after retry, want exactly 1", len(got))
	}
}

// TestTaskAppServiceCreateTask_IdempotencyKey_HitOnPendingTask_RescuesAutoStart
// pins that the IdempotencyKey get-or-create fires "start" itself when the
// existing hit is still pending and req.AutoStart is set — otherwise a
// resumed caller retrying the same idempotency_key gets the same pending
// task back forever.
func TestTaskAppServiceCreateTask_IdempotencyKey_HitOnPendingTask_RescuesAutoStart(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}
	existing := &orchestrator.Task{
		ID:        "existing-task-id",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusPending,
		ProjectID: "proj-1",
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	store := &stubTaskStore{
		idempotencyTasks: map[string]*orchestrator.Task{
			"proj-1::resume-key": existing,
		},
	}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    store,
		Meta:     stubMetaStore{meta: meta},
		Workflow: workflow,
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:      "proj-1",
		Title:          "resumed create",
		Behavior:       "dev",
		IdempotencyKey: "resume-key",
		AutoStart:      true,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task == nil {
		t.Fatal("CreateTask() returned nil task")
	}
	if task.ID != existing.ID {
		t.Fatalf("CreateTask() returned id=%q, want the existing hit's id=%q", task.ID, existing.ID)
	}
	if workflow.appliedType != "start" {
		t.Fatalf("ApplyAction not called with start on the pending idempotency-key hit; appliedType = %q, want %q", workflow.appliedType, "start")
	}
	if workflow.appliedTaskID != existing.ID {
		t.Fatalf("ApplyAction called on taskID=%q, want existing id=%q", workflow.appliedTaskID, existing.ID)
	}
}

// TestTaskAppServiceCreateTask_IdempotencyKey_HitOnExecutingTask_NoAutoStart
// is the regression guard alongside the rescue above: a hit that is already
// past pending must never re-fire start (mirrors main's
// task.Status == pending gate).
func TestTaskAppServiceCreateTask_IdempotencyKey_HitOnExecutingTask_NoAutoStart(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}
	existing := &orchestrator.Task{
		ID:        "existing-task-id",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusExecuting,
		ProjectID: "proj-1",
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	store := &stubTaskStore{
		idempotencyTasks: map[string]*orchestrator.Task{
			"proj-1::resume-key": existing,
		},
	}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    store,
		Meta:     stubMetaStore{meta: meta},
		Workflow: workflow,
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:      "proj-1",
		Title:          "resumed create",
		Behavior:       "dev",
		IdempotencyKey: "resume-key",
		AutoStart:      true,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task.ID != existing.ID {
		t.Fatalf("CreateTask() returned id=%q, want existing id=%q", task.ID, existing.ID)
	}
	if workflow.appliedType != "" {
		t.Fatalf("ApplyAction should not fire for an already-executing idempotency-key hit; appliedType = %q", workflow.appliedType)
	}
}

// TestTaskAppServiceCreateTask_RefMissAndIdempotencyKeyHit_UnderCard pins
// that a caller under a card supplying BOTH a fresh Ref (that misses
// FindTaskByRef) and an IdempotencyKey that DOES match an already-created
// live child gets that existing child back, not a false 409 from the
// single-work-slot check.
func TestTaskAppServiceCreateTask_RefMissAndIdempotencyKeyHit_UnderCard(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}
	existing := &orchestrator.Task{
		ID:        "existing-child-id",
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusPending,
		ProjectID: "proj-1",
		ParentID:  "card-1",
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	parent := &orchestrator.Task{
		ID:             "card-1",
		Type:           orchestrator.TaskTypeCard,
		Status:         orchestrator.TaskStatusWorking,
		ProjectID:      "proj-1",
		OpenChildCount: 1, // existing's own live row — not a second occupant
	}
	store := &stubTaskStore{
		tasks: map[string]*orchestrator.Task{"card-1": parent},
		idempotencyTasks: map[string]*orchestrator.Task{
			"proj-1:card-1:dup-key": existing,
		},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:      "proj-1",
		ParentID:       "card-1",
		Title:          "retry with a fresh ref, same idempotency key",
		Behavior:       "dev",
		Ref:            "brand-new-ref-this-attempt-only",
		IdempotencyKey: "dup-key",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want the existing idempotency-key hit returned instead of a slot-conflict 409", err)
	}
	if task.ID != existing.ID {
		t.Fatalf("CreateTask() returned id=%q, want existing id=%q", task.ID, existing.ID)
	}
}

// TestTaskHandlerCreate_IdempotencyKey_DifferentProject_NoCollision pins
// project scoping at the HTTP layer, mirroring
// TestCreateTask_SameIdempotencyKey_DifferentProject_NoCollision at the store
// layer.
func TestTaskHandlerCreate_IdempotencyKey_DifferentProject_NoCollision(t *testing.T) {
	svc, _ := newRealTaskAppService(t)
	h := &TaskHandler{Service: svc}

	w1 := createTaskRequest(t, http.HandlerFunc(h.Create), map[string]any{
		"project_id":      "proj-1",
		"title":           "proj-1 task",
		"initial_status":  "parked",
		"idempotency_key": "shared-key",
	})
	if w1.Code != http.StatusCreated {
		t.Fatalf("proj-1 create status = %d, want %d: body=%s", w1.Code, http.StatusCreated, w1.Body.String())
	}
	var t1 orchestrator.Task
	if err := json.Unmarshal(w1.Body.Bytes(), &t1); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	w2 := createTaskRequest(t, http.HandlerFunc(h.Create), map[string]any{
		"project_id":      "proj-2",
		"title":           "proj-2 task",
		"initial_status":  "parked",
		"idempotency_key": "shared-key",
	})
	if w2.Code != http.StatusCreated {
		t.Fatalf("proj-2 create status = %d, want %d: body=%s", w2.Code, http.StatusCreated, w2.Body.String())
	}
	var t2 orchestrator.Task
	if err := json.Unmarshal(w2.Body.Bytes(), &t2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if t2.ID == t1.ID {
		t.Fatalf("proj-2 create got the SAME id as proj-1 (%q) — idempotency_key must be project-scoped", t1.ID)
	}
}

// TestTaskHandlerCreate_NoIdempotencyKey_AlwaysCreatesNew is the regression
// guard at the HTTP layer: omitting idempotency_key must leave existing
// `POST /api/tasks` behavior completely unchanged — every call creates a
// fresh task.
func TestTaskHandlerCreate_NoIdempotencyKey_AlwaysCreatesNew(t *testing.T) {
	svc, _ := newRealTaskAppService(t)
	h := &TaskHandler{Service: svc}

	ids := make(map[string]bool)
	for i := 0; i < 3; i++ {
		w := createTaskRequest(t, http.HandlerFunc(h.Create), map[string]any{
			"project_id":     "proj-1",
			"title":          "no idempotency key",
			"initial_status": "parked",
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("create[%d] status = %d, want %d: body=%s", i, w.Code, http.StatusCreated, w.Body.String())
		}
		var got orchestrator.Task
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal[%d]: %v", i, err)
		}
		if ids[got.ID] {
			t.Fatalf("create[%d] reused id %q — every no-key create must be distinct", i, got.ID)
		}
		ids[got.ID] = true
	}
	if len(ids) != 3 {
		t.Fatalf("created %d distinct tasks, want 3", len(ids))
	}
}
