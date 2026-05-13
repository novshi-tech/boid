package orchestrator_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

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

// The coordinator must NOT take any project-level lock anymore — the workflow
// service now owns the executing-lifetime lock (see internal/api/service.go).
// These tests guard against accidental regressions where someone reintroduces
// inner locking inside DispatchAndAdvance.
func TestCoordinator_DispatchAndAdvance_DoesNotAcquireWorktreeLock(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"prompt":"done"}}`, 0)

	locker := &mockWorktreeLocker{}
	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
		Locker:       locker, // deprecated; must be ignored by the coordinator
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{
		{ID: "hook-a"},
	}, nil)
	sm := simpleStateMachine()

	_, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if len(locker.acquired) != 0 {
		t.Errorf("expected coordinator NOT to acquire any lock, got %v", locker.acquired)
	}
	if len(locker.released) != 0 {
		t.Errorf("expected coordinator NOT to release any lock, got %v", locker.released)
	}
}

func TestCoordinator_DispatchAndAdvance_DoesNotAcquireForWorktreeTask(t *testing.T) {
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
	meta := metaWithBehavior([]projectspec.Hook{
		{ID: "hook-a"},
	}, nil)
	sm := simpleStateMachine()

	_, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if len(locker.acquired) != 0 {
		t.Errorf("expected no lock for worktree=true, got %v", locker.acquired)
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
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{
		{ID: "hook-a"},
	}, nil)
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch with nil locker: %v", err)
	}
	if result.NewStatus != orchestrator.TaskStatusDone {
		t.Errorf("expected done, got %q", result.NewStatus)
	}
}
