package orchestrator

// NewCardMachine returns the state machine governing cards — the
// suggestion-as-state-transition redesign's "judgment ledger" object. See
// docs/plans/suggestion-as-state-transition.md §3.2 and
// docs/plans/suggestion-as-state-transition-impl.md §3 for the full design.
//
// Four statuses, closed: parked/working/done/dropped. A card NEVER reaches
// pending/executing/awaiting/aborted/captured/triaged/ready through this
// machine's own rule table (captured/triaged/ready are kept as legacy
// KnownTaskStatuses entries purely so pre-cutover DB rows remain readable —
// see model.go's own doc comment). Manual (human-only — see the push-down
// defense below) transitions:
//
//	go      : parked  → working   (accept(go): specced children get dispatched, then this fires)
//	working : parked  → working   (accept(working): manual work declared, no dispatch)
//	drop    : parked  → dropped
//	done    : parked  → done      (closed without ever being worked here — a card resolved
//	                               out-of-band, or a duplicate; closing is not starting work)
//	park    : working → parked    (sets a wake condition — see workflow_card.go's park side effect)
//	done    : working → done
//	reopen  : done    → parked
//	reopen  : dropped → parked
//
// This is the network's ENTIRE edge list, implementing the design doc's
// §3.2 table literally. The only rule the MACHINE itself fires is capture
// (creating a task directly into "parked" — task_create.go /
// task_resolve_or_capture.go); every edge above requires a human's accept,
// whether typed directly (Web UI / CLI hitting ApplyAction) or via the
// accept(verb) flow answering a khi suggestion
// (internal/api/suggestion_accept.go).
//
// job_failed is deliberately NOT registered here (unlike the execution
// machine — see machine_execution.go): a card never runs a job of its own,
// and job_failed's only rule targets "aborted", a status the card model
// retires entirely. progress/done_request/fail_request/child_dispatched/
// child_closed stay registered (all non-transitioning, so none of them can
// reopen a fifth status) purely so Apply treats the name as known.
//
// ---- push-down defense ----
//
// All eight Manual transition rules above are reachable through the public
// ApplyAction endpoint (HTTP API / Web UI / brokered `boid action send` /
// CLI all funnel through it). api.ApplyAction additionally checks `sm.Name
// == CardMachineName && IsCardTransitionAction(req.Type) && actor !=
// ActorHuman` and rejects with 403 before ever calling sm.Apply — this
// keeps a gateway-brokered `boid action send --type done` (or go/park/drop/
// reopen) from pushing a card's state directly, asserting a transition the
// daemon never itself decided. IsCardTransitionAction (below) is the exact
// six-verb closed set this restricts; attrs_set/child_added/child_specced/
// child_dropped/noted/answered/wake_due stay reachable from any actor.
// ActorHuman is stamped ONLY by Web UI/CLI-driven code paths; a
// gateway-brokered `boid action send` always carries
// ActorTask(TokenContext.TaskID) instead, which is never equal to
// ActorHuman. accept(verb)'s own internal call to ApplyAction
// (WebAppService.AnswerSuggestion, workflow_action.go) reuses the SAME
// request context the human's accept click established, so it is never
// blocked by its own defense.
//
// See TestApplyAction_CardTransitions_HumanCanApplyEveryEdge_NoSuggestion
// (internal/api) for the pinned proof that a human can still directly drive
// every edge with no suggestion involved.
func NewCardMachine() *StateMachine {
	rules := []Rule{
		{Action: "go", FromStatus: "parked", ToStatus: "working", Manual: true},
		{Action: "working", FromStatus: "parked", ToStatus: "working", Manual: true},
		{Action: "drop", FromStatus: "parked", ToStatus: "dropped", Manual: true},
		{Action: "done", FromStatus: "parked", ToStatus: "done", Manual: true},
		{Action: "park", FromStatus: "working", ToStatus: "parked", Manual: true},
		{Action: "done", FromStatus: "working", ToStatus: "done", Manual: true},
		{Action: "reopen", FromStatus: "done", ToStatus: "parked", Manual: true},
		{Action: "reopen", FromStatus: "dropped", ToStatus: "parked", Manual: true},

		// wake_due: SweepWake's fact-recording self-record — "the wake
		// condition on this card fired" — with NO transition and NO
		// suggestion attached. Manual:false so IsManualAction rejects a
		// khi-pushed direct send; FromStatus "*" because it's a pure
		// timestamp/log fact (in practice it only ever fires against
		// parked, since that's the only status SweepWake evaluates
		// ShouldWake against — see queue_sweep.go).
		{Action: "wake_due", FromStatus: "*"},

		// Shared with NewExecutionMachine (child_dispatched/child_closed)
		// purely so Apply/AvailableActions treat the name as "known"
		// regardless of which machine a caller holds. job_failed is
		// deliberately NOT duplicated here (see this function's doc
		// comment above).
		{Action: "child_dispatched", FromStatus: "*"},
		{Action: "child_closed", FromStatus: "*"},
		{Action: "progress", FromStatus: "*"},
		{Action: "done_request", FromStatus: "*"},
		{Action: "fail_request", FromStatus: "*"},
	}

	// Non-transitioning Manual:true vocabulary — one rule per status in EACH
	// action's own FromStatus set (explicit enumeration, never "*"). Each
	// action below gets its own set, not a single shared one, because their
	// terminal-status reach genuinely differs:
	//
	//   - answered: {parked, working, done, dropped}. The ONLY action that
	//     must reach done/dropped — without it, a done/dropped card carrying
	//     a suggestion (reopen or otherwise) can never be accepted OR
	//     rejected.
	//   - noted: {parked, working, done, dropped}. "見たが変えなかった" is
	//     meaningful on a terminal card too — a workspace script noting that
	//     it observed a done/dropped card is not a data-integrity violation
	//     the way attrs_set's queue-predicate columns would be.
	//   - attrs_set: {parked, working, dropped}. done is DELIBERATELY
	//     excluded here — that path is owned exclusively by the
	//     service-layer guard (resolveAttrsSetDoneTransition,
	//     internal/api/attrs_set_done.go), which runs BEFORE this rule table
	//     is even consulted and has its own row-existence-based admission
	//     check; adding "done" here too would create two authorities
	//     deciding the same admission question. dropped IS included: unlike
	//     done, a dropped card has no equivalent service-layer guard, and
	//     khi legitimately wants to attrs_set a reopen suggestion onto one.
	//   - child_added / child_specced / child_dropped: {parked, working}
	//     only. A terminal card's children list is frozen — there is
	//     nothing left to specc, add, or withdraw once a card is done or
	//     dropped.
	for _, status := range cardActiveAndDroppedStatuses {
		rules = append(rules, Rule{Action: "attrs_set", FromStatus: status, Manual: true})
	}
	for _, action := range []string{"child_added", "child_specced", "child_dropped"} {
		for _, status := range cardActiveStatuses {
			rules = append(rules, Rule{Action: action, FromStatus: status, Manual: true})
		}
	}
	for _, action := range []string{"noted", "answered"} {
		for _, status := range cardActiveAndTerminalStatuses {
			rules = append(rules, Rule{Action: action, FromStatus: status, Manual: true})
		}
	}

	return &StateMachine{
		Name:  CardMachineName,
		Rules: rules,
	}
}

// cardActiveStatuses is the FromStatus enumeration for the non-transitioning
// card vocabulary that only makes sense while a card is still "live"
// (child_added/child_specced/child_dropped — never "*"). attrs_set is NOT
// one of this set's users (see cardActiveAndDroppedStatuses below). What
// the set means: the two statuses where a card's children can still be
// specced/added/withdrawn.
var cardActiveStatuses = []string{"parked", "working"}

// cardActiveAndDroppedStatuses is attrs_set's own FromStatus enumeration —
// cardActiveStatuses plus "dropped". done is deliberately still excluded —
// see NewCardMachine's own doc comment for why that path belongs
// exclusively to resolveAttrsSetDoneTransition
// (internal/api/attrs_set_done.go).
var cardActiveAndDroppedStatuses = []string{"parked", "working", "dropped"}

// cardActiveAndTerminalStatuses extends cardActiveStatuses with done/dropped
// — the FromStatus enumeration for noted/answered specifically (see
// NewCardMachine's own doc comment for why these two, and not the other
// four non-transitioning actions, need to reach a terminal card).
var cardActiveAndTerminalStatuses = []string{"parked", "working", "done", "dropped"}

// cardTransitionActions is the closed set of the six human-only
// card-lifecycle verbs — every Manual:true rule on NewCardMachine whose
// ToStatus != "". This is the exact set api.ApplyAction's push-down defense
// restricts to actor==human; see NewCardMachine's own doc comment for the
// full rationale.
//
// DERIVED from NewCardMachine's own rule table (deriveCardTransitionActions,
// below), not a hand-copied literal: a hand-maintained list here and the
// actual rule table generating it could silently drift apart if a future
// edit changed one without remembering to update the other. Deriving this
// set directly from sm.Rules at package init makes that class of drift
// structurally impossible.
var cardTransitionActions = deriveCardTransitionActions()

// deriveCardTransitionActions walks NewCardMachine's own rule table and
// returns the set of every Manual:true action whose ToStatus != "" — see
// cardTransitionActions' own doc comment for why this replaces a
// hand-maintained literal.
func deriveCardTransitionActions() map[string]bool {
	set := map[string]bool{}
	for _, r := range NewCardMachine().Rules {
		if r.Manual && r.ToStatus != "" {
			set[r.Action] = true
		}
	}
	return set
}

// IsCardTransitionAction reports whether actionType is one of the six
// card-lifecycle transition verbs the push-down defense restricts to
// actor==human requests. Non-transitioning card actions (attrs_set and
// friends) and wake_due are excluded: khi may still send those directly.
func IsCardTransitionAction(actionType string) bool {
	return cardTransitionActions[actionType]
}
