package orchestrator_test

import (
	"strings"
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
//	done    : parked  → done
//	park    : working → parked
//	done    : working → done
//	reopen  : done    → parked
//	reopen  : dropped → parked
//
// This is the network's complete edge list — TestCardMachineV2_AllEdges below
// walks the full status × action cross product and asserts EXACTLY these
// eight edges succeed, everything else (including every old v1 verb —
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
			// 8 本目の辺 (2026-08-25): 「外で片付いていた」「重複と判明した」card を
			// 1 手で閉じる。詳細は machine_card.go の NewCardMachine doc comment。
			"done": orchestrator.TaskStatusDone,
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
		{orchestrator.TaskStatusParked, map[string]bool{"go": true, "working": true, "drop": true, "done": true}},
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

// TestCardMachineV2_LegacyStatuses_NoRules (captured/triaged/ready have zero
// rules in v2's rule table) is DELETED as of card-model-cleanup PR-2: those
// three statuses are not merely excluded from KnownTaskStatuses() now, they
// are structurally impossible — migration 0045's CHECK constraint rejects
// any tasks.status value outside {parked,working,done,dropped} (card) /
// {pending,executing,awaiting,done,aborted} (execution) at the DB layer, and
// the migration itself 洗い替えs every pre-cutover captured/triaged/ready row
// to parked in the same transaction. There is no code path left, in this
// package or any caller, that can ever produce a Task carrying one of these
// three values again — the scenario this test manufactured (a card "stuck"
// in a legacy status) cannot occur post-cutover. The migration-level
// coverage of the actual legacy-row handling this test's doc comment
// referenced already lives in internal/db/migrate/migrate_0045_card_sti_test.go
// (TestApply_0045_TypeJudgment converts legacy-status rows to card/parked;
// TestApply_0045_ActionsHistoryUntouched confirms the action log's own
// legacy from_status/to_status strings are left untouched) — that is now the
// sole and correct home for this concern.

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
		// card-model-cleanup PR-2: this used to also probe the legacy
		// captured/triaged/ready statuses (extra assurance that a deleted
		// verb stays rejected even from a pre-cutover row) — those statuses
		// no longer exist as constructible TaskStatus values at all (see
		// TestCardMachineV2_LegacyStatuses_NoRules's removal note above), so
		// only the four real v2 statuses remain to probe here.
		for _, status := range v2CardStatuses {
			task := &orchestrator.Task{Status: status}
			if _, err := sm.Apply(task, &orchestrator.Action{Type: action}); err == nil {
				t.Errorf("%s from %s: expected rejection (v1 rule deleted), got success", action, status)
			}
		}
	}
}

// TestCardMachineV2_TriageVocabulary_PerActionFromStatus pins each
// non-transitioning action's OWN FromStatus enumeration (PR #987 review,
// BLOCKER 3 — replacing a single shared cardActiveStatuses set, which broke
// accept(reopen) from done/dropped entirely: design doc §3.2 requires
// `done/dropped → parked : reopen (直接 or accept)` as a real edge, so
// "answered" must be reachable from done/dropped too, or a suggestion khi
// legitimately places there — e.g. "reopen" — can never be answered at
// all). See NewCardMachine's own doc comment for why each action's set is
// what it is; this test is the exhaustive cross-product pin of that design.
//
//   - attrs_set: {parked, working, dropped} — done is attrs_set's own
//     service-layer special case (resolveAttrsSetDoneTransition, NOT this
//     table); dropped IS included (PR #987 review round 2, BLOCKER N1) so
//     khi can attrs_set a "reopen" suggestion onto a dropped card, the
//     production path design doc §3.2's `dropped → parked : reopen (via
//     accept)` edge actually depends on.
//   - child_added / child_specced / child_dropped: {parked, working} only —
//     none of the three make sense once a card is terminal (its children
//     are frozen).
//   - noted / answered: {parked, working, done, dropped} — both must
//     reach a terminal card.
func TestCardMachineV2_TriageVocabulary_PerActionFromStatus(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	// card-model-cleanup PR-2: captured/triaged/ready dropped from this list
	// — they no longer exist as constructible TaskStatus values (see
	// TestCardMachineV2_LegacyStatuses_NoRules's removal note above).
	universallyDisallowed := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
		orchestrator.TaskStatusAborted,
	}

	applyNonTransitioning := func(action string, status orchestrator.TaskStatus) {
		t.Helper()
		task := &orchestrator.Task{Status: status}
		next, err := sm.Apply(task, &orchestrator.Action{Type: action})
		if err != nil {
			t.Errorf("%s from %s: unexpected error: %v", action, status, err)
			return
		}
		if next.Status != status {
			t.Errorf("%s from %s: non-transitioning action changed status to %s", action, status, next.Status)
		}
	}
	rejectFrom := func(action string, status orchestrator.TaskStatus) {
		t.Helper()
		task := &orchestrator.Task{Status: status}
		if _, err := sm.Apply(task, &orchestrator.Action{Type: action}); err == nil {
			t.Errorf("%s from %s: expected rejection, got success", action, status)
		}
	}

	// attrs_set is out of scope for "done" here — that path belongs to
	// resolveAttrsSetDoneTransition's service-layer guard (attrs_set_done.go),
	// not this rule table (see NewCardMachine's own doc comment) — so it is
	// deliberately excluded from the child_* group's universal-disallow loop
	// below, and handled in its own block (it DOES reach "dropped", unlike
	// child_added/child_specced/child_dropped — BLOCKER N1).
	for _, action := range []string{"child_added", "child_specced", "child_dropped"} {
		for _, status := range []orchestrator.TaskStatus{orchestrator.TaskStatusParked, orchestrator.TaskStatusWorking} {
			applyNonTransitioning(action, status)
		}
		for _, status := range append(universallyDisallowed, orchestrator.TaskStatusDone, orchestrator.TaskStatusDropped) {
			rejectFrom(action, status)
		}
	}
	for _, status := range []orchestrator.TaskStatus{
		orchestrator.TaskStatusParked, orchestrator.TaskStatusWorking, orchestrator.TaskStatusDropped,
	} {
		applyNonTransitioning("attrs_set", status)
	}
	rejectFrom("attrs_set", orchestrator.TaskStatusDone)
	for _, status := range universallyDisallowed {
		rejectFrom("attrs_set", status)
	}

	for _, action := range []string{"noted", "answered"} {
		for _, status := range []orchestrator.TaskStatus{
			orchestrator.TaskStatusParked, orchestrator.TaskStatusWorking,
			orchestrator.TaskStatusDone, orchestrator.TaskStatusDropped,
		} {
			applyNonTransitioning(action, status)
		}
		for _, status := range universallyDisallowed {
			rejectFrom(action, status)
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
	// card-model-cleanup PR-2: this used to also probe the legacy
	// captured/triaged/ready statuses; they no longer exist as constructible
	// TaskStatus values (see TestCardMachineV2_LegacyStatuses_NoRules's
	// removal note above), so only the four real v2 statuses remain here.
	for _, status := range v2CardStatuses {
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

// TestCardMachineV2_CanApplyManualAction_Answered pins the dedicated Web UI
// Accept/Reject gate's FromStatus set: {parked, working, done, dropped}
// (PR #987 review, BLOCKER 3 — done/dropped joined this set so a
// done/dropped card carrying a suggestion, e.g. a khi-suggested "reopen",
// can actually be answered; see NewCardMachine's own doc comment).
func TestCardMachineV2_CanApplyManualAction_Answered(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	answerable := []orchestrator.TaskStatus{
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusWorking,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusDropped,
	}
	// card-model-cleanup PR-2: captured/triaged/ready dropped from this list
	// — they no longer exist as constructible TaskStatus values (see
	// TestCardMachineV2_LegacyStatuses_NoRules's removal note above).
	notAnswerable := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
		orchestrator.TaskStatusAborted,
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

// TestCardMachineV2_Reopen_ReachableViaAcceptFromDoneAndDropped is the
// end-to-end pin (machine-rule level) for BLOCKER 3: a suggestion on a
// done/dropped card must be answerable, and specifically its "reopen" verb
// must actually apply from BOTH terminal statuses — the exact edges design
// doc §3.2 lists. This only proves the machine-rule half (answered is
// admitted, AND reopen itself still fires from done/dropped, which
// TestCardMachineV2_AllEdges already covers independently); the api-layer
// half (suggestion_accept.go actually wiring answered's accept branch
// through to this rule for a real done/dropped card) has its own dedicated
// test in internal/api.
// ---- PR-3 (suggestion 状態遷移化 follow-up): CanApplyTransitionAction ----
//
// The bug this guards against: card machine v2 admits only a NARROW set of
// statuses per verb (go/working/drop only from parked; park only from
// working; done from parked or working; reopen only from done/dropped —
// NewCardMachine's own doc comment), but
// nothing let a caller ask "would VERB actually apply from the task's
// CURRENT status" before this PR — CanApplyManualAction("answered", status)
// alone says yes for any of the four card statuses regardless of the
// suggestion's own verb, so a suggestion whose verb didn't match the card's
// status still rendered a live-looking Accept button that 409'd on click.

// cardTransitionEdges is the exact eight (verb, fromStatus) pairs
// TestCardMachineV2_AllEdges already pins as the network's entire edge list
// — reused here as CanApplyTransitionAction's own hardcoded expectation, so
// this test also serves as an explicit, human-readable pin of "exactly these
// eight combinations answer applicable" (as opposed to the drift-proof
// rule-table-derived test right below it, which reads no hardcoded list at
// all).
var cardTransitionEdges = map[string]map[orchestrator.TaskStatus]bool{
	"go":      {orchestrator.TaskStatusParked: true},
	"working": {orchestrator.TaskStatusParked: true},
	"drop":    {orchestrator.TaskStatusParked: true},
	"park":    {orchestrator.TaskStatusWorking: true},
	"done":    {orchestrator.TaskStatusParked: true, orchestrator.TaskStatusWorking: true},
	"reopen":  {orchestrator.TaskStatusDone: true, orchestrator.TaskStatusDropped: true},
}

// TestCardMachineV2_CanApplyTransitionAction_PinsExactlyEightEdges is the
// exhaustive 6-verb × 4-status (24 combination) cross-product pin: exactly
// the eight edges in cardTransitionEdges answer true, every other
// combination answers false.
func TestCardMachineV2_CanApplyTransitionAction_PinsExactlyEightEdges(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	for _, verb := range v2CardTransitionActions {
		for _, status := range v2CardStatuses {
			want := cardTransitionEdges[verb][status]
			got := sm.CanApplyTransitionAction(verb, status)
			if got != want {
				t.Errorf("CanApplyTransitionAction(%s, %s) = %v, want %v", verb, status, got, want)
			}
		}
	}
}

// TestCardMachineV2_CanApplyTransitionAction_DerivedFromRuleTable_NoDrift is
// the drift-proof twin: rather than comparing against a hardcoded list (the
// test above), it reads sm.Rules DIRECTLY to compute which (action,
// FromStatus) pairs are Manual AND status-changing (ToStatus != "") — the
// exact predicate CanApplyTransitionAction's own implementation applies —
// and asserts CanApplyTransitionAction agrees with that derivation for every
// combination. A future edit to NewCardMachine's rule table (e.g. adding an
// eighth edge) automatically updates this test's expectations; only the
// OTHER test (hardcoded seven edges) would need a human to also update the
// design-doc-level pin.
//
// Review LOW 2 (fix/unapplicable-suggestion-guard PR): the loop's OUTER
// action set is now derived from sm.Rules too (every distinct Action name
// appearing anywhere in the table — transitioning or not, Manual or not),
// not the hand-written v2CardTransitionActions this test used before. That
// earlier version's "auto-follows the rule table" doc-comment claim didn't
// actually hold for an EIGHTH verb: derived's inner map would gain the new
// key correctly, but the outer `for _, verb := range v2CardTransitionActions`
// loop would never visit it, so CanApplyTransitionAction's behavior on that
// new verb would go entirely unprobed by this test. Deriving allActions from
// sm.Rules directly closes that gap, and — as a bonus — also now probes every
// NON-transitioning action (attrs_set, noted, answered, wake_due, ...),
// pinning that CanApplyTransitionAction correctly says false for all of them
// at every status (they have no entry in `derived` at all, so `want` is
// always the zero value, false).
func TestCardMachineV2_CanApplyTransitionAction_DerivedFromRuleTable_NoDrift(t *testing.T) {
	sm := orchestrator.NewCardMachine()

	derived := map[string]map[orchestrator.TaskStatus]bool{}
	allActions := map[string]bool{}
	for _, r := range sm.Rules {
		if r.Condition != nil {
			continue
		}
		allActions[r.Action] = true
		if !r.Manual || r.ToStatus == "" {
			continue
		}
		if derived[r.Action] == nil {
			derived[r.Action] = map[orchestrator.TaskStatus]bool{}
		}
		if r.FromStatus == "*" {
			for _, status := range v2CardStatuses {
				derived[r.Action][status] = true
			}
			continue
		}
		derived[r.Action][orchestrator.TaskStatus(r.FromStatus)] = true
	}
	if len(allActions) == 0 {
		t.Fatal("derived zero actions from sm.Rules — test fixture assumption broken")
	}

	for action := range allActions {
		for _, status := range v2CardStatuses {
			want := derived[action][status]
			got := sm.CanApplyTransitionAction(action, status)
			if got != want {
				t.Errorf("CanApplyTransitionAction(%s, %s) = %v, want %v (derived from sm.Rules)", action, status, got, want)
			}
		}
	}
}

func TestCardMachineV2_Reopen_ReachableViaAcceptFromDoneAndDropped(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	for _, from := range []orchestrator.TaskStatus{orchestrator.TaskStatusDone, orchestrator.TaskStatusDropped} {
		// Step 1: answered must be admitted from this status (the gate
		// BLOCKER 3 fixes).
		if !sm.CanApplyManualAction("answered", from) {
			t.Fatalf("CanApplyManualAction(answered, %s) = false, want true (a suggestion here could never be answered)", from)
		}
		// Step 2: the verb the suggestion names (reopen) must itself still
		// apply from this status — this is the actual transition
		// accept(reopen) fires after answered is admitted.
		task := &orchestrator.Task{Status: from}
		next, err := sm.Apply(task, &orchestrator.Action{Type: "reopen"})
		if err != nil {
			t.Fatalf("reopen from %s: %v", from, err)
		}
		if next.Status != orchestrator.TaskStatusParked {
			t.Fatalf("reopen from %s: status = %q, want parked", from, next.Status)
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

// TestCardMachineV2_AvailableActionsHint_MatchesAvailableActions pins that
// AvailableActionsHint's text is built FROM AvailableActions (not a
// hand-copied literal) for every one of the 4 reachable card statuses — the
// exact "derive from the rule table" requirement. Moved here from
// internal/api's own suggestion_accept_test.go (fix/unapplicable-suggestion-
// guard PR review, LOW 4): the hint-building itself moved from an
// api-package-local function into this single orchestrator-side method
// (shared by internal/api's 409 messages AND the Web UI's inapplicable-
// suggestion notice — components.SuggestionInapplicableReason), so its pin
// belongs next to the method it tests, not duplicated in a downstream
// package.
func TestCardMachineV2_AvailableActionsHint_MatchesAvailableActions(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	for _, status := range v2CardStatuses {
		hint := sm.AvailableActionsHint(status)
		available := sm.AvailableActions(status)
		if len(available) == 0 {
			t.Fatalf("status=%s: AvailableActions is empty — no card status should have zero available actions (test fixture assumption broken)", status)
		}
		for _, a := range available {
			if !strings.Contains(hint, a) {
				t.Errorf("status=%s: hint %q missing available action %q", status, hint, a)
			}
		}
		if !strings.Contains(hint, string(status)) {
			t.Errorf("status=%s: hint %q should name the status", status, hint)
		}
	}
}

// TestStateMachine_AvailableActionsHint_EmptyStatusFallback pins the
// zero-available-actions branch (AvailableActions(status) returns nil/empty
// — e.g. a genuinely terminal status like "aborted" on the card machine,
// which has no rule reaching or leaving it at all): the hint must still say
// something self-explanatory rather than an empty "from status=X you can
// apply: " with nothing after the colon.
func TestStateMachine_AvailableActionsHint_EmptyStatusFallback(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	hint := sm.AvailableActionsHint(orchestrator.TaskStatusAborted)
	if !strings.Contains(hint, string(orchestrator.TaskStatusAborted)) {
		t.Errorf("hint %q should name the status", hint)
	}
	if !strings.Contains(hint, "no further transitions") {
		t.Errorf("hint %q should say plainly that nothing is available, not a bare empty list", hint)
	}
}
