package api

import (
	"context"
	"database/sql"
	"encoding/json"
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
	triage  map[string]*orchestrator.CardAttrs
	actions map[string][]*orchestrator.Action
	// touchedTaskUpdatedAtIDs records every TouchTaskUpdatedAt(id) call
	// (docs/plans/webui-detail-list-redesign.md PR-3) — the vanished-child
	// child_closed sweep (recordVanishedChildClosedOnParent, queue_sweep.go)
	// bumps the parent's updated_at the same way the direct self-record path
	// (recordChildClosedOnParent, workflow_card.go) does.
	touchedTaskUpdatedAtIDs []string
}

func newSweepFakeStore() *sweepFakeStore {
	return &sweepFakeStore{
		tasks:   map[string]*orchestrator.Task{},
		triage:  map[string]*orchestrator.CardAttrs{},
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
func (s *sweepFakeStore) TouchTaskUpdatedAt(id string) error {
	s.touchedTaskUpdatedAtIDs = append(s.touchedTaskUpdatedAtIDs, id)
	return nil
}
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
func (s *sweepFakeStore) UpsertTaskTriage(tt *orchestrator.CardAttrs) error {
	s.triage[tt.TaskID] = tt
	return nil
}
func (s *sweepFakeStore) GetTaskTriage(taskID string) (*orchestrator.CardAttrs, error) {
	tt, ok := s.triage[taskID]
	if !ok {
		return nil, fmt.Errorf("task_triage not found: %s: %w", taskID, sql.ErrNoRows)
	}
	return tt, nil
}
func (s *sweepFakeStore) ListTaskTriageByTaskIDs(taskIDs []string) (map[string]*orchestrator.CardAttrs, error) {
	out := map[string]*orchestrator.CardAttrs{}
	for _, id := range taskIDs {
		if tt, ok := s.triage[id]; ok {
			out[id] = tt
		}
	}
	return out, nil
}
func (s *sweepFakeStore) DeleteTaskTriage(taskID string) error { delete(s.triage, taskID); return nil }

// ParkedFrom is retained read-only display metadata (card_read.go) — no
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
	store.triage["t1"] = &orchestrator.CardAttrs{TaskID: "t1", WakeAt: &past}

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
	store.triage["t1"] = &orchestrator.CardAttrs{TaskID: "t1", WakeAt: &past}

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
	store.triage["t2"] = &orchestrator.CardAttrs{TaskID: "t2", WakeTaskID: "blocker"}

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
	store.triage["t3"] = &orchestrator.CardAttrs{TaskID: "t3", WakeAt: &future}

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
	store.triage["due"] = &orchestrator.CardAttrs{TaskID: "due", WakeAt: &past}

	store.tasks["not-due"] = &orchestrator.Task{ID: "not-due", Status: orchestrator.TaskStatusParked}
	store.triage["not-due"] = &orchestrator.CardAttrs{TaskID: "not-due", WakeAt: &future}

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
	store.triage["card"] = &orchestrator.CardAttrs{TaskID: "card", WakeTaskID: "child-pr"}

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

// ---- SweepReconcileChildren (PR #987 review, HIGH 8) ----
//
// Restores the self-healing v1's SweepTriage did as a side effect of
// evaluating auto-done every tick — deleting auto-done (決定15) took this
// bookkeeping with it as unintended collateral. This is NOT a card
// transition (task.Status never moves here — only detail.children[].status
// does), so it does not reopen design doc §3.3's "機械遷移ゼロ".

func TestSweepReconcileChildren_ClosesStaleDispatchedChild(t *testing.T) {
	store := newSweepFakeStore()
	store.tasks["card"] = &orchestrator.Task{ID: "card", ParentID: "", Status: orchestrator.TaskStatusWorking}
	store.tasks["child-1"] = &orchestrator.Task{ID: "child-1", ParentID: "card", Status: orchestrator.TaskStatusDone}
	detail := []byte(`{"children":[{"id":"c1","status":"dispatched","task_ref":"child-1"}]}`)
	store.triage["card"] = &orchestrator.CardAttrs{TaskID: "card", Detail: detail}

	svc := &TaskWorkflowService{Tasks: store, TaskTriage: store, Tx: store}
	if err := svc.SweepReconcileChildren(context.Background(), time.Now()); err != nil {
		t.Fatalf("SweepReconcileChildren: %v", err)
	}

	children, err := orchestrator.DetailChildren(store.triage["card"].Detail)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	if len(children) != 1 || children[0].Status != orchestrator.TaskTriageChildStatusClosed {
		t.Fatalf("children = %+v, want the finished child reconciled to closed", children)
	}
	found := false
	for _, a := range store.actions["card"] {
		if a.Type == "child_closed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a child_closed action recorded, got actions=%+v", store.actions["card"])
	}
}

// TestSweepReconcileChildren_VanishedChild_TreatedAsClosed pins the fail-open
// posture toward a GC'd/deleted child task — matching ShouldWake's own
// posture toward a missing reference (v1's reconcileDispatchedChildren
// established this; restoring it unchanged).
// TestSweepReconcileChildren_VanishedChild_TreatedAsClosed pins PR #987
// review round 2's MEDIUM N3: the vanished-child mapping must actually be
// PERSISTED to task_triage, not just mutated on a local in-memory slice that
// the caller discards. Before this fix, reconcileDispatchedChildren's
// vanished-child branch only did `children[i].Status = closed` on its own
// parameter — SweepReconcileChildren never wrote that slice back anywhere,
// so the sweep silently re-derived (and re-discarded) the identical no-op
// result every single tick, forever: a card whose specced child got GC'd
// would show "dispatched" in task_triage.detail permanently, and khi (which
// reads that field, not this function's return value) would never see it as
// closed and never suggest "done" for a card that has, in reality, finished.
func TestSweepReconcileChildren_VanishedChild_TreatedAsClosed(t *testing.T) {
	store := newSweepFakeStore()
	store.tasks["card"] = &orchestrator.Task{ID: "card", Status: orchestrator.TaskStatusWorking}
	// "child-gone" deliberately absent from store.tasks — GetTask returns
	// ErrTaskNotFound, simulating a GC'd child.
	detail := []byte(`{"children":[{"id":"c1","status":"dispatched","task_ref":"child-gone"}]}`)
	store.triage["card"] = &orchestrator.CardAttrs{TaskID: "card", Detail: detail}

	svc := &TaskWorkflowService{Tasks: store, TaskTriage: store, Tx: store}
	if err := svc.SweepReconcileChildren(context.Background(), time.Now()); err != nil {
		t.Fatalf("SweepReconcileChildren: %v", err)
	}

	persisted, ok := store.triage["card"]
	if !ok {
		t.Fatal("expected card's task_triage row to still exist")
	}
	children, cErr := orchestrator.DetailChildren(persisted.Detail)
	if cErr != nil {
		t.Fatalf("DetailChildren: %v", cErr)
	}
	if len(children) != 1 || children[0].Status != orchestrator.TaskTriageChildStatusClosed {
		t.Fatalf("persisted children = %+v, want c1 status=closed", children)
	}

	var found *orchestrator.Action
	for _, a := range store.actions["card"] {
		if a.Type == "child_closed" {
			found = a
			break
		}
	}
	if found == nil {
		t.Fatalf("expected a child_closed action recorded on parent %q, got actions: %+v", "card", store.actions["card"])
	}
	// Payload key must match recordChildClosedOnParent's own child_closed
	// payload exactly ("child_id", not "child_task_id") — coordinator review:
	// a future consumer parsing child_closed by key must not silently miss
	// vanished-child rows because they alone spelled the same value under a
	// different key.
	var payload struct {
		ChildID     string `json:"child_id"`
		ChildStatus string `json:"child_status"`
	}
	if err := json.Unmarshal(found.Payload, &payload); err != nil {
		t.Fatalf("unmarshal child_closed payload: %v", err)
	}
	if payload.ChildID != "child-gone" {
		t.Fatalf(`child_closed payload "child_id" = %q, want "child-gone" (must match recordChildClosedOnParent's own key)`, payload.ChildID)
	}
	if payload.ChildStatus != "vanished" {
		t.Fatalf(`child_closed payload "child_status" = %q, want "vanished"`, payload.ChildStatus)
	}
}

// TestSweepReconcileChildren_NoTaskTriageRow_SkippedNotErrored mirrors
// SweepWake's own posture: a working task with no sidecar row is not a
// triage card at all.
func TestSweepReconcileChildren_NoTaskTriageRow_SkippedNotErrored(t *testing.T) {
	store := newSweepFakeStore()
	store.tasks["ordinary"] = &orchestrator.Task{ID: "ordinary", Status: orchestrator.TaskStatusWorking}

	svc := &TaskWorkflowService{Tasks: store, TaskTriage: store, Tx: store}
	if err := svc.SweepReconcileChildren(context.Background(), time.Now()); err != nil {
		t.Fatalf("SweepReconcileChildren: %v", err)
	}
}

// ---- QueueSweepLoop.runOnce (PR #987 review, HIGH 8/MEDIUM 9) ----

// fakeQueueSweepStore records calls to both sweeps so runOnce's coordination
// (both fire every tick; a SweepWake failure does not block
// SweepReconcileChildren) can be pinned without a real TaskWorkflowService.
type fakeQueueSweepStore struct {
	wakeCalls      int
	reconcileCalls int
	wakeErr        error
	reconcileErr   error
}

func (f *fakeQueueSweepStore) SweepWake(ctx context.Context, now time.Time) ([]string, error) {
	f.wakeCalls++
	return nil, f.wakeErr
}

func (f *fakeQueueSweepStore) SweepReconcileChildren(ctx context.Context, now time.Time) error {
	f.reconcileCalls++
	return f.reconcileErr
}

func TestQueueSweepLoop_RunOnce_CallsBothSweeps(t *testing.T) {
	store := &fakeQueueSweepStore{}
	loop := &QueueSweepLoop{Store: store}
	loop.runOnce(context.Background())

	if store.wakeCalls != 1 {
		t.Errorf("SweepWake calls = %d, want 1", store.wakeCalls)
	}
	if store.reconcileCalls != 1 {
		t.Errorf("SweepReconcileChildren calls = %d, want 1", store.reconcileCalls)
	}
}

// TestQueueSweepLoop_RunOnce_WakeFailureDoesNotStopReconcile pins that the
// two sweeps are independent: SweepReconcileChildren is bookkeeping over
// detail.children[].status, unrelated to whatever SweepWake failed on.
func TestQueueSweepLoop_RunOnce_WakeFailureDoesNotStopReconcile(t *testing.T) {
	store := &fakeQueueSweepStore{wakeErr: fmt.Errorf("boom")}
	loop := &QueueSweepLoop{Store: store}
	loop.runOnce(context.Background())

	if store.reconcileCalls != 1 {
		t.Errorf("SweepReconcileChildren calls = %d, want 1 (must still run after a SweepWake failure)", store.reconcileCalls)
	}
}
