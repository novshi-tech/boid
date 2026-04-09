package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- FindDependentTasks テスト ---

func TestFindDependentTasks_NoDependents_ReturnsEmpty(t *testing.T) {
	d := createTestProject(t)

	taskA := &orchestrator.Task{ProjectID: "proj-1", Title: "A", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, taskA); err != nil {
		t.Fatalf("create taskA: %v", err)
	}

	deps, err := orchestrator.FindDependentTasks(d.Conn, taskA.ID)
	if err != nil {
		t.Fatalf("FindDependentTasks() error = %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("expected 0 dependents, got %d", len(deps))
	}
}

func TestFindDependentTasks_ReturnsTasksThatDependOnID(t *testing.T) {
	d := createTestProject(t)

	taskA := &orchestrator.Task{ProjectID: "proj-1", Title: "A", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, taskA); err != nil {
		t.Fatalf("create taskA: %v", err)
	}
	taskB := &orchestrator.Task{ProjectID: "proj-1", Title: "B", Behavior: "dev", DependsOn: []string{taskA.ID}}
	if err := orchestrator.CreateTask(d.Conn, taskB); err != nil {
		t.Fatalf("create taskB: %v", err)
	}

	deps, err := orchestrator.FindDependentTasks(d.Conn, taskA.ID)
	if err != nil {
		t.Fatalf("FindDependentTasks() error = %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dependent, got %d", len(deps))
	}
	if deps[0].ID != taskB.ID {
		t.Fatalf("dependent ID = %q, want %q", deps[0].ID, taskB.ID)
	}
}

func TestFindDependentTasks_OnlyReturnsPendingTasks(t *testing.T) {
	d := createTestProject(t)

	taskA := &orchestrator.Task{ProjectID: "proj-1", Title: "A", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, taskA); err != nil {
		t.Fatalf("create taskA: %v", err)
	}
	// pending dependent
	taskB := &orchestrator.Task{ProjectID: "proj-1", Title: "B (pending)", Behavior: "dev", DependsOn: []string{taskA.ID}}
	if err := orchestrator.CreateTask(d.Conn, taskB); err != nil {
		t.Fatalf("create taskB: %v", err)
	}
	// executing dependent (should NOT be returned)
	taskC := &orchestrator.Task{ProjectID: "proj-1", Title: "C (executing)", Behavior: "dev",
		Status: orchestrator.TaskStatusExecuting, DependsOn: []string{taskA.ID}}
	if err := orchestrator.CreateTask(d.Conn, taskC); err != nil {
		t.Fatalf("create taskC: %v", err)
	}
	// done dependent (should NOT be returned)
	taskD := &orchestrator.Task{ProjectID: "proj-1", Title: "D (done)", Behavior: "dev",
		Status: orchestrator.TaskStatusDone, DependsOn: []string{taskA.ID}}
	if err := orchestrator.CreateTask(d.Conn, taskD); err != nil {
		t.Fatalf("create taskD: %v", err)
	}

	deps, err := orchestrator.FindDependentTasks(d.Conn, taskA.ID)
	if err != nil {
		t.Fatalf("FindDependentTasks() error = %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 pending dependent, got %d: %v", len(deps), deps)
	}
	if deps[0].ID != taskB.ID {
		t.Fatalf("dependent ID = %q, want %q", deps[0].ID, taskB.ID)
	}
}

func TestFindDependentTasks_MultipleDepsBothLoaded(t *testing.T) {
	d := createTestProject(t)

	taskA := &orchestrator.Task{ProjectID: "proj-1", Title: "A", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, taskA); err != nil {
		t.Fatalf("create taskA: %v", err)
	}
	taskB := &orchestrator.Task{ProjectID: "proj-1", Title: "B", Behavior: "dev", DependsOn: []string{taskA.ID}}
	if err := orchestrator.CreateTask(d.Conn, taskB); err != nil {
		t.Fatalf("create taskB: %v", err)
	}
	taskC := &orchestrator.Task{ProjectID: "proj-1", Title: "C", Behavior: "dev", DependsOn: []string{taskA.ID}}
	if err := orchestrator.CreateTask(d.Conn, taskC); err != nil {
		t.Fatalf("create taskC: %v", err)
	}

	deps, err := orchestrator.FindDependentTasks(d.Conn, taskA.ID)
	if err != nil {
		t.Fatalf("FindDependentTasks() error = %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("expected 2 dependents, got %d", len(deps))
	}
	ids := map[string]bool{deps[0].ID: true, deps[1].ID: true}
	if !ids[taskB.ID] || !ids[taskC.ID] {
		t.Fatalf("dependents = %v, want [%s, %s]", deps, taskB.ID, taskC.ID)
	}
}

func TestFindDependentTasks_DependsOnLoaded(t *testing.T) {
	d := createTestProject(t)

	taskA := &orchestrator.Task{ProjectID: "proj-1", Title: "A", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, taskA); err != nil {
		t.Fatalf("create taskA: %v", err)
	}
	taskX := &orchestrator.Task{ProjectID: "proj-1", Title: "X", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, taskX); err != nil {
		t.Fatalf("create taskX: %v", err)
	}
	// taskB depends on BOTH taskA and taskX
	taskB := &orchestrator.Task{ProjectID: "proj-1", Title: "B", Behavior: "dev",
		DependsOn: []string{taskA.ID, taskX.ID}}
	if err := orchestrator.CreateTask(d.Conn, taskB); err != nil {
		t.Fatalf("create taskB: %v", err)
	}

	deps, err := orchestrator.FindDependentTasks(d.Conn, taskA.ID)
	if err != nil {
		t.Fatalf("FindDependentTasks() error = %v", err)
	}
	if len(deps) != 1 || deps[0].ID != taskB.ID {
		t.Fatalf("expected [%s], got %v", taskB.ID, deps)
	}
	// DependsOn should be fully loaded (both taskA and taskX)
	if len(deps[0].DependsOn) != 2 {
		t.Fatalf("DependsOn = %v, want 2 entries", deps[0].DependsOn)
	}
}

// --- 循環依存検出テスト ---

func TestCreateTask_SelfCycle_Error(t *testing.T) {
	d := createTestProject(t)

	// タスクが自分自身に依存する（自己ループ）
	task := &orchestrator.Task{
		ID:        "self-ref-id",
		ProjectID: "proj-1",
		Title:     "Self-referencing task",
		Behavior:  "dev",
		DependsOn: []string{"self-ref-id"},
	}
	err := orchestrator.CreateTask(d.Conn, task)
	if err == nil {
		t.Fatal("CreateTask() error = nil, want cycle error for self-reference")
	}
}

func TestCreateTask_IndirectCycle_Error(t *testing.T) {
	d := createTestProject(t)

	// A → B → C の依存チェーンを作る
	taskA := &orchestrator.Task{ID: "id-a", ProjectID: "proj-1", Title: "A", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, taskA); err != nil {
		t.Fatalf("create taskA: %v", err)
	}
	taskB := &orchestrator.Task{ID: "id-b", ProjectID: "proj-1", Title: "B", Behavior: "dev", DependsOn: []string{"id-a"}}
	if err := orchestrator.CreateTask(d.Conn, taskB); err != nil {
		t.Fatalf("create taskB: %v", err)
	}
	taskC := &orchestrator.Task{ID: "id-c", ProjectID: "proj-1", Title: "C", Behavior: "dev", DependsOn: []string{"id-b"}}
	if err := orchestrator.CreateTask(d.Conn, taskC); err != nil {
		t.Fatalf("create taskC: %v", err)
	}

	// id-a の ID で C に依存するタスクを作ろうとする（A→C→B→A のサイクル）
	cyclicTask := &orchestrator.Task{
		ID:        "id-a", // 既存タスク A と同じ ID
		ProjectID: "proj-1",
		Title:     "Cyclic A",
		Behavior:  "dev",
		DependsOn: []string{"id-c"},
	}
	err := orchestrator.CreateTask(d.Conn, cyclicTask)
	if err == nil {
		t.Fatal("CreateTask() error = nil, want cycle error for A→C→B→A")
	}
	// エラーに "circular" が含まれていること
	if !containsAny(err.Error(), "circular", "cycle") {
		t.Fatalf("expected cycle error, got: %v", err)
	}
}

func TestCreateTask_NoCycle_OK(t *testing.T) {
	d := createTestProject(t)

	taskA := &orchestrator.Task{ID: "id-x", ProjectID: "proj-1", Title: "X", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, taskA); err != nil {
		t.Fatalf("create taskA: %v", err)
	}
	taskB := &orchestrator.Task{ID: "id-y", ProjectID: "proj-1", Title: "Y", Behavior: "dev", DependsOn: []string{"id-x"}}
	if err := orchestrator.CreateTask(d.Conn, taskB); err != nil {
		t.Fatalf("CreateTask() error = %v, want nil for valid deps", err)
	}
	taskC := &orchestrator.Task{ID: "id-z", ProjectID: "proj-1", Title: "Z", Behavior: "dev", DependsOn: []string{"id-y"}}
	if err := orchestrator.CreateTask(d.Conn, taskC); err != nil {
		t.Fatalf("CreateTask() error = %v, want nil for valid chain", err)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
