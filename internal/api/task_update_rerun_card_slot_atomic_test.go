package api

// Coverage for the atomic (Tx-wired) path of UpdateTask's reparent gate and
// RerunTask's card-slot gate — the two write ports plan doc
// docs/plans/card-next-step-and-timeline.md §10 flagged as still
// non-transactional after createExecutionTask's own atomicCardCheck (PR-2d-6)
// closed the direct-create gap. Mirrors
// task_create_card_slot_atomic_test.go's real-DB fixture and style;
// task_create_card_slot_test.go's stub-based tests already cover the
// s.Tx == nil fallback these two write ports keep.

import (
	"context"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestUpdateTask_AtomicPath_RejectsReparentWhenGoReservationActive(t *testing.T) {
	taskSvc, goSvc, card, repo := newAtomicCardSlotFixture(t)
	counting := &countingRealTransactor{inner: taskSvc.Tx}
	taskSvc.Tx = counting

	existing := &orchestrator.Task{Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", Status: orchestrator.TaskStatusPending, Ref: "ch_00", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := repo.CreateTask(existing); err != nil {
		t.Fatalf("create existing task: %v", err)
	}

	if _, err := goSvc.reserveGoCardRequest(card.ID); err != nil {
		t.Fatalf("reserveGoCardRequest: %v", err)
	}

	newParent := card.ID
	_, err := taskSvc.UpdateTask(context.Background(), existing.ID, UpdateTaskRequest{ParentID: &newParent})
	if err == nil {
		t.Fatal("expected rejection reparenting under a card whose slot Go already holds")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
	if counting.calls != 1 {
		t.Fatalf("WithinTx calls = %d, want exactly 1 — the re-check and the (rejected) write must share one transaction", counting.calls)
	}

	got, gerr := repo.GetTask(existing.ID)
	if gerr != nil {
		t.Fatalf("GetTask: %v", gerr)
	}
	if got.ParentID != "" {
		t.Fatalf("ParentID = %q, want empty — the rejected reparent must not have been written", got.ParentID)
	}
}

func TestUpdateTask_AtomicPath_SucceedsReparentWhenSlotFree(t *testing.T) {
	taskSvc, _, card, repo := newAtomicCardSlotFixture(t)
	counting := &countingRealTransactor{inner: taskSvc.Tx}
	taskSvc.Tx = counting

	existing := &orchestrator.Task{Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", Status: orchestrator.TaskStatusPending, Ref: "ch_00", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := repo.CreateTask(existing); err != nil {
		t.Fatalf("create existing task: %v", err)
	}

	newParent := card.ID
	if _, err := taskSvc.UpdateTask(context.Background(), existing.ID, UpdateTaskRequest{ParentID: &newParent}); err != nil {
		t.Fatalf("UpdateTask() error = %v, want success (empty slot)", err)
	}
	if counting.calls != 1 {
		t.Fatalf("WithinTx calls = %d, want exactly 1", counting.calls)
	}

	got, gerr := repo.GetTask(existing.ID)
	if gerr != nil {
		t.Fatalf("GetTask: %v", gerr)
	}
	if got.ParentID != card.ID {
		t.Fatalf("ParentID = %q, want %q", got.ParentID, card.ID)
	}
}

func TestRerunTask_AtomicPath_RejectsWhenGoReservationActive(t *testing.T) {
	taskSvc, goSvc, card, repo := newAtomicCardSlotFixture(t)
	counting := &countingRealTransactor{inner: taskSvc.Tx}
	taskSvc.Tx = counting

	task := &orchestrator.Task{Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: card.ID, Ref: "ch_00", Status: orchestrator.TaskStatusAborted, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := repo.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if _, err := goSvc.reserveGoCardRequest(card.ID); err != nil {
		t.Fatalf("reserveGoCardRequest: %v", err)
	}

	_, err := taskSvc.RerunTask(task.ID, RerunTaskRequest{})
	if err == nil {
		t.Fatal("expected rejection rerunning a child while Go already holds the card's slot")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
	if counting.calls != 1 {
		t.Fatalf("WithinTx calls = %d, want exactly 1 — the re-check and the (rejected) write must share one transaction", counting.calls)
	}

	got, gerr := repo.GetTask(task.ID)
	if gerr != nil {
		t.Fatalf("GetTask: %v", gerr)
	}
	if got.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("status = %q, want unchanged aborted — the rejected rerun must not have been written", got.Status)
	}
}

func TestRerunTask_AtomicPath_SucceedsWhenSlotFree(t *testing.T) {
	taskSvc, _, card, repo := newAtomicCardSlotFixture(t)
	counting := &countingRealTransactor{inner: taskSvc.Tx}
	taskSvc.Tx = counting

	task := &orchestrator.Task{Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: card.ID, Ref: "ch_00", Status: orchestrator.TaskStatusAborted, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := repo.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if _, err := taskSvc.RerunTask(task.ID, RerunTaskRequest{}); err != nil {
		t.Fatalf("RerunTask() error = %v, want success (empty slot)", err)
	}
	if counting.calls != 1 {
		t.Fatalf("WithinTx calls = %d, want exactly 1", counting.calls)
	}

	got, gerr := repo.GetTask(task.ID)
	if gerr != nil {
		t.Fatalf("GetTask: %v", gerr)
	}
	if got.Status != orchestrator.TaskStatusPending {
		t.Fatalf("status = %q, want pending", got.Status)
	}
}
