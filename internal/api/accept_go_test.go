package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

	result, err := svc.acceptGo(context.Background(), task.ID, false)
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

	result, err := svc.acceptGo(context.Background(), task.ID, false)
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

	if _, err := svc.acceptGo(context.Background(), task.ID, false); err != nil {
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

// TestTaskWorkflowService_AcceptGo_RejectsNonParkedTask_ErrorMessageHint pins
// review MEDIUM 1 (fix/unapplicable-suggestion-guard PR): before this test
// existed, only `err != nil` was asserted here — a mutation that reverted the
// error message back to the PR's pre-fix bare "(must be parked)" text (no
// orchestrator.StateMachine.AvailableActionsHint suffix) still passed
// `go test ./internal/api/...` in full. This is the ONLY call site that
// exercises acceptGo's own
// non-parked branch (the exhaustive verb×status combination test,
// suggestion_accept_test.go, deliberately skips asserting "go"'s hint content
// since it goes through this separate branch rather than applyAnswered's
// generic sm.Apply path — see that test's own comment). Exercises BOTH
// callers that share this one message: a direct "go" click (viaAccept=false,
// this test) and accept(go) (viaAccept=true, the companion test below).
func TestTaskWorkflowService_AcceptGo_RejectsNonParkedTask_ErrorMessageHint(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newAcceptGoWorkflowService(task, txStore, nil)

	_, err := svc.acceptGo(context.Background(), task.ID, false)
	if err == nil {
		t.Fatal("expected error accepting go on a non-parked task")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected a 409 StatusError, got %v", err)
	}
	if !strings.Contains(se.Message, "go") || !strings.Contains(se.Message, "working") {
		t.Errorf("message should name the verb and the current status; got %q", se.Message)
	}
	// working's own available actions (park/done — machine_card.go) must be
	// named, mirroring applyAnswered's generic-path hint (same
	// orchestrator.StateMachine.AvailableActionsHint source, review LOW 4).
	for _, want := range []string{"park", "done"} {
		if !strings.Contains(se.Message, want) {
			t.Errorf("message should name available action %q; got %q", want, se.Message)
		}
	}
}

// TestApplyAction_Answered_AcceptGo_InapplicableStatus_ErrorMessageHint is
// TestTaskWorkflowService_AcceptGo_RejectsNonParkedTask_ErrorMessageHint's
// twin for the viaAccept=true (accept(go) via the "answered" action) path —
// the exhaustive verb×status test (suggestion_accept_test.go) deliberately
// skips this content check for "go" because it 409s through this DIFFERENT
// branch (acceptGo's own early status check, not applyAnswered's generic
// sm.Apply failure). Both callers share the SAME message-building code, but
// only a dedicated test on each actually proves it.
func TestApplyAction_Answered_AcceptGo_InapplicableStatus_ErrorMessageHint(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task: task,
		triage: map[string]*orchestrator.TaskTriage{
			"t1": {TaskID: "t1", Detail: json.RawMessage(`{"attrs":{"suggestion":{"verb":"go","reason":"stale"}}}`)},
		},
	}
	svc := newAcceptGoWorkflowService(task, txStore, nil)

	payload, _ := json.Marshal(map[string]string{"answer": answeredAnswerAccept, "verb": "go"})
	_, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "answered", Payload: payload})
	if err == nil {
		t.Fatal("expected error accepting go on a done task")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected a 409 StatusError, got %v", err)
	}
	if !strings.Contains(se.Message, "go") || !strings.Contains(se.Message, "done") {
		t.Errorf("message should name the verb and the current status; got %q", se.Message)
	}
	// done's own available action (reopen — machine_card.go) must be named.
	if !strings.Contains(se.Message, "reopen") {
		t.Errorf("message should name available action %q; got %q", "reopen", se.Message)
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

	if _, err := svc.acceptGo(context.Background(), task.ID, false); err == nil {
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

	if _, err := svc.acceptGo(context.Background(), task.ID, false); err == nil {
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

	if _, err := svc.acceptGo(context.Background(), task.ID, false); err == nil {
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

	if _, err := svc.acceptGo(context.Background(), task.ID, false); err != nil {
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
	if _, err := svc.acceptGo(context.Background(), task.ID, false); err == nil {
		t.Fatal("expected the first acceptGo attempt to fail on the second child")
	}
	// Second attempt (simulating a retry): must not create a second "child-1".
	if _, err := svc.acceptGo(context.Background(), task.ID, false); err == nil {
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

// TestApplyAction_Answered_AcceptGo_DoesNotRecordSuggestionDiscarded is PR
// #987 review round 2's MEDIUM N2 regression test. Before this fix, khi
// suggesting "go" and a human accepting it produced this action log:
//
//	answered              {"answer":"accept","verb":"go"}
//	go
//	suggestion_discarded  {"reason":"...","superseding_action":"go","verb":"go"}
//
// — the ACCEPTED suggestion recorded as "discarded", on the single most
// common accept path in the whole feature (design doc §3.9 treats this
// action-log history as future auto-accept-policy evaluation data, so this
// mislabeling would have polluted it from day one). acceptGo's viaAccept
// param (workflow_triage.go) now suppresses the suggestion_discarded record
// specifically for accept-originated "go", while still stripping the
// suggestion (it must not linger after being consumed).
func TestApplyAction_Answered_AcceptGo_DoesNotRecordSuggestionDiscarded(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task: task,
		triage: map[string]*orchestrator.TaskTriage{
			"t1": {TaskID: "t1", SuggestionVerb: "go", Detail: json.RawMessage(`{"attrs":{"suggestion":{"verb":"go","reason":"children are specced"}}}`)},
		},
	}
	svc := newAcceptGoWorkflowService(task, txStore, nil)

	payload, _ := json.Marshal(map[string]string{"answer": answeredAnswerAccept, "verb": "go"})
	result, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "answered", Payload: payload})
	if err != nil {
		t.Fatalf("ApplyAction(answered, accept go): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("status = %q, want working", result.Task.Status)
	}

	if discard := findAction(txStore.actions, "suggestion_discarded"); discard != nil {
		t.Fatalf("suggestion_discarded recorded for the ACCEPTED suggestion itself: %+v (full action log: %+v)", discard, txStore.actions)
	}
	suggestion, ok := orchestrator.DetailSuggestion(txStore.triage["t1"].Detail)
	if ok || suggestion.Verb != "" {
		t.Errorf("suggestion still present after accept(go): %+v", suggestion)
	}
	// PR-2 (docs/plans/suggestion-as-state-transition-impl.md §4.1): the
	// viaAccept=true strip (applyAnsweredSideEffect) must clear
	// suggestion_verb too, same as the viaAccept=false discard path
	// (suggestion_discard_test.go's AcceptGo test).
	if got := txStore.triage["t1"].SuggestionVerb; got != "" {
		t.Errorf("suggestion_verb column = %q after accept(go), want cleared", got)
	}
	if answered := findAction(txStore.actions, "answered"); answered == nil {
		t.Fatal("expected an \"answered\" action recording the accept — audit trail must not be empty")
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

// TestTaskWorkflowService_AcceptGo_TransitionFailure_RecordsOrphanedChildIDs_DoesNotAbort
// is step 4's dedicated regression test (PR #987 review, BLOCKER 4 — this
// exact branch had ZERO test coverage before). It simulates the transition
// Tx failing AFTER the child was already created and auto-started
// (s.Tasks's pre-Tx read sees "parked", but the in-Tx tx.GetTask read sees a
// DIFFERENT status — modeling a concurrent transition that landed in the
// gap between acceptGo's pre-Tx child creation and its own Tx opening,
// exactly the scenario acceptGo's "task status changed to ... before the
// parked->working transition could commit" guard exists to catch).
//
// Asserts the CURRENT (fixed) contract: a synchronous error, a dispatch_error
// action whose payload carries the created child's task id under
// "orphaned_child_task_ids" (for a human to inspect/hand-abort), and —
// the actual regression this test guards — no attempt to abort the child
// task. The removed abortChildrenBestEffort used to call
// ApplyAction("abort") against the child's own task id; this test's
// s.Tasks is scoped to ONLY the card's own task ("t1"), so if any code
// path still tried to look up/mutate the child task ("child-1") through
// it, that would surface as an unrelated "task not found" error path
// rather than the clean dispatch_error this test expects — a structural
// tripwire against the old behavior silently coming back, in addition to
// the explicit acceptGo doc comment on why it was removed (BLOCKER 4:
// aborting a child this call did not necessarily "own" — CreateTask's own
// get-or-create dedup means a concurrent accept(go) could have created and
// already be relying on the SAME child).
// TestTaskWorkflowService_AcceptGo_ConcurrentTransitionWon_DoesNotRecordOrphanedChildIDs
// is PR #987 review round 2's LOW N4 regression test: BLOCKER 4 removed the
// unsafe compensation-abort for this exact race (a concurrent transition
// already committed the card out of "parked" before this Tx opened), but the
// replacement dispatch_error payload used to still fold newlyDispatched into
// orphaned_child_task_ids even here — misleadingly, since a Tx that LOST this
// specific race cannot tell whether those task ids are its own unclaimed
// children or the WINNING caller's own legitimately-running children
// (CreateTask's get-or-create dedup can hand back the same task id to both
// callers). Reporting them as "orphaned" would give an operator a false lead
// to hand-abort real, successful work. This is the one case where NOTHING
// should be reported.
func TestTaskWorkflowService_AcceptGo_ConcurrentTransitionWon_DoesNotRecordOrphanedChildIDs(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	detail := []byte(`{"children": [{"id": "ch_00", "title": "do it", "status": "specced", "spec": {"project": "p2", "behavior": "impl"}}]}`)
	txStore := &recordingTxStore{
		// The in-Tx read sees the card already "working" — simulating a
		// concurrent transition that committed in the window between the
		// child being created (below) and this Tx opening.
		task:   &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusWorking},
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
	}
	creator := &fakeTaskCreator{}
	svc := &TaskWorkflowService{
		Tasks:       &stubTaskStore{task: task}, // pre-Tx read: parked
		Tx:          recordingTransactor{store: txStore},
		TaskTriage:  txStore,
		TaskCreator: creator,
	}

	_, err := svc.acceptGo(context.Background(), task.ID, false)
	if err == nil {
		t.Fatal("expected an error when the transition Tx fails after the child was already created")
	}
	if len(creator.calls) != 1 {
		t.Fatalf("expected the child to have been created before the Tx failure, got %d CreateTask calls", len(creator.calls))
	}

	var dispatchErrorAction *orchestrator.Action
	for _, a := range txStore.actions {
		if a.TaskID == task.ID && a.Type == "dispatch_error" {
			dispatchErrorAction = a
		}
	}
	if dispatchErrorAction == nil {
		t.Fatalf("expected a dispatch_error action recorded, got actions=%+v", txStore.actions)
	}
	var payload struct {
		Error                string   `json:"error"`
		OrphanedChildTaskIDs []string `json:"orphaned_child_task_ids"`
	}
	if err := json.Unmarshal(dispatchErrorAction.Payload, &payload); err != nil {
		t.Fatalf("unmarshal dispatch_error payload: %v", err)
	}
	if len(payload.OrphanedChildTaskIDs) != 0 {
		t.Fatalf("orphaned_child_task_ids = %v, want none (this Tx lost the race — it cannot tell these ids apart from the winner's own legitimate children, LOW N4)", payload.OrphanedChildTaskIDs)
	}

	// The card itself must stay parked (this Tx never committed).
	if txStore.updatedTask != nil {
		t.Fatalf("card must stay parked when the transition Tx fails, got updatedTask=%+v", txStore.updatedTask)
	}
	// No "abort" (or any other) action was ever recorded against the CHILD
	// task id — the removed compensation used to record exactly that via
	// ApplyAction("abort").
	for _, a := range txStore.actions {
		if a.TaskID == "child-1" {
			t.Fatalf("no action should ever be recorded against the child task (compensation abort was removed, BLOCKER 4), got %+v", a)
		}
	}
}

// TestTaskWorkflowService_AcceptGo_GenuineTxFailure_StillRecordsOrphanedChildIDs
// is LOW N4's other half: a Tx failure that is NOT "someone else already won
// the race" (fresh.Status was still "parked" — this call was not racing
// anyone) must keep reporting orphaned_child_task_ids exactly as BLOCKER 4
// established. Simulated via updateTaskErr: the transition Tx fails on
// tx.UpdateTask itself, AFTER fresh.Status==parked was confirmed (so
// concurrentTransitionWon stays false) and after the child was already
// created — a genuine write failure, not a race loss.
func TestTaskWorkflowService_AcceptGo_GenuineTxFailure_StillRecordsOrphanedChildIDs(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	detail := []byte(`{"children": [{"id": "ch_00", "title": "do it", "status": "specced", "spec": {"project": "p2", "behavior": "impl"}}]}`)
	txStore := &recordingTxStore{
		task:          &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusParked}, // still parked: no race
		triage:        map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
		updateTaskErr: fmt.Errorf("simulated write failure"),
	}
	creator := &fakeTaskCreator{}
	svc := &TaskWorkflowService{
		Tasks:       &stubTaskStore{task: task},
		Tx:          recordingTransactor{store: txStore},
		TaskTriage:  txStore,
		TaskCreator: creator,
	}

	_, err := svc.acceptGo(context.Background(), task.ID, false)
	if err == nil {
		t.Fatal("expected an error when tx.UpdateTask itself fails")
	}

	var dispatchErrorAction *orchestrator.Action
	for _, a := range txStore.actions {
		if a.TaskID == task.ID && a.Type == "dispatch_error" {
			dispatchErrorAction = a
		}
	}
	if dispatchErrorAction == nil {
		t.Fatalf("expected a dispatch_error action recorded, got actions=%+v", txStore.actions)
	}
	var payload struct {
		Error                string   `json:"error"`
		OrphanedChildTaskIDs []string `json:"orphaned_child_task_ids"`
	}
	if err := json.Unmarshal(dispatchErrorAction.Payload, &payload); err != nil {
		t.Fatalf("unmarshal dispatch_error payload: %v", err)
	}
	if len(payload.OrphanedChildTaskIDs) != 1 || payload.OrphanedChildTaskIDs[0] != "child-1" {
		t.Fatalf("orphaned_child_task_ids = %v, want [child-1] (a genuine failure, not a lost race, must still report this call's own unclaimed child)", payload.OrphanedChildTaskIDs)
	}
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
