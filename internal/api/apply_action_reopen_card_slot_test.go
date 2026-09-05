package api

import (
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- card-next-step-and-timeline.md §3.2's single-work-slot invariant on
// the reopen write port: "作業子を card の履歴から直接 reopen する経路も枠を
// 取得する。別の実行中に再開させない。" ----

// TestApplyAction_Reopen_RejectsWhenCardSlotOccupiedByAnotherLiveChild pins
// the case where a DIFFERENT child of the same card already has a live
// (non-terminal) task row: reopening a terminal sibling must not create a
// second concurrent occupant.
func TestApplyAction_Reopen_RejectsWhenCardSlotOccupiedByAnotherLiveChild(t *testing.T) {
	child := &orchestrator.Task{ID: "child-1", Type: orchestrator.TaskTypeExecution, ProjectID: "p1", ParentID: "card-1", Status: orchestrator.TaskStatusAborted, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	parent := &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}, OpenChildCount: 1}
	txStore := &recordingTxStore{
		task:  child,
		tasks: map[string]*orchestrator.Task{"card-1": parent, "child-1": child},
	}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: child},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	_, err := svc.ApplyAction(humanCtx(), child.ID, ApplyActionRequest{Type: "reopen"})
	if err == nil {
		t.Fatal("expected rejection reopening a child while the card's slot is occupied by another live child")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
	if txStore.updatedTask != nil {
		t.Fatal("child must not have been transitioned to executing")
	}
}

// TestApplyAction_Reopen_RejectsWhenCardSlotOccupiedByOpenJSONChild is the
// JSON-only half: an open/specced child in the card's detail (not yet
// task-ified) also occupies the slot.
func TestApplyAction_Reopen_RejectsWhenCardSlotOccupiedByOpenJSONChild(t *testing.T) {
	child := &orchestrator.Task{ID: "child-1", Type: orchestrator.TaskTypeExecution, ProjectID: "p1", ParentID: "card-1", Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	parent := &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{
		task:   child,
		tasks:  map[string]*orchestrator.Task{"card-1": parent, "child-1": child},
		triage: map[string]*orchestrator.CardAttrs{"card-1": {TaskID: "card-1", Detail: []byte(`{"children":[{"id":"ch_02","status":"open"}]}`)}},
	}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: child},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	_, err := svc.ApplyAction(humanCtx(), child.ID, ApplyActionRequest{Type: "reopen"})
	if err == nil {
		t.Fatal("expected rejection reopening a child while the card has an unresolved open child")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
}

// TestApplyAction_Reopen_AllowedWhenCardSlotIsFree is the sanity regression:
// reopening the ONLY child (the card's slot is otherwise empty) must still
// work normally.
func TestApplyAction_Reopen_AllowedWhenCardSlotIsFree(t *testing.T) {
	child := &orchestrator.Task{ID: "child-1", Type: orchestrator.TaskTypeExecution, ProjectID: "p1", ParentID: "card-1", Status: orchestrator.TaskStatusAborted, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	parent := &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{
		task:  child,
		tasks: map[string]*orchestrator.Task{"card-1": parent, "child-1": child},
	}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: child},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	result, err := svc.ApplyAction(humanCtx(), child.ID, ApplyActionRequest{Type: "reopen"})
	if err != nil {
		t.Fatalf("ApplyAction(reopen): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("status = %q, want executing", result.Task.Status)
	}
}

// TestApplyAction_Reopen_RootTask_NotGated pins the scope limit: a
// parent-less (root) execution task's reopen is never card-slot-gated at
// all (there is no parent to look up).
func TestApplyAction_Reopen_RootTask_NotGated(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeExecution, ProjectID: "p1", Status: orchestrator.TaskStatusAborted, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	result, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "reopen"})
	if err != nil {
		t.Fatalf("ApplyAction(reopen): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("status = %q, want executing", result.Task.Status)
	}
}
