package orchestrator_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- Manual transitions ----

func TestExecutionMachine_PendingToExecuting(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusPending}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "start"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if next.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("expected executing, got %s", next.Status)
	}
}

func TestExecutionMachine_Reopen_DoneToExecuting(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusDone}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "reopen"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if next.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("expected executing, got %s", next.Status)
	}
}

func TestExecutionMachine_InvalidTransition(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusPending}
	_, err := sm.Apply(task, &orchestrator.Action{Type: "done"})
	if err == nil {
		t.Fatal("expected error for invalid transition pending -> done")
	}
	if !strings.Contains(err.Error(), "no transition") {
		t.Fatalf("expected no transition error, got: %v", err)
	}
}

func TestExecutionMachine_ExecutingToAwaiting(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "ask"})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if next.Status != orchestrator.TaskStatusAwaiting {
		t.Fatalf("expected awaiting, got %s", next.Status)
	}
}

func TestExecutionMachine_AwaitingToExecuting(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusAwaiting}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "answer"})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if next.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("expected executing, got %s", next.Status)
	}
}

// TestExecutionMachine_AwaitingToDone verifies that a parent supervisor can
// approve a child's `done_request` (= terminate the awaiting child) via
// `boid action send --type done`. The transition is the canonical
// down-action documented in docs/plans/lifecycle-accountability.md and
// boid-task/SKILL.md (Supervisor Mode); without it the supervisor falls back
// to `boid task answer` which forces a wasteful agent re-spawn.
func TestExecutionMachine_AwaitingToDone(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusAwaiting}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "done"})
	if err != nil {
		t.Fatalf("done from awaiting: %v", err)
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

// TestExecutionMachine_FailFromExecuting verifies that an executor reporting an
// unrecoverable failure transitions directly to aborted. The `fail` action is
// the up-event canonical counterpart of `done` from executing: the agent
// self-reports the outcome and the parent supervisor's polling decides
// recovery (reopen / leave aborted). Symmetric to `done: executing → done`.
func TestExecutionMachine_FailFromExecuting(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "fail"})
	if err != nil {
		t.Fatalf("fail from executing: %v", err)
	}
	if next.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("expected aborted, got %s", next.Status)
	}
}

// TestExecutionMachine_Reopen_AbortedToExecuting verifies that the parent
// supervisor can recover a failed child via `boid task reopen` — symmetric
// to reopen from done. Without this transition `--fail` would be a dead-end
// and failure_report's "Recoverable with a hint" path could not be expressed
// without first un-aborting through some other mechanism.
func TestExecutionMachine_Reopen_AbortedToExecuting(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusAborted}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "reopen"})
	if err != nil {
		t.Fatalf("reopen from aborted: %v", err)
	}
	if next.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("expected executing, got %s", next.Status)
	}
}

func TestExecutionMachine_InvalidTransition_PendingToAwaiting(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusPending}
	_, err := sm.Apply(task, &orchestrator.Action{Type: "ask"})
	if err == nil {
		t.Fatal("expected error: ask from pending is invalid")
	}
}

func TestExecutionMachine_Abort_FromAnyState(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
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

func TestExecutionMachine_JobFailed_FromAnyState(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
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

// ---- Auto transitions ----

func TestExecutionMachine_Executing_LifecycleExecuted_Done(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{
		Type:   orchestrator.TaskTypeExecution,
		Status: orchestrator.TaskStatusExecuting,
		Exec:   &orchestrator.ExecAttrs{Payload: json.RawMessage(`{"lifecycle":{"executed":true}}`)},
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when lifecycle.executed=true")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestExecutionMachine_Executing_NoLifecycleExecuted_NoTransition(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{
		Type:   orchestrator.TaskTypeExecution,
		Status: orchestrator.TaskStatusExecuting,
		Exec:   &orchestrator.ExecAttrs{Payload: json.RawMessage(`{}`)},
	}
	if _, ok := sm.Advance(task); ok {
		t.Fatal("expected no advance when lifecycle.executed not set")
	}
}

// When the agent has reported via notify --done (recorded as done_request,
// surfaced as lifecycle.done) AND the runtime has cleanly completed
// (lifecycle.executed), the auto-advance fires the executing→done path and
// the resulting action carries the agent's message.
func TestExecutionMachine_LifecycleDone_AutoAdvanceWithMessage(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{
		Type:   orchestrator.TaskTypeExecution,
		Status: orchestrator.TaskStatusExecuting,
		Exec: &orchestrator.ExecAttrs{Payload: json.RawMessage(
			`{"lifecycle":{"executed":true,"done":{"message":"PR #439 merged"}}}`)},
	}
	outcome := sm.AdvanceFull(task)
	if outcome == nil {
		t.Fatal("expected advance, got nil")
	}
	if outcome.Task.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected status=done, got %s", outcome.Task.Status)
	}
	var m map[string]string
	if err := json.Unmarshal(outcome.ActionPayload, &m); err != nil {
		t.Fatalf("ActionPayload not parseable: %v (%s)", err, outcome.ActionPayload)
	}
	if m["message"] != "PR #439 merged" {
		t.Errorf("ActionPayload.message = %q, want %q", m["message"], "PR #439 merged")
	}
}

// Symmetric path for lifecycle.fail → executing→aborted with the message
// preserved on the auto_advance action.
func TestExecutionMachine_LifecycleFail_AutoAdvanceToAborted(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{
		Type:   orchestrator.TaskTypeExecution,
		Status: orchestrator.TaskStatusExecuting,
		Exec: &orchestrator.ExecAttrs{Payload: json.RawMessage(
			`{"lifecycle":{"executed":true,"fail":{"message":"tests broken"}}}`)},
	}
	outcome := sm.AdvanceFull(task)
	if outcome == nil {
		t.Fatal("expected advance, got nil")
	}
	if outcome.Task.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("expected status=aborted, got %s", outcome.Task.Status)
	}
	var m map[string]string
	if err := json.Unmarshal(outcome.ActionPayload, &m); err != nil {
		t.Fatalf("ActionPayload not parseable: %v (%s)", err, outcome.ActionPayload)
	}
	if m["message"] != "tests broken" {
		t.Errorf("ActionPayload.message = %q, want %q", m["message"], "tests broken")
	}
}

// Rule ordering: lifecycle.fail must take precedence over the bare
// lifecycle.executed → done fallback. If both rules could fire, evaluating
// `done` first would silently turn a fail report into a success.
func TestExecutionMachine_LifecycleFail_TakesPrecedenceOverBareExecuted(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{
		Type:   orchestrator.TaskTypeExecution,
		Status: orchestrator.TaskStatusExecuting,
		Exec: &orchestrator.ExecAttrs{Payload: json.RawMessage(
			`{"lifecycle":{"executed":true,"fail":{"message":"x"}}}`)},
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance, got none")
	}
	if next.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("expected aborted (fail wins), got %s", next.Status)
	}
}

// done_request / fail_request are recorded as non-transitioning actions. The
// state machine must accept them (no error) but leave the task status
// unchanged so NotifyTask can persist the intent without re-entering the
// state machine.
func TestExecutionMachine_DoneRequest_NonTransitioning(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "done_request"})
	if err != nil {
		t.Fatalf("done_request must be a recognized noop: %v", err)
	}
	if next.Status != orchestrator.TaskStatusExecuting {
		t.Errorf("expected status unchanged, got %s", next.Status)
	}
}

func TestExecutionMachine_FailRequest_NonTransitioning(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "fail_request"})
	if err != nil {
		t.Fatalf("fail_request must be a recognized noop: %v", err)
	}
	if next.Status != orchestrator.TaskStatusExecuting {
		t.Errorf("expected status unchanged, got %s", next.Status)
	}
}

// ---- AvailableActions ----

func TestExecutionMachine_AvailableActions_Pending(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
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

func TestExecutionMachine_AvailableActions_Executing(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusExecuting)
	want := map[string]bool{"done": true, "fail": true, "ask": true, "abort": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(executing) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(executing)", a)
		}
	}
}

func TestExecutionMachine_AvailableActions_Awaiting(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusAwaiting)
	want := map[string]bool{"answer": true, "done": true, "abort": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(awaiting) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(awaiting)", a)
		}
	}
}

func TestExecutionMachine_AvailableActions_Done(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusDone)
	want := map[string]bool{"reopen": true, "abort": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(done) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(done)", a)
		}
	}
}

// TestExecutionMachine_AvailableActions_Aborted verifies that aborted tasks
// can be reopened. This is the recovery path for `--fail` (executing →
// aborted): supervisor inspects the failure_report, then either reopens
// with a hint or leaves the task aborted as final.
func TestExecutionMachine_AvailableActions_Aborted(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusAborted)
	want := map[string]bool{"reopen": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(aborted) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(aborted)", a)
		}
	}
}

func TestExecutionMachine_AvailableActions_ExcludesJobFailed(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	for _, status := range []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
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

// TestJobCompletedNotAnAction verifies that job_completed does not trigger a
// state transition. State transitions driven by hook completion happen via
// auto-advance (lifecycle.executed condition), not through sm.Apply.
func TestJobCompletedNotAnAction(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	_, err := sm.Apply(task, &orchestrator.Action{Type: "job_completed"})
	if err == nil {
		t.Errorf("job_completed should not transition (got no error)")
	}
}

// 既存 reopen (done/aborted → executing) に card 機械側の第3の FromStatus
// (dropped → triaged, machine_card.go) が増えても、この機械自身の既存遷移は
// 壊れないことを確認する — PR-B で機械を分けた後は「別の機械が別の rule を持つ」
// だけなので、そもそも同じテーブルの中で衝突しようがないが、分割前からの回帰
// ピンとして残す。
func TestExecutionMachine_Reopen_StillWorksForDoneAndAborted(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	for _, from := range []orchestrator.TaskStatus{orchestrator.TaskStatusDone, orchestrator.TaskStatusAborted} {
		task := &orchestrator.Task{Status: from}
		next, err := sm.Apply(task, &orchestrator.Action{Type: "reopen"})
		if err != nil {
			t.Fatalf("reopen from %s: %v", from, err)
		}
		if next.Status != orchestrator.TaskStatusExecuting {
			t.Fatalf("reopen from %s: expected executing, got %s", from, next.Status)
		}
	}
}

// IsManualAction は「公開 ApplyAction から呼んでよい action か」を機械側から
// 判定する唯一の情報源 (codex review round 2 Blocker: internal/api 側の
// ハードコードされた拒否リストは job_failed を登録し忘れていた —
// triaged →(job_failed)→ aborted →(reopen)→ executing で ready 昇格の
// Go-gate を丸ごと迂回できた)。Manual:false の action 名は全部 false を
// 返すべきで、新しい internal-only rule を追加するたびに別リストの更新を
// 覚えている必要が無い設計にする。
func TestExecutionMachine_IsManualAction(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	manual := []string{"start", "done", "fail", "reopen", "ask", "answer", "abort"}
	nonManual := []string{"job_failed", "progress", "done_request", "fail_request", "garbage"}
	for _, a := range manual {
		if !sm.IsManualAction(a) {
			t.Errorf("IsManualAction(%q) = false, want true", a)
		}
	}
	for _, a := range nonManual {
		if sm.IsManualAction(a) {
			t.Errorf("IsManualAction(%q) = true, want false", a)
		}
	}
}
