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

	// Non-transitioning Manual:true vocabulary (attrs_set/child_added/
	// child_specced/child_dropped/noted/answered) — one rule per status in
	// cardNonTransitionStatuses (論点6-3: explicit enumeration, never "*").
	// v1 called this set preExecutionStatuses and enumerated FIVE statuses
	// (captured/triaged/parked/ready/working); v2 shrinks it to exactly TWO
	// (parked/working) because captured/triaged/ready no longer exist as
	// reachable card statuses at all. See cardNonTransitionStatuses' own doc
	// comment for the rename rationale.
	//
	// done is handled separately and is NOT in this loop: attrs_set landing
	// on a done card is a service-layer special case
	// (resolveAttrsSetDoneTransition, internal/api/attrs_set_done.go, I-5b/
	// I-5c) that this PR keeps unchanged — khi's terminal-card observation
	// writes still land, just via that guard rather than a machine.go rule.
	for _, action := range []string{"attrs_set", "child_added", "child_specced", "child_dropped", "noted", "answered"} {
		for _, status := range cardNonTransitionStatuses {
			rules = append(rules, Rule{Action: action, FromStatus: status, Manual: true})
		}
	}

	return &StateMachine{
		Name:  CardMachineName,
		Rules: rules,
	}
}

// cardNonTransitionStatuses is the explicit FromStatus enumeration the
// non-transitioning card vocabulary (attrs_set/child_added/child_specced/
// child_dropped/noted/answered) uses (論点6-3: never "*"). Renamed from v1's
// preExecutionStatuses — that name stopped describing anything real once
// captured/triaged/ready dropped out of the reachable set (they were never
// "pre-execution" vs "in-progress" in v2's sense; there is no execution
// concept on the card side at all). What the set actually means now: the two
// statuses where khi is still actively watching a card and may write to it.
var cardNonTransitionStatuses = []string{"parked", "working"}

// cardTransitionActions is the closed set of the six human-only
// card-lifecycle verbs — every Manual:true rule on NewCardMachine whose
// ToStatus != "". This is the exact set api.ApplyAction's push-down defense
// (穴11) restricts to actor==human; see NewCardMachine's own doc comment for
// the full rationale.
var cardTransitionActions = map[string]bool{
	"go": true, "working": true, "park": true, "drop": true, "done": true, "reopen": true,
}

// IsCardTransitionAction reports whether actionType is one of the six
// card-lifecycle transition verbs the push-down defense (穴11) restricts to
// actor==human requests. Non-transitioning card actions (attrs_set and
// friends) and wake_due are excluded: khi may still send those directly.
func IsCardTransitionAction(actionType string) bool {
	return cardTransitionActions[actionType]
}
