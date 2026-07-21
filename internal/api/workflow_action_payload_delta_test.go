package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// Phase 5b PR7 codex review Blocker 1 regression (wiring-seams.md #17):
// runDispatchLoop must persist DispatchResult.PayloadDelta (only what this
// cycle's hooks actually wrote) onto a freshly re-read task row, never
// DispatchResult.FinalPayload (a snapshot of task.Payload taken BEFORE the
// hook ran). The concrete failure this pins: a reopened task with a
// pre-existing artifact.report, whose agent job calls
// `boid task update --payload-patch` mid-flight to write a NEW report (the
// call succeeds immediately, so the DB row reflects it right away) and then
// exits with no file-based output — before this fix, the hook's own
// (necessarily stale) FinalPayload snapshot got merged on top of the
// freshly re-read row once the hook completed, silently reverting the
// report an agent had just successfully written back to its pre-reopen
// value.
func TestTaskWorkflowServiceRunDispatchLoop_PreservesMidHookRPCWrite_ReopenScenario(t *testing.T) {
	staleReport := `{"artifact":{"report":{"summary":"OLD (before reopen)"}}}`
	freshReport := `{"artifact":{"report":{"summary":"NEW (written via --payload-patch mid-job)"}}}`

	// task is the snapshot ApplyAction("reopen") captured and handed to
	// runDispatchLoop BEFORE the hook (the agent job) ever ran — it carries
	// the OLD report from before the reopen.
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   []byte(staleReport),
	}
	// taskInDB is what a fresh GetTask returns AFTER the hook's job called
	// `boid task update --payload-patch` mid-flight (via
	// TaskAppService.UpdateTaskPayloadPatch, which writes immediately, not
	// gated on job completion) — the report is already the NEW one.
	taskInDB := &orchestrator.Task{
		ID:        task.ID,
		ProjectID: task.ProjectID,
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  task.Behavior,
		Payload:   []byte(freshReport),
	}

	txStore := &recordingTxStore{task: taskInDB}
	lifecycle := &stubLifecycle{}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{
				// The hook produced no file-based output (the agent reported
				// exclusively through the RPC), so its own PayloadPatch was
				// empty — FinalPayload is therefore just the STALE snapshot,
				// unchanged. PayloadDelta correctly reflects "this cycle's
				// hooks wrote nothing".
				FinalPayload: []byte(staleReport),
				PayloadDelta: []byte(`{}`),
			},
		},
		Lifecycle: lifecycle,
	}

	svc.runDispatchLoop(
		context.Background(),
		task,
		&orchestrator.ProjectMeta{},
		orchestrator.DefaultMachine(),
	)

	if txStore.updatedTask == nil {
		t.Fatal("expected payload persistence update")
	}
	var payload struct {
		Artifact struct {
			Report struct {
				Summary string `json:"summary"`
			} `json:"report"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(txStore.updatedTask.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if payload.Artifact.Report.Summary != "NEW (written via --payload-patch mid-job)" {
		t.Fatalf("report.summary = %q, want the mid-hook RPC write to survive — got the stale pre-reopen value reverted, the exact bug this test guards against",
			payload.Artifact.Report.Summary)
	}
}

// TestTaskWorkflowServiceRunDispatchLoop_MergesNonEmptyPayloadDeltaOntoFreshRow
// is the positive counterpart: when a hook DOES produce file-based output,
// its delta must still land on the freshly re-read row (not the stale
// snapshot), so a concurrent unrelated write (e.g. a different trait
// written by the same mid-flight RPC) is preserved alongside it.
func TestTaskWorkflowServiceRunDispatchLoop_MergesNonEmptyPayloadDeltaOntoFreshRow(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
	}
	// DB row already has a "verification" key an RPC call wrote mid-flight.
	taskInDB := &orchestrator.Task{
		ID:        task.ID,
		ProjectID: task.ProjectID,
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  task.Behavior,
		Payload:   []byte(`{"verification":{"quality-review":{"passed":true}}}`),
	}

	txStore := &recordingTxStore{task: taskInDB}
	lifecycle := &stubLifecycle{}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{
				FinalPayload: []byte(`{"artifact":{"summary":"ok"}}`),
				PayloadDelta: []byte(`{"artifact":{"summary":"ok"}}`),
			},
		},
		Lifecycle: lifecycle,
	}

	svc.runDispatchLoop(
		context.Background(),
		task,
		&orchestrator.ProjectMeta{},
		orchestrator.DefaultMachine(),
	)

	if txStore.updatedTask == nil {
		t.Fatal("expected payload persistence update")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(txStore.updatedTask.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if _, ok := payload["artifact"]; !ok {
		t.Errorf("expected artifact key from the hook's own delta, got %s", txStore.updatedTask.Payload)
	}
	if _, ok := payload["verification"]; !ok {
		t.Errorf("expected verification key from the concurrent RPC write to survive, got %s", txStore.updatedTask.Payload)
	}
}
