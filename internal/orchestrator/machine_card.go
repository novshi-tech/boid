package orchestrator

// NewCardMachine returns the state machine governing cards — the
// cross-project-issue-triage lifecycle (captured/triaged/parked/ready/
// working, plus the shared terminal dropped/done/aborted). This is the
// "judgment ledger" island the unified NewMachine (pre-PR-B) used to combine
// with the execution lifecycle above; see machine.go's package doc comment
// for why the two were split and what stayed unchanged. Selected via
// internal/api's machineFor for any task that DOES carry a task_triage
// sidecar row.
//
// Pre-execution transitions (cross-project-issue-triage Phase 1, captured/triaged/
// parked/ready/dropped — docs/plans/cross-project-issue-triage.md):
//
//	triage       : captured → triaged
//	ready        : triaged → ready
//	park         : triaged → parked
//	park         : ready → parked
//	wake_triaged : parked → triaged  (Manual:false — machine-internal, see below)
//	wake_ready   : parked → ready    (Manual:false — machine-internal, see below)
//	wake_working : parked → working  (Manual:false — machine-internal, added by BD-9;
//	                                   see the Phase 1 PR-4 section further below, where
//	                                   the matching "park: working → parked" exit lives)
//	drop         : captured/triaged/parked/ready → dropped
//	reopen       : dropped → triaged (recovery from a mistaken drop)
//
// wake_triaged/wake_ready/wake_working are deliberately non-manual so they never
// appear in AvailableActions. A parked task's "起点" (which status it was parked
// from) decides which one is correct: getting triaged vs ready wrong would silently
// promote a task past Go (triaged → ready) without nose's judgment — the one thing
// 決定9/逆輸入2 protect — and wake_working (BD-9 fix, for 論点8's "park +
// wake_task_id" sequential-PR pattern) simply returns a task that was already past
// Go back to where it was, so the same "let the origin decide, not the caller"
// discipline applies even though there is no Go-gate risk on that particular branch.
// The single user-facing verb is TaskWorkflowService.Wake, which looks up the origin
// via ParkedFrom (derived from the actions log, not a duplicated column — 決定13) and
// calls sm.Apply with the correct action name.
//
// drop is intentionally NOT a "*" wildcard like NewExecutionMachine's abort: it
// simply has no rule outside the four pre-execution statuses, and — since the
// split — no execution-lifecycle status (pending/executing/awaiting) is even
// reachable in THIS machine's rule table at all. A dropped task never had a
// runtime to clean up (drop can't fire mid-execution).
//
// Phase 1 PR-2 (docs/plans/cross-project-issue-triage.md, 逆輸入2):
//
//	dispatch : ready → working  (Manual:false — machine-internal, see below)
//
// dispatch is the second of the two stages 逆輸入2 describes ("Go 操作 = ready
// 遷移 + 機械 dispatch の 2 段"): the manual "ready" action IS Go (nose's
// judgment); "dispatch" is the purely mechanical follow-up (task-ify any
// `specced` entries in task_triage.detail.children, then flip the status) that
// requires no further human judgment (決定12) and so must never be reachable
// directly through the public ApplyAction endpoint — same rationale, and same
// IsManualAction gate, as wake_triaged/wake_ready above. The single caller is
// TaskWorkflowService.Dispatch, invoked automatically by ApplyAction right
// after a "ready" action commits (see workflow_action.go and
// workflow_triage.go's Dispatch doc comment for the child-task-creation
// details this rule alone doesn't capture).
//
// Phase 1 PR-4 (docs/plans/cross-project-issue-triage.md, 論点8 — 「working
// からの出口3本」): a card that reached working (a child was dispatched, or
// nose is manually handling it) can be re-surfaced without ever going
// through done/aborted, because more work on the SAME card routinely shows
// up while its first child is still executing (a PR review comment, the next
// PR in a sequential series). Three Manual exits, reusing the SAME verbs the
// pre-execution statuses already use (so khi/nose only ever have to know
// "ready"/"triage"/"park", not a working-specific vocabulary):
//
//	ready  : working → ready    (next child is already specced — Go is one click away)
//	triage : working → triaged  (next child is still open — needs shaping first)
//	park   : working → parked   (Go-time park with wake_task_id=<dispatched child>;
//	                              QueueSweepLoop's existing wake-on-child-termination
//	                              re-surfaces the card once that child finishes — this
//	                              is the "sequential PR consumption" pattern expressed
//	                              via park+wake, not a new mechanism)
//
// ApplyAction's "ready"→Dispatch chaining (workflow_action.go) already
// triggers on newTask.Status == ready regardless of fromStatus, so
// working→ready gets the same automatic Dispatch follow-up as triaged→ready
// for free. Dispatch() itself is UNCHANGED: it still task-ifies every
// `specced` child in one call — the discipline that only one child should be
// specced at a time in a sequential PR series is a caller/khi-side
// convention (決定12 の外側), not something the machine enforces.
//
// wake_working : parked → working  (Manual:false — machine-internal, see above)
//
// BD-9 (2026-08-18): the park:working exit above needs a matching return path
// — QueueSweepLoop wakes a working-origin park via the SAME
// TaskWorkflowService.Wake entry point used for wake_triaged/wake_ready, so
// Wake's origin switch needs a third case. wake_working was missing since
// this park:working exit's own PR (the rule set above added the park side
// but never revisited Wake's ParkedFrom switch to match), so every such wake
// 500'd with "unexpected park origin \"working\"" and a wake_task_id-carrying
// working-park could never re-surface — the exact 論点8 sequential-PR-
// consumption flow this rule set exists for. Waking to working intentionally
// skips the ready→working Dispatch chain in TaskWorkflowService.Wake
// (workflow_triage.go — it only chains when newTask.Status ==
// TaskStatusReady): a working-origin park already has its child dispatched,
// so there is nothing left to (re-)dispatch.
//
// Phase 1 PR-5b (決定15/17) closes the terminal end of that lifecycle, which
// PR-4 had left explicitly 未決:
//
//	triage_done    : working → done     (Manual:false — machine-internal)
//	reopen_triaged : done → triaged     (Manual:false — machine-internal)
//	reopen_triaged : aborted → triaged  (Manual:false — machine-internal)
//
// triage_done is DELIBERATELY NOT named "done", even though the split (PR-B)
// removed the ORIGINAL reason for that (the unified machine's shared
// IsManualAction namespace, where reusing "done" would have collided with
// NewExecutionMachine's own Manual:true done rules and let khi push
// `boid action send --type done` from working, asserting a completion
// without the daemon ever evaluating 決定15's 「全子 closed ∧
// observed.source_closed」). Renaming
// action HISTORY is out of scope for this refactor regardless (docs/plans/
// suggestion-as-state-transition-impl.md §0's "action 履歴の rename はやらない"
// — existing `triage_done` rows stay `triage_done`); only a later PR that
// also migrates every reader of that action name may switch new writes to
// plain `done`. What the split DOES remove structurally: with card and
// execution now separate rule tables, `done` simply is not a rule in
// NewCardMachine at all, so there is no adjacent Manual:true `done` rule for
// a same-named card rule to collide with even if a future PR does rename it.
// Until then, the judgment stays where 決定12/15 put it: in the daemon's own
// deterministic sweep (api.TaskWorkflowService.SweepDone / the
// child_closed hook), never in a pushed assertion. done は承認不要で自動で
// 落ちる (逆輸入2).
//
// reopen_triaged is Manual:false for the same reason wake_triaged/wake_ready
// are: it is an INTERNAL target that a different verb resolves on the
// caller's behalf. `reopen` stays the single user/khi-facing verb, and
// api.ApplyAction picks the variant by looking at something the machine
// cannot see — whether the task has a task_triage sidecar row (the SAME
// judgment api.machineFor now uses to pick this very machine object in the
// first place, PR-B). A done triage task and a done executor task are
// indistinguishable by status here, so leaving the choice to the caller
// would mean every caller (Web UI button, brokered task.reopen, CLI) could
// pick the one that flips a card into `executing` — i.e. tries to RUN the
// meta project's card. Wake's doc comment spells out the same principle:
// resolve internally so no caller can get it wrong. Being Manual:false also
// keeps reopen_triaged out of AvailableActions, so ordinary tasks never
// sprout a second, meaningless reopen button.
//
// working still has no abort/drop exit: 破棄 stays a pre-execution-only verb
// (状態機械節 — dropped は nose の判断でしか入らない、 and a card that already
// has dispatched children is past the point where dropping it is meaningful).
// Since the split, this is doubly true: NewCardMachine has no abort rule at
// all (abort lives only in NewExecutionMachine — see its own doc comment),
// so a card cannot reach aborted through this machine no matter its status.
//
// Phase 1 PR-4 action vocabulary for khi/agent-driven card updates (論点4/6):
// attrs_set / child_added / child_specced are non-transitioning (ToStatus
// "") Manual:true actions — reachable through ApplyAction/`boid action
// send` exactly like park, but they never change task.Status and (per the
// AvailableActions ToStatus=="" skip above) never show up as a Web UI
// button. FromStatus is an explicit enumerated list per 論点6-3 — NOT "*" —
// so these can't be pushed against an ordinary (non-triage) task sitting in
// executing/done/etc; they only make sense on a card that's somewhere in the
// captured→triaged→parked→ready→working lifecycle. Since the split, that
// discipline is reinforced structurally too: executing/pending/awaiting are
// not even reachable statuses in THIS machine's rule table, so the
// enumeration below is now belt-and-suspenders rather than the only thing
// standing between these rules and an ordinary task. Side-effects (folding
// the action payload into task_triage.detail) live in
// internal/api/workflow_triage.go, following applyParkSideEffect's
// established pattern (payload validated before the Tx, read-modify-write on
// task_triage.detail inside the Tx via GetTaskTriage).
//
// docs/plans/ingestion-identity.md PR-3 (B-3+B-4): noted (J-5) and answered
// (J-6) join the SAME non-transitioning/Manual:true/preExecutionStatuses
// shape as attrs_set/child_added/child_specced above — same "reachable
// through ApplyAction, never a Web UI button, FromStatus never *" story.
//
//   - noted { <any JSON> } is a fully opaque record ("見たが変えなかった" の
//     記録): the daemon parses it only far enough to confirm it is valid
//     JSON (so `action_list`'s JSON-array response stays well-formed) and
//     never interprets a single key inside it. Consumer is workspace-side
//     scripts reading it back via action_list, never the daemon.
//   - answered { answer: "accept"|"reject", verb, basis } is the Web UI's
//     accept/reject record for a task_triage suggestion (書き手は daemon
//     自身の Web UI, 決定14). answer is validated (closed two-value set);
//     verb/basis are recorded but never cross-checked against the
//     suggestion they answer (J-7 — that would mean reading the opaque
//     suggestion blob, crossing the boundary). Its side effect (dropping
//     detail.attrs.suggestion) lives in workflow_triage.go's
//     applyAnsweredSideEffect, same fold-side placement as attrs_set's own
//     side effect below (決定13: event 追記が正, state は導出).
//
// child_dispatched / child_closed are DELIBERATELY ABSENT from Manual:true
// here (論点9: 語彙の役割分担 — khi sends attrs_set/child_added/child_specced;
// the daemon self-records child_dispatched/child_closed as machine facts it
// already knows first-hand). They get a Manual:false, FromStatus:"*"
// registration below purely so sm.Apply/AvailableActions treat the action
// name as "known" the same way progress/done_request do (and are duplicated
// onto NewExecutionMachine for the identical reason — see that function's
// own doc comment) — the actual self-recording writers
// (TaskWorkflowService.Dispatch for child_dispatched,
// TaskWorkflowService.finalizeTerminal's recordChildClosedOnParent for
// child_closed) call tx.CreateAction directly, the same way
// persistFiredEvents/recordDispatchError do, rather than routing through
// sm.Apply. Because these rules are Manual:false, IsManualAction returns
// false for both names, so ApplyAction/BoidOpActionSend automatically reject
// any khi-pushed attempt to send them directly — no separate blocklist
// needed (same mechanism that already protects wake_triaged/wake_ready/
// dispatch above).
//
// job_failed / progress / done_request / fail_request are ALSO duplicated
// here from NewExecutionMachine, for the same "Apply treats the name as
// known" reason — see that function's own doc comment. A card's own status
// is never pending/executing/awaiting, so job_failed's "* → aborted" rule
// and the non-transitioning trio never actually fire against a card in
// practice (no job runs directly against a card task — see machine.go's
// package doc comment on why a card never bears its own session); they are
// registered here purely so a caller holding a NewCardMachine never gets a
// spurious "no transition for action" surprise for these six shared names.
func NewCardMachine() *StateMachine {
	rules := []Rule{
		// Pre-execution actions (cross-project-issue-triage Phase 1)
		{Action: "triage", FromStatus: "captured", ToStatus: "triaged", Manual: true},
		{Action: "ready", FromStatus: "triaged", ToStatus: "ready", Manual: true},
		{Action: "park", FromStatus: "triaged", ToStatus: "parked", Manual: true},
		{Action: "park", FromStatus: "ready", ToStatus: "parked", Manual: true},
		// wake_triaged/wake_ready/wake_working: Manual:false — see doc comment above NewCardMachine.
		{Action: "wake_triaged", FromStatus: "parked", ToStatus: "triaged"},
		{Action: "wake_ready", FromStatus: "parked", ToStatus: "ready"},
		{Action: "wake_working", FromStatus: "parked", ToStatus: "working"},
		{Action: "drop", FromStatus: "captured", ToStatus: "dropped", Manual: true},
		{Action: "drop", FromStatus: "triaged", ToStatus: "dropped", Manual: true},
		{Action: "drop", FromStatus: "parked", ToStatus: "dropped", Manual: true},
		{Action: "drop", FromStatus: "ready", ToStatus: "dropped", Manual: true},
		{Action: "reopen", FromStatus: "dropped", ToStatus: "triaged", Manual: true},

		// Phase 1 PR-2: dispatch is Manual:false — see doc comment above NewCardMachine.
		{Action: "dispatch", FromStatus: "ready", ToStatus: "working"},

		// Phase 1 PR-4 (論点8): the three "working からの出口" exits — see doc
		// comment above NewCardMachine. Verbs are reused from the pre-execution
		// vocabulary above; only the FromStatus is new.
		{Action: "ready", FromStatus: "working", ToStatus: "ready", Manual: true},
		{Action: "triage", FromStatus: "working", ToStatus: "triaged", Manual: true},
		{Action: "park", FromStatus: "working", ToStatus: "parked", Manual: true},

		// Phase 1 PR-5b (決定15/17) — see the doc comment above NewCardMachine.
		// triage_done is Manual:false (machine-only); reopen_triaged is the
		// manual return path from a done triage task.
		{Action: "triage_done", FromStatus: "working", ToStatus: "done"},
		{Action: "reopen_triaged", FromStatus: "done", ToStatus: "triaged"},
		{Action: "reopen_triaged", FromStatus: "aborted", ToStatus: "triaged"},

		// Phase 1 PR-4 (論点4/6): khi-pushed non-transitioning card-update
		// actions — see doc comment above NewCardMachine for why FromStatus is an
		// explicit enumeration rather than "*".
		{Action: "child_dispatched", FromStatus: "*"},
		{Action: "child_closed", FromStatus: "*"},

		// Shared with NewExecutionMachine — see this function's own doc
		// comment above and machine_execution.go's.
		{Action: "job_failed", FromStatus: "*", ToStatus: "aborted"},
		{Action: "progress", FromStatus: "*"},
		{Action: "done_request", FromStatus: "*"},
		{Action: "fail_request", FromStatus: "*"},
	}

	// Phase 1 PR-4 (論点4/6): attrs_set / child_added / child_specced, one
	// rule per status in preExecutionStatuses (explicit enumeration, not
	// "*" — see doc comment above). Generated rather than hand-listed 3x5
	// times to keep the FromStatus set for all three actions mechanically
	// identical (a hand-copied list risks the three verbs silently drifting
	// out of sync with each other).
	//
	// docs/plans/ingestion-identity.md PR-3: noted (J-5) and answered (J-6)
	// join the same loop — same non-transitioning/Manual:true shape, same
	// "never *" FromStatus discipline (論点6-3). noted is a fully opaque
	// record ("見た" の記録) the daemon never interprets; answered is the Web
	// UI's accept/reject record (its `suggestion`-drop side effect lives in
	// workflow_triage.go's applyAnsweredSideEffect, mirroring
	// applyAttrsSetSideEffect — see workflow_action.go's switch).
	//
	// child_dropped joins the same loop: khi withdrawing a child it decided
	// not to pursue. It stays on khi's side of 論点9's split because it is a
	// JUDGEMENT ("this child should not be pursued"), not a machine fact —
	// child_closed remains daemon-only and keeps meaning "the child's task
	// terminated". DropDetailChild refuses a dispatched child, so the two
	// never overlap.
	for _, action := range []string{"attrs_set", "child_added", "child_specced", "child_dropped", "noted", "answered"} {
		for _, status := range preExecutionStatuses {
			rules = append(rules, Rule{Action: action, FromStatus: status, Manual: true})
		}
	}

	return &StateMachine{
		Name:  "card",
		Rules: rules,
	}
}

// preExecutionStatuses is the explicit FromStatus enumeration Phase 1 PR-4's
// attrs_set/child_added/child_specced rules use (論点6-3: never "*" — that
// would let these fire against executing/done/etc on an ordinary
// non-triage task too).
var preExecutionStatuses = []string{"captured", "triaged", "parked", "ready", "working"}
