package server

// TestBoidBuiltinExecutor_TaskCreate_IdempotencyKey_* pin the sandboxed
// `boid task create --idempotency-key <key>` / task_create builtin op entry
// point (docs/plans/signal-ingest-detailed-design.md §8) — the third of the
// "CLI・sandbox op・host API の3経路すべて" this PR wires. Unlike
// capturingTaskStore (boid_executor_test.go), which is a pure in-memory spy
// with no real get-or-create semantics, these tests run against a REAL
// sqlite DB (newBoidExecutorTestDB, this package's own precedent — see
// boid_executor_task_identity_test.go's file-level comment for why this
// package can't use testutil) so the actual dedup enforcement
// (orchestrator.CreateTask, pinned independently by internal/orchestrator/
// idempotency_key_store_test.go) is exercised end to end through
// boid_shim.go's "forward the whole YAML map" idiom
// (parseBoidTaskCreate) → the broker → boidBuiltinExecutor.ExecuteBoidBuiltin
// → api.TaskAppService.CreateTask → orchestrator.CreateTask.

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// TestBoidBuiltinExecutor_TaskCreate_IdempotencyKey_RetryReturnsExistingTask
// pins §8's "衝突時は既存 task の id を返して exit 0": a second task_create op
// call with the same idempotency_key in its CreatePatch must not insert a
// second row, and both calls must succeed (exit 0).
func TestBoidBuiltinExecutor_TaskCreate_IdempotencyKey_RetryReturnsExistingTask(t *testing.T) {
	conn := newBoidExecutorTestDB(t)
	if err := orchestrator.CreateProject(conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	tasks := orchestrator.NewTaskRepository(conn)
	exec := &boidBuiltinExecutor{
		tasks: &api.TaskAppService{Tasks: tasks, Meta: executorMetaStub{meta: &orchestrator.ProjectMeta{}}},
	}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp1 := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: []byte(`{"title":"first attempt","initial_status":"parked","idempotency_key":"card-1:child-gen-1"}`),
	})
	if resp1.ExitCode != 0 {
		t.Fatalf("first create exit code = %d, stderr: %s", resp1.ExitCode, resp1.Stderr)
	}

	resp2 := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: []byte(`{"title":"retry (should return existing)","initial_status":"parked","idempotency_key":"card-1:child-gen-1"}`),
	})
	if resp2.ExitCode != 0 {
		t.Fatalf("retry create exit code = %d, stderr: %s", resp2.ExitCode, resp2.Stderr)
	}

	got, err := tasks.ListTasks(orchestrator.TaskFilter{ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListTasks returned %d tasks after retry via task_create op, want exactly 1", len(got))
	}
	if got[0].Title != "first attempt" {
		t.Errorf("surviving task title = %q, want the FIRST attempt's title (existing row, not the retry's)", got[0].Title)
	}
}

// TestBoidBuiltinExecutor_TaskCreate_NoIdempotencyKey_AlwaysCreatesNew is the
// regression guard: a task_create op call with no idempotency_key in its
// CreatePatch must behave exactly as before this feature — every call
// creates a fresh task.
func TestBoidBuiltinExecutor_TaskCreate_NoIdempotencyKey_AlwaysCreatesNew(t *testing.T) {
	conn := newBoidExecutorTestDB(t)
	if err := orchestrator.CreateProject(conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	tasks := orchestrator.NewTaskRepository(conn)
	exec := &boidBuiltinExecutor{
		tasks: &api.TaskAppService{Tasks: tasks, Meta: executorMetaStub{meta: &orchestrator.ProjectMeta{}}},
	}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	for i := 0; i < 2; i++ {
		resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
			Op:          sandbox.BoidOpTaskCreate,
			CreatePatch: []byte(`{"title":"no idempotency key","initial_status":"parked"}`),
		})
		if resp.ExitCode != 0 {
			t.Fatalf("create[%d] exit code = %d, stderr: %s", i, resp.ExitCode, resp.Stderr)
		}
	}

	got, err := tasks.ListTasks(orchestrator.TaskFilter{ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListTasks returned %d tasks, want 2 (no dedup without idempotency_key)", len(got))
	}
}
