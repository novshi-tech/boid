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
				Type:        orchestrator.TaskTypeExecution,
				Title:       "hello",
				Status:      orchestrator.TaskStatusExecuting,
				Description: "world",
				Exec: &orchestrator.ExecAttrs{
					Behavior: "dev",
					Readonly: true,
				},
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
			"t1": {ID: "t1", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "executor", Readonly: false}},
			"t2": {ID: "t2", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "supervisor", Readonly: true}},
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
			"t1": {ID: "t1", Type: orchestrator.TaskTypeExecution, Title: "hello", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}},
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
	store := &fieldTaskStore{tasks: map[string]*orchestrator.Task{"t1": {ID: "t1", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}}}}
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
				Type:   orchestrator.TaskTypeExecution,
				Status: orchestrator.TaskStatusExecuting,
				Exec: &orchestrator.ExecAttrs{
					Instructions: orchestrator.Instructions{
						{Agent: "claude-code", Name: "dev", Message: "do it", Model: "claude-sonnet-4-6"},
					},
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
			"t1": {ID: "t1", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusPending, Exec: &orchestrator.ExecAttrs{}},
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
				Type:   orchestrator.TaskTypeExecution,
				Status: orchestrator.TaskStatusExecuting,
				Exec: &orchestrator.ExecAttrs{
					Instructions: orchestrator.Instructions{
						{Agent: "claude-code", Name: "dev", Message: "do it"},
					},
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

// TestGetTaskCurrent_CardTask_ReturnsNilSnapshot pins a behavior change
// card-model-cleanup PR-2 review flagged as untested: orchestrator.
// SnapshotTask (called by GetTaskCurrent, the `boid task current` RPC's data
// source) now returns nil for a card task instead of a TaskSnapshot with
// zero-value Behavior/Readonly — before the ExecAttrs split, Behavior/
// Readonly were flat Task fields that existed (as their zero values) on
// every task regardless of type, so a card previously got a real (if mostly
// empty) snapshot back; SnapshotTask's own doc comment explains this is
// intentional (it's only ever called from a context that already knows
// task.Exec != nil, so degrading to card zero values it "could never
// legitimately produce" would be misleading). No card ever has a runtime
// backing `boid task current` to call it from in the first place — a card
// has no dispatched job — so this is not reachable from a live sandbox, but
// the RPC itself has no type guard of its own, so an external caller
// (or a stray/misrouted call) hitting a card id sees `null` where it used to
// see an object. Pinned here so a future change to that contract is a
// visible, deliberate diff instead of a silent one.
func TestGetTaskCurrent_CardTask_ReturnsNilSnapshot(t *testing.T) {
	store := &fieldTaskStore{
		tasks: map[string]*orchestrator.Task{
			"card-1": {
				ID:     "card-1",
				Type:   orchestrator.TaskTypeCard,
				Title:  "a card",
				Status: orchestrator.TaskStatusWorking,
				Card:   &orchestrator.CardAttrs{},
			},
		},
	}
	svc := &TaskAppService{Tasks: store}

	snap, err := svc.GetTaskCurrent("card-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap != nil {
		t.Errorf("GetTaskCurrent(card) = %+v, want nil (SnapshotTask degrades to nil for a card, not a zero-value snapshot)", snap)
	}

	// GetTaskCurrentField must degrade gracefully (empty string, no error) —
	// resolveMarshaledField(nil, ...) marshals to JSON `null`, and
	// traverseSegments' `case nil` branch returns ("", nil) rather than
	// erroring or panicking.
	got, err := svc.GetTaskCurrentField("card-1", "behavior")
	if err != nil {
		t.Fatalf("GetTaskCurrentField(card, \"behavior\"): unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("GetTaskCurrentField(card, \"behavior\") = %q, want empty string", got)
	}
}
