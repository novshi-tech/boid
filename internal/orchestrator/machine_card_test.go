package orchestrator_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- Pre-execution states (cross-project-issue-triage Phase 1) ----

func TestCardMachine_Captured_Triage_ToTriaged(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusCaptured}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "triage"})
	if err != nil {
		t.Fatalf("triage: %v", err)
	}
	if next.Status != orchestrator.TaskStatusTriaged {
		t.Fatalf("expected triaged, got %s", next.Status)
	}
}

func TestCardMachine_Triaged_Ready_ToReady(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusTriaged}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "ready"})
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	if next.Status != orchestrator.TaskStatusReady {
		t.Fatalf("expected ready, got %s", next.Status)
	}
}

func TestCardMachine_Park_FromTriagedAndReady(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	for _, from := range []orchestrator.TaskStatus{orchestrator.TaskStatusTriaged, orchestrator.TaskStatusReady} {
		task := &orchestrator.Task{Status: from}
		next, err := sm.Apply(task, &orchestrator.Action{Type: "park"})
		if err != nil {
			t.Fatalf("park from %s: %v", from, err)
		}
		if next.Status != orchestrator.TaskStatusParked {
			t.Fatalf("park from %s: expected parked, got %s", from, next.Status)
		}
	}
}

func TestCardMachine_Park_InvalidFromPending(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusPending}
	if _, err := sm.Apply(task, &orchestrator.Action{Type: "park"}); err == nil {
		t.Fatal("expected error: pending tasks (execution lifecycle) must not be parkable")
	}
}

func TestCardMachine_WakeTriagedAndWakeReady(t *testing.T) {
	sm := orchestrator.NewCardMachine()

	task := &orchestrator.Task{Status: orchestrator.TaskStatusParked}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "wake_triaged"})
	if err != nil {
		t.Fatalf("wake_triaged: %v", err)
	}
	if next.Status != orchestrator.TaskStatusTriaged {
		t.Fatalf("wake_triaged: expected triaged, got %s", next.Status)
	}

	task = &orchestrator.Task{Status: orchestrator.TaskStatusParked}
	next, err = sm.Apply(task, &orchestrator.Action{Type: "wake_ready"})
	if err != nil {
		t.Fatalf("wake_ready: %v", err)
	}
	if next.Status != orchestrator.TaskStatusReady {
		t.Fatalf("wake_ready: expected ready, got %s", next.Status)
	}
}

// TestCardMachine_WakeWorking is the BD-9 regression pin: PR-4 (論点8) added
// a third park origin (working, via the "park: working → parked" exit) but
// PR-3's Wake vocabulary only ever had wake_triaged/wake_ready — a
// working-origin park had no matching internal action to resolve to and
// 500'd. This confirms the machine rule itself (the api.TaskWorkflowService.Wake
// switch-case wiring is pinned separately in internal/api).
func TestCardMachine_WakeWorking(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusParked}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "wake_working"})
	if err != nil {
		t.Fatalf("wake_working: %v", err)
	}
	if next.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("wake_working: expected working, got %s", next.Status)
	}
}

// wake_triaged/wake_ready/wake_working は Manual:false — 機構内部専用
// （TaskWorkflowService.Wake が ParkedFrom を見てどれを送るか選ぶ）。誤操作で
// 「起点と違う方に wake する」事故を AvailableActions に出さないことで構造的に防ぐ。
func TestCardMachine_AvailableActions_Parked_ExcludesWakeActions(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusParked)
	for _, a := range actions {
		if a == "wake_triaged" || a == "wake_ready" || a == "wake_working" {
			t.Errorf("wake_triaged/wake_ready/wake_working must not appear in AvailableActions(parked), got %v", actions)
		}
	}
	want := map[string]bool{"drop": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(parked) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(parked)", a)
		}
	}
}

func TestCardMachine_AvailableActions_Captured(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusCaptured)
	want := map[string]bool{"triage": true, "drop": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(captured) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(captured)", a)
		}
	}
}

func TestCardMachine_AvailableActions_Triaged(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusTriaged)
	want := map[string]bool{"ready": true, "park": true, "drop": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(triaged) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(triaged)", a)
		}
	}
}

func TestCardMachine_AvailableActions_Ready(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusReady)
	want := map[string]bool{"park": true, "drop": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(ready) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(ready)", a)
		}
	}
}

// drop は wildcard (*→dropped) にしない — pending/executing 等の実行中タスクに
// 破壊的ボタンが生えるのを避ける (Opus レビュー指摘)。pre-execution 4状態限定。
// PR-B 以降は pending/executing/awaiting/done/aborted はそもそもこの機械の
// rule テーブルに登場しないので、drop に限らずどんな action でも
// 「no transition」で拒否される。
func TestCardMachine_Drop_OnlyFromPreExecutionStates(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	for _, from := range []orchestrator.TaskStatus{
		orchestrator.TaskStatusCaptured,
		orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusReady,
	} {
		task := &orchestrator.Task{Status: from}
		next, err := sm.Apply(task, &orchestrator.Action{Type: "drop"})
		if err != nil {
			t.Fatalf("drop from %s: %v", from, err)
		}
		if next.Status != orchestrator.TaskStatusDropped {
			t.Fatalf("drop from %s: expected dropped, got %s", from, next.Status)
		}
	}
	for _, from := range []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
	} {
		task := &orchestrator.Task{Status: from}
		if _, err := sm.Apply(task, &orchestrator.Action{Type: "drop"}); err == nil {
			t.Fatalf("drop from %s: expected error, drop must not apply to execution-lifecycle statuses", from)
		}
	}
}

func TestCardMachine_Reopen_DroppedToTriaged(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusDropped}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "reopen"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if next.Status != orchestrator.TaskStatusTriaged {
		t.Fatalf("expected triaged, got %s", next.Status)
	}
}

func TestCardMachine_AvailableActions_Dropped(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusDropped)
	want := map[string]bool{"reopen": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(dropped) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(dropped)", a)
		}
	}
}

// IsManualAction は「公開 ApplyAction から呼んでよい action か」を機械側から
// 判定する唯一の情報源。card 機械の Manual 語彙は execution 機械と disjoint
// (reopen だけは名前を共有するが FromStatus が違う — dropped→triaged のみ)。
func TestCardMachine_IsManualAction(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	manual := []string{"triage", "ready", "park", "reopen", "drop", "attrs_set", "child_added", "child_specced", "child_dropped", "noted", "answered"}
	// child_dispatched/child_closed are Phase 1 PR-4's daemon-self-record-only
	// actions (論点9): they must stay non-manual so ApplyAction/
	// BoidOpActionSend reject any khi-pushed attempt to send them directly —
	// see TestApplyAction_ChildDispatchedAndChildClosed_RejectedWhenPushed
	// in internal/api for the end-to-end version of this guarantee.
	nonManual := []string{"job_failed", "progress", "done_request", "fail_request", "wake_triaged", "wake_ready", "wake_working", "dispatch", "child_dispatched", "child_closed", "garbage"}
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

// ---- Phase 1 PR-4: working からの出口3本 (論点8) ----

func TestCardMachine_Working_ThreeExits(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	cases := []struct {
		action string
		want   orchestrator.TaskStatus
	}{
		{"ready", orchestrator.TaskStatusReady},
		{"triage", orchestrator.TaskStatusTriaged},
		{"park", orchestrator.TaskStatusParked},
	}
	for _, c := range cases {
		task := &orchestrator.Task{Status: orchestrator.TaskStatusWorking}
		next, err := sm.Apply(task, &orchestrator.Action{Type: c.action})
		if err != nil {
			t.Fatalf("%s from working: %v", c.action, err)
		}
		if next.Status != c.want {
			t.Fatalf("%s from working: got %s, want %s", c.action, next.Status, c.want)
		}
	}
}

// ---- Phase 1 PR-4: attrs_set/child_added/child_specced FromStatus is an
// explicit enumeration, never "*" (論点6-3) ----
//
// docs/plans/ingestion-identity.md PR-3 adds noted/answered to the SAME loop
// (machine_card.go) that generates these rules, so both lists below include
// them too — pinning that the shared generator, not a hand-copied rule,
// produced their FromStatus set.

func TestCardMachine_TriageVocabulary_FromStatusEnumerated_NotWildcard(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	allowed := []orchestrator.TaskStatus{
		orchestrator.TaskStatusCaptured,
		orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusReady,
		orchestrator.TaskStatusWorking,
	}
	disallowed := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
		orchestrator.TaskStatusDropped,
	}
	for _, actionType := range []string{"attrs_set", "child_added", "child_specced", "child_dropped", "noted", "answered"} {
		for _, status := range allowed {
			task := &orchestrator.Task{Status: status}
			if _, err := sm.Apply(task, &orchestrator.Action{Type: actionType}); err != nil {
				t.Errorf("%s from %s: unexpected error: %v", actionType, status, err)
			}
		}
		for _, status := range disallowed {
			task := &orchestrator.Task{Status: status}
			if _, err := sm.Apply(task, &orchestrator.Action{Type: actionType}); err == nil {
				t.Errorf("%s from %s: expected error (must not fire on non-triage/ordinary-lifecycle statuses), got none", actionType, status)
			}
		}
	}
}

// attrs_set/child_added/child_specced/noted/answered are non-transitioning
// (ToStatus==""): applying them must leave task.Status unchanged.
func TestCardMachine_TriageVocabulary_NonTransitioning(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	for _, actionType := range []string{"attrs_set", "child_added", "child_specced", "child_dropped", "noted", "answered"} {
		task := &orchestrator.Task{Status: orchestrator.TaskStatusWorking}
		next, err := sm.Apply(task, &orchestrator.Action{Type: actionType})
		if err != nil {
			t.Fatalf("%s: %v", actionType, err)
		}
		if next.Status != orchestrator.TaskStatusWorking {
			t.Fatalf("%s: status changed to %s, want unchanged (working)", actionType, next.Status)
		}
	}
}

// TestCardMachine_CanApplyManualAction_Answered pins Opus review finding #3
// (2026-08-19 revisit of PR-3): CanApplyManualAction("answered", status) must
// agree with what sm.Apply actually accepts/rejects for every status — this
// is the check the Web UI's Accept/Reject buttons gate on BEFORE rendering,
// so it must never say "yes" for a status Apply would then reject.
func TestCardMachine_CanApplyManualAction_Answered(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	answerable := []orchestrator.TaskStatus{
		orchestrator.TaskStatusCaptured,
		orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusReady,
		orchestrator.TaskStatusWorking,
	}
	notAnswerable := []orchestrator.TaskStatus{
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
		orchestrator.TaskStatusDropped,
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
	}
	for _, status := range answerable {
		if !sm.CanApplyManualAction("answered", status) {
			t.Errorf("CanApplyManualAction(answered, %s) = false, want true", status)
		}
		if _, err := sm.Apply(&orchestrator.Task{Status: status}, &orchestrator.Action{Type: "answered"}); err != nil {
			t.Errorf("sanity: Apply(answered) from %s unexpectedly errored: %v", status, err)
		}
	}
	for _, status := range notAnswerable {
		if sm.CanApplyManualAction("answered", status) {
			t.Errorf("CanApplyManualAction(answered, %s) = true, want false", status)
		}
		if _, err := sm.Apply(&orchestrator.Task{Status: status}, &orchestrator.Action{Type: "answered"}); err == nil {
			t.Errorf("sanity: Apply(answered) from %s unexpectedly succeeded (CanApplyManualAction and Apply must agree)", status)
		}
	}
}

// ---- Phase 1 PR-2: ready → working (逆輸入2: dispatch) ----

func TestCardMachine_Dispatch_ReadyToWorking(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusReady}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "dispatch"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if next.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("dispatch: expected working, got %s", next.Status)
	}
}

// dispatch is machine-internal (Manual:false, mirrors wake_triaged/wake_ready):
// it must never appear as a button in AvailableActions(ready), and any
// non-ready FromStatus must be rejected the same way an unknown action is.
func TestCardMachine_Dispatch_NotManualNotFromOtherStatuses(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	if sm.IsManualAction("dispatch") {
		t.Fatal("dispatch must not be a Manual action")
	}
	for _, from := range []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusCaptured,
		orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusWorking,
		orchestrator.TaskStatusDone,
	} {
		task := &orchestrator.Task{Status: from}
		if _, err := sm.Apply(task, &orchestrator.Action{Type: "dispatch"}); err == nil {
			t.Errorf("dispatch from %s: expected error, got none", from)
		}
	}
}

// TestCardMachine_AvailableActions_Working_HasThreeExits pins Phase 1
// PR-4's 論点8 fix: working now has three manual exits (ready/triage/park),
// reusing the pre-execution verbs. This replaces the former "known PR-3 gap"
// assertion (working had zero exits) that PR-4 explicitly closes. attrs_set/
// child_added/child_specced/child_dispatched/child_closed must NOT appear
// here even though attrs_set/child_added/child_specced are Manual:true from
// working — they are non-transitioning (ToStatus=="") and AvailableActions
// filters those out (論点6-1).
func TestCardMachine_AvailableActions_Working_HasThreeExits(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusWorking)
	want := map[string]bool{"ready": true, "triage": true, "park": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(working) = %v, want exactly %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Fatalf("AvailableActions(working) contains unexpected action %q (full list: %v)", a, actions)
		}
	}
}

// TestCardMachine_ParkOrigins_AllHaveWakeRule is the BD-9 recurrence
// guard: the bug's actual mechanism was that PR-4 added a third "park: X →
// parked" rule (working) without PR-3's Wake vocabulary growing a matching
// "wake_X: parked → X" rule to resolve it, so a working-origin park 500'd
// (TestCardMachine_WakeWorking / TestTaskWorkflowService_Wake_
// FromWorking_ReturnsToWorking_NoDispatch in internal/api pin that specific
// case by name, but a name-pinned test does not stop a FOURTH park origin
// from reproducing the same class of bug later).
//
// This test derives the set of park origins directly from
// NewCardMachine().Rules — deliberately NOT a hardcoded
// []string{"triaged","ready","working"} literal, since a hardcoded list
// would silently stop covering a newly-added origin instead of catching it.
// For every "park" rule's FromStatus, it asserts a "parked → FromStatus"
// rule (i.e. the corresponding wake_* rule) exists somewhere in the same
// rule table. If a future PR adds a fourth park origin without adding its
// wake counterpart, this fails by name instead of waiting for a 500 in
// production the way BD-9 did.
func TestCardMachine_ParkOrigins_AllHaveWakeRule(t *testing.T) {
	sm := orchestrator.NewCardMachine()

	var parkOrigins []string
	for _, r := range sm.Rules {
		if r.Action == "park" && r.ToStatus == string(orchestrator.TaskStatusParked) {
			parkOrigins = append(parkOrigins, r.FromStatus)
		}
	}
	if len(parkOrigins) == 0 {
		t.Fatal("no park rules found in NewCardMachine().Rules — did the rule table's shape change?")
	}

	for _, origin := range parkOrigins {
		found := false
		for _, r := range sm.Rules {
			// Require an actual wake_* action rule, not merely any
			// parked→origin transition (e.g. "drop" also starts from parked
			// but to "dropped", not to a park origin, so it wouldn't match
			// here anyway — this guards against a hypothetical future
			// non-wake_* rule that happens to land on the same ToStatus,
			// which would satisfy "a transition exists" without actually
			// being what TaskWorkflowService.Wake's ParkedFrom switch
			// dispatches to).
			if r.FromStatus == string(orchestrator.TaskStatusParked) && r.ToStatus == origin && strings.HasPrefix(r.Action, "wake_") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("park origin %q has no matching wake rule (parked → %s) in NewCardMachine().Rules — TaskWorkflowService.Wake's ParkedFrom switch cannot resolve this origin without one (BD-9 recurrence)", origin, origin)
		}
	}
}
