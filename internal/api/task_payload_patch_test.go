package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// Phase 5b PR7 (docs/plans/phase5-shim-and-task-context.md): TaskAppService.
// UpdateTaskPayloadPatch is the job_done payload_patch direct-pass RPC's
// server-side merge. Unlike UpdateTask's top-level shallow merge (used by
// --payload-file), this routes through orchestrator.MergePayloadPatch with
// the allowedTraits gate derived from the CALLING job's own HandlerID — the
// same merge semantics the file-based payload_patch.json → job_done →
// Coordinator pipeline has always applied to a hook's own PayloadPatch (see
// wiring-seams.md #13/#17 and orchestrator/coordinator.go's
// HandlerResult.allowedTraits). A hook not found in behavior.Hooks (the
// common case for a virtual/synthesized agent hook — see
// orchestrator.synthesizeAgentHook, whose Traits are always the zero value)
// falls back to an unrestricted merge, matching allowedTraits' own
// not-found behavior exactly.

func hookProducingArtifact(handlerID string) orchestrator.ProjectMeta {
	return orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {
				Hooks: []orchestrator.Hook{
					{
						ID:   handlerID,
						Kind: orchestrator.HandlerKindAgent,
						Traits: orchestrator.HandlerTraits{
							Produces: []orchestrator.TraitType{"artifact"},
						},
					},
				},
			},
		},
	}
}

func TestUpdateTaskPayloadPatch_MergesWhenTraitAllowed(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Behavior:  "executor",
		Payload:   json.RawMessage(`{}`),
	}
	job := &Job{ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "run-agent"}
	meta := hookProducingArtifact("run-agent")

	tasks := &stubTaskStore{task: task}
	jobs := &stubJobStore{job: job}
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs, Meta: stubMetaStore{meta: &meta}}

	got, err := svc.UpdateTaskPayloadPatch("job-1", json.RawMessage(`{"artifact":{"report":{"summary":"done"}}}`))
	if err != nil {
		t.Fatalf("UpdateTaskPayloadPatch() error = %v", err)
	}
	if got.ID != "task-1" {
		t.Fatalf("unexpected task returned: %+v", got)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if _, ok := payload["artifact"]; !ok {
		t.Fatalf("expected artifact key in merged payload, got %s", got.Payload)
	}
	if tasks.updateCalls != 1 {
		t.Fatalf("UpdateTask calls = %d, want 1", tasks.updateCalls)
	}
}

func TestUpdateTaskPayloadPatch_DropsTraitNotInProduces(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Behavior:  "executor",
		Payload:   json.RawMessage(`{}`),
	}
	job := &Job{ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "verify-only"}
	meta := orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {
				Hooks: []orchestrator.Hook{
					{
						ID:   "verify-only",
						Kind: orchestrator.HandlerKindAgent,
						Traits: orchestrator.HandlerTraits{
							Produces: []orchestrator.TraitType{"verification"},
						},
					},
				},
			},
		},
	}

	tasks := &stubTaskStore{task: task}
	jobs := &stubJobStore{job: job}
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs, Meta: stubMetaStore{meta: &meta}}

	// "artifact" is not in this hook's Produces list, so it must be dropped
	// (silently, mirroring MergePayloadPatch's existing behavior for the
	// file-based job_done path) rather than merged.
	got, err := svc.UpdateTaskPayloadPatch("job-1", json.RawMessage(`{"artifact":{"report":{"summary":"done"}}}`))
	if err != nil {
		t.Fatalf("UpdateTaskPayloadPatch() error = %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if _, ok := payload["artifact"]; ok {
		t.Fatalf("expected artifact key to be dropped (not in Produces), got %s", got.Payload)
	}
}

func TestUpdateTaskPayloadPatch_HookNotFound_UnrestrictedMerge(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Behavior:  "executor",
		Payload:   json.RawMessage(`{}`),
	}
	// HandlerID "agent:claude-code" mirrors orchestrator.synthesizeAgentHook's
	// virtual hook ID form — it never appears in behavior.Hooks on disk
	// because it's synthesized in-memory at Evaluate() time, not declared in
	// project.yaml. This is the common case for a project with no explicit
	// hooks: block (e.g. boid's own .boid/project.yaml).
	job := &Job{ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "agent:claude-code"}
	meta := orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {}, // no hooks declared
		},
	}

	tasks := &stubTaskStore{task: task}
	jobs := &stubJobStore{job: job}
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs, Meta: stubMetaStore{meta: &meta}}

	got, err := svc.UpdateTaskPayloadPatch("job-1", json.RawMessage(`{"artifact":{"report":{"summary":"done"}}}`))
	if err != nil {
		t.Fatalf("UpdateTaskPayloadPatch() error = %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if _, ok := payload["artifact"]; !ok {
		t.Fatalf("expected unrestricted merge (hook not found -> nil allowedTraits), got %s", got.Payload)
	}
}

func TestUpdateTaskPayloadPatch_NoProjectMeta_UnrestrictedMerge(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", ProjectID: "proj-1", Behavior: "executor", Payload: json.RawMessage(`{}`)}
	job := &Job{ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "run-agent"}

	tasks := &stubTaskStore{task: task}
	jobs := &stubJobStore{job: job}
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs, Meta: stubMetaStore{}} // meta.Get returns false

	got, err := svc.UpdateTaskPayloadPatch("job-1", json.RawMessage(`{"artifact":{"report":{"summary":"done"}}}`))
	if err != nil {
		t.Fatalf("UpdateTaskPayloadPatch() error = %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if _, ok := payload["artifact"]; !ok {
		t.Fatalf("expected unrestricted merge when project meta is unavailable, got %s", got.Payload)
	}
}

func TestUpdateTaskPayloadPatch_JobNotFound(t *testing.T) {
	tasks := &stubTaskStore{}
	jobs := &stubJobStore{getErr: fmt.Errorf("job not found: job-missing")}
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs, Meta: stubMetaStore{}}

	_, err := svc.UpdateTaskPayloadPatch("job-missing", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected StatusNotFound, got %v", err)
	}
}

func TestUpdateTaskPayloadPatch_TaskNotFound(t *testing.T) {
	job := &Job{ID: "job-1", TaskID: "task-missing", ProjectID: "proj-1", HandlerID: "run-agent"}
	tasks := &stubTaskStore{err: fmt.Errorf("task not found")}
	jobs := &stubJobStore{job: job}
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs, Meta: stubMetaStore{}}

	_, err := svc.UpdateTaskPayloadPatch("job-1", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected StatusNotFound, got %v", err)
	}
}

func TestUpdateTaskPayloadPatch_RejectsReservedKeys(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", ProjectID: "proj-1", Behavior: "executor", Payload: json.RawMessage(`{}`)}
	job := &Job{ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "run-agent"}
	meta := hookProducingArtifact("run-agent")

	tasks := &stubTaskStore{task: task}
	jobs := &stubJobStore{job: job}
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs, Meta: stubMetaStore{meta: &meta}}

	_, err := svc.UpdateTaskPayloadPatch("job-1", json.RawMessage(`{"artifact":{"children":{"x":1}}}`))
	if err == nil {
		t.Fatal("expected error for reserved artifact.children key, got nil")
	}
}

// TestUpdateTaskPayloadPatch_SharedTraitMergesByHandlerID proves the new op
// really routes through orchestrator.MergePayloadPatch's trait-mode-aware
// merge (not a hand-rolled shallow merge): the "verification" trait uses
// MergeModeShared, which keys sub-entries by handler id (job.HandlerID here)
// rather than overwriting the whole trait.
func TestUpdateTaskPayloadPatch_SharedTraitMergesByHandlerID(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Behavior:  "executor",
		Payload:   json.RawMessage(`{"verification":{"security-review":{"passed":true}}}`),
	}
	job := &Job{ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "quality-review"}
	meta := orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {
				Hooks: []orchestrator.Hook{
					{
						ID:   "quality-review",
						Kind: orchestrator.HandlerKindAgent,
						Traits: orchestrator.HandlerTraits{
							Produces: []orchestrator.TraitType{"verification"},
						},
					},
				},
			},
		},
	}

	tasks := &stubTaskStore{task: task}
	jobs := &stubJobStore{job: job}
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs, Meta: stubMetaStore{meta: &meta}}

	got, err := svc.UpdateTaskPayloadPatch("job-1", json.RawMessage(`{"verification":{"passed":true}}`))
	if err != nil {
		t.Fatalf("UpdateTaskPayloadPatch() error = %v", err)
	}
	var payload struct {
		Verification map[string]json.RawMessage `json:"verification"`
	}
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if _, ok := payload.Verification["security-review"]; !ok {
		t.Fatalf("expected prior handler's shared entry to survive, got %s", got.Payload)
	}
	if _, ok := payload.Verification["quality-review"]; !ok {
		t.Fatalf("expected this job's HandlerID as the shared-trait key, got %s", got.Payload)
	}
}
