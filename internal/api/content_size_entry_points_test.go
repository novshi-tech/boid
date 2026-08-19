package api

// docs/plans/ingestion-identity.md PR-2 (B-2), J-10/A-5: pins that the
// description / action payload size cap is enforced at 3 of the 4 mandatory
// entry points that live in this package — task_create, task_update, and
// action_send (TaskWorkflowService.ApplyAction). The 4th entry point,
// BoidOpTaskResolveOrCapture, is pinned in task_resolve_or_capture_test.go.
//
// Each test also asserts the oversized write never reached the store (no
// truncated row left behind) — J-10's "切り詰めずにエラーにする" half of the
// contract, not just "an error came back".

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func oversizedContent() string {
	return strings.Repeat("a", orchestrator.MaxContentBytes+1)
}

func TestCreateTask_DescriptionOverLimit_RejectsAndDoesNotCreate(t *testing.T) {
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta: stubMetaStore{meta: &orchestrator.ProjectMeta{
			DefaultTaskBehavior: "dev",
			TaskBehaviors:       map[string]orchestrator.TaskBehavior{"dev": {}},
		}},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:   "proj-1",
		Title:       "too big",
		Description: oversizedContent(),
	})
	if err == nil {
		t.Fatal("CreateTask() with an oversized description = nil error, want a rejection")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected StatusBadRequest, got %v", err)
	}
	if store.createdTask != nil {
		t.Error("an oversized description must not reach the store — no truncated row left behind")
	}
}

func TestUpdateTask_DescriptionOverLimit_RejectsAndDoesNotWrite(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "proj-1", Status: orchestrator.TaskStatusPending, Description: "original"}
	store := &stubTaskStore{task: task}
	svc := &TaskAppService{Tasks: store}

	_, err := svc.UpdateTask("t1", UpdateTaskRequest{Description: oversizedContent()})
	if err == nil {
		t.Fatal("UpdateTask() with an oversized description = nil error, want a rejection")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected StatusBadRequest, got %v", err)
	}
	if store.updateCalls != 0 {
		t.Errorf("UpdateTask store call count = %d, want 0 (oversized description must never reach the store)", store.updateCalls)
	}
	if task.Description != "original" {
		t.Errorf("task.Description mutated to a partial/truncated value: %q", task.Description)
	}
}

func TestApplyAction_PayloadOverLimit_RejectsAndDoesNotRecord(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusCaptured, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	oversized := []byte(`{"note":"` + oversizedContent() + `"}`)
	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "triage", Payload: oversized})
	if err == nil {
		t.Fatal("ApplyAction() with an oversized payload = nil error, want a rejection")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected StatusBadRequest, got %v", err)
	}
	if len(txStore.actions) != 0 {
		t.Errorf("actions recorded = %d, want 0 (oversized payload must never reach CreateAction)", len(txStore.actions))
	}
	if task.Status != orchestrator.TaskStatusCaptured {
		t.Errorf("task status changed to %q despite the rejected action", task.Status)
	}
}
