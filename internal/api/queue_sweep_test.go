package api

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// sweepFakeStore is a minimal in-memory TxStore fake supporting multiple
// tasks (unlike recordingTxStore in apply_action_phase1_test.go, which only
// tracks a single task) — SweepWake needs to list N parked tasks and, for
// each, look up an arbitrary wake_task_id target.
type sweepFakeStore struct {
	tasks       map[string]*orchestrator.Task
	triage      map[string]*orchestrator.TaskTriage
	actions     map[string][]*orchestrator.Action
	parkedFroms map[string]orchestrator.TaskStatus
}

func newSweepFakeStore() *sweepFakeStore {
	return &sweepFakeStore{
		tasks:       map[string]*orchestrator.Task{},
		triage:      map[string]*orchestrator.TaskTriage{},
		actions:     map[string][]*orchestrator.Action{},
		parkedFroms: map[string]orchestrator.TaskStatus{},
	}
}

func (s *sweepFakeStore) CreateTask(task *orchestrator.Task) error { return nil }
func (s *sweepFakeStore) GetTask(id string) (*orchestrator.Task, error) {
	t, ok := s.tasks[id]
	if !ok {
		return nil, orchestrator.ErrTaskNotFound
	}
	return t, nil
}
func (s *sweepFakeStore) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	var out []*orchestrator.Task
	for _, t := range s.tasks {
		if filter.Status != "" && string(t.Status) != filter.Status {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}
func (s *sweepFakeStore) UpdateTask(task *orchestrator.Task) error {
	s.tasks[task.ID] = task
	return nil
}
func (s *sweepFakeStore) DeleteTask(id string) error { delete(s.tasks, id); return nil }
func (s *sweepFakeStore) FindTaskByRemote(remoteID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *sweepFakeStore) FindTaskByRef(ref, parentID, projectID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *sweepFakeStore) ListChildren(parentID string) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *sweepFakeStore) CreateAction(action *orchestrator.Action) error {
	s.actions[action.TaskID] = append(s.actions[action.TaskID], action)
	return nil
}
func (s *sweepFakeStore) ListActionsByTask(taskID string) ([]*orchestrator.Action, error) {
	return s.actions[taskID], nil
}
func (s *sweepFakeStore) UpsertTaskTriage(tt *orchestrator.TaskTriage) error {
	s.triage[tt.TaskID] = tt
	return nil
}
func (s *sweepFakeStore) GetTaskTriage(taskID string) (*orchestrator.TaskTriage, error) {
	tt, ok := s.triage[taskID]
	if !ok {
		return nil, fmt.Errorf("task_triage not found: %s: %w", taskID, sql.ErrNoRows)
	}
	return tt, nil
}
func (s *sweepFakeStore) DeleteTaskTriage(taskID string) error { delete(s.triage, taskID); return nil }
func (s *sweepFakeStore) ParkedFrom(taskID string) (orchestrator.TaskStatus, error) {
	from, ok := s.parkedFroms[taskID]
	if !ok {
		return "", fmt.Errorf("parked_from not found: %s", taskID)
	}
	return from, nil
}
func (s *sweepFakeStore) GetJob(id string) (*Job, error)               { return nil, fmt.Errorf("not implemented") }
func (s *sweepFakeStore) ListJobsByTask(taskID string) ([]*Job, error) { return nil, nil }
func (s *sweepFakeStore) UpdateJob(job *Job) error                     { return nil }

func (s *sweepFakeStore) WithinTx(fn func(TxStore) error) error {
	return fn(s)
}

func TestSweepWake_DateCondition_WakesToOrigin(t *testing.T) {
	store := newSweepFakeStore()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)

	store.tasks["t1"] = &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusParked}
	store.triage["t1"] = &orchestrator.TaskTriage{TaskID: "t1", WakeAt: &past}
	store.parkedFroms["t1"] = orchestrator.TaskStatusTriaged

	svc := &TaskWorkflowService{Tasks: store, TaskTriage: store, Tx: store}
	woken, err := svc.SweepWake(context.Background(), now)
	if err != nil {
		t.Fatalf("SweepWake: %v", err)
	}
	if len(woken) != 1 || woken[0] != "t1" {
		t.Fatalf("woken = %v, want [t1]", woken)
	}
	if store.tasks["t1"].Status != orchestrator.TaskStatusTriaged {
		t.Errorf("t1 status = %s, want triaged (its recorded park origin)", store.tasks["t1"].Status)
	}
}

// TestSweepWake_StampsActorDaemon verifies the machine-driven wake sweep
// (決定12, no human in the loop) records ActorDaemon on the resulting
// wake_triaged/wake_ready action — this is the automatic counterpart to a
// human pressing Wake (web_service.go, ActorHuman), and 論点11 depends on the
// two never being confused.
func TestSweepWake_StampsActorDaemon(t *testing.T) {
	store := newSweepFakeStore()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)

	store.tasks["t1"] = &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusParked}
	store.triage["t1"] = &orchestrator.TaskTriage{TaskID: "t1", WakeAt: &past}
	store.parkedFroms["t1"] = orchestrator.TaskStatusTriaged

	svc := &TaskWorkflowService{Tasks: store, TaskTriage: store, Tx: store}
	woken, err := svc.SweepWake(context.Background(), now)
	if err != nil {
		t.Fatalf("SweepWake: %v", err)
	}
	if len(woken) != 1 {
		t.Fatalf("woken = %v, want 1 task", woken)
	}
	actions := store.actions["t1"]
	if len(actions) != 1 {
		t.Fatalf("expected 1 recorded action, got %d", len(actions))
	}
	if actions[0].Actor != orchestrator.ActorDaemon {
		t.Errorf("actor = %q, want %q", actions[0].Actor, orchestrator.ActorDaemon)
	}
}

func TestSweepWake_TaskCondition_WakesWhenReferencedTaskTerminal(t *testing.T) {
	store := newSweepFakeStore()
	now := time.Now()

	store.tasks["blocker"] = &orchestrator.Task{ID: "blocker", Status: orchestrator.TaskStatusDone}
	store.tasks["t2"] = &orchestrator.Task{ID: "t2", Status: orchestrator.TaskStatusParked}
	store.triage["t2"] = &orchestrator.TaskTriage{TaskID: "t2", WakeTaskID: "blocker"}
	store.parkedFroms["t2"] = orchestrator.TaskStatusReady

	svc := &TaskWorkflowService{Tasks: store, TaskTriage: store, Tx: store}
	woken, err := svc.SweepWake(context.Background(), now)
	if err != nil {
		t.Fatalf("SweepWake: %v", err)
	}
	if len(woken) != 1 || woken[0] != "t2" {
		t.Fatalf("woken = %v, want [t2]", woken)
	}
	// t2 was parked FROM ready, so Wake resolves it back to ready and then
	// immediately chains into Dispatch (ready->working, same as
	// ApplyAction("ready") — see Wake's own doc comment) since this fake
	// store's Tasks/TaskTriage/Tx all share the same map and so correctly
	// observe the just-committed ready status, unlike a stale test double.
	if store.tasks["t2"].Status != orchestrator.TaskStatusWorking {
		t.Errorf("t2 status = %s, want working (wake_ready chains into Dispatch)", store.tasks["t2"].Status)
	}
}

func TestSweepWake_NotYetDue_LeavesParked(t *testing.T) {
	store := newSweepFakeStore()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)

	store.tasks["t3"] = &orchestrator.Task{ID: "t3", Status: orchestrator.TaskStatusParked}
	store.triage["t3"] = &orchestrator.TaskTriage{TaskID: "t3", WakeAt: &future}
	store.parkedFroms["t3"] = orchestrator.TaskStatusTriaged

	svc := &TaskWorkflowService{Tasks: store, TaskTriage: store, Tx: store}
	woken, err := svc.SweepWake(context.Background(), now)
	if err != nil {
		t.Fatalf("SweepWake: %v", err)
	}
	if len(woken) != 0 {
		t.Fatalf("woken = %v, want none", woken)
	}
	if store.tasks["t3"].Status != orchestrator.TaskStatusParked {
		t.Errorf("t3 status = %s, want still parked", store.tasks["t3"].Status)
	}
}

func TestSweepWake_NoTaskTriageRow_SkippedNotErrored(t *testing.T) {
	store := newSweepFakeStore()
	store.tasks["t4"] = &orchestrator.Task{ID: "t4", Status: orchestrator.TaskStatusParked}
	// deliberately no task_triage row for t4

	svc := &TaskWorkflowService{Tasks: store, TaskTriage: store, Tx: store}
	woken, err := svc.SweepWake(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepWake: %v", err)
	}
	if len(woken) != 0 {
		t.Fatalf("woken = %v, want none", woken)
	}
}

func TestSweepWake_MultipleParkedTasks_EachEvaluatedIndependently(t *testing.T) {
	store := newSweepFakeStore()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	store.tasks["due"] = &orchestrator.Task{ID: "due", Status: orchestrator.TaskStatusParked}
	store.triage["due"] = &orchestrator.TaskTriage{TaskID: "due", WakeAt: &past}
	store.parkedFroms["due"] = orchestrator.TaskStatusTriaged

	store.tasks["not-due"] = &orchestrator.Task{ID: "not-due", Status: orchestrator.TaskStatusParked}
	store.triage["not-due"] = &orchestrator.TaskTriage{TaskID: "not-due", WakeAt: &future}
	store.parkedFroms["not-due"] = orchestrator.TaskStatusTriaged

	store.tasks["irrelevant"] = &orchestrator.Task{ID: "irrelevant", Status: orchestrator.TaskStatusExecuting}

	svc := &TaskWorkflowService{Tasks: store, TaskTriage: store, Tx: store}
	woken, err := svc.SweepWake(context.Background(), now)
	if err != nil {
		t.Fatalf("SweepWake: %v", err)
	}
	if len(woken) != 1 || woken[0] != "due" {
		t.Fatalf("woken = %v, want [due]", woken)
	}
	if store.tasks["not-due"].Status != orchestrator.TaskStatusParked {
		t.Errorf("not-due status = %s, want still parked", store.tasks["not-due"].Status)
	}
}
