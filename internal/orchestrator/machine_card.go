package orchestrator

// NewCardMachine returns the state machine governing cards — the
// suggestion-as-state-transition redesign's "judgment ledger" object
// (docs/plans/suggestion-as-state-transition.md §3.2, implemented per
// docs/plans/suggestion-as-state-transition-impl.md §3). This is card
// machine v2, replacing the nine-status/eight-rule captured/triaged/parked/
// ready/working/dropped/done/aborted machine PR-B (#986) split out of the
// old unified NewMachine — see git history for that shape if it's ever
// needed for reference; this doc comment describes only what exists now.
//
// Four statuses, closed: parked/working/done/dropped. A card NEVER reaches
// pending/executing/awaiting/aborted/captured/triaged/ready through this
// machine's own rule table (captured/triaged/ready are kept as legacy
// KnownTaskStatuses entries purely so pre-cutover DB rows remain readable —
// see model.go's own doc comment — this machine has zero rules admitting
// them). Manual (human-only — see the push-down defense below) transitions:
//
//	go      : parked  → working   (accept(go): specced children get dispatched, then this fires)
//	working : parked  → working   (accept(working): manual work declared, no dispatch)
//	drop    : parked  → dropped
//	park    : working → parked    (sets a wake condition — see workflow_triage.go's park side effect)
//	done    : working → done
//	reopen  : done    → parked
//	reopen  : dropped → parked
//
// This is the network's ENTIRE edge list — see docs/plans/
// suggestion-as-state-transition.md §3.2's table, which this rule set
// implements literally. The invariant it encodes: the only rule the MACHINE
// itself fires is capture (creating a task directly into "parked" —
// task_create.go / task_resolve_or_capture.go); every edge above requires a
// human's accept, whether typed directly (Web UI / CLI hitting ApplyAction)
// or via the accept(verb) flow answering a khi suggestion
// (internal/api/suggestion_accept.go). The old wake_triaged/wake_ready/
// wake_working three-way split is gone entirely: parked has exactly one
// origin now (park only ever fires from working), so there is nothing left
// for a resurfacing mechanism to disambiguate — see queue_sweep.go's
// SweepWake, now reduced to recording a "wake_due" FACT (below) rather than
// resolving and applying a transition on the task's behalf.
//
// job_failed is DELIBERATELY NOT registered here, unlike the old machine
// (which duplicated it from NewExecutionMachine purely so Apply/
// AvailableActions never surprised a caller with "unknown action" — see
// machine_execution.go's own doc comment for why it stays there). A card
// never runs a job of its own (machine.go's package doc comment: "カードには
// エージェントセッションが起きない"), and job_failed's only rule targets
// "aborted" — the one status the redesign explicitly retires from the card's
// reachable set (design doc §3.2: "card は aborted にも到達しなくなる...
// card 機械は4状態で本当に閉じる"). Registering it would reopen exactly that
// fifth status through the rule table even though nothing production-real
// ever fires it here. progress/done_request/fail_request/child_dispatched/
// child_closed stay registered (all non-transitioning, ToStatus=="", so
// none of them can reopen a fifth status) purely for the same "Apply treats
// the name as known" parity the old machine established.
//
// ---- push-down defense (穴11, docs/plans/suggestion-as-state-transition.md
// §3.8's closing note) ----
//
// All seven Manual transition rules above are reachable through the public
// ApplyAction endpoint (HTTP API / Web UI / brokered `boid action send` / CLI
// all funnel through it) because IsManualAction says yes for all of them —
// unlike v1, where wake_triaged/wake_ready/dispatch/triage_done were kept
// Manual:false specifically to keep khi from pushing them directly (a
// NAME-based defense: give the machine-internal step a different name than
// any Manual:true rule, so IsManualAction structurally rejects it). v2 folds
// triage_done back into a plain Manual:true "done" and gives khi no
// machine-internal step to accidentally collide with at all — so the old
// name-based trick has nothing left to hide behind, and a naive port would
// let khi's gateway-brokered `boid action send --type done` (or --type go,
// park, drop, reopen) push a card's state directly, asserting a transition
// the daemon never itself decided.
//
// The replacement is actor-based, not name-based: api.ApplyAction checks
// `sm.Name == CardMachineName && IsCardTransitionAction(req.Type) &&
// actor != ActorHuman` and rejects with 403 before ever calling sm.Apply.
// IsCardTransitionAction (below) is the exact seven-verb closed set — go/
// working/park/drop/done/reopen — the only actions on this machine whose
// ToStatus != "" (attrs_set/child_added/child_specced/child_dropped/noted/
// answered/wake_due all stay reachable from any actor, including khi's
// gateway-brokered pushes, exactly as before). ActorHuman is stamped ONLY by
// Web UI/CLI-driven code paths (web_service.go, action.go's HTTP handler,
// task.go) — a gateway-brokered `boid action send` always carries
// ActorTask(TokenContext.TaskID) instead (internal/server/boid_executor.go),
// which is never equal to ActorHuman even when TaskID is empty (khi's own
// trigger-job calls carry TaskID=="", producing the literal string "task:" —
// still not ActorHuman). accept(verb)'s own internal call to ApplyAction
// (WebAppService.AnswerSuggestion, workflow_action.go) reuses the SAME
// request context the human's accept click established, so it carries
// ActorHuman through and is never blocked by its own defense.
//
// escape hatch (design doc §3.2's explicit requirement): this defense must
// never block a HUMAN from directly driving every one of the seven edges
// with no suggestion involved — see
// TestApplyAction_CardTransitions_HumanCanApplyEveryEdge_NoSuggestion
// (internal/api) for the pinned proof.
func NewCardMachine() *StateMachine {
	rules := []Rule{
		{Action: "go", FromStatus: "parked", ToStatus: "working", Manual: true},
		{Action: "working", FromStatus: "parked", ToStatus: "working", Manual: true},
		{Action: "drop", FromStatus: "parked", ToStatus: "dropped", Manual: true},
		{Action: "park", FromStatus: "working", ToStatus: "parked", Manual: true},
		{Action: "done", FromStatus: "working", ToStatus: "done", Manual: true},
		{Action: "reopen", FromStatus: "done", ToStatus: "parked", Manual: true},
		{Action: "reopen", FromStatus: "dropped", ToStatus: "parked", Manual: true},

		// wake_due (docs/plans/suggestion-as-state-transition-impl.md §C):
		// SweepWake's fact-recording self-record — "the wake condition on
		// this card fired" — with NO transition and NO suggestion attached.
		// Manual:false so IsManualAction rejects a khi-pushed direct send
		// (same protection child_dispatched/child_closed already get);
		// FromStatus "*" because it's a pure timestamp/log fact, unrelated to
		// which of the four card statuses currently applies (in practice it
		// only ever fires against parked, since that's the only status
		// SweepWake evaluates ShouldWake against — see queue_sweep.go).
		{Action: "wake_due", FromStatus: "*"},

		// Shared with NewExecutionMachine (child_dispatched/child_closed —
		// 論点9) purely so Apply/AvailableActions treat the name as "known"
		// regardless of which machine a caller holds. See
		// machine_execution.go's own doc comment; job_failed is deliberately
		// NOT duplicated here (see this function's doc comment above).
		{Action: "child_dispatched", FromStatus: "*"},
		{Action: "child_closed", FromStatus: "*"},
		{Action: "progress", FromStatus: "*"},
		{Action: "done_request", FromStatus: "*"},
		{Action: "fail_request", FromStatus: "*"},
	}

	// Non-transitioning Manual:true vocabulary — one rule per status in EACH
	// action's own FromStatus set (論点6-3: explicit enumeration, never "*").
	// v1 shared ONE set (preExecutionStatuses, five statuses: captured/
	// triaged/parked/ready/working) across all six actions. v2 initially
	// also shared one set (cardNonTransitionStatuses, {parked, working}) —
	// but that broke accept(reopen) from done/dropped entirely (PR #987
	// review, BLOCKER 3): "answered" needs done/dropped too, since design
	// doc §3.2 explicitly lists `done → parked : reopen (直接 or accept)` /
	// `dropped → parked : reopen (直接 or accept)` as required edges, and a
	// suggestion CAN legitimately exist on a done/dropped card (khi is
	// allowed to suggest "reopen" there — see resolveAttrsSetDoneTransition,
	// internal/api/attrs_set_done.go, I-5b, which is exactly how a
	// done card's attrs_set/suggestion write lands). Sharing one set across
	// all six actions is no longer correct — each action below gets its own,
	// with the reason spelled out per action:
	//
	//   - answered: {parked, working, done, dropped}. The ONLY action that
	//     must reach done/dropped — without it, a done/dropped card carrying
	//     a suggestion (reopen or otherwise) can never be accepted OR
	//     rejected: PR-2's future `suggestion_verb IS NOT NULL` queue
	//     predicate would then show an unanswerable entry forever.
	//   - noted: {parked, working, done, dropped}. "見たが変えなかった" (J-5)
	//     is meaningful on a terminal card too — a workspace script noting
	//     that it observed a done/dropped card is not a data-integrity
	//     violation the way attrs_set's queue-predicate columns would be.
	//   - attrs_set: {parked, working, dropped}. done is DELIBERATELY
	//     excluded here — that path is owned exclusively by the
	//     service-layer guard (resolveAttrsSetDoneTransition,
	//     internal/api/attrs_set_done.go, I-5b/I-5c), which runs BEFORE this
	//     rule table is even consulted and has its own row-existence-based
	//     admission check. Adding "done" here too would create two
	//     authorities deciding the same (attrs_set, done) admission
	//     question — the guard's own doc comment already reasons in detail
	//     about exactly which done tasks may receive attrs_set, and
	//     duplicating that reasoning as a blanket machine.go rule would let
	//     the two silently drift out of sync. dropped IS included: unlike
	//     done, a dropped card has no equivalent service-layer guard, and
	//     khi legitimately wants to attrs_set a reopen suggestion onto one
	//     (the mirror of the done→reopen case, with no guard needed because
	//     dropped carries no analogous "which done tasks" ambiguity — any
	//     task_triage-carrying dropped card is unambiguously a card).
	//   - child_added / child_specced / child_dropped: {parked, working}
	//     unchanged. A terminal card's children list is frozen — there is
	//     nothing left to specc, add, or withdraw once a card is done or
	//     dropped, and card machine v2 does not need those three reachable
	//     there the way it needs answered/noted to be.
	// PR #987 review round 2, BLOCKER N1: attrs_set is NOT like
	// child_added/child_specced/child_dropped — this doc comment's own
	// attrs_set bullet above already says {parked, working, dropped}, but an
	// earlier version of this loop folded attrs_set in with the three
	// child_* actions under cardActiveStatuses ({parked, working} only),
	// silently dropping "dropped" and making the doc comment wrong. Without
	// this, a dropped card can never receive an attrs_set-carried suggestion
	// (e.g. khi's "reopen" recommendation) at all, and — compounded by LOW
	// 10's direct-drop strip — a card that reaches "dropped" is GUARANTEED to
	// have no suggestion, making design doc §3.2's `dropped → parked : reopen
	// (via accept)` edge permanently unreachable through its real production
	// path (attrs_set → accept), even though the machine-level rule and the
	// direct Reopen button both work fine.
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
// (child_added/child_specced/child_dropped — 論点6-3: never "*"). attrs_set is
// NOT one of this set's users (see cardActiveAndDroppedStatuses below and
// NewCardMachine's own doc comment, BLOCKER N1). Renamed from v1's
// preExecutionStatuses — that name stopped describing anything real once
// captured/triaged/ready dropped out of the reachable set (they were never
// "pre-execution" vs "in-progress" in v2's sense; there is no execution
// concept on the card side at all). What the set actually means now: the two
// statuses where a card's children can still be specced/added/withdrawn.
var cardActiveStatuses = []string{"parked", "working"}

// cardActiveAndDroppedStatuses is attrs_set's own FromStatus enumeration —
// cardActiveStatuses plus "dropped" (PR #987 review round 2, BLOCKER N1: an
// earlier version of NewCardMachine folded attrs_set into cardActiveStatuses
// alongside child_added/child_specced/child_dropped, silently excluding
// dropped despite this file's own doc comment already documenting dropped as
// included). done is deliberately still excluded — see NewCardMachine's own
// doc comment for why that path belongs exclusively to
// resolveAttrsSetDoneTransition (internal/api/attrs_set_done.go).
var cardActiveAndDroppedStatuses = []string{"parked", "working", "dropped"}

// cardActiveAndTerminalStatuses extends cardActiveStatuses with done/dropped
// — the FromStatus enumeration for noted/answered specifically (see
// NewCardMachine's own doc comment for why these two, and not the other
// four non-transitioning actions, need to reach a terminal card).
var cardActiveAndTerminalStatuses = []string{"parked", "working", "done", "dropped"}

// cardTransitionActions is the closed set of the six human-only
// card-lifecycle verbs — every Manual:true rule on NewCardMachine whose
// ToStatus != "". This is the exact set api.ApplyAction's push-down defense
// (穴11) restricts to actor==human; see NewCardMachine's own doc comment for
// the full rationale.
//
// DERIVED from NewCardMachine's own rule table (deriveCardTransitionActions,
// below), not a hand-copied literal (PR #987 review, MEDIUM 5): a
// hand-maintained list here and the actual rule table generating it could
// silently drift apart if a future edit changed one without remembering to
// update the other — TestCardMachineV2_AllEdges and
// TestIsCardTransitionAction each test one side independently, so neither
// existing test would catch the other half changing. Deriving this set
// directly from sm.Rules at package init makes that class of drift
// structurally impossible instead of relying on a reviewer remembering.
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
// card-lifecycle transition verbs the push-down defense (穴11) restricts to
// actor==human requests. Non-transitioning card actions (attrs_set and
// friends) and wake_due are excluded: khi may still send those directly.
func IsCardTransitionAction(actionType string) bool {
	return cardTransitionActions[actionType]
}
