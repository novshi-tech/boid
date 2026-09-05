package api

import (
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- card-next-step-and-timeline.md §3.2's single-work-slot invariant on
// the direct-CreateTask write port (createExecutionTask, task_create.go) ----
//
// This is the "全書き込み口を棚卸し" port a Go accept (acceptGo,
// workflow_card.go) ALSO funnels through — its own dedicated tests
// (accept_go_test.go) exercise it via a fake TaskCreator that bypasses this
// real code path entirely, so the fulfill-the-reservation exception below
// needs its own coverage here against the actual TaskAppService.CreateTask.

func cardParentWithDetail(id string, detail []byte, openChildCount int) *orchestrator.Task {
	return &orchestrator.Task{
		ID:             id,
		Type:           orchestrator.TaskTypeCard,
		Status:         orchestrator.TaskStatusWorking,
		Card:           &orchestrator.CardAttrs{TaskID: id, Detail: detail},
		OpenChildCount: openChildCount,
	}
}

func TestCreateTask_RejectsWhenCardSlotOccupiedByOpenChild(t *testing.T) {
	parent := cardParentWithDetail("card-1", []byte(`{"children":[{"id":"ch_00","status":"open"}]}`), 0)
	store := &stubTaskStore{
		tasks:    map[string]*orchestrator.Task{"card-1": parent},
		refTasks: map[string]*orchestrator.Task{},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "a new child",
		Behavior:  "dev",
		ParentID:  "card-1",
		Ref:       "some-other-id",
	})
	if err == nil {
		t.Fatal("expected rejection creating a second child under a card whose slot is occupied")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
	if store.createdTask != nil {
		t.Fatal("must not have inserted a task row")
	}
}

func TestCreateTask_RejectsWhenCardSlotOccupiedByLiveTaskRow(t *testing.T) {
	parent := cardParentWithDetail("card-1", nil, 1) // live row, no JSON child at all
	store := &stubTaskStore{
		tasks:    map[string]*orchestrator.Task{"card-1": parent},
		refTasks: map[string]*orchestrator.Task{},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "a new child",
		Behavior:  "dev",
		ParentID:  "card-1",
		Ref:       "some-other-id",
	})
	if err == nil {
		t.Fatal("expected rejection creating a child while a live task row already occupies the slot")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
}

// TestCreateTask_AllowsFulfillingTheSpeccedChildsOwnReservation pins that
// acceptGo's own CreateTask call (Ref: children[i].ID, workflow_card.go)
// is NOT treated as a new occupant — it is completing the reservation the
// specced JSON child itself already holds.
func TestCreateTask_AllowsFulfillingTheSpeccedChildsOwnReservation(t *testing.T) {
	parent := cardParentWithDetail("card-1", []byte(`{"children":[{"id":"ch_00","status":"specced","spec":{"project":"proj-1","behavior":"dev"}}]}`), 0)
	store := &stubTaskStore{
		tasks:    map[string]*orchestrator.Task{"card-1": parent},
		refTasks: map[string]*orchestrator.Task{},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	got, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "do it",
		Behavior:  "dev",
		ParentID:  "card-1",
		Ref:       "ch_00",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want success (fulfills ch_00's own reservation)", err)
	}
	if got == nil || store.createdTask == nil {
		t.Fatal("expected a task to have been created")
	}
}

// TestCreateTask_AllowsWhenCardHasNoOpenSlot is the sanity regression: a
// card with nothing occupying its slot must let a fresh child through
// normally.
func TestCreateTask_AllowsWhenCardHasNoOpenSlot(t *testing.T) {
	parent := cardParentWithDetail("card-1", nil, 0)
	store := &stubTaskStore{
		tasks:    map[string]*orchestrator.Task{"card-1": parent},
		refTasks: map[string]*orchestrator.Task{},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	got, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "first child",
		Behavior:  "dev",
		ParentID:  "card-1",
		Ref:       "ch_00",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want success (empty slot)", err)
	}
	if got == nil {
		t.Fatal("expected a task to have been created")
	}
}

// ---- UpdateTask's own reparenting write port ----
//
// `boid task update <id> --parent-id <card-id>` reparents an EXISTING task —
// neither the child_added gate nor createExecutionTask's own gate runs at
// update time, so this is a distinct write port the invariant audit must
// cover separately.

func TestUpdateTask_RejectsReparentingWhenCardSlotOccupied(t *testing.T) {
	parent := cardParentWithDetail("card-1", []byte(`{"children":[{"id":"ch_00","status":"open"}]}`), 0)
	existing := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeExecution, ProjectID: "p1", Status: orchestrator.TaskStatusPending, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	store := &stubTaskStore{
		task:  existing,
		tasks: map[string]*orchestrator.Task{"t1": existing, "card-1": parent},
	}
	svc := &TaskAppService{Tasks: store}

	newParent := "card-1"
	_, err := svc.UpdateTask("t1", UpdateTaskRequest{ParentID: &newParent})
	if err == nil {
		t.Fatal("expected rejection reparenting under a card whose slot is occupied")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
	if store.updateCalls != 0 {
		t.Errorf("UpdateTask store call count = %d, want 0", store.updateCalls)
	}
}

func TestUpdateTask_AllowsReparentingWhenCardSlotIsFree(t *testing.T) {
	parent := cardParentWithDetail("card-1", nil, 0)
	existing := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeExecution, ProjectID: "p1", Status: orchestrator.TaskStatusPending, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	store := &stubTaskStore{
		task:  existing,
		tasks: map[string]*orchestrator.Task{"t1": existing, "card-1": parent},
	}
	svc := &TaskAppService{Tasks: store}

	newParent := "card-1"
	if _, err := svc.UpdateTask("t1", UpdateTaskRequest{ParentID: &newParent}); err != nil {
		t.Fatalf("UpdateTask() error = %v, want success (empty slot)", err)
	}
	if store.updateCalls != 1 {
		t.Errorf("UpdateTask store call count = %d, want 1", store.updateCalls)
	}
}

func TestUpdateTask_ExecutionParentReparent_NotGated(t *testing.T) {
	parent := &orchestrator.Task{ID: "supervisor-1", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "supervisor"}, OpenChildCount: 1}
	existing := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeExecution, ProjectID: "p1", Status: orchestrator.TaskStatusPending, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	store := &stubTaskStore{
		task:  existing,
		tasks: map[string]*orchestrator.Task{"t1": existing, "supervisor-1": parent},
	}
	svc := &TaskAppService{Tasks: store}

	newParent := "supervisor-1"
	if _, err := svc.UpdateTask("t1", UpdateTaskRequest{ParentID: &newParent}); err != nil {
		t.Fatalf("UpdateTask() error = %v, want success (execution parents are not slot-gated)", err)
	}
}

// TestCreateTask_ExecutionParent_NotGated pins card-next-step-and-timeline.md
// §3.2's own scope limit: "制限は card 直下だけ。execution task の子や並列
// 実行には適用しない。" A supervisor (execution-type parent) with an already
// non-terminal child must NOT block a second, parallel child.
func TestCreateTask_ExecutionParent_NotGated(t *testing.T) {
	parent := &orchestrator.Task{
		ID:             "supervisor-1",
		Type:           orchestrator.TaskTypeExecution,
		Status:         orchestrator.TaskStatusExecuting,
		Exec:           &orchestrator.ExecAttrs{Behavior: "supervisor"},
		OpenChildCount: 1,
	}
	store := &stubTaskStore{
		tasks:    map[string]*orchestrator.Task{"supervisor-1": parent},
		refTasks: map[string]*orchestrator.Task{},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	got, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "parallel child #2",
		Behavior:  "dev",
		ParentID:  "supervisor-1",
		Ref:       "child-2",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want success (execution parents are not slot-gated)", err)
	}
	if got == nil {
		t.Fatal("expected a task to have been created")
	}
}
