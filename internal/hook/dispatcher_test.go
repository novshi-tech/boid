package hook_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/novshi-tech/boid/internal/hook"
	"github.com/novshi-tech/boid/internal/model"
)

type mockRunner struct {
	calls []*model.HookFireEvent
	err   error
}

func (m *mockRunner) Execute(ctx context.Context, event *model.HookFireEvent) error {
	m.calls = append(m.calls, event)
	return m.err
}

func TestDispatch_CallsRunnerForEachHook(t *testing.T) {
	runner := &mockRunner{}
	disp := &hook.Dispatcher{
		Runner:   runner,
		MaxDepth: 5,
	}

	task := &model.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    model.TaskStatusExecuting,
	}
	hooks := []model.Hook{
		{ID: "hook-a", On: "executing"},
		{ID: "hook-b", On: "executing"},
	}

	err := disp.Dispatch(context.Background(), task, hooks)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 runner calls, got %d", len(runner.calls))
	}
	if runner.calls[0].Hook.ID != "hook-a" {
		t.Fatalf("expected first call hook-a, got %s", runner.calls[0].Hook.ID)
	}
	if runner.calls[1].Hook.ID != "hook-b" {
		t.Fatalf("expected second call hook-b, got %s", runner.calls[1].Hook.ID)
	}
	if runner.calls[0].TaskID != task.ID {
		t.Fatalf("expected task_id %s, got %s", task.ID, runner.calls[0].TaskID)
	}
	if runner.calls[0].ProjectID != "proj-1" {
		t.Fatalf("expected project_id proj-1, got %s", runner.calls[0].ProjectID)
	}
}

func TestDispatch_RunnerError(t *testing.T) {
	runner := &mockRunner{err: fmt.Errorf("execution failed")}
	disp := &hook.Dispatcher{
		Runner:   runner,
		MaxDepth: 5,
	}

	task := &model.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
	}
	hooks := []model.Hook{
		{ID: "hook-a", On: "executing"},
	}

	err := disp.Dispatch(context.Background(), task, hooks)
	if err == nil {
		t.Fatal("expected error from runner")
	}
}

func TestDispatch_DepthLimitReached(t *testing.T) {
	runner := &mockRunner{}
	disp := &hook.Dispatcher{
		Runner:   runner,
		MaxDepth: 0, // immediately hit depth limit
	}

	task := &model.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
	}
	hooks := []model.Hook{
		{ID: "hook-a", On: "executing"},
	}

	err := disp.Dispatch(context.Background(), task, hooks)
	if err == nil {
		t.Fatal("expected depth limit error")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("expected no runner calls when depth limit reached, got %d", len(runner.calls))
	}
}

func TestDispatch_EmptyHooks(t *testing.T) {
	runner := &mockRunner{}
	disp := &hook.Dispatcher{
		Runner:   runner,
		MaxDepth: 5,
	}

	task := &model.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
	}

	err := disp.Dispatch(context.Background(), task, nil)
	if err != nil {
		t.Fatalf("dispatch empty: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("expected 0 runner calls, got %d", len(runner.calls))
	}
}
