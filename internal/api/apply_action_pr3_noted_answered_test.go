package api

// docs/plans/ingestion-identity.md PR-3 (B-3+B-4): 「検証」節を直接 pin する
// noted/answered の ApplyAction 側テスト。broker/executor scoping は別ファイル
// (internal/sandbox, internal/server) で、action_list の読み戻しは
// action_list_test.go で扱う — ここは ApplyAction が noted/answered をどう
// 記録し、どう副作用を及ぼす(及ぼさない)かに絞る。
//
//   - noted が任意形状の JSON payload を通し、daemon はそれを一切解釈しない
//     (J-5) — object/array/string/number いずれも素通しされることを pin する
//   - noted / answered は attrs_set と同じ「task.Payload を汚さない・
//     tx.UpdateTask しない」扱い (非遷移アクション共通の pollution 防止)
//   - answered.answer は accept/reject の閉集合として検証される (J-6) が
//     verb/basis は検証しない (J-7)
//   - answered が detail.attrs.suggestion を落とす — accept 側だけでなく
//     reject 側も同じく pin する (khi の実データが reject 0 件だったという
//     設計 doc の指摘に対応: reject 経路こそテストで固める)

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestApplyAction_Noted_PassesThroughArbitraryJSONPayloadUnvalidated(t *testing.T) {
	shapes := []string{
		`{"kind":"suggestion_reviewed","basis":"issue #42"}`, // object
		`["a","b","c"]`,   // array
		`"just a string"`, // string
		`42`,              // number
		`true`,            // bool
		`null`,            // null
	}
	for _, payload := range shapes {
		task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
		txStore := &recordingTxStore{task: task}
		svc := newTriageWorkflowService(task, txStore)

		result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
			Type:    "noted",
			Payload: []byte(payload),
		})
		if err != nil {
			t.Fatalf("ApplyAction(noted) payload=%s: unexpected error: %v", payload, err)
		}
		if result.Task.Status != orchestrator.TaskStatusTriaged {
			t.Fatalf("payload=%s: status = %q, want unchanged (triaged)", payload, result.Task.Status)
		}
		if len(txStore.actions) != 1 {
			t.Fatalf("payload=%s: actions recorded = %d, want 1", payload, len(txStore.actions))
		}
		if string(txStore.actions[0].Payload) != payload {
			t.Errorf("payload=%s: recorded action payload = %s, want it stored verbatim (daemon never interprets noted)", payload, txStore.actions[0].Payload)
		}
	}
}

func TestApplyAction_Noted_RejectsInvalidJSON(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "noted",
		Payload: []byte(`this is not json`),
	})
	if err == nil {
		t.Fatal("ApplyAction(noted) with invalid JSON payload = nil error, want a rejection")
	}
	if len(txStore.actions) != 0 {
		t.Errorf("actions recorded = %d, want 0 (invalid JSON must never reach CreateAction)", len(txStore.actions))
	}
}

func TestApplyAction_Noted_EmptyPayloadIsAccepted(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "noted"}); err != nil {
		t.Fatalf("ApplyAction(noted) with no payload: unexpected error: %v", err)
	}
	if len(txStore.actions) != 1 {
		t.Fatalf("actions recorded = %d, want 1", len(txStore.actions))
	}
}

// TestApplyAction_Noted_DoesNotWriteTaskRowOrPollutePayload mirrors
// TestApplyAction_AttrsSet_DoesNotWriteTaskRow /
// ..._UpdatesDetailWithoutTransitionOrPayloadPollution (apply_action_pr4_test.go)
// for noted: non-transitioning, no tx.UpdateTask, task.Payload untouched.
func TestApplyAction_Noted_DoesNotWriteTaskRowOrPollutePayload(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Behavior: "dev", Payload: []byte(`{"existing":"keep"}`)}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "noted",
		Payload: []byte(`{"seen_at":"2026-08-19T00:00:00Z"}`),
	})
	if err != nil {
		t.Fatalf("ApplyAction(noted): %v", err)
	}
	if string(result.Task.Payload) != `{"existing":"keep"}` {
		t.Fatalf("task.Payload = %s, want untouched (noted must not merge into task.Payload)", result.Task.Payload)
	}
	if txStore.updatedTask != nil {
		t.Fatalf("noted called tx.UpdateTask (updatedTask=%+v), want no task-row write", txStore.updatedTask)
	}
}

func TestApplyAction_Answered_RequiresValidAnswer(t *testing.T) {
	cases := []string{"", "maybe", "ACCEPT", "accept "}
	for _, answer := range cases {
		task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
		txStore := &recordingTxStore{task: task}
		svc := newTriageWorkflowService(task, txStore)

		payload, _ := json.Marshal(map[string]string{"answer": answer})
		_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "answered", Payload: payload})
		if err == nil {
			t.Errorf("answer=%q: ApplyAction(answered) = nil error, want a rejection", answer)
		}
		if len(txStore.actions) != 0 {
			t.Errorf("answer=%q: actions recorded = %d, want 0", answer, len(txStore.actions))
		}
	}
}

func TestApplyAction_Answered_MissingPayloadRejected(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "answered"})
	if err == nil {
		t.Fatal("ApplyAction(answered) with no payload = nil error, want a rejection")
	}
}

// TestApplyAction_Answered_VerbBasisRecordedButNotValidated pins J-7: verb
// and basis are recorded verbatim (even nonsense values) — the daemon never
// cross-checks them against anything.
func TestApplyAction_Answered_VerbBasisRecordedButNotValidated(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	payload := []byte(`{"answer":"accept","verb":"totally-made-up-verb","basis":""}`)
	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "answered", Payload: payload}); err != nil {
		t.Fatalf("ApplyAction(answered): unexpected error for an unvetted verb: %v", err)
	}
	if len(txStore.actions) != 1 || string(txStore.actions[0].Payload) != string(payload) {
		t.Errorf("recorded action payload = %s, want verbatim %s", txStore.actions[0].Payload, payload)
	}
}

// TestApplyAction_Answered_StripsSuggestion_Accept and
// TestApplyAction_Answered_StripsSuggestion_Reject both pin the fold-side
// effect (workflow_triage.go's applyAnsweredSideEffect): detail.attrs.suggestion
// is dropped regardless of accept/reject. The reject case is the one the
// design doc calls out by name — khi's real suggestion_answered claims are
// 25/25 "accept" and 0 "reject" (実測, 2026-08-19), so the reject path had
// never actually been exercised anywhere before this PR.
func TestApplyAction_Answered_StripsSuggestion_Accept(t *testing.T) {
	testApplyActionAnsweredStripsSuggestion(t, "accept")
}

func TestApplyAction_Answered_StripsSuggestion_Reject(t *testing.T) {
	testApplyActionAnsweredStripsSuggestion(t, "reject")
}

func testApplyActionAnsweredStripsSuggestion(t *testing.T, answer string) {
	t.Helper()
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task: task,
		triage: map[string]*orchestrator.TaskTriage{
			"t1": {
				TaskID: "t1",
				Detail: json.RawMessage(`{"attrs":{"suggestion":{"verb":"go","basis":"issue #42"},"kept_key":"kept_value"}}`),
			},
		},
	}
	svc := newTriageWorkflowService(task, txStore)

	payload, _ := json.Marshal(map[string]string{"answer": answer, "verb": "go", "basis": "issue #42"})
	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "answered", Payload: payload})
	if err != nil {
		t.Fatalf("ApplyAction(answered, answer=%s): %v", answer, err)
	}
	if result.Task.Status != orchestrator.TaskStatusTriaged {
		t.Fatalf("status = %q, want unchanged (triaged)", result.Task.Status)
	}

	tt := txStore.triage["t1"]
	if tt == nil {
		t.Fatal("expected task_triage row to still exist")
	}
	suggestion, ok := orchestrator.DetailSuggestion(tt.Detail)
	if ok || suggestion.Verb != "" {
		t.Errorf("answer=%s: suggestion still present after answered: %+v", answer, suggestion)
	}
	var m map[string]map[string]json.RawMessage
	if err := json.Unmarshal(tt.Detail, &m); err != nil {
		t.Fatalf("parse detail: %v", err)
	}
	if _, present := m["attrs"]["kept_key"]; !present {
		t.Errorf("answer=%s: unrelated attrs key was dropped too, want only suggestion removed: %s", answer, tt.Detail)
	}
	if txStore.updatedTask != nil {
		t.Fatalf("answered called tx.UpdateTask (updatedTask=%+v), want no task-row write", txStore.updatedTask)
	}
	if string(result.Task.Payload) != `{}` {
		t.Fatalf("task.Payload = %s, want untouched (answered must not merge into task.Payload)", result.Task.Payload)
	}
}

// TestApplyAction_Answered_NoTaskTriageRow_RejectedByMachineFor pins a PR-B
// behavior change (docs/plans/suggestion-as-state-transition-impl.md §2):
// before the machine split, `answered` was reachable via the public
// ApplyAction path against ANY task sitting in a preExecutionStatuses status
// (`boid action send --type answered`), regardless of whether that task
// actually carried a task_triage sidecar row — the unified machine matched
// purely on FromStatus, never checking the sidecar for ELIGIBILITY (only
// applyAnsweredSideEffect's own side-effect checked it, and tolerated a
// miss). Now that machineFor picks NewCardMachine vs NewExecutionMachine by
// sidecar existence, a task with no row is routed to NewExecutionMachine —
// which has no "triaged" status or "answered" rule at all — so the request
// is rejected with 400 before ever reaching applyAnsweredSideEffect. In
// practice every real card gets its sidecar row seeded at creation
// (task_create.go's CreateTask / task_resolve_or_capture.go's
// ResolveOrCapture both call SeedTaskTriage unconditionally for any
// pre-execution initial_status), so this only affects a task manufactured
// directly into a pre-execution status with no sidecar — not a real
// production path today. See TestApplyAnsweredSideEffect_NoExistingTriageRow_NoOp
// below for the (still-preserved) invariant this test used to pin: the
// side-effect function ITSELF, called directly, still tolerates a missing
// row without creating a phantom one.
func TestApplyAction_Answered_NoTaskTriageRow_RejectedByMachineFor(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		// TaskTriage deliberately wired (not nil) so machineFor performs a
		// REAL lookup and finds no row — as opposed to falling back on a nil
		// store, which would be a different (and less interesting) reason
		// to land on NewExecutionMachine.
		TaskTriage: txStore,
	}

	payload, _ := json.Marshal(map[string]string{"answer": "reject"})
	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "answered", Payload: payload})
	if err == nil {
		t.Fatal("expected rejection: a task with no task_triage row is routed to NewExecutionMachine, which has no \"answered\" rule")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 StatusError, got %v", err)
	}
	if txStore.triage["t1"] != nil {
		t.Fatalf("expected NO task_triage row to be created, got: %+v", txStore.triage["t1"])
	}
}

// TestApplyAnsweredSideEffect_NoExistingTriageRow_NoOp pins Opus review
// finding #4 (2026-08-19 revisit of PR-3), decoupled from ApplyAction's own
// routing (see the PR-B note on
// TestApplyAction_Answered_NoTaskTriageRow_RejectedByMachineFor above for why
// that decoupling is now necessary): applyAnsweredSideEffect's
// GetTaskTriage-miss path (sql.ErrNoRows) must NOT create an empty
// task_triage row. Unlike applyAttrsSetSideEffect (which keeps creating an
// empty row on miss — see its own doc comment) `answered` has nothing
// positive to write when there is no pre-existing row: its only side effect
// is stripping detail.attrs.suggestion, and a row that never existed has no
// suggestion to strip, so skipping the row entirely loses no information.
// That asymmetry with attrs_set is deliberate, not an inconsistency —
// attrs_set's patch always carries real triage data (urgency/kind/
// suggestion) to write, so creating the row there is establishing real
// content, not a bare side effect.
func TestApplyAnsweredSideEffect_NoExistingTriageRow_NoOp(t *testing.T) {
	txStore := &recordingTxStore{}
	if err := applyAnsweredSideEffect(txStore, "t1"); err != nil {
		t.Fatalf("applyAnsweredSideEffect with no pre-existing task_triage row: %v", err)
	}
	if txStore.triage["t1"] != nil {
		t.Fatalf("expected NO task_triage row to be created for a task with no pre-existing row, got: %+v", txStore.triage["t1"])
	}
}
