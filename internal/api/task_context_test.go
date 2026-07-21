package api

import (
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// Phase 5b PR1 (docs/plans/phase5-shim-and-task-context.md): TaskAppService
// gains GetTaskCurrent / GetInstructions (and their *Field counterparts),
// the broker-RPC data sources for `boid task current` / `boid task
// instructions`. Both are live re-derivations from the task row — see
// orchestrator.SnapshotTask / orchestrator.CurrentInstructions doc comments
// for why that's safe without job-scoped context.

func TestGetTaskCurrent_HappyPath(t *testing.T) {
	store := &fieldTaskStore{
		tasks: map[string]*orchestrator.Task{
			"t1": {
				ID:          "t1",
				Title:       "hello",
				Status:      orchestrator.TaskStatusExecuting,
				Behavior:    "dev",
				Description: "world",
				Readonly:    true,
			},
		},
	}
	svc := &TaskAppService{Tasks: store}

	snap, err := svc.GetTaskCurrent("t1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.ID != "t1" || snap.Title != "hello" || snap.Status != "executing" || snap.Behavior != "dev" || snap.Description != "world" || !snap.Readonly {
		t.Errorf("unexpected snapshot: %+v", snap)
	}
}

// TestGetTaskCurrentField_Readonly pins the Phase 5b PR4 addition (docs/plans/
// phase5-shim-and-task-context.md「PR 分割案 > 5b」4): `boid task current
// --field readonly` is the boid-task skill's new mode-determination source,
// replacing the retired environment.yaml `readonly` file read.
func TestGetTaskCurrentField_Readonly(t *testing.T) {
	store := &fieldTaskStore{
		tasks: map[string]*orchestrator.Task{
			"t1": {ID: "t1", Behavior: "executor", Readonly: false},
			"t2": {ID: "t2", Behavior: "supervisor", Readonly: true},
		},
	}
	svc := &TaskAppService{Tasks: store}

	got, err := svc.GetTaskCurrentField("t1", "readonly")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "false" {
		t.Errorf("t1 readonly = %q, want %q", got, "false")
	}

	got, err = svc.GetTaskCurrentField("t2", "readonly")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "true" {
		t.Errorf("t2 readonly = %q, want %q", got, "true")
	}
}

func TestGetTaskCurrent_NotFound(t *testing.T) {
	svc := &TaskAppService{Tasks: &fieldTaskStore{tasks: map[string]*orchestrator.Task{}}}

	_, err := svc.GetTaskCurrent("missing")
	if err == nil {
		t.Fatal("expected error for missing task")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Errorf("error = %v, want *StatusError{Code: 404}", err)
	}
}

func TestGetTaskCurrentField_TopLevel(t *testing.T) {
	store := &fieldTaskStore{
		tasks: map[string]*orchestrator.Task{
			"t1": {ID: "t1", Title: "hello", Status: orchestrator.TaskStatusExecuting, Behavior: "dev"},
		},
	}
	svc := &TaskAppService{Tasks: store}

	got, err := svc.GetTaskCurrentField("t1", "title")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestGetTaskCurrentField_EmptyPath(t *testing.T) {
	store := &fieldTaskStore{tasks: map[string]*orchestrator.Task{"t1": {ID: "t1"}}}
	svc := &TaskAppService{Tasks: store}

	if _, err := svc.GetTaskCurrentField("t1", ""); err == nil {
		t.Fatal("expected error for empty field path")
	}
}

func TestGetTaskCurrentField_UnknownField_ReturnsEmpty(t *testing.T) {
	store := &fieldTaskStore{tasks: map[string]*orchestrator.Task{"t1": {ID: "t1"}}}
	svc := &TaskAppService{Tasks: store}

	got, err := svc.GetTaskCurrentField("t1", "no_such_field")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (missing field is not an error)", got)
	}
}

func TestGetInstructions_ExecutingWithActiveInstruction(t *testing.T) {
	store := &fieldTaskStore{
		tasks: map[string]*orchestrator.Task{
			"t1": {
				ID:     "t1",
				Status: orchestrator.TaskStatusExecuting,
				Instructions: orchestrator.Instructions{
					{Agent: "claude-code", Name: "dev", Message: "do it", Model: "claude-sonnet-4-6"},
				},
			},
		},
	}
	svc := &TaskAppService{Tasks: store}

	got, err := svc.GetInstructions("t1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Agent != "claude-code" || got[0].Message != "do it" {
		t.Errorf("unexpected instructions: %+v", got)
	}
}

func TestGetInstructions_NoActiveInstruction_ReturnsEmptySlice(t *testing.T) {
	store := &fieldTaskStore{
		tasks: map[string]*orchestrator.Task{
			"t1": {ID: "t1", Status: orchestrator.TaskStatusPending},
		},
	}
	svc := &TaskAppService{Tasks: store}

	got, err := svc.GetInstructions("t1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want empty", got)
	}
}

func TestGetInstructions_NotFound(t *testing.T) {
	svc := &TaskAppService{Tasks: &fieldTaskStore{tasks: map[string]*orchestrator.Task{}}}

	_, err := svc.GetInstructions("missing")
	if err == nil {
		t.Fatal("expected error for missing task")
	}
}

func TestGetInstructionsField_Nested(t *testing.T) {
	store := &fieldTaskStore{
		tasks: map[string]*orchestrator.Task{
			"t1": {
				ID:     "t1",
				Status: orchestrator.TaskStatusExecuting,
				Instructions: orchestrator.Instructions{
					{Agent: "claude-code", Name: "dev", Message: "do it"},
				},
			},
		},
	}
	svc := &TaskAppService{Tasks: store}

	// Whole-array field: no dotted path into a specific element (array
	// indices are not object keys, matching ResolveJSONField/ResolveTaskField).
	got, err := svc.GetInstructionsField("t1", "")
	if err == nil {
		t.Fatalf("expected error for empty path, got %q", got)
	}
}
