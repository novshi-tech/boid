package server

// TestBoidBuiltinExecutor_TaskCreate_IdempotencyKey_* pin the sandboxed
// task_create builtin op's DEDUP ENFORCEMENT (docs/plans/
// signal-ingest-detailed-design.md §8) — one of the "CLI・sandbox op・host
// API の3経路すべて" this PR wires. Unlike capturingTaskStore
// (boid_executor_test.go), which is a pure in-memory spy with no real
// get-or-create semantics, these tests run against a REAL sqlite DB
// (newBoidExecutorTestDB, this package's own precedent — see
// boid_executor_task_identity_test.go's file-level comment for why this
// package can't use testutil) so the actual dedup enforcement
// (orchestrator.CreateTask, pinned independently by internal/orchestrator/
// idempotency_key_store_test.go) is exercised through
// boidBuiltinExecutor.ExecuteBoidBuiltin → api.TaskAppService.CreateTask →
// orchestrator.CreateTask.
//
// This file does NOT go through boid_shim.go's parseBoidTaskCreate (it
// builds sandbox.BoidRequest{CreatePatch: ...} directly) — an earlier
// version of this comment claimed it did, which was wrong (PR #1012 review,
// Opus L1) and, worse, meant NOTHING in this PR's test suite actually
// exercised parseBoidTaskCreate's own flag parsing; that's exactly how
// Opus M1 (parseBoidTaskCreate had no case for --idempotency-key at all, so
// the flag form was unusable from inside a sandbox — the feature's primary
// intended call site) shipped undetected. The shim-level coverage now lives
// in internal/sandbox/boid_shim_test.go's
// TestRunBoidShim_TaskCreate_IdempotencyKeyFlag(EqualsForm), which calls the
// real sandbox.RunBoidShim entry point. This file's job is narrower and
// still worth keeping separately: proving the broker/executor/store chain
// underneath actually enforces the (project_id, parent_id, idempotency_key)
// dedup once a CreatePatch — however it was produced — carries the field.

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

// TestBoidBuiltinExecutor_TaskCreate_IdempotencyKey_DifferentParent_NoCollision
// is the M3 regression guard (PR #1012 review, Opus) exercised through the
// actual task_create op path a judgment task uses: two DIFFERENT parent
// tasks in the same project, both minting a child with the SAME
// idempotency_key, must each get their OWN child — not have the second
// call's op silently succeed while handing back the first parent's child
// (see internal/orchestrator/idempotency_key_store_test.go's
// TestCreateTask_SameIdempotencyKey_DifferentParent_NoCollision for the
// store-layer version of this same scenario, and Task.IdempotencyKey's doc
// comment for the full story of the bug this guards against).
func TestBoidBuiltinExecutor_TaskCreate_IdempotencyKey_DifferentParent_NoCollision(t *testing.T) {
	conn := newBoidExecutorTestDB(t)
	if err := orchestrator.CreateProject(conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	tasks := orchestrator.NewTaskRepository(conn)
	if err := tasks.CreateTask(&orchestrator.Task{ID: "parent-a", ProjectID: "proj-1", Type: orchestrator.TaskTypeCard, Card: &orchestrator.CardAttrs{}}); err != nil {
		t.Fatalf("create parent-a: %v", err)
	}
	if err := tasks.CreateTask(&orchestrator.Task{ID: "parent-b", ProjectID: "proj-1", Type: orchestrator.TaskTypeCard, Card: &orchestrator.CardAttrs{}}); err != nil {
		t.Fatalf("create parent-b: %v", err)
	}
	exec := &boidBuiltinExecutor{
		tasks: &api.TaskAppService{Tasks: tasks, Meta: executorMetaStub{meta: &orchestrator.ProjectMeta{}}},
	}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	respA := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: []byte(`{"title":"parent-a's child","initial_status":"parked","parent_id":"parent-a","ref":"child-a-ref","idempotency_key":"child-gen-1"}`),
	})
	if respA.ExitCode != 0 {
		t.Fatalf("create under parent-a: exit code = %d, stderr: %s", respA.ExitCode, respA.Stderr)
	}

	respB := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskCreate,
		CreatePatch: []byte(`{"title":"parent-b's child","initial_status":"parked","parent_id":"parent-b","ref":"child-b-ref","idempotency_key":"child-gen-1"}`),
	})
	if respB.ExitCode != 0 {
		t.Fatalf("create under parent-b with same idempotency key as parent-a's child: exit code = %d, stderr: %s (want success, not collision)", respB.ExitCode, respB.Stderr)
	}

	bChildren, err := tasks.ListChildren("parent-b")
	if err != nil {
		t.Fatalf("ListChildren(parent-b): %v", err)
	}
	if len(bChildren) != 1 || bChildren[0].Title != "parent-b's child" {
		t.Fatalf("ListChildren(parent-b) = %+v, want exactly one task titled %q (parent-b must not come back empty while parent-a silently gained it)", bChildren, "parent-b's child")
	}

	aChildren, err := tasks.ListChildren("parent-a")
	if err != nil {
		t.Fatalf("ListChildren(parent-a): %v", err)
	}
	if len(aChildren) != 1 || aChildren[0].Title != "parent-a's child" {
		t.Fatalf("ListChildren(parent-a) = %+v, want exactly one task titled %q", aChildren, "parent-a's child")
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
