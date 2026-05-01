package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestListGatesForStatus_ReturnsMatchingGates(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "task-list-1",
		Behavior: "dev",
		Status:   orchestrator.TaskStatusExecuting,
		Payload:  json.RawMessage(`{}`),
	}
	meta := metaWithBehavior(nil, []projectspec.Gate{
		{ID: "entry-gate", Phase: projectspec.GatePhaseEntry},
		{ID: "exit-gate", Phase: projectspec.GatePhaseExit},
	})

	gates := orchestrator.ListGatesForStatus(meta, task, orchestrator.TaskStatusExecuting)
	if len(gates) != 2 {
		t.Fatalf("expected 2 gates for executing, got %d", len(gates))
	}
	ids := map[string]bool{}
	for _, g := range gates {
		ids[g.ID] = true
	}
	if !ids["entry-gate"] || !ids["exit-gate"] {
		t.Errorf("expected entry-gate and exit-gate, got %v", ids)
	}
}

func TestListGatesForStatus_NoBehavior(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "task-list-2",
		Behavior: "unknown",
		Status:   orchestrator.TaskStatusExecuting,
		Payload:  json.RawMessage(`{}`),
	}
	meta := metaWithBehavior(nil, nil)

	gates := orchestrator.ListGatesForStatus(meta, task, orchestrator.TaskStatusExecuting)
	if len(gates) != 0 {
		t.Errorf("expected 0 gates for unknown behavior, got %d", len(gates))
	}
}
