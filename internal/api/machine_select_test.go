package api

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// card-model-cleanup PR-2 (docs/plans/card-model-cleanup.md §3.6) replaced
// machineFor's old sidecar-row lookup (a DB round trip through CardStore,
// with a status-based fallback for a rowless card and a fail-open/
// fail-closed split between write and read call sites — machineForDisplay,
// hasTaskTriageRow, isCardLifecycleStatus) with a direct switch on
// task.Type. That machinery — and the "a card can exist with no confirmed
// task_triage row yet" premise it existed to work around (task_create.go's
// SeedTaskTriage used to be best-effort) — no longer exists as a concept:
// SeedTaskTriage itself is gone, and a card's kind/urgency/wake_at/etc are
// now columns on the SAME tasks row CreateTask inserts, populated the
// instant the row is type='card' (see orchestrator/card.go's package doc
// comment). So every test this file used to carry — machineFor/
// machineForDisplay's fail-open-vs-fail-closed split, hasTaskTriageRow's
// three-way return, the "rowless card" end-to-end recovery tests (PR #986
// review Blockers 1/2), and the GetTaskDetail "TaskTriage lookup failure"
// tolerance tests (TaskAppService/WebAppService.GetTaskDetail no longer call
// TaskTriage at all — see task_service.go/web_service.go's own doc comments
// on machineFor) — tested machinery or scenarios that are structurally
// impossible now. There is nothing left to adapt them into; see git history
// for the pre-PR-2 version of this file.
//
// machineFor is now a pure, total function of task.Type alone (see its own
// doc comment, machine_select.go) — these two tests are what is left to pin.

func TestMachineFor_Card_ReturnsCardMachine(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}

	sm := machineFor(task)
	if sm.Name != orchestrator.CardMachineName {
		t.Fatalf("Name = %q, want %q", sm.Name, orchestrator.CardMachineName)
	}
	if !sm.IsManualAction("go") {
		t.Error("expected the card machine to know the card-only \"go\" verb")
	}
	// "start" is now shared vocabulary (card-next-step-and-timeline.md
	// §3.1's rename of "working") — machine_test.go's own
	// TestCardMachine_HasNoExecutionVocabulary/
	// TestCardMachine_Reopen_ExecutionFromStatusNotHandled pin that it does
	// NOT share the execution machine's FromStatus for it. "abort" remains
	// execution-only.
	if sm.IsManualAction("abort") {
		t.Error("expected the card machine to NOT know execution-only actions (abort)")
	}
}

func TestMachineFor_Execution_ReturnsExecutionMachine(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusPending, Exec: &orchestrator.ExecAttrs{}}

	sm := machineFor(task)
	if sm.Name != orchestrator.ExecutionMachineName {
		t.Fatalf("Name = %q, want %q", sm.Name, orchestrator.ExecutionMachineName)
	}
	if !sm.IsManualAction("start") || !sm.IsManualAction("abort") {
		t.Error("expected the execution machine to know start/abort")
	}
	if sm.IsManualAction("go") || sm.IsManualAction("park") {
		t.Error("expected the execution machine to NOT know card-only actions (go/park)")
	}
}
