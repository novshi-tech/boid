package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// Phase 5b PR7 (docs/plans/phase5-shim-and-task-context.md): TaskAppService.
// UpdateTaskPayloadPatch is the job_done payload_patch direct-pass RPC's
// server-side merge. Unlike UpdateTask's top-level shallow merge (used by
// --payload-file), this routes through orchestrator.MergePayloadPatch with
// an allowedTraits gate — the same merge semantics the file-based
// payload_patch.json → job_done → Coordinator pipeline has always applied
// to a hook's own PayloadPatch (see wiring-seams.md #13/#17 and
// orchestrator/coordinator.go's HandlerResult.allowedTraits).
//
// allowedTraits is a caller-supplied parameter (not looked up here) —
// codex review on the first cut of this method (which re-derived it from a
// live project-meta lookup keyed on the job's HandlerID) caught a TOCTOU
// staleness bug: project.yaml can be edited/reloaded between dispatch and
// this call, so a live lookup can silently apply the wrong trait list.
// dispatcher.JobContextSnapshot.PayloadPatchAllowedTraits (captured from
// orchestrator.JobSpec.HookTraitsProduces at dispatch time) is the actual
// source of truth in production; these tests exercise the merge directly
// with an explicit allowedTraits value, since the lookup itself no longer
// happens in this package (see internal/orchestrator/planner_test.go for
// dispatch-time capture coverage and
// internal/server/boid_executor_task_update_payload_patch_test.go for the
// executor wiring that threads JobContextSnapshot through).

func TestUpdateTaskPayloadPatch_MergesWhenTraitAllowed(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Payload:   json.RawMessage(`{}`),
	}
	job := &Job{ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "run-agent"}

	tasks := &stubTaskStore{task: task}
	jobs := &stubJobStore{job: job}
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs}

	got, err := svc.UpdateTaskPayloadPatch("job-1", json.RawMessage(`{"artifact":{"report":{"summary":"done"}}}`), []orchestrator.TraitType{"artifact"})
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
		Payload:   json.RawMessage(`{}`),
	}
	job := &Job{ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "verify-only"}

	tasks := &stubTaskStore{task: task}
	jobs := &stubJobStore{job: job}
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs}

	// "artifact" is not in the caller-supplied allowedTraits, so it must be
	// dropped (silently, mirroring MergePayloadPatch's existing behavior for
	// the file-based job_done path) rather than merged.
	got, err := svc.UpdateTaskPayloadPatch("job-1", json.RawMessage(`{"artifact":{"report":{"summary":"done"}}}`), []orchestrator.TraitType{"verification"})
	if err != nil {
		t.Fatalf("UpdateTaskPayloadPatch() error = %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if _, ok := payload["artifact"]; ok {
		t.Fatalf("expected artifact key to be dropped (not in allowedTraits), got %s", got.Payload)
	}
}

// TestUpdateTaskPayloadPatch_NilAllowedTraits_UnrestrictedMerge pins nil's
// meaning: unrestricted. This is the value dispatcher.JobContextSnapshot
// carries for a virtual/synthesized agent hook (orchestrator.
// synthesizeAgentHook, whose Traits are always the zero value — the common
// case for a behavior with no explicit `hooks:` block) as well as for an
// explicitly declared hook with no traits.produces list of its own; both
// are indistinguishable from "unrestricted" on the file-based path too.
func TestUpdateTaskPayloadPatch_NilAllowedTraits_UnrestrictedMerge(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", ProjectID: "proj-1", Payload: json.RawMessage(`{}`)}
	job := &Job{ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "agent:claude-code"}

	tasks := &stubTaskStore{task: task}
	jobs := &stubJobStore{job: job}
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs}

	got, err := svc.UpdateTaskPayloadPatch("job-1", json.RawMessage(`{"artifact":{"report":{"summary":"done"}}}`), nil)
	if err != nil {
		t.Fatalf("UpdateTaskPayloadPatch() error = %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if _, ok := payload["artifact"]; !ok {
		t.Fatalf("expected unrestricted merge for nil allowedTraits, got %s", got.Payload)
	}
}

func TestUpdateTaskPayloadPatch_JobNotFound(t *testing.T) {
	tasks := &stubTaskStore{}
	jobs := &stubJobStore{getErr: fmt.Errorf("job not found: job-missing")}
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs}

	_, err := svc.UpdateTaskPayloadPatch("job-missing", json.RawMessage(`{}`), nil)
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
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs}

	_, err := svc.UpdateTaskPayloadPatch("job-1", json.RawMessage(`{}`), nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected StatusNotFound, got %v", err)
	}
}

func TestUpdateTaskPayloadPatch_RejectsReservedKeys(t *testing.T) {
	task := &orchestrator.Task{ID: "task-1", ProjectID: "proj-1", Payload: json.RawMessage(`{}`)}
	job := &Job{ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "run-agent"}

	tasks := &stubTaskStore{task: task}
	jobs := &stubJobStore{job: job}
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs}

	_, err := svc.UpdateTaskPayloadPatch("job-1", json.RawMessage(`{"artifact":{"children":{"x":1}}}`), []orchestrator.TraitType{"artifact"})
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
		Payload:   json.RawMessage(`{"verification":{"security-review":{"passed":true}}}`),
	}
	job := &Job{ID: "job-1", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "quality-review"}

	tasks := &stubTaskStore{task: task}
	jobs := &stubJobStore{job: job}
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs}

	got, err := svc.UpdateTaskPayloadPatch("job-1", json.RawMessage(`{"verification":{"passed":true}}`), []orchestrator.TraitType{"verification"})
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

// --- Blocker 2 regression: concurrent UpdateTaskPayloadPatch calls must not
// lose updates (Phase 5b PR7 codex review, wiring-seams.md #17) ---

// barrierTaskStore is a TaskStore whose GetTask sleeps briefly while holding
// no lock, widening the read-modify-write race window enough to make a
// missing payloadPatchLockFor deterministically observable: two goroutines
// that both call GetTask at nearly the same wall-clock time will both
// return the SAME pre-write snapshot, so if UpdateTaskPayloadPatch does not
// serialize its own critical section, their two UpdateTask calls race and
// the second one's full-row write silently discards the first's change.
// With the lock in place, the second goroutine's GetTask cannot even start
// until the first's entire critical section (including its own sleep) has
// finished, so the two calls simply run sequentially — slower, but with
// both updates preserved. All access to the shared task pointer is behind
// its own mutex purely so the test itself is race-detector-clean
// regardless of whether the lock under test is doing its job.
type barrierTaskStore struct {
	mu   sync.Mutex
	task *orchestrator.Task
}

func (s *barrierTaskStore) CreateTask(task *orchestrator.Task) error { return nil }

func (s *barrierTaskStore) GetTask(id string) (*orchestrator.Task, error) {
	time.Sleep(20 * time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *s.task
	return &cp, nil
}

func (s *barrierTaskStore) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return nil, nil
}

func (s *barrierTaskStore) UpdateTask(task *orchestrator.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.task = task
	return nil
}

func (s *barrierTaskStore) DeleteTask(id string) error { return nil }
func (s *barrierTaskStore) FindTaskByRemote(remoteID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *barrierTaskStore) FindTaskByRef(ref, parentID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *barrierTaskStore) ListChildren(parentID string) ([]*orchestrator.Task, error) {
	return nil, nil
}

func TestUpdateTaskPayloadPatch_ConcurrentCallsDoNotLoseUpdates(t *testing.T) {
	tasks := &barrierTaskStore{task: &orchestrator.Task{ID: "task-1", ProjectID: "proj-1", Payload: json.RawMessage(`{}`)}}
	// Two concurrent "hooks" of the same readonly task's parallel dispatch
	// round, each patching a different artifact sub-key. stubJobStore only
	// tracks a single job, so route both goroutines through a small wrapper
	// that looks up by id.
	jobs := &multiJobStore{byID: map[string]*Job{
		"job-a": {ID: "job-a", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "hook-a"},
		"job-b": {ID: "job-b", TaskID: "task-1", ProjectID: "proj-1", HandlerID: "hook-b"},
	}}

	svc := &TaskAppService{Tasks: tasks, Jobs: jobs}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := svc.UpdateTaskPayloadPatch("job-a", json.RawMessage(`{"artifact":{"a":"1"}}`), nil)
		errs <- err
	}()
	go func() {
		defer wg.Done()
		_, err := svc.UpdateTaskPayloadPatch("job-b", json.RawMessage(`{"artifact":{"b":"2"}}`), nil)
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("UpdateTaskPayloadPatch() error = %v", err)
		}
	}

	final, err := tasks.GetTask("task-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	var payload struct {
		Artifact map[string]string `json:"artifact"`
	}
	if err := json.Unmarshal(final.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if payload.Artifact["a"] != "1" || payload.Artifact["b"] != "2" {
		t.Fatalf("lost update: expected both a and b keys, got %+v (full payload: %s)", payload.Artifact, final.Payload)
	}
}

// multiJobStore is a minimal JobStore keyed by id, used only by the
// concurrency test above (stubJobStore holds a single job, which can't
// represent two distinct concurrent jobs of the same task).
type multiJobStore struct {
	byID map[string]*Job
}

func (s *multiJobStore) GetJob(id string) (*Job, error) {
	j, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("job not found: %s", id)
	}
	return j, nil
}

func (s *multiJobStore) ListJobsByTask(taskID string) ([]*Job, error) { return nil, nil }
func (s *multiJobStore) UpdateJob(job *Job) error                     { return nil }
