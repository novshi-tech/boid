package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// fakeTaskCreator is a minimal TaskCreator stub for TestTaskWorkflowService_AcceptGo*.
// It mimics TaskAppService.CreateTask's own (ref, parent_id) get-or-create
// dedup (task_create.go) — a call whose Ref+ParentID match a task it already
// created returns that SAME task instead of minting a new one — so tests can
// exercise acceptGo's replay-safety (codex review round 2, Major, carried
// over from v1's Dispatch).
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
		// default — acceptGo treats a still-pending returned task as a failed
		// auto-start. Tests that want to exercise the failed-auto-start path
		// set createFn explicitly.
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

// newAcceptGoWorkflowService wires a TaskWorkflowService the way wire.go does
// for accept(go)'s machine dispatch: Tasks for the pre-tx read, Tx for the
// transactional write, TaskTriage for the pre-tx children read
// (recordingTxStore implements all three via the same underlying map — same
// as taskRepo implementing TaskStore/TxStore/TaskTriageStore in wire.go), and
// TaskCreator for child task-ification.
func newAcceptGoWorkflowService(task *orchestrator.Task, txStore *recordingTxStore, creator *fakeTaskCreator) *TaskWorkflowService {
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

func TestTaskWorkflowService_AcceptGo_NoChildren_ParkedToWorking(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newAcceptGoWorkflowService(task, txStore, nil)

	result, err := svc.acceptGo(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("acceptGo: %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("status = %q, want working", result.Task.Status)
	}
	if result.Action.Type != "go" || result.Action.FromStatus != orchestrator.TaskStatusParked || result.Action.ToStatus != orchestrator.TaskStatusWorking {
		t.Fatalf("action = %+v, want go parked->working", result.Action)
	}
}

func TestTaskWorkflowService_AcceptGo_SpeccedChildren_CreatesTasksAndMarksDispatched(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
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
	svc := newAcceptGoWorkflowService(task, txStore, creator)

	result, err := svc.acceptGo(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("acceptGo: %v", err)
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
		t.Fatal("CreateTask request must set AutoStart:true — otherwise a dispatched child never runs")
	}
	if call.Ref != "ch_00" {
		t.Fatalf("CreateTask request Ref = %q, want the child's own id (\"ch_00\") for get-or-create replay-safety", call.Ref)
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
// description と instruction を別フィールドとして持つ想定だが、acceptGo は
// instruction しか CreateTaskRequest に渡していなかった (v1's Dispatch, same
// bug carried forward until fixed 2026-08-14). spec.description が子タスクの
// description に届くことを確認する。
func TestTaskWorkflowService_AcceptGo_SpeccedChild_PassesDescriptionSeparatelyFromInstruction(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
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
	svc := newAcceptGoWorkflowService(task, txStore, creator)

	if _, err := svc.acceptGo(context.Background(), task.ID); err != nil {
		t.Fatalf("acceptGo: %v", err)
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

func TestTaskWorkflowService_AcceptGo_RejectsNonParkedTask(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newAcceptGoWorkflowService(task, txStore, nil)

	if _, err := svc.acceptGo(context.Background(), task.ID); err == nil {
		t.Fatal("expected error accepting go on a non-parked task")
	}
}

// A specced child with no Spec is a data-integrity error (specced requires a
// spec by definition) — must fail loudly rather than silently skip.
func TestTaskWorkflowService_AcceptGo_SpeccedChildWithoutSpec_Errors(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	detail := []byte(`{"children": [{"id": "ch_00", "status": "specced"}]}`)
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
	}
	svc := newAcceptGoWorkflowService(task, txStore, &fakeTaskCreator{})

	if _, err := svc.acceptGo(context.Background(), task.ID); err == nil {
		t.Fatal("expected error for a specced child with no spec")
	}
	if txStore.updatedTask != nil {
		t.Fatal("task must not be transitioned when a specced child is malformed")
	}
	// dispatch_error must be recorded even for this pre-transaction failure
	// (docs/plans/suggestion-as-state-transition-impl.md §B — the explicit
	// fix over v1's slog-only failure path).
	assertDispatchErrorRecorded(t, txStore, task.ID)
}

func TestTaskWorkflowService_AcceptGo_UnrecognizedChildStatus_Errors(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	detail := []byte(`{"children": [{"id": "ch_00", "status": "speced", "spec": {"project": "p2", "behavior": "impl"}}]}`)
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
	}
	creator := &fakeTaskCreator{}
	svc := newAcceptGoWorkflowService(task, txStore, creator)

	if _, err := svc.acceptGo(context.Background(), task.ID); err == nil {
		t.Fatal("expected error for an unrecognized child status")
	}
	if len(creator.calls) != 0 {
		t.Fatal("must not create any child task when validation fails")
	}
	if txStore.updatedTask != nil {
		t.Fatal("task must not be transitioned when a child has an unrecognized status")
	}
}

// TestTaskWorkflowService_AcceptGo_ChildAutoStartFails_TreatedAsDispatchFailure
// is the regression test for codex review round 1's Major (v1): a child task
// whose auto_start itself silently failed (TaskAppService.CreateTask logs
// and continues rather than erroring, leaving the returned task in
// "pending") must not be marked "dispatched" with a task_ref pointing at a
// task that will never run.
func TestTaskWorkflowService_AcceptGo_ChildAutoStartFails_TreatedAsDispatchFailure(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	detail := []byte(`{"children": [{"id": "ch_00", "title": "do it", "status": "specced", "spec": {"project": "p2", "behavior": "impl"}}]}`)
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
	}
	creator := &fakeTaskCreator{createFn: func(req CreateTaskRequest) (*orchestrator.Task, error) {
		return &orchestrator.Task{ID: "child-1", ProjectID: req.ProjectID, ParentID: req.ParentID, Status: orchestrator.TaskStatusPending}, nil
	}}
	svc := newAcceptGoWorkflowService(task, txStore, creator)

	if _, err := svc.acceptGo(context.Background(), task.ID); err == nil {
		t.Fatal("expected error when a created child's auto-start silently failed")
	}
	if txStore.updatedTask != nil {
		t.Fatal("task must not be transitioned to working when a child failed to auto-start")
	}
}

// TestTaskWorkflowService_AcceptGo_ChildFinishesBeforeCommit_ReconciledAsClosed
// is the regression test for codex review's Major finding (v1): child creation
// + auto-start happens BEFORE the Tx that persists the child's TaskRef into
// task_triage.detail.children. If that child is fast enough to reach done in
// that window, its own finalizeTerminal call finds no matching TaskRef yet
// and silently no-ops. acceptGo must reconcile this itself right after its
// own Tx commits.
func TestTaskWorkflowService_AcceptGo_ChildFinishesBeforeCommit_ReconciledAsClosed(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	detail := []byte(`{"children": [{"id": "ch_00", "title": "quick", "status": "specced", "spec": {"project": "p2", "behavior": "impl"}}]}`)
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
		tasks:  map[string]*orchestrator.Task{},
	}
	creator := &fakeTaskCreator{createFn: func(req CreateTaskRequest) (*orchestrator.Task, error) {
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

	if _, err := svc.acceptGo(context.Background(), task.ID); err != nil {
		t.Fatalf("acceptGo: %v", err)
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

// TestTaskWorkflowService_AcceptGo_RetryAfterPartialFailure_DoesNotDuplicateEarlierChild
// is the regression test for codex review round 2's Major (v1): with two
// specced children, if the first is created (and started) successfully but
// the second's create/auto-start fails, acceptGo returns an error and the
// card stays parked — task_triage.detail.children is never persisted with
// the first child's "dispatched"/task_ref. Without a stable Ref per child, a
// retried acceptGo would replay the loop from the top and CREATE-AND-START A
// DUPLICATE of the already-succeeded first child. Ref: children[i].ID makes
// the create idempotent.
func TestTaskWorkflowService_AcceptGo_RetryAfterPartialFailure_DoesNotDuplicateEarlierChild(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
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
	svc := newAcceptGoWorkflowService(task, txStore, creator)

	// First attempt: fails on the second child.
	if _, err := svc.acceptGo(context.Background(), task.ID); err == nil {
		t.Fatal("expected the first acceptGo attempt to fail on the second child")
	}
	// Second attempt (simulating a retry): must not create a second "child-1".
	if _, err := svc.acceptGo(context.Background(), task.ID); err == nil {
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
	if got := creator.byRef["ch_00:t1"].ID; got != "child-1" {
		t.Fatalf("ch_00's dedup entry = %q, want the single task created on the first attempt (child-1)", got)
	}
}

// TestApplyAction_Go_DelegatesToAcceptGo pins that ApplyAction("go") is a
// thin bypass into acceptGo (this file's own doc comment, workflow_action.go)
// — both a direct human click and accept(go) reach the identical code path.
func TestApplyAction_Go_DelegatesToAcceptGo(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task, triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1"}}}
	creator := &fakeTaskCreator{}
	svc := newAcceptGoWorkflowService(task, txStore, creator)

	result, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "go"})
	if err != nil {
		t.Fatalf("ApplyAction(go): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("status = %q, want working", result.Task.Status)
	}
}

// TestApplyAction_Go_DispatchFailure_ReturnsSyncErrorAndRecordsDispatchError
// is the CONCRETE IMPROVEMENT over v1's known gap (see acceptGo's own doc
// comment, workflow_triage.go): v1's equivalent failure (Dispatch erroring
// after "ready" already committed) only ever reached slog.Error, with the
// caller already getting an HTTP 200. accept(go)/"go" failing now ALWAYS
// surfaces as a synchronous error, with a dispatch_error action recorded and
// the card left parked (never even reaching a "ready"-like resting state,
// since v2 has none).
func TestApplyAction_Go_DispatchFailure_ReturnsSyncErrorAndRecordsDispatchError(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	detail := []byte(`{"children": [{"id": "ch_00", "status": "specced", "spec": {"project": "p2", "behavior": "impl"}}]}`)
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
	}
	creator := &fakeTaskCreator{createFn: func(req CreateTaskRequest) (*orchestrator.Task, error) {
		return nil, fmt.Errorf("project p2 not found")
	}}
	svc := newAcceptGoWorkflowService(task, txStore, creator)

	_, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "go"})
	if err == nil {
		t.Fatal("expected ApplyAction(go) to return a synchronous error when child creation fails — this is the fix, not the v1 tolerance")
	}
	if txStore.updatedTask != nil {
		t.Fatal("card must stay parked when accept(go) fails")
	}
	assertDispatchErrorRecorded(t, txStore, task.ID)
}

// assertDispatchErrorRecorded is the shared assertion every acceptGo failure
// test above uses: a dispatch_error action must be in the audit trail
// regardless of which stage failed (child creation vs. the transition Tx).
func assertDispatchErrorRecorded(t *testing.T, txStore *recordingTxStore, taskID string) {
	t.Helper()
	for _, a := range txStore.actions {
		if a.TaskID == taskID && a.Type == "dispatch_error" {
			return
		}
	}
	t.Fatalf("expected a dispatch_error action recorded for task %q, got actions=%+v", taskID, txStore.actions)
}

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

// syncedTaskStore backs TaskWorkflowService.Tasks with the SAME state
// recordingTxStore's WithinTx commits write to (s.tx.updatedTask), so a
// GetTask right after a commit sees the fresh status — exactly what happens
// in production, where Tasks and Tx are both views onto the same underlying
// DB/connection. The plain stubTaskStore used elsewhere in this file holds a
// static snapshot instead, which is fine for single-transition tests but not
// for the race-reconciliation test above, which needs a fresh post-commit read.
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
