package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// Phase 5b PR7 (docs/plans/phase5-shim-and-task-context.md): executor-level
// dispatch tests for BoidOpTaskUpdatePayloadPatch. The core merge-semantics
// coverage (trait allowlist, unrestricted fallback, shared-trait mode) lives
// in internal/api's TestUpdateTaskPayloadPatch_* suite — these tests pin the
// executor's wiring: allowedTraits is sourced from the JobContextSnapshot
// tracked at dispatch time (jobContextProvider), never from a live meta
// lookup — codex review caught a TOCTOU staleness bug in an earlier cut
// that re-resolved the firing hook by ID against CURRENT project meta at
// merge time (wiring-seams.md #17's Major 1 finding).

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
			{ID: "task-1", ProjectID: "proj-1", Status: orchestrator.TaskStatusExecuting, Payload: json.RawMessage(`{}`)},
		},
	}
	js := &stubJobStore{
		jobs: map[string]*api.Job{
			"job-1": {ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "run-agent"},
		},
	}
	jobContexts := &stubJobContextProvider{contexts: map[string]dispatcher.JobContextSnapshot{
		"job-1": {PayloadPatchAllowedTraits: []orchestrator.TraitType{"artifact"}},
	}}
	exec := &boidBuiltinExecutor{
		tasks:       &api.TaskAppService{Tasks: ts, Jobs: js},
		jobContexts: jobContexts,
	}
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

// TestBoidBuiltinExecutor_TaskUpdatePayloadPatch_DropsTraitNotInDispatchTimeAllowlist
// proves the executor actually threads PayloadPatchAllowedTraits through
// (not just an unrestricted merge regardless of what's tracked) — a trait
// absent from the dispatch-time snapshot must be dropped even though the
// CURRENT (hypothetically edited) project meta is never consulted at all.
func TestBoidBuiltinExecutor_TaskUpdatePayloadPatch_DropsTraitNotInDispatchTimeAllowlist(t *testing.T) {
	ts := &capturingTaskStore{
		created: []*orchestrator.Task{
			{ID: "task-1", ProjectID: "proj-1", Status: orchestrator.TaskStatusExecuting, Payload: json.RawMessage(`{}`)},
		},
	}
	js := &stubJobStore{
		jobs: map[string]*api.Job{
			"job-1": {ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "verify-only"},
		},
	}
	jobContexts := &stubJobContextProvider{contexts: map[string]dispatcher.JobContextSnapshot{
		"job-1": {PayloadPatchAllowedTraits: []orchestrator.TraitType{"verification"}},
	}}
	exec := &boidBuiltinExecutor{
		tasks:       &api.TaskAppService{Tasks: ts, Jobs: js},
		jobContexts: jobContexts,
	}
	ctx := sandbox.TokenContext{JobID: "job-1", TaskID: "task-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:           sandbox.BoidOpTaskUpdatePayloadPatch,
		JobID:        "job-1",
		PayloadPatch: json.RawMessage(`{"artifact":{"report":{"summary":"done"}}}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(ts.updated[0].Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if _, ok := payload["artifact"]; ok {
		t.Fatalf("expected artifact key to be dropped (not in dispatch-time allowedTraits), got %s", ts.updated[0].Payload)
	}
}

func TestBoidBuiltinExecutor_TaskUpdatePayloadPatch_JobContextNotTracked(t *testing.T) {
	ts := &capturingTaskStore{}
	js := &stubJobStore{}
	exec := &boidBuiltinExecutor{
		tasks:       &api.TaskAppService{Tasks: ts, Jobs: js},
		jobContexts: &stubJobContextProvider{},
	}
	ctx := sandbox.TokenContext{JobID: "job-untracked", TaskID: "task-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:           sandbox.BoidOpTaskUpdatePayloadPatch,
		JobID:        "job-untracked",
		PayloadPatch: json.RawMessage(`{}`),
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "no context tracked for job") {
		t.Errorf("stderr = %q, want a no-context-tracked message", resp.Stderr)
	}
}

func TestBoidBuiltinExecutor_TaskUpdatePayloadPatch_JobNotFound(t *testing.T) {
	ts := &capturingTaskStore{}
	js := &stubJobStore{}
	jobContexts := &stubJobContextProvider{contexts: map[string]dispatcher.JobContextSnapshot{
		"job-missing": {},
	}}
	exec := &boidBuiltinExecutor{
		tasks:       &api.TaskAppService{Tasks: ts, Jobs: js},
		jobContexts: jobContexts,
	}
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
