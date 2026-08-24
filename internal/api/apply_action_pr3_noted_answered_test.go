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
		task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
		txStore := &recordingTxStore{task: task}
		svc := newTriageWorkflowService(task, txStore)

		result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
			Type:    "noted",
			Payload: []byte(payload),
		})
		if err != nil {
			t.Fatalf("ApplyAction(noted) payload=%s: unexpected error: %v", payload, err)
		}
		if result.Task.Status != orchestrator.TaskStatusParked {
			t.Fatalf("payload=%s: status = %q, want unchanged (parked)", payload, result.Task.Status)
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
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
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
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
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
		task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
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
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "answered"})
	if err == nil {
		t.Fatal("ApplyAction(answered) with no payload = nil error, want a rejection")
	}
}

// TestApplyAction_Answered_VerbBasisRecordedButNotValidated pins J-7: the
// answered PAYLOAD's own verb/basis fields (what the Web UI's hidden form
// fields last displayed) are recorded verbatim, even when they don't match
// the CARD's real current suggestion — the daemon never cross-checks them.
// Unlike v1 (where accept never applied anything, so the payload's verb was
// pure metadata), v2's accept(verb) actually decides what transition fires
// by reading the REAL suggestion from task_triage.detail (§3.1) — never from
// this payload — so this test sets up a real suggestion (verb="drop") that
// disagrees with the payload's fabricated verb to prove which one wins.
func TestApplyAction_Answered_VerbBasisRecordedButNotValidated(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task: task,
		triage: map[string]*orchestrator.TaskTriage{
			"t1": {TaskID: "t1", Detail: json.RawMessage(`{"attrs":{"suggestion":{"verb":"drop"}}}`)},
		},
	}
	svc := newTriageWorkflowService(task, txStore)

	payload := []byte(`{"answer":"accept","verb":"totally-made-up-verb","basis":""}`)
	ctx := orchestrator.WithActor(context.Background(), orchestrator.ActorHuman)
	result, err := svc.ApplyAction(ctx, task.ID, ApplyActionRequest{Type: "answered", Payload: payload})
	if err != nil {
		t.Fatalf("ApplyAction(answered): unexpected error for an unvetted payload verb: %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusDropped {
		t.Fatalf("status = %q, want dropped (driven by the REAL suggestion's verb, not the payload's fabricated one)", result.Task.Status)
	}

	var found bool
	for _, a := range txStore.actions {
		if a.Type == "answered" {
			found = true
			if string(a.Payload) != string(payload) {
				t.Errorf("recorded answered action payload = %s, want verbatim %s", a.Payload, payload)
			}
		}
	}
	if !found {
		t.Fatalf("expected an \"answered\" action recorded, got actions=%+v", txStore.actions)
	}
}

// TestApplyAction_Answered_Accept_RejectedForNonHumanActor pins 穴11's OTHER
// half (suggestion_accept.go's own doc comment): a non-human actor cannot
// bypass the direct-verb push-down defense
// (apply_action_phase1_test.go's TestApplyAction_CardTransitions_
// RejectedForNonHumanActor) by wrapping the same verb inside
// answered{accept} instead — "answered" itself is non-transitioning and not
// in orchestrator.IsCardTransitionAction's set, so the generic
// ApplyAction-level guard alone would let it through; applyAnswered must
// enforce this independently. reject is deliberately NOT covered here (it
// never transitions, so it stays reachable from any actor — see
// TestApplyAction_Answered_StripsSuggestion_Reject).
func TestApplyAction_Answered_Accept_RejectedForNonHumanActor(t *testing.T) {
	actorCases := []struct {
		name string
		ctx  context.Context
	}{
		{"khi_trigger_job_empty_task_id", orchestrator.WithActor(context.Background(), orchestrator.ActorTask(""))},
		{"khi_via_task", orchestrator.WithActor(context.Background(), orchestrator.ActorTask("some-task-id"))},
		{"daemon", orchestrator.WithActor(context.Background(), orchestrator.ActorDaemon)},
		{"unset", context.Background()},
	}
	for _, a := range actorCases {
		t.Run(a.name, func(t *testing.T) {
			task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
			txStore := &recordingTxStore{
				task: task,
				triage: map[string]*orchestrator.TaskTriage{
					"t1": {TaskID: "t1", Detail: json.RawMessage(`{"attrs":{"suggestion":{"verb":"go"}}}`)},
				},
			}
			svc := newTriageWorkflowService(task, txStore)

			payload := []byte(`{"answer":"accept","verb":"go"}`)
			_, err := svc.ApplyAction(a.ctx, task.ID, ApplyActionRequest{Type: "answered", Payload: payload})
			if err == nil {
				t.Fatalf("%s: expected rejection of answered{accept} from a non-human actor", a.name)
			}
			se, ok := err.(*StatusError)
			if !ok || se.Code != http.StatusForbidden {
				t.Fatalf("%s: expected 403 StatusError, got %v", a.name, err)
			}
			if txStore.updatedTask != nil {
				t.Fatalf("%s: the go transition must not have committed, got updatedTask=%+v", a.name, txStore.updatedTask)
			}
			for _, act := range txStore.actions {
				if act.Type == "answered" {
					t.Fatalf("%s: the answered action must not be recorded either — rejected before any Tx opens, got %+v", a.name, act)
				}
			}
		})
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
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
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
	ctx := orchestrator.WithActor(context.Background(), orchestrator.ActorHuman)
	result, err := svc.ApplyAction(ctx, task.ID, ApplyActionRequest{Type: "answered", Payload: payload})
	if err != nil {
		t.Fatalf("ApplyAction(answered, answer=%s): %v", answer, err)
	}

	// reject never transitions (unchanged from v1); accept applies the
	// suggestion's own verb ("go": parked->working, no specced children in
	// this fixture so acceptGo needs no TaskCreator) BEFORE stripping it
	// (design doc §3.1) — a real behavior change from v1, where accept was a
	// no-op besides the strip.
	wantStatus := orchestrator.TaskStatusParked
	if answer == answeredAnswerAccept {
		wantStatus = orchestrator.TaskStatusWorking
	}
	if result.Task.Status != wantStatus {
		t.Fatalf("answer=%s: status = %q, want %q", answer, result.Task.Status, wantStatus)
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
	if answer == answeredAnswerReject && txStore.updatedTask != nil {
		t.Fatalf("reject called tx.UpdateTask (updatedTask=%+v), want no task-row write (reject never transitions)", txStore.updatedTask)
	}
	if answer == answeredAnswerAccept && (txStore.updatedTask == nil || txStore.updatedTask.Status != orchestrator.TaskStatusWorking) {
		t.Fatalf("accept must commit the go transition via tx.UpdateTask, got updatedTask=%+v", txStore.updatedTask)
	}
	if string(result.Task.Payload) != `{}` {
		t.Fatalf("task.Payload = %s, want untouched (answered must not merge into task.Payload)", result.Task.Payload)
	}
}

// TestApplyAction_Answered_NoTaskTriageRow_PreExecutionStatus_StillSucceeds
// pins the FULL round trip of PR #986 review's Blocker 1 fix, for the
// `answered` verb specifically: `answered` is Manual:true, so (pre-PR-B) it
// was reachable via the public ApplyAction path against ANY task sitting in
// a preExecutionStatuses status (`boid action send --type answered`),
// regardless of whether that task actually carried a task_triage sidecar
// row — the unified machine matched purely on FromStatus, never checking
// the sidecar for ELIGIBILITY (only applyAnsweredSideEffect's own
// side-effect checked it, and tolerated a miss).
//
// An EARLIER version of PR-B's machineFor collapsed "no confirmed sidecar
// row" straight to NewExecutionMachine — which has no "triaged" status or
// "answered" rule at all — so this exact scenario 400'd before ever reaching
// applyAnsweredSideEffect (a real, if narrow, regression the PR #986 review
// caught as part of Blocker 1: a task in a pre-execution status can
// legitimately have no row yet, since task_create.go's SeedTaskTriage is
// deliberately best-effort at creation time). machineFor's Blocker-1 fix
// (machine_select.go's isCardLifecycleStatus fallback) restores
// NewCardMachine for exactly this case, which restores this original
// behavior: the request succeeds, and — per
// TestApplyAnsweredSideEffect_NoExistingTriageRow_NoOp below —
// applyAnsweredSideEffect's own no-op-on-miss behavior means no phantom row
// gets created either.
func TestApplyAction_Answered_NoTaskTriageRow_PreExecutionStatus_StillSucceeds(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		// TaskTriage deliberately wired (not nil) so machineFor performs a
		// REAL lookup and confirms no row — as opposed to falling back on a
		// nil store, which would be a different (and less interesting)
		// reason to land on the same isCardLifecycleStatus fallback.
		TaskTriage: txStore,
	}

	payload, _ := json.Marshal(map[string]string{"answer": "reject"})
	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "answered", Payload: payload}); err != nil {
		t.Fatalf("ApplyAction(answered) with no pre-existing task_triage row: %v", err)
	}
	if txStore.triage["t1"] != nil {
		t.Fatalf("expected NO task_triage row to be created (applyAnsweredSideEffect's own no-op-on-miss behavior), got: %+v", txStore.triage["t1"])
	}
}

// TestApplyAction_Answered_AcceptReopen_FromDoneAndDropped is the api-layer
// end-to-end pin for PR #987 review's BLOCKER 3: a done/dropped card
// carrying a khi-suggested "reopen" must actually be acceptable — before
// this fix, "answered" was rejected from done/dropped entirely (a shared
// FromStatus set with attrs_set/child_added/etc that never reached a
// terminal card), so a card stuck in this exact situation could never be
// answered at all, accept or reject.
func TestApplyAction_Answered_AcceptReopen_FromDoneAndDropped(t *testing.T) {
	for _, from := range []orchestrator.TaskStatus{orchestrator.TaskStatusDone, orchestrator.TaskStatusDropped} {
		t.Run(string(from), func(t *testing.T) {
			task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: from, Behavior: "dev", Payload: []byte(`{}`)}
			txStore := &recordingTxStore{
				task: task,
				triage: map[string]*orchestrator.TaskTriage{
					"t1": {TaskID: "t1", Detail: json.RawMessage(`{"attrs":{"suggestion":{"verb":"reopen","reason":"issue reopened"}}}`)},
				},
			}
			// Deliberately NOT newTriageWorkflowService: done/dropped are
			// excluded from isPreExecutionCardStatus's auto-seed set (that
			// helper's own doc comment — machineFor needs a CONFIRMED row
			// for done/aborted specifically, not the status-based fallback),
			// so TaskTriage must be wired by hand here, matching the fixture
			// that actually exercises the confirmed-row path.
			svc := &TaskWorkflowService{
				Tasks:      &stubTaskStore{task: task},
				Tx:         recordingTransactor{store: txStore},
				Meta:       stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
				TaskTriage: txStore,
			}

			payload, _ := json.Marshal(map[string]string{"answer": answeredAnswerAccept, "verb": "reopen"})
			ctx := orchestrator.WithActor(context.Background(), orchestrator.ActorHuman)
			result, err := svc.ApplyAction(ctx, task.ID, ApplyActionRequest{Type: "answered", Payload: payload})
			if err != nil {
				t.Fatalf("ApplyAction(answered, accept reopen) from %s: %v", from, err)
			}
			if result.Task.Status != orchestrator.TaskStatusParked {
				t.Fatalf("status = %q, want parked", result.Task.Status)
			}
			suggestion, ok := orchestrator.DetailSuggestion(txStore.triage["t1"].Detail)
			if ok || suggestion.Verb != "" {
				t.Errorf("suggestion still present after accept: %+v", suggestion)
			}
		})
	}
}

// TestApplyAnsweredSideEffect_NoExistingTriageRow_NoOp pins Opus review
// finding #4 (2026-08-19 revisit of PR-3), decoupled from ApplyAction's own
// routing (see the PR-B note on
// TestApplyAction_Answered_NoTaskTriageRow_PreExecutionStatus_StillSucceeds
// above — the two now assert compatible things at different layers, but are
// kept as separate tests so a future regression in EITHER machineFor's
// routing OR the side effect's own no-op-on-miss behavior fails by name):
// applyAnsweredSideEffect's
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
