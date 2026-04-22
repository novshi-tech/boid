package orchestrator_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- DefaultMachine: manual transitions ----

func TestDefaultMachine_PendingToExecuting(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusPending}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "start"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if next.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("expected executing, got %s", next.Status)
	}
}

func TestDefaultMachine_ExecutingToDone_Manual(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "done"})
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestDefaultMachine_VerifyingToDone_Manual(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusVerifying}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "done"})
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestDefaultMachine_ReworkingToDone_Manual(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusReworking}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "done"})
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestDefaultMachine_Reopen_DoneToReworking(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusDone}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "reopen"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Fatalf("expected reworking, got %s", next.Status)
	}
}

func TestDefaultMachine_InvalidTransition(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusPending}
	_, err := sm.Apply(task, &orchestrator.Action{Type: "done"})
	if err == nil {
		t.Fatal("expected error for invalid transition pending -> done")
	}
	if !strings.Contains(err.Error(), "no transition") {
		t.Fatalf("expected no transition error, got: %v", err)
	}
}

func TestDefaultMachine_Abort_FromAnyState(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusVerifying,
		orchestrator.TaskStatusReworking,
	}
	for _, status := range statuses {
		task := &orchestrator.Task{Status: status}
		next, err := sm.Apply(task, &orchestrator.Action{Type: "abort"})
		if err != nil {
			t.Fatalf("abort from %s: %v", status, err)
		}
		if next.Status != orchestrator.TaskStatusAborted {
			t.Fatalf("expected aborted from %s, got %s", status, next.Status)
		}
	}
}

func TestDefaultMachine_JobFailed_FromAnyState(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusVerifying,
		orchestrator.TaskStatusReworking,
	}
	for _, status := range statuses {
		task := &orchestrator.Task{Status: status}
		next, err := sm.Apply(task, &orchestrator.Action{Type: "job_failed"})
		if err != nil {
			t.Fatalf("job_failed from %s: %v", status, err)
		}
		if next.Status != orchestrator.TaskStatusAborted {
			t.Fatalf("expected aborted from %s, got %s", status, next.Status)
		}
	}
}

// ---- DefaultMachine: auto transitions from executing ----

func TestDefaultMachine_Executing_TasksReady_Verifying(t *testing.T) {
	// tasks trait は artifact と対称に扱われ、executing → verifying に進む。
	// verifying で reviewer hook/gate を噛ませられる余地を残す設計。
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"tasks":[{"title":"subtask"}]}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to verifying when tasks ready")
	}
	if next.Status != orchestrator.TaskStatusVerifying {
		t.Fatalf("expected verifying, got %s", next.Status)
	}
}

func TestDefaultMachine_Executing_TasksReady_OpenExecutingFindings_Reworking(t *testing.T) {
	// plan タスクでも executing 段階の finding が残っていれば reworking に戻す。
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{
			"tasks":[{"title":"subtask"}],
			"verification":{
				"plan-sanity":{"source_state":"executing","findings":[{"message":"missing context","status":"open"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to reworking when tasks present but executing findings open")
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Fatalf("expected reworking, got %s", next.Status)
	}
}

func TestDefaultMachine_Executing_NoTasks_NoAdvance(t *testing.T) {
	// tasks trait が空配列のときは未完了扱いで advance しない。
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"tasks":[]}`),
	}
	if _, ok := sm.Advance(task); ok {
		t.Fatal("expected no advance when tasks array is empty")
	}
}

func TestDefaultMachine_Executing_Artifact_NoUnresolvedFindings_Verifying(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	// artifact present, no executing-state findings
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"}}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to verifying when artifact present and no unresolved executing findings")
	}
	if next.Status != orchestrator.TaskStatusVerifying {
		t.Fatalf("expected verifying, got %s", next.Status)
	}
}

func TestDefaultMachine_Executing_Artifact_AllExecutingResolved_Verifying(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"},
			"verification":{
				"pr-verify":{"source_state":"executing","findings":[{"message":"CI passed","status":"resolved"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to verifying when all executing findings resolved")
	}
	if next.Status != orchestrator.TaskStatusVerifying {
		t.Fatalf("expected verifying, got %s", next.Status)
	}
}

func TestDefaultMachine_Executing_Artifact_OpenExecutingFindings_Reworking(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"},
			"verification":{
				"pr-verify":{"source_state":"executing","findings":[{"message":"CI failed","status":"open"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to reworking when executing findings open")
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Fatalf("expected reworking, got %s", next.Status)
	}
}

func TestDefaultMachine_Executing_NoArtifact_NoAdvance(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{}`),
	}
	_, ok := sm.Advance(task)
	if ok {
		t.Fatal("expected no advance when no artifact and no tasks")
	}
}

// ---- DefaultMachine: auto transitions from verifying ----

func TestDefaultMachine_Verifying_OpenFindings_Reworking(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusVerifying,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://..."},
			"verification":{
				"verify-gate":{"source_state":"verifying","findings":[{"message":"needs fix","status":"open"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to reworking when verifying findings open")
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Fatalf("expected reworking, got %s", next.Status)
	}
}

func TestDefaultMachine_Verifying_AllResolved_Done(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusVerifying,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://..."},
			"verification":{
				"verify-gate":{"source_state":"verifying","findings":[{"message":"looks good","status":"resolved"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when verifying findings resolved")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestDefaultMachine_Verifying_NoFindings_PassThrough_Done(t *testing.T) {
	// verify gate を持たない単純タスク: executing → verifying → done の pass-through
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusVerifying,
		Payload: json.RawMessage(`{"artifact":{"pr_url":"https://..."}}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when no verifying-state findings (pass-through)")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

// ---- DefaultMachine: auto transitions from reworking ----

func TestDefaultMachine_Reworking_AllResolved_TransitionsToVerifying(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://..."},
			"verification":{
				"pr-verify":{"source_state":"reworking","findings":[{"message":"CI passed","status":"resolved"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to verifying when all findings resolved")
	}
	if next.Status != orchestrator.TaskStatusVerifying {
		t.Fatalf("expected verifying, got %s", next.Status)
	}
}

func TestDefaultMachine_Reworking_OpenFindings_SelfLoop(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://..."},
			"verification":{
				"pr-verify":{"source_state":"reworking","findings":[{"message":"CI still failing","status":"open"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected self-loop when unresolved findings in reworking")
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Fatalf("expected reworking (self-loop), got %s", next.Status)
	}
}

func TestDefaultMachine_Reworking_NoFindings_TransitionsToVerifying(t *testing.T) {
	// 検証エントリが一切ない場合: NoUnresolvedFindings() = true → verifying
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{"artifact":{"pr_url":"https://..."}}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to verifying when no findings exist")
	}
	if next.Status != orchestrator.TaskStatusVerifying {
		t.Fatalf("expected verifying, got %s", next.Status)
	}
}

func TestDefaultMachine_Reworking_VerifyingSourceOpen_TransitionsToVerifying(t *testing.T) {
	// reworking → verifying の判定は source_state=reworking のみを見る。
	// verifying-source の open finding は verifying 再入場時に gate が
	// 再実行されて subkey を上書きする設計なので、reworking を抜ける条件にしない。
	// （全 source を見るとデッドロックする: mergeable-check による rework ループ不具合）
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://..."},
			"verification":{
				"gate-a":{"source_state":"verifying","findings":[{"message":"issue","status":"open"}]},
				"gate-b":{"source_state":"reworking","findings":[{"message":"ok","status":"resolved"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected transition when no reworking-source open findings")
	}
	if next.Status != orchestrator.TaskStatusVerifying {
		t.Fatalf("expected verifying, got %s", next.Status)
	}
}

func TestDefaultMachine_ReworkCycle_VerifyingReworkingVerifyingDone(t *testing.T) {
	sm := orchestrator.DefaultMachine()

	// 1. verifying → reworking (open finding at verifying)
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusVerifying,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://..."},
			"verification":{
				"reviewer":{"source_state":"verifying","findings":[{"message":"fix this","status":"open"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok || next.Status != orchestrator.TaskStatusReworking {
		t.Fatalf("step 1: expected reworking, got %s (ok=%v)", safeStatus(next), ok)
	}

	// 2. reworking → verifying: rework hook が走って reworking-source の finding が全て resolved に
	//    なれば、verifying-source の open finding が残っていても verifying に戻す。
	//    verifying 再入場時に同じ gate (例: mergeable-check) が再実行されて subkey を上書きする設計。
	next.Status = orchestrator.TaskStatusReworking
	next.Payload = json.RawMessage(`{
		"artifact":{"pr_url":"https://..."},
		"verification":{
			"reviewer":{"source_state":"verifying","findings":[{"message":"fix this","status":"open"}]},
			"pr-verify":{"source_state":"reworking","findings":[{"message":"CI passed","status":"resolved"}]}
		}
	}`)
	step2, ok := sm.Advance(next)
	if !ok || step2.Status != orchestrator.TaskStatusVerifying {
		t.Fatalf("step 2: expected verifying (reworking-source resolved), got %s (ok=%v)", safeStatus(step2), ok)
	}

	// 3. verifying → done: verifying gate が再実行され subkey が上書きされて findings が空になる想定。
	step2.Payload = json.RawMessage(`{
		"artifact":{"pr_url":"https://..."},
		"verification":{
			"reviewer":{"source_state":"verifying","findings":[]},
			"pr-verify":{"source_state":"reworking","findings":[{"message":"CI passed","status":"resolved"}]}
		}
	}`)
	step3, ok := sm.Advance(step2)
	if !ok || step3.Status != orchestrator.TaskStatusDone {
		t.Fatalf("step 3: expected done, got %s (ok=%v)", safeStatus(step3), ok)
	}
}

func safeStatus(t *orchestrator.Task) orchestrator.TaskStatus {
	if t == nil {
		return ""
	}
	return t.Status
}

// ---- DefaultMachine: AvailableActions ----

func TestDefaultMachine_AvailableActions_Pending(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusPending)
	want := map[string]bool{"start": true, "abort": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(pending) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(pending)", a)
		}
	}
}

func TestDefaultMachine_AvailableActions_Executing(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusExecuting)
	want := map[string]bool{"done": true, "abort": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(executing) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(executing)", a)
		}
	}
}

func TestDefaultMachine_AvailableActions_Verifying(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusVerifying)
	want := map[string]bool{"done": true, "abort": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(verifying) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(verifying)", a)
		}
	}
}

func TestDefaultMachine_AvailableActions_Reworking(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusReworking)
	want := map[string]bool{"done": true, "abort": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(reworking) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(reworking)", a)
		}
	}
}

func TestDefaultMachine_AvailableActions_DoneIsEmpty(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	for _, status := range []orchestrator.TaskStatus{orchestrator.TaskStatusDone, orchestrator.TaskStatusAborted} {
		if actions := sm.AvailableActions(status); len(actions) != 0 {
			t.Errorf("AvailableActions(%q) = %v, want empty", status, actions)
		}
	}
}

func TestDefaultMachine_AvailableActions_ExcludesJobFailed(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	for _, status := range []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusVerifying,
		orchestrator.TaskStatusReworking,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
	} {
		for _, a := range sm.AvailableActions(status) {
			if a == "job_failed" {
				t.Errorf("job_failed must not appear in AvailableActions(%q)", status)
			}
		}
	}
}

// ---- Generic StateMachine infrastructure tests ----

func TestStateMachine_Advance_ConditionMet(t *testing.T) {
	sm := &orchestrator.StateMachine{
		Name: "test",
		Rules: []orchestrator.Rule{
			{Action: "start", FromStatus: "pending", ToStatus: "executing"},
			{
				FromStatus: "executing",
				ToStatus:   "verifying",
				Condition: func(payload json.RawMessage) bool {
					var m map[string]json.RawMessage
					json.Unmarshal(payload, &m)
					_, ok := m["artifact"]
					return ok
				},
			},
		},
	}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"artifact":{"url":"https://github.com/..."}}`),
	}

	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected Advance to return ok=true")
	}
	if next.Status != orchestrator.TaskStatusVerifying {
		t.Fatalf("expected verifying, got %s", next.Status)
	}
}

func TestStateMachine_Apply_IgnoresConditionRules(t *testing.T) {
	sm := &orchestrator.StateMachine{
		Name: "test",
		Rules: []orchestrator.Rule{
			{
				FromStatus: "executing",
				ToStatus:   "verifying",
				Condition: func(payload json.RawMessage) bool {
					return true
				},
			},
		},
	}

	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	_, err := sm.Apply(task, &orchestrator.Action{Type: "verify"})
	if err == nil {
		t.Fatal("Apply should not match condition-based rules via action")
	}
}

// ---- DefaultMachine: auto transitions from executing (lifecycle.executed) ----

func TestDefaultMachine_Executing_EmptyPayload_LifecycleExecuted_Done(t *testing.T) {
	// lifecycle.executed=true かつ artifact も tasks も無い → done
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"lifecycle":{"executed":true,"rework_count":0}}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when lifecycle.executed=true and no artifact/tasks")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestDefaultMachine_Executing_LifecycleExecutedWithArtifact_Verifying(t *testing.T) {
	// lifecycle.executed=true かつ artifact あり → verifying (既存ルート維持)
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"lifecycle":{"executed":true,"rework_count":0},"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"}}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to verifying when lifecycle.executed=true and artifact present")
	}
	if next.Status != orchestrator.TaskStatusVerifying {
		t.Fatalf("expected verifying, got %s", next.Status)
	}
}

func TestDefaultMachine_Executing_NoLifecycleExecuted_NoTransition(t *testing.T) {
	// lifecycle.executed 未設定、成果物も無し → executing のまま（早期遷移しない）
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{}`),
	}
	if _, ok := sm.Advance(task); ok {
		t.Fatal("expected no advance when lifecycle.executed not set and no artifact/tasks")
	}
}

func TestDefaultMachine_Executing_LifecycleExecutedWithFindings_Done(t *testing.T) {
	// 空成果物時は findings があっても done（成果物が無いので rework する対象が無い）
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{
			"lifecycle":{"executed":true,"rework_count":0},
			"verification":{
				"some-gate":{"source_state":"executing","findings":[{"message":"something","status":"open"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when lifecycle.executed=true, no artifact, even with findings")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

// TestJobCompletedNotAnAction verifies that job_completed does not trigger a
// state transition in DefaultMachine. State transitions driven by hook/gate job
// completion must happen exclusively through DispatchAndAdvance (condition-based
// auto-advance), not through sm.Apply.
func TestJobCompletedNotAnAction(t *testing.T) {
	sm := orchestrator.DefaultMachine()

	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusReworking,
	}

	for _, status := range statuses {
		task := &orchestrator.Task{Status: status}
		_, err := sm.Apply(task, &orchestrator.Action{Type: "job_completed"})
		if err == nil {
			t.Errorf("job_completed from %q should not transition (got no error)", status)
		}
	}
}

// ---- NewMachine: rework limit abort ----

func TestNewMachine_ReworkLimit_AbortOnExceed(t *testing.T) {
	// rework_count=6 > limit=5 → aborted
	sm := orchestrator.NewMachine(5)
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{"lifecycle":{"executed":false,"rework_count":6},"artifact":{"pr_url":"https://..."}}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to aborted when rework_count exceeds limit")
	}
	if next.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("expected aborted, got %s", next.Status)
	}
}

func TestNewMachine_ReworkLimit_NoAbortAtLimit(t *testing.T) {
	// rework_count=5 == limit=5 → does NOT abort (> not >=)
	sm := orchestrator.NewMachine(5)
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{
			"lifecycle":{"executed":false,"rework_count":5},
			"artifact":{"pr_url":"https://..."},
			"verification":{
				"pr-verify":{"source_state":"reworking","findings":[{"message":"CI failing","status":"open"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance (self-loop) when rework_count equals limit")
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Fatalf("expected reworking (self-loop), got %s", next.Status)
	}
}

func TestNewMachine_ReworkLimit_ActionPayload(t *testing.T) {
	// abort due to rework_limit_exceeded should carry code in action payload
	sm := orchestrator.NewMachine(2)
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{"lifecycle":{"executed":false,"rework_count":3}}`),
	}
	outcome := sm.AdvanceFull(task)
	if outcome == nil {
		t.Fatal("expected AdvanceFull to return outcome")
	}
	if outcome.Task.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("expected aborted, got %s", outcome.Task.Status)
	}
	var m map[string]string
	if err := json.Unmarshal(outcome.ActionPayload, &m); err != nil {
		t.Fatalf("failed to parse action payload: %v", err)
	}
	if m["code"] != "rework_limit_exceeded" {
		t.Errorf("expected code=rework_limit_exceeded, got %q", m["code"])
	}
}

func TestNewMachine_ReworkLimit_ZeroReworkCount_NoAbort(t *testing.T) {
	// rework_count=0 → never exceeds limit
	sm := orchestrator.NewMachine(5)
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{"lifecycle":{"executed":false,"rework_count":0},"artifact":{"pr_url":"https://..."}}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to verifying (no findings)")
	}
	if next.Status == orchestrator.TaskStatusAborted {
		t.Fatal("expected not aborted when rework_count=0")
	}
}

// ---- NewMachine: fatal finding abort ----

func TestNewMachine_FatalFinding_AbortFromReworking(t *testing.T) {
	sm := orchestrator.NewMachine(5)
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{
			"lifecycle":{"executed":false,"rework_count":1},
			"verification":{
				"pr-verify":{"source_state":"reworking","findings":[{"message":"agent exited with no commit","status":"open","severity":"fatal"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to aborted on fatal finding")
	}
	if next.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("expected aborted, got %s", next.Status)
	}
}

func TestNewMachine_FatalFinding_AbortFromVerifying(t *testing.T) {
	sm := orchestrator.NewMachine(5)
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusVerifying,
		Payload: json.RawMessage(`{
			"verification":{
				"gate-a":{"source_state":"verifying","findings":[{"message":"critical failure","status":"open","severity":"fatal"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to aborted on fatal finding from verifying")
	}
	if next.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("expected aborted, got %s", next.Status)
	}
}

func TestNewMachine_FatalFinding_AbortFromExecuting(t *testing.T) {
	sm := orchestrator.NewMachine(5)
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://..."},
			"verification":{
				"pre-check":{"source_state":"executing","findings":[{"message":"unrecoverable error","status":"open","severity":"fatal"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to aborted on fatal finding from executing")
	}
	if next.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("expected aborted, got %s", next.Status)
	}
}

func TestNewMachine_FatalFinding_ActionPayload(t *testing.T) {
	// fatal finding abort should carry code=fatal_finding and message in action payload
	sm := orchestrator.NewMachine(5)
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusVerifying,
		Payload: json.RawMessage(`{
			"verification":{
				"gate-a":{"source_state":"verifying","findings":[{"message":"the fatal message","status":"open","severity":"fatal"}]}
			}
		}`),
	}
	outcome := sm.AdvanceFull(task)
	if outcome == nil {
		t.Fatal("expected AdvanceFull to return outcome")
	}
	if outcome.Task.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("expected aborted, got %s", outcome.Task.Status)
	}
	var m map[string]string
	if err := json.Unmarshal(outcome.ActionPayload, &m); err != nil {
		t.Fatalf("failed to parse action payload: %v", err)
	}
	if m["code"] != "fatal_finding" {
		t.Errorf("expected code=fatal_finding, got %q", m["code"])
	}
	if m["message"] != "the fatal message" {
		t.Errorf("expected message=%q, got %q", "the fatal message", m["message"])
	}
}

func TestNewMachine_FatalFinding_ResolvedDoesNotAbort(t *testing.T) {
	// fatal finding that is resolved should NOT trigger abort
	sm := orchestrator.NewMachine(5)
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusVerifying,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://..."},
			"verification":{
				"gate-a":{"source_state":"verifying","findings":[{"message":"was fatal but fixed","status":"resolved","severity":"fatal"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done (no open fatal findings)")
	}
	if next.Status == orchestrator.TaskStatusAborted {
		t.Fatal("expected not aborted when fatal finding is resolved")
	}
}

func TestNewMachine_FatalFinding_TakesPriorityOverReworkLimit(t *testing.T) {
	// fatal finding abort fires before rework_limit abort (rule order check)
	sm := orchestrator.NewMachine(5)
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{
			"lifecycle":{"executed":false,"rework_count":6},
			"verification":{
				"gate-a":{"source_state":"reworking","findings":[{"message":"fatal","status":"open","severity":"fatal"}]}
			}
		}`),
	}
	outcome := sm.AdvanceFull(task)
	if outcome == nil {
		t.Fatal("expected outcome")
	}
	if outcome.Task.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("expected aborted, got %s", outcome.Task.Status)
	}
	var m map[string]string
	if err := json.Unmarshal(outcome.ActionPayload, &m); err != nil {
		t.Fatalf("failed to parse action payload: %v", err)
	}
	// fatal_finding rule fires first
	if m["code"] != "fatal_finding" {
		t.Errorf("expected fatal_finding to take priority, got code=%q", m["code"])
	}
}

// ---- DefaultMachine: default rework_limit ----

func TestDefaultMachine_DefaultReworkLimit_NotAbortedAtFive(t *testing.T) {
	// デフォルト rework_limit=5: rework_count=5 は abort しない
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{
			"lifecycle":{"executed":false,"rework_count":5},
			"artifact":{"pr_url":"https://..."},
			"verification":{
				"pr-verify":{"source_state":"reworking","findings":[{"message":"still failing","status":"open"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance (self-loop or similar)")
	}
	if next.Status == orchestrator.TaskStatusAborted {
		t.Fatal("expected not aborted at rework_count=5 (limit=5, needs >5)")
	}
}

func TestDefaultMachine_DefaultReworkLimit_AbortedAtSix(t *testing.T) {
	// デフォルト rework_limit=5: rework_count=6 は abort する
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{"lifecycle":{"executed":false,"rework_count":6}}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to aborted")
	}
	if next.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("expected aborted at rework_count=6, got %s", next.Status)
	}
}
