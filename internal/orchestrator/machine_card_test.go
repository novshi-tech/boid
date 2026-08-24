package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- card 機械 v2 (docs/plans/suggestion-as-state-transition.md §3.2) ----
//
// Four statuses: parked/working/done/dropped. Manual transitions:
//
//	go      : parked  → working
//	working : parked  → working
//	drop    : parked  → dropped
//	park    : working → parked
//	done    : working → done
//	reopen  : done    → parked
//	reopen  : dropped → parked
//
// This is the network's complete edge list — TestCardMachineV2_AllEdges below
// walks the full status × action cross product and asserts EXACTLY these
// seven edges succeed, everything else (including every old v1 verb —
// triage/ready/wake_*/dispatch/triage_done/reopen_triaged) is rejected.

// v2CardStatuses is every status the v2 card machine actually reaches
// (excludes the legacy captured/triaged/ready statuses, which v2 has zero
// rules for at all — see TestCardMachineV2_LegacyStatuses_NoRules).
var v2CardStatuses = []orchestrator.TaskStatus{
	orchestrator.TaskStatusParked,
	orchestrator.TaskStatusWorking,
	orchestrator.TaskStatusDone,
	orchestrator.TaskStatusDropped,
}

// v2CardTransitionActions is the closed set of the six human-only
// card-lifecycle verbs (穴11's push-down defense keys off this exact set —
// see orchestrator.IsCardTransitionAction).
var v2CardTransitionActions = []string{"go", "working", "park", "drop", "done", "reopen"}

func TestCardMachineV2_AllEdges(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	want := map[orchestrator.TaskStatus]map[string]orchestrator.TaskStatus{
		orchestrator.TaskStatusParked: {
			"go":      orchestrator.TaskStatusWorking,
			"working": orchestrator.TaskStatusWorking,
			"drop":    orchestrator.TaskStatusDropped,
		},
		orchestrator.TaskStatusWorking: {
			"park": orchestrator.TaskStatusParked,
			"done": orchestrator.TaskStatusDone,
		},
		orchestrator.TaskStatusDone: {
			"reopen": orchestrator.TaskStatusParked,
		},
		orchestrator.TaskStatusDropped: {
			"reopen": orchestrator.TaskStatusParked,
		},
	}

	for _, from := range v2CardStatuses {
		for _, action := range v2CardTransitionActions {
			task := &orchestrator.Task{Status: from}
			next, err := sm.Apply(task, &orchestrator.Action{Type: action})
			wantTo, ok := want[from][action]
			if ok {
				if err != nil {
					t.Errorf("%s: %s -> ? : unexpected error: %v", action, from, err)
					continue
				}
				if next.Status != wantTo {
					t.Errorf("%s: %s -> got %s, want %s", action, from, next.Status, wantTo)
				}
			} else if err == nil {
				t.Errorf("%s: %s -> %s : expected rejection (not a v2 edge), got success", action, from, next.Status)
			}
		}
	}
}

// TestCardMachineV2_AvailableActions pins the exact button set per status —
// this is what api.ApplyAction's response and the Web UI's AvailableActions
// rendering key off.
func TestCardMachineV2_AvailableActions(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	cases := []struct {
		status orchestrator.TaskStatus
		want   map[string]bool
	}{
		{orchestrator.TaskStatusParked, map[string]bool{"go": true, "working": true, "drop": true}},
		{orchestrator.TaskStatusWorking, map[string]bool{"park": true, "done": true}},
		{orchestrator.TaskStatusDone, map[string]bool{"reopen": true}},
		{orchestrator.TaskStatusDropped, map[string]bool{"reopen": true}},
	}
	for _, c := range cases {
		actions := sm.AvailableActions(c.status)
		if len(actions) != len(c.want) {
			t.Fatalf("AvailableActions(%s) = %v, want exactly %v", c.status, actions, c.want)
		}
		for _, a := range actions {
			if !c.want[a] {
				t.Errorf("AvailableActions(%s) contains unexpected action %q (full list: %v)", c.status, a, actions)
			}
		}
	}
}

// TestCardMachineV2_LegacyStatuses_NoRules pins that captured/triaged/ready
// (KnownTaskStatuses' legacy carve-out, kept only for reading pre-cutover DB
// rows — docs/plans/suggestion-as-state-transition-impl.md §3.5) have ZERO
// rules anywhere in v2's rule table: no verb, old or new, transitions a task
// sitting in one of these statuses. A pre-cutover card stuck in one of these
// is unstuck only by direct DB/ops intervention (the 洗い替え runbook), not
// through this machine.
func TestCardMachineV2_LegacyStatuses_NoRules(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	legacy := []orchestrator.TaskStatus{
		orchestrator.TaskStatusCaptured,
		orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusReady,
	}
	// wake_due is deliberately excluded from this list: its FromStatus is "*"
	// by design (a pure fact-record, unrelated to which status a card sits
	// in — see NewCardMachine's own doc comment), so it succeeds from a
	// legacy status too. That is harmless (nothing ever routes SweepWake at
	// a captured/triaged/ready card in practice) and not a rule-table bug.
	allActions := append([]string{
		"triage", "ready", "wake_triaged", "wake_ready", "wake_working", "dispatch",
		"triage_done", "reopen_triaged", "attrs_set", "child_added", "child_specced",
		"child_dropped", "noted", "answered",
	}, v2CardTransitionActions...)
	for _, status := range legacy {
		if got := sm.AvailableActions(status); len(got) != 0 {
			t.Errorf("AvailableActions(%s) = %v, want empty (legacy status has no v2 rules)", status, got)
		}
		for _, action := range allActions {
			task := &orchestrator.Task{Status: status}
			if _, err := sm.Apply(task, &orchestrator.Action{Type: action}); err == nil {
				t.Errorf("%s from legacy status %s: expected rejection, got success", action, status)
			}
		}
	}
}

// TestCardMachineV2_DeletedV1Rules pins that every v1-only verb (triage/
// ready/wake_triaged/wake_ready/wake_working/dispatch/triage_done/
// reopen_triaged) is gone from v2 entirely — IsManualAction and Apply must
// both refuse it from EVERY status, not just the legacy ones.
func TestCardMachineV2_DeletedV1Rules(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	deleted := []string{
		"triage", "ready", "wake_triaged", "wake_ready", "wake_working",
		"dispatch", "triage_done", "reopen_triaged",
	}
	for _, action := range deleted {
		if sm.IsManualAction(action) {
			t.Errorf("IsManualAction(%q) = true, want false (v1 rule must be deleted)", action)
		}
		for _, status := range append(v2CardStatuses, orchestrator.TaskStatusCaptured, orchestrator.TaskStatusTriaged, orchestrator.TaskStatusReady) {
			task := &orchestrator.Task{Status: status}
			if _, err := sm.Apply(task, &orchestrator.Action{Type: action}); err == nil {
				t.Errorf("%s from %s: expected rejection (v1 rule deleted), got success", action, status)
			}
		}
	}
}

// TestCardMachineV2_TriageVocabulary_FromStatusIsParkedOrWorking pins that
// the non-transitioning Manual vocabulary (attrs_set/child_added/
// child_specced/child_dropped/noted/answered) FromStatus enumeration shrank
// from the old five-status preExecutionStatuses set down to exactly
// {parked, working} — captured/triaged/ready/done/dropped must all reject.
func TestCardMachineV2_TriageVocabulary_FromStatusIsParkedOrWorking(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	allowed := []orchestrator.TaskStatus{orchestrator.TaskStatusParked, orchestrator.TaskStatusWorking}
	disallowed := []orchestrator.TaskStatus{
		orchestrator.TaskStatusCaptured,
		orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusReady,
		orchestrator.TaskStatusDropped,
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
		orchestrator.TaskStatusAborted,
	}
	for _, action := range []string{"attrs_set", "child_added", "child_specced", "child_dropped", "noted", "answered"} {
		for _, status := range allowed {
			task := &orchestrator.Task{Status: status}
			next, err := sm.Apply(task, &orchestrator.Action{Type: action})
			if err != nil {
				t.Errorf("%s from %s: unexpected error: %v", action, status, err)
				continue
			}
			if next.Status != status {
				t.Errorf("%s from %s: non-transitioning action changed status to %s", action, status, next.Status)
			}
		}
		// done is handled separately — see TestCardMachineV2_AttrsSetOnDone_
		// StillNonTransitioning_ViaServiceLayerGuard (attrs_set_done.go's
		// guard, NOT a machine.go rule) for why "done" is deliberately not
		// listed in `disallowed` for attrs_set specifically. It IS listed
		// here for the other five verbs, which have no such service-layer
		// exception.
		for _, status := range disallowed {
			task := &orchestrator.Task{Status: status}
			if _, err := sm.Apply(task, &orchestrator.Action{Type: action}); err == nil {
				t.Errorf("%s from %s: expected rejection, got success", action, status)
			}
		}
	}
	// done: attrs_set alone is out of scope for machine.go (I-5b's
	// service-layer guard, attrs_set_done.go); the other five verbs still
	// reject it here.
	for _, action := range []string{"child_added", "child_specced", "child_dropped", "noted", "answered"} {
		task := &orchestrator.Task{Status: orchestrator.TaskStatusDone}
		if _, err := sm.Apply(task, &orchestrator.Action{Type: action}); err == nil {
			t.Errorf("%s from done: expected rejection, got success", action)
		}
	}
}

// TestCardMachineV2_WakeDue pins the new wake_due rule (docs/plans/
// suggestion-as-state-transition-impl.md §C): a non-transitioning,
// Manual:false, FromStatus "*" fact-recording action. Registering it means
// IsManualAction rejects any khi-pushed direct send (same protection
// child_dispatched/child_closed already get), while queue_sweep.go's
// SweepWake can still self-record it via tx.CreateAction directly (which
// never routes through sm.Apply's rule table at all) or, if it chooses to
// route through Apply, gets a clean non-transitioning no-op.
func TestCardMachineV2_WakeDue(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	if sm.IsManualAction("wake_due") {
		t.Fatal("IsManualAction(wake_due) = true, want false (machine-internal, daemon self-record only)")
	}
	for _, status := range append(v2CardStatuses, orchestrator.TaskStatusCaptured, orchestrator.TaskStatusTriaged, orchestrator.TaskStatusReady) {
		task := &orchestrator.Task{Status: status}
		next, err := sm.Apply(task, &orchestrator.Action{Type: "wake_due"})
		if err != nil {
			t.Errorf("wake_due from %s: unexpected error: %v", status, err)
			continue
		}
		if next.Status != status {
			t.Errorf("wake_due from %s: non-transitioning action changed status to %s", status, next.Status)
		}
	}
	for _, a := range sm.AvailableActions(orchestrator.TaskStatusParked) {
		if a == "wake_due" {
			t.Fatal(`AvailableActions(parked) must not contain "wake_due" (non-transitioning + Manual:false)`)
		}
	}
}

// TestCardMachineV2_IsManualAction pins the full Manual/non-Manual split.
func TestCardMachineV2_IsManualAction(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	manual := append([]string{
		"attrs_set", "child_added", "child_specced", "child_dropped", "noted", "answered",
	}, v2CardTransitionActions...)
	nonManual := []string{
		"job_failed", "progress", "done_request", "fail_request",
		"child_dispatched", "child_closed", "wake_due", "garbage",
		// v1 verbs, fully deleted:
		"triage", "ready", "wake_triaged", "wake_ready", "wake_working", "dispatch",
		"triage_done", "reopen_triaged",
	}
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

// TestCardMachineV2_JobFailed_NotRegistered pins a deliberate departure from
// v1 (which duplicated job_failed onto both machines purely for "Apply
// treats the name as known" parity): design doc §3.2 states a v2 card
// "reaches aborted never again... card 機械は4状態で本当に閉じる" — keeping
// job_failed's `* → aborted` rule on the card machine would reopen exactly
// the fifth status the redesign closes, even though nothing in production
// ever fires it against a card (a card never runs a job of its own). See
// this PR's description for the full rationale.
func TestCardMachineV2_JobFailed_NotRegistered(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	for _, r := range sm.Rules {
		if r.Action == "job_failed" {
			t.Fatalf("job_failed must not be a rule on NewCardMachine in v2 (found FromStatus=%q ToStatus=%q) — it would reopen the aborted status the 4-state closure explicitly retires", r.FromStatus, r.ToStatus)
		}
		if r.ToStatus == string(orchestrator.TaskStatusAborted) {
			t.Fatalf("no rule may target aborted on the v2 card machine (found Action=%q FromStatus=%q)", r.Action, r.FromStatus)
		}
	}
}

// TestCardMachineV2_CanApplyManualAction_Answered mirrors the shrunk
// preExecutionStatuses set (renamed cardNonTransitionStatuses in v2) for the
// dedicated Web UI Accept/Reject gate.
func TestCardMachineV2_CanApplyManualAction_Answered(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	answerable := []orchestrator.TaskStatus{orchestrator.TaskStatusParked, orchestrator.TaskStatusWorking}
	notAnswerable := []orchestrator.TaskStatus{
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusDropped,
		orchestrator.TaskStatusCaptured,
		orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusReady,
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
	}
	for _, status := range answerable {
		if !sm.CanApplyManualAction("answered", status) {
			t.Errorf("CanApplyManualAction(answered, %s) = false, want true", status)
		}
	}
	for _, status := range notAnswerable {
		if sm.CanApplyManualAction("answered", status) {
			t.Errorf("CanApplyManualAction(answered, %s) = true, want false", status)
		}
	}
}

// TestCardMachineV2_MachineName pins sm.Name == orchestrator.CardMachineName,
// which api.ApplyAction's push-down defense (穴11) keys off to decide
// whether to apply the actor==human check at all.
func TestCardMachineV2_MachineName(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	if sm.Name != orchestrator.CardMachineName {
		t.Fatalf("NewCardMachine().Name = %q, want %q", sm.Name, orchestrator.CardMachineName)
	}
}

// TestIsCardTransitionAction pins the exact closed set the push-down defense
// (穴11, api.ApplyAction) restricts to actor==human.
func TestIsCardTransitionAction(t *testing.T) {
	for _, a := range v2CardTransitionActions {
		if !orchestrator.IsCardTransitionAction(a) {
			t.Errorf("IsCardTransitionAction(%q) = false, want true", a)
		}
	}
	nonTransition := []string{
		"attrs_set", "child_added", "child_specced", "child_dropped", "noted", "answered",
		"wake_due", "job_failed", "progress", "done_request", "fail_request",
		"child_dispatched", "child_closed", "start", "abort", "ask", "answer", "garbage",
	}
	for _, a := range nonTransition {
		if orchestrator.IsCardTransitionAction(a) {
			t.Errorf("IsCardTransitionAction(%q) = true, want false", a)
		}
	}
}
