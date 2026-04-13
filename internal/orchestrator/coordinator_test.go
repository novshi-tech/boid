package orchestrator_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

// mockExecutorWaiter implements HookExecutor, GateExecutor, and JobWaiter.
type mockExecutorWaiter struct {
	mu          sync.Mutex
	hookCalls   []*projectspec.HookFireEvent
	gateCalls   []*projectspec.GateFireEvent
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

func (m *mockExecutorWaiter) ExecuteHook(ctx context.Context, event *projectspec.HookFireEvent) (string, error) {
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

func (m *mockExecutorWaiter) ExecuteGate(ctx context.Context, event *projectspec.GateFireEvent) (string, error) {
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

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Hooks: []projectspec.Hook{
			{ID: "hook-a", On: orchestrator.OnValues{"executing"}},
			{ID: "hook-b", On: orchestrator.OnValues{"executing"}},
		},
	}
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
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

	if result.NewStatus != orchestrator.TaskStatusDone {
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

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Hooks: []projectspec.Hook{
			{ID: "hook-a", On: orchestrator.OnValues{"executing"}},
		},
	}
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
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

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Hooks: []projectspec.Hook{
			{ID: "hook-a", On: orchestrator.OnValues{"executing"}},
		},
		Gates: []projectspec.Gate{
			{ID: "gate-push", On: orchestrator.OnValues{"executing"}},
		},
	}
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
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

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Hooks: []projectspec.Hook{
			{ID: "hook-a", On: orchestrator.OnValues{"executing"}},
			{ID: "hook-b", On: orchestrator.OnValues{"executing"}},
		},
	}
	sm := simpleStateMachine()

	_, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
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

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Hooks: []projectspec.Hook{
			{ID: "hook-a", On: orchestrator.OnValues{"executing"}},
			{ID: "hook-b", On: orchestrator.OnValues{"executing"}},
		},
	}
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
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

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{}
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(result.Results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(result.Results))
	}
}

// mockWorktreeLocker records Acquire calls for testing.
type mockWorktreeLocker struct {
	mu       sync.Mutex
	acquired []string // keys that were acquired
	released []string // keys that were released
}

func (m *mockWorktreeLocker) Acquire(ctx context.Context, key string) (func(), error) {
	m.mu.Lock()
	m.acquired = append(m.acquired, key)
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		m.released = append(m.released, key)
		m.mu.Unlock()
	}, nil
}

func TestCoordinator_DispatchAndAdvance_LockerAcquiredForNonReadonlyNonWorktree(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"prompt":"done"}}`, 0)

	locker := &mockWorktreeLocker{}
	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
		Locker:       locker,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Hooks: []projectspec.Hook{
			{ID: "hook-a", On: orchestrator.OnValues{"executing"}},
		},
	}
	sm := simpleStateMachine()

	_, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if len(locker.acquired) != 1 || locker.acquired[0] != "proj-1" {
		t.Errorf("expected lock acquired for proj-1, got %v", locker.acquired)
	}
	if len(locker.released) != 1 || locker.released[0] != "proj-1" {
		t.Errorf("expected lock released for proj-1, got %v", locker.released)
	}
}

func TestCoordinator_DispatchAndAdvance_LockerSkippedForReadonly(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{}}`, 0)

	locker := &mockWorktreeLocker{}
	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
		Locker:       locker,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusVerifying,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Hooks: []projectspec.Hook{
			{ID: "hook-a", On: orchestrator.OnValues{"verifying"}},
		},
	}
	sm := simpleStateMachine()

	_, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if len(locker.acquired) != 0 {
		t.Errorf("expected no lock for readonly, got %v", locker.acquired)
	}
}

func TestCoordinator_DispatchAndAdvance_LockerSkippedForWorktree(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"prompt":"done"}}`, 0)

	locker := &mockWorktreeLocker{}
	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
		Locker:       locker,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Worktree:  true,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Hooks: []projectspec.Hook{
			{ID: "hook-a", On: orchestrator.OnValues{"executing"}},
		},
	}
	sm := simpleStateMachine()

	_, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if len(locker.acquired) != 0 {
		t.Errorf("expected no lock for worktree=true, got %v", locker.acquired)
	}
}

func TestCoordinator_DispatchAndAdvance_GateExitZeroEmptyOutput_NoArtifactInjected(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setGateCompletion("regular-gate", "", 0) // exit 0, empty output

	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "custom-behavior",
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Gates: []projectspec.Gate{
			{
				ID:       "regular-gate",
				On:       orchestrator.OnValues{"executing"},
				Behavior: projectspec.BehaviorValues{"custom-behavior"},
			},
		},
	}
	sm := orchestrator.DefaultMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var payload map[string]json.RawMessage
	json.Unmarshal(result.FinalPayload, &payload)
	if _, ok := payload["artifact"]; ok {
		t.Error("expected no artifact injection for gate with empty output")
	}

	// gate のみ実行の場合（hook なし）は execution_complete は注入されない
	if result.NewStatus != "" {
		t.Errorf("expected no advance for gate-only execution with empty output, got %q", result.NewStatus)
	}
}

func TestCoordinator_DispatchAndAdvance_ExecutionComplete_InjectedOnExitZero(t *testing.T) {
	// hook が exit 0 で完了（成果物なし）→ execution_complete=true が注入され done に遷移
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("main-hook", `{"payload_patch":{}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Hooks: []projectspec.Hook{
			{ID: "main-hook", On: orchestrator.OnValues{"executing"}},
		},
	}
	sm := orchestrator.DefaultMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if !orchestrator.TraitBool(result.FinalPayload, "execution_complete") {
		t.Error("expected execution_complete=true in final payload after exit 0")
	}
	if result.NewStatus != orchestrator.TaskStatusDone {
		t.Errorf("expected new status done (via empty result), got %q", result.NewStatus)
	}
}

func TestCoordinator_DispatchAndAdvance_ExecutionComplete_NotInjectedOnJobFailure(t *testing.T) {
	// hook が exit 1 で失敗 → error が返され execution_complete は注入されない
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("main-hook", ``, 1) // exit code 1

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Hooks: []projectspec.Hook{
			{ID: "main-hook", On: orchestrator.OnValues{"executing"}},
		},
	}
	sm := orchestrator.DefaultMachine()

	_, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err == nil {
		t.Fatal("expected error for failed job (exit code 1)")
	}
}

func TestCoordinator_DispatchEntryGates_NoMatch(t *testing.T) {
	mock := newMockExecutorWaiter()
	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Gates: []projectspec.Gate{
			{ID: "exit-gate", On: projectspec.OnValues{"executing"}, Phase: projectspec.GatePhaseExit},
		},
	}

	result, err := coord.DispatchEntryGates(context.Background(), task, meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(result.Results))
	}
	if string(result.FinalPayload) != string(task.Payload) {
		t.Fatalf("expected payload unchanged, got %s", result.FinalPayload)
	}
}

func TestCoordinator_DispatchEntryGates_SingleGate(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setGateCompletion("fetch-jira", `{"payload_patch":{"artifact":{"jira":"PROJ-1"}}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Gates: []projectspec.Gate{
			{
				ID:    "fetch-jira",
				On:    projectspec.OnValues{"executing"},
				Phase: projectspec.GatePhaseEntry,
				Traits: projectspec.HandlerTraits{
					Produces: []projectspec.TraitType{projectspec.TraitArtifact},
				},
			},
		},
	}

	result, err := coord.DispatchEntryGates(context.Background(), task, meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(result.FinalPayload, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := payload["artifact"]; !ok {
		t.Fatal("expected artifact in final payload")
	}
}

func TestCoordinator_DispatchEntryGates_EmptyOutput(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setGateCompletion("noop-gate", `{"payload_patch":{}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusDone,
		Payload:   json.RawMessage(`{"artifact":"existing"}`),
	}
	meta := &projectspec.ProjectMeta{
		Gates: []projectspec.Gate{
			{
				ID:    "noop-gate",
				On:    projectspec.OnValues{"done"},
				Phase: projectspec.GatePhaseEntry,
				Traits: projectspec.HandlerTraits{
					Produces: []projectspec.TraitType{projectspec.TraitArtifact},
				},
			},
		},
	}

	result, err := coord.DispatchEntryGates(context.Background(), task, meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty payload_patch should not alter existing payload
	if string(result.FinalPayload) != `{"artifact":"existing"}` {
		t.Fatalf("expected payload unchanged, got %s", result.FinalPayload)
	}
}

func TestCoordinator_DispatchEntryGates_ExclusiveCollision(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setGateCompletion("gate-a", `{"payload_patch":{"artifact":"a"}}`, 0)
	mock.setGateCompletion("gate-b", `{"payload_patch":{"artifact":"b"}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Gates: []projectspec.Gate{
			{ID: "gate-a", On: projectspec.OnValues{"executing"}, Phase: projectspec.GatePhaseEntry,
				Traits: projectspec.HandlerTraits{Produces: []projectspec.TraitType{projectspec.TraitArtifact}}},
			{ID: "gate-b", On: projectspec.OnValues{"executing"}, Phase: projectspec.GatePhaseEntry,
				Traits: projectspec.HandlerTraits{Produces: []projectspec.TraitType{projectspec.TraitArtifact}}},
		},
	}

	_, err := coord.DispatchEntryGates(context.Background(), task, meta)
	if err == nil {
		t.Fatal("expected exclusive trait collision error")
	}
}

func TestCoordinator_DispatchAndAdvance_IgnoresEntryGates(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setGateCompletion("entry-gate", `{"payload_patch":{"artifact":"should-not-appear"}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Gates: []projectspec.Gate{
			{ID: "entry-gate", On: projectspec.OnValues{"executing"}, Phase: projectspec.GatePhaseEntry,
				Traits: projectspec.HandlerTraits{Produces: []projectspec.TraitType{projectspec.TraitArtifact}}},
		},
	}
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var payload map[string]json.RawMessage
	json.Unmarshal(result.FinalPayload, &payload)
	if _, ok := payload["artifact"]; ok {
		t.Error("DispatchAndAdvance should not fire entry gates")
	}
}

func TestCoordinator_DispatchAndAdvance_NilLockerOK(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"prompt":"done"}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
		// Locker is nil — should work without error
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		Hooks: []projectspec.Hook{
			{ID: "hook-a", On: orchestrator.OnValues{"executing"}},
		},
	}
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch with nil locker: %v", err)
	}
	if result.NewStatus != orchestrator.TaskStatusDone {
		t.Errorf("expected done, got %q", result.NewStatus)
	}
}
