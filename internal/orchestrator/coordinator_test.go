package orchestrator_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// mockExecutorWaiter implements HookExecutor, GateExecutor, and JobWaiter.
type mockExecutorWaiter struct {
	mu          sync.Mutex
	hookCalls   []*model.HookFireEvent
	gateCalls   []*model.GateFireEvent
	jobCounter  int
	completions map[string]orchestrator.JobCompletion
	execOrder   []string
}

func newMockExecutorWaiter() *mockExecutorWaiter {
	return &mockExecutorWaiter{
		completions: make(map[string]orchestrator.JobCompletion),
	}
}

func (m *mockExecutorWaiter) setHookCompletion(hookID string, output string, exitCode int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobCounter++
	jobID := fmt.Sprintf("job-%s-%d", hookID, m.jobCounter)
	m.completions[jobID] = orchestrator.JobCompletion{
		JobID:    jobID,
		Output:   output,
		ExitCode: exitCode,
	}
	return jobID
}

func (m *mockExecutorWaiter) setGateCompletion(gateID string, output string, exitCode int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobCounter++
	jobID := fmt.Sprintf("job-%s-%d", gateID, m.jobCounter)
	m.completions[jobID] = orchestrator.JobCompletion{
		JobID:    jobID,
		Output:   output,
		ExitCode: exitCode,
	}
	return jobID
}

func (m *mockExecutorWaiter) findJobForID(id string) string {
	prefix := "job-" + id + "-"
	for jobID := range m.completions {
		if len(jobID) >= len(prefix) && jobID[:len(prefix)] == prefix {
			return jobID
		}
	}
	return ""
}

func (m *mockExecutorWaiter) ExecuteHook(ctx context.Context, event *model.HookFireEvent) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hookCalls = append(m.hookCalls, event)
	m.execOrder = append(m.execOrder, "hook:"+event.Hook.ID)
	if jobID := m.findJobForID(event.Hook.ID); jobID != "" {
		return jobID, nil
	}
	m.jobCounter++
	jobID := fmt.Sprintf("job-%s-%d", event.Hook.ID, m.jobCounter)
	m.completions[jobID] = orchestrator.JobCompletion{JobID: jobID, Output: `{"payload_patch":{}}`, ExitCode: 0}
	return jobID, nil
}

func (m *mockExecutorWaiter) ExecuteGate(ctx context.Context, event *model.GateFireEvent) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gateCalls = append(m.gateCalls, event)
	m.execOrder = append(m.execOrder, "gate:"+event.Gate.ID)
	if jobID := m.findJobForID(event.Gate.ID); jobID != "" {
		return jobID, nil
	}
	m.jobCounter++
	jobID := fmt.Sprintf("job-%s-%d", event.Gate.ID, m.jobCounter)
	m.completions[jobID] = orchestrator.JobCompletion{JobID: jobID, Output: `{"payload_patch":{}}`, ExitCode: 0}
	return jobID, nil
}

func (m *mockExecutorWaiter) WaitForJob(ctx context.Context, jobID string) (orchestrator.JobCompletion, error) {
	m.mu.Lock()
	c, ok := m.completions[jobID]
	m.mu.Unlock()
	if !ok {
		return orchestrator.JobCompletion{}, fmt.Errorf("unknown job: %s", jobID)
	}
	if c.ExitCode != 0 {
		return c, fmt.Errorf("job failed with exit code %d", c.ExitCode)
	}
	return c, nil
}

func simpleStateMachine() *orchestrator.StateMachine {
	return &orchestrator.StateMachine{
		Name: "test",
		Rules: []orchestrator.Rule{
			{Action: "start", FromStatus: "pending", ToStatus: "executing"},
			{
				FromStatus: "executing",
				ToStatus:   "done",
				Condition: func(p json.RawMessage) bool {
					var m map[string]json.RawMessage
					json.Unmarshal(p, &m)
					_, ok := m["prompt"]
					return ok && string(m["prompt"]) != "null"
				},
			},
			{Action: "abort", FromStatus: "*", ToStatus: "aborted"},
		},
	}
}

func TestCoordinator_DispatchAndAdvance_HooksSequential(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"prompt":"result-a"}}`, 0)
	mock.setHookCompletion("hook-b", `{"payload_patch":{"pr":"http://example.com"}}`, 0)

	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &model.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    model.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &model.ProjectMeta{
		Hooks: []model.Hook{
			{ID: "hook-a", On: "executing", RequiresTraits: nil},
			{ID: "hook-b", On: "executing", RequiresTraits: nil},
		},
	}
	behavior := &model.TaskBehavior{Readonly: false}
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, behavior, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if len(result.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result.Results))
	}

	var payload map[string]json.RawMessage
	json.Unmarshal(result.FinalPayload, &payload)
	if _, ok := payload["prompt"]; !ok {
		t.Error("expected prompt in final payload")
	}

	if result.NewStatus != model.TaskStatusDone {
		t.Errorf("expected new status done, got %q", result.NewStatus)
	}
}

func TestCoordinator_DispatchAndAdvance_NoAdvanceWhenConditionNotMet(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{}}`, 0)

	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &model.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    model.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &model.ProjectMeta{
		Hooks: []model.Hook{
			{ID: "hook-a", On: "executing"},
		},
	}
	behavior := &model.TaskBehavior{Readonly: false}
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, behavior, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if result.NewStatus != "" {
		t.Errorf("expected empty new status, got %q", result.NewStatus)
	}
}

func TestCoordinator_DispatchAndAdvance_GatesExecuteAfterHooks(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"prompt":"done"}}`, 0)
	mock.setGateCompletion("gate-push", `{"payload_patch":{"pr":"http://pr-url"}}`, 0)

	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &model.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    model.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &model.ProjectMeta{
		Hooks: []model.Hook{
			{ID: "hook-a", On: "executing"},
		},
		Gates: []model.Gate{
			{ID: "gate-push", On: "executing"},
		},
	}
	behavior := &model.TaskBehavior{Readonly: false}
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, behavior, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if len(result.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result.Results))
	}

	if len(mock.execOrder) != 2 {
		t.Fatalf("expected 2 executions, got %d", len(mock.execOrder))
	}
	if mock.execOrder[0] != "hook:hook-a" {
		t.Errorf("expected hook first, got %s", mock.execOrder[0])
	}
	if mock.execOrder[1] != "gate:gate-push" {
		t.Errorf("expected gate second, got %s", mock.execOrder[1])
	}
}

func TestCoordinator_DispatchAndAdvance_ExclusiveTraitCollision(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"prompt":"from-a"}}`, 0)
	mock.setHookCompletion("hook-b", `{"payload_patch":{"prompt":"from-b"}}`, 0)

	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &model.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    model.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &model.ProjectMeta{
		Hooks: []model.Hook{
			{ID: "hook-a", On: "executing"},
			{ID: "hook-b", On: "executing"},
		},
	}
	behavior := &model.TaskBehavior{Readonly: false}
	sm := simpleStateMachine()

	_, err := coord.DispatchAndAdvance(context.Background(), task, meta, behavior, sm)
	if err == nil {
		t.Fatal("expected error for exclusive trait collision")
	}
}

func TestCoordinator_DispatchAndAdvance_SharedTraitNoCollision(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"verification":{"findings":[{"message":"ok","status":"resolved"}]}}}`, 0)
	mock.setHookCompletion("hook-b", `{"payload_patch":{"verification":{"findings":[{"message":"bug","status":"open"}]}}}`, 0)

	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &model.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    model.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &model.ProjectMeta{
		Hooks: []model.Hook{
			{ID: "hook-a", On: "executing"},
			{ID: "hook-b", On: "executing"},
		},
	}
	behavior := &model.TaskBehavior{Readonly: false}
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, behavior, sm)
	if err != nil {
		t.Fatalf("shared trait should not collide: %v", err)
	}

	var payload map[string]json.RawMessage
	json.Unmarshal(result.FinalPayload, &payload)
	var verification map[string]json.RawMessage
	json.Unmarshal(payload["verification"], &verification)
	if _, ok := verification["hook-a"]; !ok {
		t.Error("expected hook-a sub-key in verification")
	}
	if _, ok := verification["hook-b"]; !ok {
		t.Error("expected hook-b sub-key in verification")
	}

	for _, key := range []string{"hook-a", "hook-b"} {
		var entry struct {
			SourceState string `json:"source_state"`
		}
		if err := json.Unmarshal(verification[key], &entry); err != nil {
			t.Fatalf("unmarshal %s: %v", key, err)
		}
		if entry.SourceState != "executing" {
			t.Errorf("%s: source_state = %q, want %q", key, entry.SourceState, "executing")
		}
	}
}

func TestCoordinator_DispatchAndAdvance_EmptyHooksAndGates(t *testing.T) {
	mock := newMockExecutorWaiter()
	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &model.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    model.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &model.ProjectMeta{}
	behavior := &model.TaskBehavior{}
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, behavior, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(result.Results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(result.Results))
	}
}
