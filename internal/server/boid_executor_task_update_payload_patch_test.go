package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// Phase 5b PR7 (docs/plans/phase5-shim-and-task-context.md): executor-level
// dispatch tests for BoidOpTaskUpdatePayloadPatch. The core merge-semantics
// coverage (trait allowlist, unrestricted fallback, shared-trait mode) lives
// in internal/api's TestUpdateTaskPayloadPatch_* suite — these tests only
// pin the executor's wiring (unavailable-without-tasks, happy path reaching
// api.TaskAppService, error surfacing).

func TestBoidBuiltinExecutor_TaskUpdatePayloadPatch_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{tasks: nil}
	ctx := sandbox.TokenContext{JobID: "job-1", TaskID: "task-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:           sandbox.BoidOpTaskUpdatePayloadPatch,
		JobID:        "job-1",
		PayloadPatch: json.RawMessage(`{"artifact":{"report":{"summary":"x"}}}`),
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "unavailable") {
		t.Errorf("stderr = %q, want an unavailable message", resp.Stderr)
	}
}

func TestBoidBuiltinExecutor_TaskUpdatePayloadPatch_HappyPath(t *testing.T) {
	ts := &capturingTaskStore{
		created: []*orchestrator.Task{
			{ID: "task-1", ProjectID: "proj-1", Behavior: "executor", Status: orchestrator.TaskStatusExecuting, Payload: json.RawMessage(`{}`)},
		},
	}
	js := &stubJobStore{
		jobs: map[string]*api.Job{
			"job-1": {ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "run-agent"},
		},
	}
	meta := executorMetaStub{meta: &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {
				Hooks: []orchestrator.Hook{
					{ID: "run-agent", Kind: orchestrator.HandlerKindAgent, Traits: orchestrator.HandlerTraits{
						Produces: []orchestrator.TraitType{"artifact"},
					}},
				},
			},
		},
	}}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: ts, Jobs: js, Meta: meta}}
	ctx := sandbox.TokenContext{JobID: "job-1", TaskID: "task-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:           sandbox.BoidOpTaskUpdatePayloadPatch,
		JobID:        "job-1",
		PayloadPatch: json.RawMessage(`{"artifact":{"report":{"summary":"done"}}}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(ts.updated) != 1 {
		t.Fatalf("UpdateTask calls = %d, want 1", len(ts.updated))
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(ts.updated[0].Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if _, ok := payload["artifact"]; !ok {
		t.Fatalf("expected artifact key in merged payload, got %s", ts.updated[0].Payload)
	}
}

func TestBoidBuiltinExecutor_TaskUpdatePayloadPatch_JobNotFound(t *testing.T) {
	ts := &capturingTaskStore{}
	js := &stubJobStore{}
	meta := executorMetaStub{meta: &orchestrator.ProjectMeta{}}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: ts, Jobs: js, Meta: meta}}
	ctx := sandbox.TokenContext{JobID: "job-missing", TaskID: "task-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:           sandbox.BoidOpTaskUpdatePayloadPatch,
		JobID:        "job-missing",
		PayloadPatch: json.RawMessage(`{}`),
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
}
