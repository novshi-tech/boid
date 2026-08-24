package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// fakeTaskCreator is a minimal TaskCreator stub for TestTaskWorkflowService_Dispatch*.
// It mimics TaskAppService.CreateTask's own (ref, parent_id) get-or-create
// dedup (task_create.go) — a call whose Ref+ParentID match a task it already
// created returns that SAME task instead of minting a new one — so tests can
// exercise Dispatch's replay-safety (codex review round 2, Major).
type fakeTaskCreator struct {
	createFn func(req CreateTaskRequest) (*orchestrator.Task, error)
	calls    []CreateTaskRequest
	byRef    map[string]*orchestrator.Task // "ref:parentID" -> previously created task
}

func (f *fakeTaskCreator) CreateTask(req CreateTaskRequest) (*orchestrator.Task, error) {
	f.calls = append(f.calls, req)
	if req.Ref != "" && req.ParentID != "" {
		if f.byRef == nil {
			f.byRef = map[string]*orchestrator.Task{}
		}
		key := req.Ref + ":" + req.ParentID
		if existing, ok := f.byRef[key]; ok {
			return existing, nil
		}
	}
	var task *orchestrator.Task
	var err error
	if f.createFn != nil {
		task, err = f.createFn(req)
	} else {
		// Simulate a successful AutoStart (childTask.Status != pending) by
		// default — Dispatch treats a still-pending returned task as a failed
		// auto-start (codex review round 1, Major). Tests that want to
		// exercise the failed-auto-start path set createFn explicitly.
		task = &orchestrator.Task{
			ID:        fmt.Sprintf("child-%d", len(f.calls)),
			ProjectID: req.ProjectID,
			ParentID:  req.ParentID,
			Title:     req.Title,
			Behavior:  req.Behavior,
			Status:    orchestrator.TaskStatusExecuting,
		}
	}
	if err == nil && task != nil && req.Ref != "" && req.ParentID != "" {
		if f.byRef == nil {
			f.byRef = map[string]*orchestrator.Task{}
		}
		f.byRef[req.Ref+":"+req.ParentID] = task
	}
	return task, err
}

// newDispatchWorkflowService wires a TaskWorkflowService the way wire.go does
// for PR-2's ready→working machine dispatch: Tasks for the pre-tx read, Tx
// for the transactional write, TaskTriage for the pre-tx children read
// (recordingTxStore implements all three via the same underlying map — same
// as taskRepo implementing TaskStore/TxStore/TaskTriageStore in wire.go), and
// TaskCreator for child task-ification.
func newDispatchWorkflowService(task *orchestrator.Task, txStore *recordingTxStore, creator *fakeTaskCreator) *TaskWorkflowService {
	return &TaskWorkflowService{
		Tasks:      &stubTaskStore{task: task},
		Tx:         recordingTransactor{store: txStore},
		TaskTriage: txStore,
		TaskCreator: func() TaskCreator {
			if creator == nil {
				return nil
			}
			return creator
		}(),
	}
}

func TestTaskWorkflowService_Dispatch_NoChildren_ReadyToWorking(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusReady, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newDispatchWorkflowService(task, txStore, nil)

	result, err := svc.Dispatch(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("status = %q, want working", result.Task.Status)
	}
	if result.Action.Type != "dispatch" || result.Action.FromStatus != orchestrator.TaskStatusReady || result.Action.ToStatus != orchestrator.TaskStatusWorking {
		t.Fatalf("action = %+v, want dispatch ready->working", result.Action)
	}
}

func TestTaskWorkflowService_Dispatch_SpeccedChildren_CreatesTasksAndMarksDispatched(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusReady, Behavior: "dev", Payload: []byte(`{}`)}
	detail := []byte(`{
		"summary": "keep me",
		"children": [
			{"id": "ch_00", "title": "do it", "status": "specced", "spec": {"project": "p2", "behavior": "impl", "instruction": "go do it"}},
			{"id": "ch_01", "title": "vague", "status": "open"},
			{"id": "ch_02", "title": "already done", "status": "closed"}
		]
	}`)
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
	}
	creator := &fakeTaskCreator{}
	svc := newDispatchWorkflowService(task, txStore, creator)

	result, err := svc.Dispatch(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("status = %q, want working", result.Task.Status)
	}

	if len(creator.calls) != 1 {
		t.Fatalf("CreateTask calls = %d, want 1 (only the specced child)", len(creator.calls))
	}
	call := creator.calls[0]
	if call.ProjectID != "p2" || call.Behavior != "impl" || call.Title != "do it" || call.ParentID != "t1" {
		t.Fatalf("CreateTask request = %+v, unexpected", call)
	}
	if !call.AutoStart {
		t.Fatal("CreateTask request must set AutoStart:true — otherwise a dispatched child never runs (codex review round 1, Major)")
	}
	if call.Ref != "ch_00" {
		t.Fatalf("CreateTask request Ref = %q, want the child's own id (\"ch_00\") for get-or-create replay-safety (codex review round 2, Major)", call.Ref)
	}

	got := txStore.triage["t1"]
	if got == nil {
		t.Fatal("expected task_triage row to still exist")
	}
	children, err := orchestrator.DetailChildren(got.Detail)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("len(children) = %d, want 3", len(children))
	}
	if children[0].Status != orchestrator.TaskTriageChildStatusDispatched || children[0].TaskRef != "child-1" {
		t.Fatalf("children[0] = %+v, want dispatched with task_ref=child-1", children[0])
	}
	if children[1].Status != orchestrator.TaskTriageChildStatusOpen || children[1].TaskRef != "" {
		t.Fatalf("children[1] (open) must be untouched, got %+v", children[1])
	}
	if children[2].Status != orchestrator.TaskTriageChildStatusClosed {
		t.Fatalf("children[2] (closed) must be untouched, got %+v", children[2])
	}
	// summary must survive the children round-trip (SetDetailChildren preserves other keys).
	var m map[string]any
	if err := json.Unmarshal(got.Detail, &m); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if m["summary"] != "keep me" {
		t.Fatalf("summary = %v, want preserved", m["summary"])
	}
}

// docs/plans/cross-project-issue-triage.md の spec スキーマ (note-format.md) は
// description と instruction を別フィールドとして持つ想定だが、Dispatch は
// instruction しか CreateTaskRequest に渡していなかった (2026-08-14, khi 側で
// 実際に子タスクを起こしたところ、Web UI の description が空のまま instruction
// に全文を詰め込む形になっていて見づらいと判明)。spec.description が子タスクの
// description に届くことを確認する。
func TestTaskWorkflowService_Dispatch_SpeccedChild_PassesDescriptionSeparatelyFromInstruction(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusReady, Behavior: "dev", Payload: []byte(`{}`)}
	detail := []byte(`{
		"children": [
			{"id": "ch_00", "title": "do it", "status": "specced", "spec": {
				"project": "p2", "behavior": "impl",
				"description": "background context for the Web UI",
				"instruction": "go do it"
			}}
		]
	}`)
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
	}
	creator := &fakeTaskCreator{}
	svc := newDispatchWorkflowService(task, txStore, creator)

	if _, err := svc.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if len(creator.calls) != 1 {
		t.Fatalf("CreateTask calls = %d, want 1", len(creator.calls))
	}
	call := creator.calls[0]
	if call.Description != "background context for the Web UI" {
		t.Fatalf("CreateTask request Description = %q, want spec.description to pass through", call.Description)
	}
	var instructions []orchestrator.Instruction
	if err := json.Unmarshal(call.Instructions, &instructions); err != nil {
		t.Fatalf("unmarshal instructions: %v", err)
	}
	if len(instructions) != 1 || instructions[0].Message != "go do it" {
		t.Fatalf("instructions = %+v, want spec.instruction unchanged", instructions)
	}
}

func TestTaskWorkflowService_Dispatch_RejectsNonReadyTask(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newDispatchWorkflowService(task, txStore, nil)

	if _, err := svc.Dispatch(context.Background(), task.ID); err == nil {
		t.Fatal("expected error dispatching a non-ready task")
	}
}

// A specced child with no Spec is a data-integrity error (specced requires a
// spec by definition) — must fail loudly rather than silently skip.
func TestTaskWorkflowService_Dispatch_SpeccedChildWithoutSpec_Errors(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusReady, Behavior: "dev", Payload: []byte(`{}`)}
	detail := []byte(`{"children": [{"id": "ch_00", "status": "specced"}]}`)
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
	}
	svc := newDispatchWorkflowService(task, txStore, &fakeTaskCreator{})

	if _, err := svc.Dispatch(context.Background(), task.ID); err == nil {
		t.Fatal("expected error for a specced child with no spec")
	}
	if txStore.updatedTask != nil {
		t.Fatal("task must not be transitioned when a specced child is malformed")
	}
}

// TestTaskWorkflowService_Dispatch_UnrecognizedChildStatus_Errors is the
// regression test for codex review round 1's Minor: a typo'd/unrecognized
// child status (e.g. "speced" instead of "specced") used to silently fall
// through the "not specced, skip" branch — the child was never task-ified
// and the task still advanced to working with no error at all.
func TestTaskWorkflowService_Dispatch_UnrecognizedChildStatus_Errors(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusReady, Behavior: "dev", Payload: []byte(`{}`)}
	detail := []byte(`{"children": [{"id": "ch_00", "status": "speced", "spec": {"project": "p2", "behavior": "impl"}}]}`)
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
	}
	creator := &fakeTaskCreator{}
	svc := newDispatchWorkflowService(task, txStore, creator)

	if _, err := svc.Dispatch(context.Background(), task.ID); err == nil {
		t.Fatal("expected error for an unrecognized child status")
	}
	if len(creator.calls) != 0 {
		t.Fatal("must not create any child task when validation fails")
	}
	if txStore.updatedTask != nil {
		t.Fatal("task must not be transitioned when a child has an unrecognized status")
	}
}

// TestTaskWorkflowService_Dispatch_ChildAutoStartFails_TreatedAsDispatchFailure
// is the regression test for codex review round 1's Major: a child task
// whose auto_start itself silently failed (TaskAppService.CreateTask logs
// and continues rather than erroring, leaving the returned task in
// "pending") must not be marked "dispatched" with a task_ref pointing at a
// task that will never run.
func TestTaskWorkflowService_Dispatch_ChildAutoStartFails_TreatedAsDispatchFailure(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusReady, Behavior: "dev", Payload: []byte(`{}`)}
	detail := []byte(`{"children": [{"id": "ch_00", "title": "do it", "status": "specced", "spec": {"project": "p2", "behavior": "impl"}}]}`)
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
	}
	creator := &fakeTaskCreator{createFn: func(req CreateTaskRequest) (*orchestrator.Task, error) {
		// Simulates TaskAppService.CreateTask's own tolerant auto_start
		// failure path: no error, but the returned task never left pending.
		return &orchestrator.Task{ID: "child-1", ProjectID: req.ProjectID, ParentID: req.ParentID, Status: orchestrator.TaskStatusPending}, nil
	}}
	svc := newDispatchWorkflowService(task, txStore, creator)

	if _, err := svc.Dispatch(context.Background(), task.ID); err == nil {
		t.Fatal("expected error when a created child's auto-start silently failed")
	}
	if txStore.updatedTask != nil {
		t.Fatal("task must not be transitioned to working when a child failed to auto-start")
	}
}

// TestTaskWorkflowService_Dispatch_ChildFinishesBeforeCommit_ReconciledAsClosed
// is the regression test for codex review's Major finding: child creation +
// auto-start happens BEFORE the Tx that persists the child's TaskRef into
// task_triage.detail.children (see Dispatch's own doc comment on why child
// creation cannot run nested inside that Tx). If the child is fast enough to
// reach "done" in that window, its own finalizeTerminal call (fired from
// TaskCreator.CreateTask's synchronous auto_start path, in this test
// simulated directly) would find no matching TaskRef yet and silently no-op
// — permanently missing the child_closed record. Dispatch must reconcile
// this itself right after its own Tx commits.
func TestTaskWorkflowService_Dispatch_ChildFinishesBeforeCommit_ReconciledAsClosed(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusReady, Behavior: "dev", Payload: []byte(`{}`)}
	detail := []byte(`{"children": [{"id": "ch_00", "title": "quick", "status": "specced", "spec": {"project": "p2", "behavior": "impl"}}]}`)
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
		tasks:  map[string]*orchestrator.Task{},
	}
	creator := &fakeTaskCreator{createFn: func(req CreateTaskRequest) (*orchestrator.Task, error) {
		// Simulate the child racing all the way to "done" before Dispatch's
		// own Tx (which persists its TaskRef into the parent's detail) has
		// committed.
		child := &orchestrator.Task{ID: "child-1", ProjectID: req.ProjectID, ParentID: "t1", Status: orchestrator.TaskStatusDone}
		txStore.tasks["child-1"] = child
		return child, nil
	}}
	svc := &TaskWorkflowService{
		Tasks:      syncedTaskStore{tx: txStore},
		Tx:         recordingTransactor{store: txStore},
		TaskTriage: txStore,
		TaskCreator: func() TaskCreator {
			return creator
		}(),
	}

	if _, err := svc.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	tt := txStore.triage["t1"]
	if tt == nil {
		t.Fatal("expected task_triage row to exist")
	}
	children, err := orchestrator.DetailChildren(tt.Detail)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	if len(children) != 1 || children[0].Status != orchestrator.TaskTriageChildStatusClosed {
		t.Fatalf("children = %+v, want the race-finished child reconciled to closed", children)
	}

	found := false
	for _, a := range txStore.actions {
		if a.Type == "child_closed" && a.TaskID == "t1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a child_closed action recorded against the parent, got actions=%+v", txStore.actions)
	}
}

// TestTaskWorkflowService_Dispatch_RetryAfterPartialFailure_DoesNotDuplicateEarlierChild
// is the regression test for codex review round 2's Major: with two specced
// children, if the first is created (and started) successfully but the
// second's create/auto-start fails, Dispatch returns an error and the task
// stays in ready — task_triage.detail.children is never persisted with the
// first child's "dispatched"/task_ref (a known, documented PR-2 gap: no
// atomicity between child creation and the transition). Without a stable Ref
// per child, a hypothetical retry of Dispatch (the obvious PR-3 queue-sweep
// shape) would replay the loop from the top and CREATE-AND-START A DUPLICATE
// of the already-succeeded first child. Setting CreateTaskRequest.Ref to the
// child's own id makes the create idempotent (TaskCreator's real
// implementation, TaskAppService.CreateTask, dedups on (ref, parent_id) via
// idx_tasks_ref_parent) — this test's fakeTaskCreator models that same
// dedup, so a second Dispatch call must return the SAME child-1 task instead
// of minting a new one.
func TestTaskWorkflowService_Dispatch_RetryAfterPartialFailure_DoesNotDuplicateEarlierChild(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusReady, Behavior: "dev", Payload: []byte(`{}`)}
	detail := []byte(`{"children": [
		{"id": "ch_00", "title": "first", "status": "specced", "spec": {"project": "p2", "behavior": "impl"}},
		{"id": "ch_01", "title": "second", "status": "specced", "spec": {"project": "p2", "behavior": "impl"}}
	]}`)
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
	}
	creator := &fakeTaskCreator{createFn: func(req CreateTaskRequest) (*orchestrator.Task, error) {
		if req.Ref == "ch_01" {
			// Second child's auto-start silently fails, every attempt.
			return &orchestrator.Task{ID: "child-2", ProjectID: req.ProjectID, ParentID: req.ParentID, Status: orchestrator.TaskStatusPending}, nil
		}
		return &orchestrator.Task{ID: "child-1", ProjectID: req.ProjectID, ParentID: req.ParentID, Status: orchestrator.TaskStatusExecuting}, nil
	}}
	svc := newDispatchWorkflowService(task, txStore, creator)

	// First attempt: fails on the second child.
	if _, err := svc.Dispatch(context.Background(), task.ID); err == nil {
		t.Fatal("expected the first Dispatch attempt to fail on the second child")
	}
	// Second attempt (simulating a retry): must not create a second "child-1".
	if _, err := svc.Dispatch(context.Background(), task.ID); err == nil {
		t.Fatal("expected the retry to fail again on the second child")
	}

	firstChildCreateCalls := 0
	for _, c := range creator.calls {
		if c.Ref == "ch_00" {
			firstChildCreateCalls++
		}
	}
	if firstChildCreateCalls != 2 {
		t.Fatalf("CreateTask must be CALLED for ch_00 on both attempts (2 — the fake's own dedup, mirroring TaskAppService.CreateTask, is what prevents a SECOND ROW), got %d", firstChildCreateCalls)
	}
	// The crux of the fix: both attempts' get-or-create lookups for ch_00
	// must resolve to the SAME underlying task (id "child-1") — i.e. no
	// duplicate row/second start ever happens for the already-succeeded
	// child, even though Dispatch itself failed and was retried.
	if got := creator.byRef["ch_00:t1"].ID; got != "child-1" {
		t.Fatalf("ch_00's dedup entry = %q, want the single task created on the first attempt (child-1)", got)
	}
}

// TestTaskWorkflowService_Dispatch_NotReachableViaApplyAction pins down that
// "dispatch" is Manual:false — the same StateMachine.IsManualAction gate
// wake_triaged/wake_ready use — so it can never be sent directly through the
// public ApplyAction endpoint (HTTP API / brokered action_send / `boid action
// send`).
func TestTaskWorkflowService_Dispatch_NotReachableViaApplyAction(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusReady, Behavior: "dev", Payload: []byte(`{}`)}
	svc := newTriageWorkflowService(task, &recordingTxStore{task: task})

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "dispatch"})
	if err == nil {
		t.Fatal("expected rejection of \"dispatch\" via public ApplyAction")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 StatusError, got %v", err)
	}
}

// syncedTaskStore backs TaskWorkflowService.Tasks with the SAME state
// recordingTxStore's WithinTx commits write to (s.tx.updatedTask), so a
// GetTask right after a commit sees the fresh status — exactly what happens
// in production, where Tasks and Tx are both views onto the same underlying
// DB/connection. The plain stubTaskStore used elsewhere in this file holds a
// static snapshot instead, which is fine for single-transition tests but not
// for the ready→(auto-chained)dispatch integration tests below, which need
// the second (Dispatch) transaction's pre-check to observe the first
// (ApplyAction "ready") transaction's commit.
type syncedTaskStore struct {
	tx *recordingTxStore
}

func (s syncedTaskStore) CreateTask(task *orchestrator.Task) error { return s.tx.CreateTask(task) }
func (s syncedTaskStore) GetTask(id string) (*orchestrator.Task, error) {
	if s.tx.updatedTask != nil && s.tx.updatedTask.ID == id {
		return s.tx.updatedTask, nil
	}
	return s.tx.GetTask(id)
}
func (s syncedTaskStore) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return s.tx.ListTasks(filter)
}
func (s syncedTaskStore) UpdateTask(task *orchestrator.Task) error { return s.tx.UpdateTask(task) }
func (s syncedTaskStore) DeleteTask(id string) error               { return s.tx.DeleteTask(id) }
func (s syncedTaskStore) FindTaskByRemote(remoteID string) (*orchestrator.Task, error) {
	return s.tx.FindTaskByRemote(remoteID)
}
func (s syncedTaskStore) FindTaskByRef(ref, parentID, projectID string) (*orchestrator.Task, error) {
	return s.tx.FindTaskByRef(ref, parentID, projectID)
}
func (s syncedTaskStore) ListChildren(parentID string) ([]*orchestrator.Task, error) {
	return s.tx.ListChildren(parentID)
}

// TestTaskWorkflowServiceApplyAction_Ready_AutoChainsToWorking is the
// integration test for 逆輸入2's 2-stage Go: ApplyAction("ready") — the
// user-facing Go — must itself return a task already in "working", because
// the mechanical ready→working dispatch chains synchronously right after
// "ready" commits.
func TestTaskWorkflowServiceApplyAction_Ready_AutoChainsToWorking(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task: task,
		// PR-B: machineFor needs a task_triage row to pick NewCardMachine
		// (which is where "ready" lives) — see newTriageWorkflowService's
		// own doc comment for the same requirement.
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1"}},
	}
	creator := &fakeTaskCreator{}
	svc := &TaskWorkflowService{
		Tasks:       syncedTaskStore{tx: txStore},
		Tx:          recordingTransactor{store: txStore},
		Meta:        stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		TaskTriage:  txStore,
		TaskCreator: creator,
	}

	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "ready"})
	if err != nil {
		t.Fatalf("ApplyAction(ready): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("status = %q, want working (auto-chained dispatch)", result.Task.Status)
	}

	// Both the "ready" and "dispatch" actions must be in the audit trail.
	var types []string
	for _, a := range txStore.actions {
		types = append(types, a.Type)
	}
	foundReady, foundDispatch := false, false
	for _, ty := range types {
		if ty == "ready" {
			foundReady = true
		}
		if ty == "dispatch" {
			foundDispatch = true
		}
	}
	if !foundReady || !foundDispatch {
		t.Fatalf("actions = %v, want both ready and dispatch recorded", types)
	}
}

// TestTaskWorkflowServiceApplyAction_Ready_DispatchFailureDoesNotFailReadyAction
// confirms a Dispatch-stage failure (e.g. TaskCreator erroring) does not
// unwind the already-committed "ready" transition — ready is the real Go
// judgment and must not be lost because of a downstream mechanical hiccup
// (machine.go's own doc comment: a task stuck in ready is the intended,
// visible failure signal, not a lost Go).
func TestTaskWorkflowServiceApplyAction_Ready_DispatchFailureDoesNotFailReadyAction(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	detail := []byte(`{"children": [{"id": "ch_00", "status": "specced", "spec": {"project": "p2", "behavior": "impl"}}]}`)
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
	}
	creator := &fakeTaskCreator{createFn: func(req CreateTaskRequest) (*orchestrator.Task, error) {
		return nil, fmt.Errorf("project p2 not found")
	}}
	svc := &TaskWorkflowService{
		Tasks:       syncedTaskStore{tx: txStore},
		Tx:          recordingTransactor{store: txStore},
		Meta:        stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		TaskTriage:  txStore,
		TaskCreator: creator,
	}

	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "ready"})
	if err != nil {
		t.Fatalf("ApplyAction(ready) must not fail even though Dispatch failed: %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusReady {
		t.Fatalf("status = %q, want ready (dispatch failure must leave the task in ready, not working)", result.Task.Status)
	}
}

// TestStatusErrorForGetTaskErr is the regression test for codex review round
// 1's Minor: Dispatch used to collapse every GetTask error into 404, which
// misreports a transient DB failure as "task not found". Only
// orchestrator.ErrTaskNotFound should map to 404; everything else is a 500.
func TestStatusErrorForGetTaskErr(t *testing.T) {
	notFound := statusErrorForGetTaskErr(fmt.Errorf("get task %q: %w", "t1", orchestrator.ErrTaskNotFound))
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("ErrTaskNotFound: code = %d, want 404", notFound.Code)
	}

	other := statusErrorForGetTaskErr(fmt.Errorf("db connection reset"))
	if other.Code != http.StatusInternalServerError {
		t.Fatalf("generic error: code = %d, want 500", other.Code)
	}
}
