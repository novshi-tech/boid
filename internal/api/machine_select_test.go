package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestMachineFor_SidecarRowExists_PicksCardMachine pins machineFor's core
// discriminator (PR-B, docs/plans/suggestion-as-state-transition-impl.md
// §2): a task carrying a task_triage sidecar row is a card, so machineFor
// must return a machine that knows card-vocabulary rules (triage/park/drop/
// etc.) and does NOT know execution-only rules (start/ask/answer/abort).
func TestMachineFor_SidecarRowExists_PicksCardMachine(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusTriaged}
	store := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1"}}}

	sm, err := machineFor(store, task)
	if err != nil {
		t.Fatalf("machineFor: %v", err)
	}
	if !sm.IsManualAction("triage") {
		t.Error("expected the card machine (IsManualAction(\"triage\") = true)")
	}
	if sm.IsManualAction("start") || sm.IsManualAction("abort") {
		t.Error("expected the card machine to NOT know execution-only actions (start/abort)")
	}
}

// TestMachineFor_NoSidecarRow_PicksExecutionMachine is the mirror: an
// ordinary task (no task_triage row — sql.ErrNoRows) gets the execution
// machine.
func TestMachineFor_NoSidecarRow_PicksExecutionMachine(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusExecuting}
	store := &stubTriageStore{} // empty rows: GetTaskTriage returns errNoTriageRow

	sm, err := machineFor(store, task)
	if err != nil {
		t.Fatalf("machineFor: %v", err)
	}
	if !sm.IsManualAction("start") || !sm.IsManualAction("abort") {
		t.Error("expected the execution machine (IsManualAction(\"start\"/\"abort\") = true)")
	}
	if sm.IsManualAction("triage") || sm.IsManualAction("park") {
		t.Error("expected the execution machine to NOT know card-only actions (triage/park)")
	}
}

// TestMachineFor_NilStore_FallsBackToExecutionMachine pins machineFor's own
// documented nil-store fallback (see its doc comment, machine_select.go):
// unlike resolveReopenVariant (which 503s on a nil store, the dangerous
// branch), machineFor falls back to NewExecutionMachine — the overwhelmingly
// common case and the one every pre-PR-B caller effectively assumed.
func TestMachineFor_NilStore_FallsBackToExecutionMachine(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusPending}

	sm, err := machineFor(nil, task)
	if err != nil {
		t.Fatalf("machineFor(nil store): %v", err)
	}
	if !sm.IsManualAction("start") {
		t.Error("expected the execution machine for a nil TaskTriageStore")
	}
}

// TestMachineFor_LookupError_ReturnsServiceUnavailable pins that a GENUINE
// task_triage lookup failure (not sql.ErrNoRows) is indeterminate and must
// not be silently guessed either way — same posture as
// resolveReopenVariant's own equivalent branch (triage_done.go).
func TestMachineFor_LookupError_ReturnsServiceUnavailable(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusDone}
	store := &stubTriageStore{getErr: errors.New("db unavailable")}

	sm, err := machineFor(store, task)
	if sm != nil {
		t.Errorf("expected a nil machine on lookup failure, got %+v", sm)
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if se.Code != http.StatusServiceUnavailable {
		t.Errorf("status code = %d, want %d", se.Code, http.StatusServiceUnavailable)
	}
}

// TestHasTaskTriageRow_SharedBySelectorAndReopenRouting pins that
// hasTaskTriageRow (the primitive machineFor and resolveReopenVariant both
// now share) reports the correct bool/error split across all three of a
// store's possible answers: row exists, sql.ErrNoRows, and a genuine other
// error.
func TestHasTaskTriageRow_SharedBySelectorAndReopenRouting(t *testing.T) {
	t.Run("row exists", func(t *testing.T) {
		store := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1"}}}
		isCard, err := hasTaskTriageRow(store, "t1")
		if err != nil || !isCard {
			t.Fatalf("isCard=%v err=%v, want true/nil", isCard, err)
		}
	})
	t.Run("no row (ErrNoRows)", func(t *testing.T) {
		store := &stubTriageStore{}
		isCard, err := hasTaskTriageRow(store, "t1")
		if err != nil || isCard {
			t.Fatalf("isCard=%v err=%v, want false/nil", isCard, err)
		}
	})
	t.Run("genuine lookup error", func(t *testing.T) {
		store := &stubTriageStore{getErr: errors.New("db unavailable")}
		isCard, err := hasTaskTriageRow(store, "t1")
		if err == nil || isCard {
			t.Fatalf("isCard=%v err=%v, want false/non-nil", isCard, err)
		}
	})
}
