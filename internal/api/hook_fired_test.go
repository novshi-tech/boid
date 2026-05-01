package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// hookFiredCoordinator is a test double that returns fixed FiredEvents.
type hookFiredCoordinator struct {
	dispatchResult *orchestrator.DispatchResult
	dispatchErr    error
	entryResult    *orchestrator.EntryGateResult
	entryErr       error
	dispatchCalls  int
	entryGateCalls int
}

func (c *hookFiredCoordinator) DispatchAndAdvance(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) (*orchestrator.DispatchResult, error) {
	c.dispatchCalls++
	if c.dispatchResult != nil {
		return c.dispatchResult, c.dispatchErr
	}
	return &orchestrator.DispatchResult{FinalPayload: task.Payload}, c.dispatchErr
}

func (c *hookFiredCoordinator) DispatchEntryGates(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta) (*orchestrator.EntryGateResult, error) {
	c.entryGateCalls++
	if c.entryResult != nil {
		return c.entryResult, c.entryErr
	}
	return &orchestrator.EntryGateResult{FinalPayload: task.Payload}, c.entryErr
}

func (c *hookFiredCoordinator) ReplayGate(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, gateID string) (*orchestrator.ReplayResult, error) {
	return &orchestrator.ReplayResult{FinalPayload: task.Payload}, nil
}

// TestRunDispatchLoop_HookFiredActionsRecorded verifies that hook_fired and
// exit_gate_fired actions are persisted when FiredEvents are returned from dispatch.
func TestRunDispatchLoop_HookFiredActionsRecorded(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-fired-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}

	firedEvents := []orchestrator.FiredEvent{
		{KitID: "go-dev", HandlerID: "go-dev/pr-verify", Kind: "hook", SourceState: "executing", Success: true},
		{KitID: "go-dev", HandlerID: "go-dev/pr-push", Kind: "exit_gate", SourceState: "executing", Success: false, Error: "exit code 1"},
	}

	coord := &hookFiredCoordinator{
		dispatchResult: &orchestrator.DispatchResult{
			FiredEvents:  firedEvents,
			FinalPayload: task.Payload,
		},
	}

	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tx:          recordingTransactor{store: txStore},
		Coordinator: coord,
	}

	svc.runDispatchLoop(context.Background(), task, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())

	actionTypes := make(map[string]int)
	for _, a := range txStore.actions {
		actionTypes[a.Type]++
	}
	if actionTypes["hook_fired"] != 1 {
		t.Errorf("hook_fired actions = %d, want 1", actionTypes["hook_fired"])
	}
	if actionTypes["exit_gate_fired"] != 1 {
		t.Errorf("exit_gate_fired actions = %d, want 1", actionTypes["exit_gate_fired"])
	}
}

// TestRunDispatchLoop_HookFiredAction_PayloadContents verifies that the hook_fired
// action payload contains kit_id, hook_id, source_state, success, and error fields.
func TestRunDispatchLoop_HookFiredAction_PayloadContents(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-fired-2",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}

	coord := &hookFiredCoordinator{
		dispatchResult: &orchestrator.DispatchResult{
			FiredEvents: []orchestrator.FiredEvent{
				{KitID: "go-dev", HandlerID: "go-dev/pr-verify", Kind: "hook", SourceState: "executing", Success: true},
			},
			FinalPayload: task.Payload,
		},
	}

	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tx:          recordingTransactor{store: txStore},
		Coordinator: coord,
	}

	svc.runDispatchLoop(context.Background(), task, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())

	var hookFiredAction *orchestrator.Action
	for _, a := range txStore.actions {
		if a.Type == "hook_fired" {
			hookFiredAction = a
			break
		}
	}
	if hookFiredAction == nil {
		t.Fatal("hook_fired action not found")
	}

	var payload map[string]any
	if err := json.Unmarshal(hookFiredAction.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["kit_id"] != "go-dev" {
		t.Errorf("kit_id = %v, want %q", payload["kit_id"], "go-dev")
	}
	if payload["hook_id"] != "go-dev/pr-verify" {
		t.Errorf("hook_id = %v, want %q", payload["hook_id"], "go-dev/pr-verify")
	}
	if payload["source_state"] != "executing" {
		t.Errorf("source_state = %v, want %q", payload["source_state"], "executing")
	}
	if payload["success"] != true {
		t.Errorf("success = %v, want true", payload["success"])
	}
	// FromStatus and ToStatus must match the current status (no transition)
	if hookFiredAction.FromStatus != orchestrator.TaskStatusExecuting {
		t.Errorf("FromStatus = %q, want %q", hookFiredAction.FromStatus, orchestrator.TaskStatusExecuting)
	}
	if hookFiredAction.ToStatus != orchestrator.TaskStatusExecuting {
		t.Errorf("ToStatus = %q, want %q", hookFiredAction.ToStatus, orchestrator.TaskStatusExecuting)
	}
}

// TestRunDispatchLoop_PersistsFiredEventsOnFailedDispatch verifies that when
// DispatchAndAdvance returns (result, err) — the case we hit when a hook
// exits non-zero — the partial FiredEvents are still persisted so the
// failing hook stays visible in the timeline.
func TestRunDispatchLoop_PersistsFiredEventsOnFailedDispatch(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-fired-fail-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}

	coord := &hookFiredCoordinator{
		dispatchResult: &orchestrator.DispatchResult{
			FiredEvents: []orchestrator.FiredEvent{
				{KitID: "claude-code", HandlerID: "claude-code/run-agent", Kind: "hook", SourceState: "executing", Success: false, Error: "exit code 1"},
			},
		},
		dispatchErr: errors.New(`hook dispatch: hook "claude-code/run-agent" failed: exit code 1`),
	}

	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tx:          recordingTransactor{store: txStore},
		Coordinator: coord,
	}

	svc.runDispatchLoop(context.Background(), task, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())

	actionTypes := make(map[string]int)
	for _, a := range txStore.actions {
		actionTypes[a.Type]++
	}
	if actionTypes["hook_fired"] != 1 {
		t.Errorf("hook_fired actions on failed dispatch = %d, want 1", actionTypes["hook_fired"])
	}
	if actionTypes["dispatch_error"] != 1 {
		t.Errorf("dispatch_error actions on failed dispatch = %d, want 1", actionTypes["dispatch_error"])
	}
}

// TestRunDispatchLoop_PersistsFiredEventsOnFailedEntryGate mirrors the above
// for entry-gate dispatch failures.
func TestRunDispatchLoop_PersistsFiredEventsOnFailedEntryGate(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-fired-fail-2",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}

	coord := &hookFiredCoordinator{
		dispatchResult: &orchestrator.DispatchResult{
			FinalPayload: task.Payload,
			NewStatus:    orchestrator.TaskStatusDone,
		},
		entryResult: &orchestrator.EntryGateResult{
			FiredEvents: []orchestrator.FiredEvent{
				{KitID: "go-dev", HandlerID: "go-dev/fetch-jira", Kind: "entry_gate", SourceState: "verifying", Success: false, Error: "exit code 2"},
			},
		},
		entryErr: errors.New(`entry gate dispatch: gate "go-dev/fetch-jira" failed: exit code 2`),
	}

	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tx:          recordingTransactor{store: txStore},
		Coordinator: coord,
	}

	svc.runDispatchLoop(context.Background(), task, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())

	actionTypes := make(map[string]int)
	for _, a := range txStore.actions {
		actionTypes[a.Type]++
	}
	if actionTypes["entry_gate_fired"] != 1 {
		t.Errorf("entry_gate_fired actions on failed entry-gate dispatch = %d, want 1", actionTypes["entry_gate_fired"])
	}
	if actionTypes["dispatch_error"] != 1 {
		t.Errorf("dispatch_error actions on failed entry-gate dispatch = %d, want 1", actionTypes["dispatch_error"])
	}
}

// TestRunDispatchLoop_EntryGateFiredActionsRecorded verifies that entry_gate_fired
// actions are persisted when entry gates return FiredEvents.
func TestRunDispatchLoop_EntryGateFiredActionsRecorded(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-fired-3",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}

	coord := &hookFiredCoordinator{
		// First dispatch advances to verifying, second returns no advance
		dispatchResult: &orchestrator.DispatchResult{
			FinalPayload: task.Payload,
			NewStatus:    orchestrator.TaskStatusDone,
		},
		entryResult: &orchestrator.EntryGateResult{
			FiredEvents: []orchestrator.FiredEvent{
				{KitID: "go-dev", HandlerID: "go-dev/fetch-jira", Kind: "entry_gate", SourceState: "verifying", Success: true},
			},
			FinalPayload: task.Payload,
		},
	}

	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tx:          recordingTransactor{store: txStore},
		Coordinator: coord,
	}

	svc.runDispatchLoop(context.Background(), task, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())

	actionTypes := make(map[string]int)
	for _, a := range txStore.actions {
		actionTypes[a.Type]++
	}
	if actionTypes["entry_gate_fired"] != 1 {
		t.Errorf("entry_gate_fired actions = %d, want 1", actionTypes["entry_gate_fired"])
	}
}
