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

// TestApplyAction_Answered_NoExistingTriageRow_StillSucceeds pins that
// applyAnsweredSideEffect's GetTaskTriage-miss path (sql.ErrNoRows) is
// tolerated the same way applyAttrsSetSideEffect's is — a task_triage row is
// created (empty) rather than erroring.
func TestApplyAction_Answered_NoExistingTriageRow_StillSucceeds(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	payload, _ := json.Marshal(map[string]string{"answer": "reject"})
	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "answered", Payload: payload}); err != nil {
		t.Fatalf("ApplyAction(answered) with no pre-existing task_triage row: %v", err)
	}
	if txStore.triage["t1"] == nil {
		t.Fatal("expected an (empty) task_triage row to be created")
	}
}
