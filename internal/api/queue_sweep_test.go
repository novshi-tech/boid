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
	tasks   map[string]*orchestrator.Task
	triage  map[string]*orchestrator.TaskTriage
	actions map[string][]*orchestrator.Action
}

func newSweepFakeStore() *sweepFakeStore {
	return &sweepFakeStore{
		tasks:   map[string]*orchestrator.Task{},
		triage:  map[string]*orchestrator.TaskTriage{},
		actions: map[string][]*orchestrator.Action{},
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
func (s *sweepFakeStore) SeedTaskTriage(taskID string) error {
	if s.triage == nil {
		s.triage = map[string]*orchestrator.TaskTriage{}
	}
	if _, ok := s.triage[taskID]; !ok {
		s.triage[taskID] = &orchestrator.TaskTriage{TaskID: taskID}
	}
	return nil
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
func (s *sweepFakeStore) ListTaskTriageByTaskIDs(taskIDs []string) (map[string]*orchestrator.TaskTriage, error) {
	out := map[string]*orchestrator.TaskTriage{}
	for _, id := range taskIDs {
		if tt, ok := s.triage[id]; ok {
			out[id] = tt
		}
	}
	return out, nil
}
func (s *sweepFakeStore) DeleteTaskTriage(taskID string) error { delete(s.triage, taskID); return nil }

// ParkedFrom is retained read-only display metadata (triage_read.go) — no
// queue-sweep test needs it since Wake/ParkedFrom-based resolution no longer
// exists (v2's card machine has exactly one park origin, working).
func (s *sweepFakeStore) ParkedFrom(taskID string) (orchestrator.TaskStatus, error) {
	return "", fmt.Errorf("parked_from not found: %s", taskID)
}
func (s *sweepFakeStore) GetJob(id string) (*Job, error)               { return nil, fmt.Errorf("not implemented") }
func (s *sweepFakeStore) ListJobsByTask(taskID string) ([]*Job, error) { return nil, nil }
func (s *sweepFakeStore) UpdateJob(job *Job) error                     { return nil }

// LinkIdentity / UnlinkIdentity / UnlinkAllForTask / ResolveIdentity /
// ListIdentitiesByTask: no queue-sweep test exercises the identity index —
// trivial no-ops, kept only to satisfy TxStore.
func (s *sweepFakeStore) LinkIdentity(projectID, identity, taskID string) error { return nil }
func (s *sweepFakeStore) UnlinkIdentity(projectID, identity string) error       { return nil }
func (s *sweepFakeStore) UnlinkAllForTask(taskID string) error                  { return nil }
func (s *sweepFakeStore) ResolveIdentity(projectID, identity string) (*orchestrator.Task, error) {
	return nil, orchestrator.ErrTaskNotFound
}
func (s *sweepFakeStore) ListIdentitiesByTask(taskID string) ([]string, error) { return nil, nil }

func (s *sweepFakeStore) WithinTx(fn func(TxStore) error) error {
	return fn(s)
}

// ---- card machine v2 (docs/plans/suggestion-as-state-transition-impl.md
// §3.4, §C): SweepWake no longer resolves a park origin or applies any
// transition — it records a "wake_due" fact and consumes the wake condition
// (clears WakeAt/WakeTaskID) so the same condition cannot fire again next
// tick. The task's status is UNCHANGED by every test below; that is the
// point, not an oversight. ----

func TestSweepWake_DateCondition_RecordsWakeDue(t *testing.T) {
	store := newSweepFakeStore()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)

	store.tasks["t1"] = &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusParked}
	store.triage["t1"] = &orchestrator.TaskTriage{TaskID: "t1", WakeAt: &past}

	svc := &TaskWorkflowService{Tasks: store, TaskTriage: store, Tx: store}
	woken, err := svc.SweepWake(context.Background(), now)
	if err != nil {
		t.Fatalf("SweepWake: %v", err)
	}
	if len(woken) != 1 || woken[0] != "t1" {
		t.Fatalf("woken = %v, want [t1]", woken)
	}
	if store.tasks["t1"].Status != orchestrator.TaskStatusParked {
		t.Errorf("t1 status = %s, want still parked (wake_due never transitions)", store.tasks["t1"].Status)
	}
	if tt := store.triage["t1"]; tt.WakeAt != nil {
		t.Error("WakeAt must be cleared once wake_due is recorded (consumes the condition — otherwise it fires every tick forever)")
	}
}

// TestSweepWake_StampsActorDaemon verifies the machine-driven wake sweep
// (決定12, no human in the loop) records ActorDaemon on the resulting
// wake_due action.
func TestSweepWake_StampsActorDaemon(t *testing.T) {
	store := newSweepFakeStore()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)

	store.tasks["t1"] = &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusParked}
	store.triage["t1"] = &orchestrator.TaskTriage{TaskID: "t1", WakeAt: &past}

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
	if actions[0].Type != "wake_due" {
		t.Errorf("action type = %q, want wake_due", actions[0].Type)
	}
	if actions[0].Actor != orchestrator.ActorDaemon {
		t.Errorf("actor = %q, want %q", actions[0].Actor, orchestrator.ActorDaemon)
	}
}

func TestSweepWake_TaskCondition_RecordsWakeDueWhenReferencedTaskTerminal(t *testing.T) {
	store := newSweepFakeStore()
	now := time.Now()

	store.tasks["blocker"] = &orchestrator.Task{ID: "blocker", Status: orchestrator.TaskStatusDone}
	store.tasks["t2"] = &orchestrator.Task{ID: "t2", Status: orchestrator.TaskStatusParked}
	store.triage["t2"] = &orchestrator.TaskTriage{TaskID: "t2", WakeTaskID: "blocker"}

	svc := &TaskWorkflowService{Tasks: store, TaskTriage: store, Tx: store}
	woken, err := svc.SweepWake(context.Background(), now)
	if err != nil {
		t.Fatalf("SweepWake: %v", err)
	}
	if len(woken) != 1 || woken[0] != "t2" {
		t.Fatalf("woken = %v, want [t2]", woken)
	}
	if store.tasks["t2"].Status != orchestrator.TaskStatusParked {
		t.Errorf("t2 status = %s, want still parked", store.tasks["t2"].Status)
	}
	if tt := store.triage["t2"]; tt.WakeTaskID != "" {
		t.Error("WakeTaskID must be cleared once wake_due is recorded")
	}
}

func TestSweepWake_NotYetDue_LeavesParked(t *testing.T) {
	store := newSweepFakeStore()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)

	store.tasks["t3"] = &orchestrator.Task{ID: "t3", Status: orchestrator.TaskStatusParked}
	store.triage["t3"] = &orchestrator.TaskTriage{TaskID: "t3", WakeAt: &future}

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
	if tt := store.triage["t3"]; tt.WakeAt == nil {
		t.Error("WakeAt must NOT be cleared when the condition has not fired yet")
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

	store.tasks["not-due"] = &orchestrator.Task{ID: "not-due", Status: orchestrator.TaskStatusParked}
	store.triage["not-due"] = &orchestrator.TaskTriage{TaskID: "not-due", WakeAt: &future}

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

// TestSweepWake_WorkingOriginPark_RecordsWakeDue_NoTransition is the v2
// mirror of the old BD-9 regression pin: a card parked FROM working with a
// wake_task_id (the sequential-PR-consumption pattern — Go-time park while a
// child is dispatched, re-surfaced when that child terminates) records
// wake_due and stays parked. There is no "origin" to resolve or misroute
// anymore — v2's card machine has exactly one park rule (working→parked), so
// there is nothing left for a fourth origin to break (see machine_card.go's
// TestCardMachineV2_JobFailed_NotRegistered and the doc comment on
// NewCardMachine for why the whole wake_triaged/wake_ready/wake_working
// three-way split is gone).
func TestSweepWake_WorkingOriginPark_RecordsWakeDue_NoTransition(t *testing.T) {
	store := newSweepFakeStore()
	now := time.Now()

	store.tasks["child-pr"] = &orchestrator.Task{ID: "child-pr", Status: orchestrator.TaskStatusDone}
	store.tasks["card"] = &orchestrator.Task{ID: "card", Status: orchestrator.TaskStatusParked}
	store.triage["card"] = &orchestrator.TaskTriage{TaskID: "card", WakeTaskID: "child-pr"}

	svc := &TaskWorkflowService{Tasks: store, TaskTriage: store, Tx: store}
	woken, err := svc.SweepWake(context.Background(), now)
	if err != nil {
		t.Fatalf("SweepWake: %v", err)
	}
	if len(woken) != 1 || woken[0] != "card" {
		t.Fatalf("woken = %v, want [card]", woken)
	}
	if store.tasks["card"].Status != orchestrator.TaskStatusParked {
		t.Errorf("card status = %s, want still parked (no transition)", store.tasks["card"].Status)
	}
	actions := store.actions["card"]
	if len(actions) != 1 {
		t.Fatalf("expected exactly 1 recorded action (wake_due only), got %d: %v", len(actions), actions)
	}
	if actions[0].Type != "wake_due" {
		t.Errorf("action type = %q, want wake_due", actions[0].Type)
	}
}
