package orchestrator

import (
	"encoding/json"
	"fmt"
)

// TransitionCondition evaluates whether a condition-based transition should fire.
type TransitionCondition func(payload json.RawMessage) bool

type Rule struct {
	Action          string // manual transition trigger (mutually exclusive with Condition)
	FromStatus      string // "*" matches any
	ToStatus        string
	Condition       TransitionCondition                   // auto transition trigger (mutually exclusive with Action)
	Manual          bool                                  // true if the action is user-initiated (shown in available_actions)
	ActionPayloadFn func(json.RawMessage) json.RawMessage // optional; generates action.Payload when the rule fires
}

// AdvanceOutcome carries the result of a successful condition-based transition.
type AdvanceOutcome struct {
	Task          *Task
	ActionPayload json.RawMessage // nil unless the fired rule has ActionPayloadFn
}

type StateMachine struct {
	Name  string
	Rules []Rule
}

// Apply finds an action-based rule matching the action type and current status.
// Condition-based rules are ignored by Apply.
// When a matching rule has an empty ToStatus the task status is left unchanged
// (non-transitioning action, e.g. "progress").
func (sm *StateMachine) Apply(task *Task, action *Action) (*Task, error) {
	for _, r := range sm.Rules {
		if r.Condition != nil {
			continue // skip condition-based rules
		}
		if r.Action == action.Type && (r.FromStatus == "*" || r.FromStatus == string(task.Status)) {
			newTask := *task
			if r.ToStatus != "" {
				newTask.Status = TaskStatus(r.ToStatus)
			}
			return &newTask, nil
		}
	}
	return nil, fmt.Errorf("no transition for action %q from status %q", action.Type, task.Status)
}

// AdvanceFull evaluates condition-based rules for the task's current status and payload.
// Returns an AdvanceOutcome (including optional action payload) if a condition was met, or nil otherwise.
func (sm *StateMachine) AdvanceFull(task *Task) *AdvanceOutcome {
	for _, r := range sm.Rules {
		if r.Condition == nil {
			continue // skip action-based rules
		}
		if r.FromStatus != "*" && r.FromStatus != string(task.Status) {
			continue
		}
		if r.Condition(task.Payload) {
			newTask := *task
			newTask.Status = TaskStatus(r.ToStatus)
			o := &AdvanceOutcome{Task: &newTask}
			if r.ActionPayloadFn != nil {
				o.ActionPayload = r.ActionPayloadFn(task.Payload)
			}
			return o
		}
	}
	return nil
}

// Advance evaluates condition-based rules for the task's current status and payload.
// Returns the transitioned task and true if a condition was met, or (nil, false) otherwise.
// Use AdvanceFull when the action payload is also needed.
func (sm *StateMachine) Advance(task *Task) (*Task, bool) {
	if o := sm.AdvanceFull(task); o != nil {
		return o.Task, true
	}
	return nil, false
}

// AvailableActions returns the list of manual actions that can be applied to a
// task in the given status. Condition-based (automatic) rules and non-manual
// rules are excluded. Terminal statuses (done, aborted) return an empty list.
func (sm *StateMachine) AvailableActions(status TaskStatus) []string {
	var actions []string
	seen := map[string]bool{}
	for _, r := range sm.Rules {
		if r.Condition != nil || !r.Manual {
			continue
		}
		if r.FromStatus != "*" && r.FromStatus != string(status) {
			continue
		}
		// Skip non-transitioning rules (ToStatus == "", e.g. attrs_set /
		// child_added / child_specced — Phase 1 PR-4). These are Manual:true
		// so IsManualAction lets them through ApplyAction, but they must
		// never appear as a Web UI button/available action: unlike a normal
		// manual transition there is no "and then the status changes" story
		// to show the user, and khi sends them itself via `boid action
		// send`, not a button click (論点6-1, Fable レビュー第9版).
		if r.ToStatus == "" {
			continue
		}
		// Skip self-loops (e.g. abort: * → aborted when status=aborted).
		// A user-actionable transition must change state.
		if r.ToStatus == string(status) {
			continue
		}
		if !seen[r.Action] {
			seen[r.Action] = true
			actions = append(actions, r.Action)
		}
	}
	return actions
}

// IsManualAction reports whether actionType has at least one Manual:true
// rule anywhere in the machine, regardless of the task's current status.
// This is the single source of truth for "is this action name allowed
// through the public ApplyAction endpoint" (internal/api's HTTP handler /
// brokered action_send / `boid action send` CLI all funnel through
// ApplyAction). Event-driven rules (job_failed) and internal-only manual
// transitions a different method resolves and applies on the caller's
// behalf (wake_triaged/wake_ready) must return false here — codex review
// round 2 found job_failed missing from a hand-maintained blocklist in
// internal/api, which let triaged→(job_failed)→aborted→(reopen)→executing
// bypass the ready-gate entirely. A per-name blocklist requires remembering
// to update it every time a new non-manual rule is added; checking the
// Rule's own Manual flag here removes that whole class of omission.
func (sm *StateMachine) IsManualAction(actionType string) bool {
	for _, r := range sm.Rules {
		if r.Action == actionType && r.Manual {
			return true
		}
	}
	return false
}

// DefaultMachine returns the unified state machine used by all tasks.
func DefaultMachine() *StateMachine {
	return NewMachine()
}

// NewMachine returns the unified state machine.
//
// Manual transitions:
//
//	start  : pending → executing
//	done   : executing → done     (UI button; agent path goes through done_request + auto)
//	done   : awaiting → done      (parent confirms child's done_request)
//	fail   : executing → aborted  (UI button; agent path goes through fail_request + auto)
//	reopen : done → executing
//	reopen : aborted → executing  (recover from failure via fix)
//	ask    : executing → awaiting
//	answer : awaiting → executing
//	abort  : pending/executing/awaiting/done/aborted → aborted (execution lifecycle only)
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
// drop is intentionally NOT a "*" wildcard like abort: scoping it to the four
// pre-execution statuses keeps it from becoming a second, overlapping destructive
// action on every ordinary (pending/executing/...) task in the system, and it means a
// dropped task never had a runtime to clean up (drop can't fire mid-execution).
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
// triage_done is DELIBERATELY NOT named "done". IsManualAction is keyed on the
// action NAME across every rule, and "done" already has Manual:true rules
// (executing→done, awaiting→done) — reusing the name would have made
// `boid action send --type done` pushable from working, letting khi assert a
// completion without the daemon ever evaluating 決定15's 「全子 closed ∧
// observed.source_closed」. A distinct non-manual name keeps the judgment
// where 決定12/15 put it: in the daemon's own deterministic sweep
// (api.TaskWorkflowService.SweepDone / the child_closed hook), never in a
// pushed assertion. done は承認不要で自動で落ちる (逆輸入2).
//
// reopen_triaged is Manual:false for the same reason wake_triaged/wake_ready
// are: it is an INTERNAL target that a different verb resolves on the
// caller's behalf. `reopen` stays the single user/khi-facing verb, and
// api.ApplyAction picks the variant by looking at something the machine
// cannot see — whether the task has a task_triage sidecar row. A done triage
// task and a done executor task are indistinguishable by status here, so
// leaving the choice to the caller would mean every caller (Web UI button,
// brokered task.reopen, CLI) could pick the one that flips a card into
// `executing` — i.e. tries to RUN the meta project's card. Wake's doc comment
// spells out the same principle: resolve internally so no caller can get it
// wrong. Being Manual:false also keeps reopen_triaged out of AvailableActions,
// so ordinary tasks never sprout a second, meaningless reopen button.
//
// working still has no abort/drop exit: 破棄 stays a pre-execution-only verb
// (状態機械節 — dropped は nose の判断でしか入らない、 and a card that already
// has dispatched children is past the point where dropping it is meaningful).
//
// Phase 1 PR-4 action vocabulary for khi/agent-driven card updates (論点4/6):
// attrs_set / child_added / child_specced are non-transitioning (ToStatus
// "") Manual:true actions — reachable through ApplyAction/`boid action send`
// exactly like park, but they never change task.Status and (per the
// AvailableActions ToStatus=="" skip above) never show up as a Web UI
// button. FromStatus is an explicit enumerated list per 論点6-3 — NOT "*" —
// so these can't be pushed against an ordinary (non-triage) task sitting in
// executing/done/etc; they only make sense on a card that's somewhere in the
// captured→triaged→parked→ready→working lifecycle. Side-effects (folding the
// action payload into task_triage.detail) live in
// internal/api/workflow_triage.go, following applyParkSideEffect's
// established pattern (payload validated before the Tx, read-modify-write on
// task_triage.detail inside the Tx via GetTaskTriage).
//
// child_dispatched / child_closed are DELIBERATELY ABSENT from Manual:true
// here (論点9: 語彙の役割分担 — khi sends attrs_set/child_added/child_specced;
// the daemon self-records child_dispatched/child_closed as machine facts it
// already knows first-hand). They get a Manual:false, FromStatus:"*"
// registration below purely so sm.Apply/AvailableActions treat the action
// name as "known" the same way progress/done_request do — the actual
// self-recording writers (TaskWorkflowService.Dispatch for child_dispatched,
// TaskWorkflowService.finalizeTerminal's recordChildClosedOnParent for
// child_closed) call tx.CreateAction directly, the same way
// persistFiredEvents/recordDispatchError do, rather than routing through
// sm.Apply. Because these rules are Manual:false, IsManualAction returns
// false for both names, so ApplyAction/BoidOpActionSend automatically reject
// any khi-pushed attempt to send them directly — no separate blocklist
// needed (same mechanism that already protects wake_triaged/wake_ready/
// dispatch above).
//
// Event-driven transitions:
//
//	job_failed : * → aborted
//
// Non-transitioning records (created directly by NotifyTask, bypassing
// ApplyAction):
//
//	progress      : * → *   (FYI timeline note)
//	done_request  : * → *   (agent's `notify --done` intent; consumed by DeriveLifecycle)
//	fail_request  : * → *   (agent's `notify --fail` intent; consumed by DeriveLifecycle)
//
// Auto transitions (condition-based, evaluated after dispatch). Order
// matters — first match wins:
//
//	executing → aborted when lifecycle.executed && lifecycle.fail
//	executing → done    when lifecycle.executed && lifecycle.done
//	executing → done    when lifecycle.executed                     (legacy bare; non-agent hooks)
//
// `lifecycle.{executed,done,fail}` are transient traits injected by the
// coordinator; they are never persisted to the payload. The state machine
// treats them as input signals derived from the action history (done_request
// / fail_request) plus the just-finished hook outcome.
//
// The split between `done_request` (intent recorded immediately) and the
// auto-advance (state transition after `lifecycle.executed` confirms the
// runtime exited cleanly) preserves the bash EXIT trap → `boid job done`
// path. Without this split NotifyTask had to SIGTERM the runtime to apply
// the state transition synchronously, which raced against the SIGUSR1
// graceful-stop path and left jobs marked failed.
//
// Hook failures surface as job_failed via the dispatcher path, which routes
// the task to aborted.
func NewMachine() *StateMachine {
	rules := []Rule{
		// Manual actions
		{Action: "start", FromStatus: "pending", ToStatus: "executing", Manual: true},
		{Action: "done", FromStatus: "executing", ToStatus: "done", Manual: true},
		{Action: "done", FromStatus: "awaiting", ToStatus: "done", Manual: true},
		{Action: "fail", FromStatus: "executing", ToStatus: "aborted", Manual: true},
		{Action: "reopen", FromStatus: "done", ToStatus: "executing", Manual: true},
		{Action: "reopen", FromStatus: "aborted", ToStatus: "executing", Manual: true},
		{Action: "ask", FromStatus: "executing", ToStatus: "awaiting", Manual: true},
		{Action: "answer", FromStatus: "awaiting", ToStatus: "executing", Manual: true},
		// abort is scoped to the execution-lifecycle statuses only (not a "*"
		// wildcard) so it doesn't overlap with drop's coverage of the
		// pre-execution statuses below.
		{Action: "abort", FromStatus: "pending", ToStatus: "aborted", Manual: true},
		{Action: "abort", FromStatus: "executing", ToStatus: "aborted", Manual: true},
		{Action: "abort", FromStatus: "awaiting", ToStatus: "aborted", Manual: true},
		{Action: "abort", FromStatus: "done", ToStatus: "aborted", Manual: true},
		{Action: "abort", FromStatus: "aborted", ToStatus: "aborted", Manual: true},

		// Pre-execution actions (cross-project-issue-triage Phase 1)
		{Action: "triage", FromStatus: "captured", ToStatus: "triaged", Manual: true},
		{Action: "ready", FromStatus: "triaged", ToStatus: "ready", Manual: true},
		{Action: "park", FromStatus: "triaged", ToStatus: "parked", Manual: true},
		{Action: "park", FromStatus: "ready", ToStatus: "parked", Manual: true},
		// wake_triaged/wake_ready/wake_working: Manual:false — see doc comment above NewMachine.
		{Action: "wake_triaged", FromStatus: "parked", ToStatus: "triaged"},
		{Action: "wake_ready", FromStatus: "parked", ToStatus: "ready"},
		{Action: "wake_working", FromStatus: "parked", ToStatus: "working"},
		{Action: "drop", FromStatus: "captured", ToStatus: "dropped", Manual: true},
		{Action: "drop", FromStatus: "triaged", ToStatus: "dropped", Manual: true},
		{Action: "drop", FromStatus: "parked", ToStatus: "dropped", Manual: true},
		{Action: "drop", FromStatus: "ready", ToStatus: "dropped", Manual: true},
		{Action: "reopen", FromStatus: "dropped", ToStatus: "triaged", Manual: true},

		// Phase 1 PR-2: dispatch is Manual:false — see doc comment above NewMachine.
		{Action: "dispatch", FromStatus: "ready", ToStatus: "working"},

		// Phase 1 PR-4 (論点8): the three "working からの出口" exits — see doc
		// comment above NewMachine. Verbs are reused from the pre-execution
		// vocabulary above; only the FromStatus is new.
		{Action: "ready", FromStatus: "working", ToStatus: "ready", Manual: true},
		{Action: "triage", FromStatus: "working", ToStatus: "triaged", Manual: true},
		{Action: "park", FromStatus: "working", ToStatus: "parked", Manual: true},

		// Phase 1 PR-5b (決定15/17) — see the doc comment above NewMachine.
		// triage_done is Manual:false (machine-only); reopen_triaged is the
		// manual return path from a done triage task.
		{Action: "triage_done", FromStatus: "working", ToStatus: "done"},
		{Action: "reopen_triaged", FromStatus: "done", ToStatus: "triaged"},
		{Action: "reopen_triaged", FromStatus: "aborted", ToStatus: "triaged"},

		// Phase 1 PR-4 (論点4/6): khi-pushed non-transitioning card-update
		// actions — see doc comment above NewMachine for why FromStatus is an
		// explicit enumeration rather than "*".
		{Action: "child_dispatched", FromStatus: "*"},
		{Action: "child_closed", FromStatus: "*"},

		// Event-driven (non-manual)
		{Action: "job_failed", FromStatus: "*", ToStatus: "aborted"},

		// Non-transitioning records (created directly by NotifyTask). Registered
		// for completeness; Apply() will accept these actions as valid noops.
		{Action: "progress", FromStatus: "*"},
		{Action: "done_request", FromStatus: "*"},
		{Action: "fail_request", FromStatus: "*"},

		// Auto: lifecycle.fail wins, then lifecycle.done, then bare executed.
		// The fail / done variants carry the agent's report message into the
		// auto_advance action via ActionPayloadFn so the timeline preserves it.
		{
			FromStatus: "executing", ToStatus: "aborted",
			Condition: func(p json.RawMessage) bool {
				return TraitBool(p, "lifecycle.executed") && TraitExists(p, "lifecycle.fail")
			},
			ActionPayloadFn: func(p json.RawMessage) json.RawMessage {
				msg, _ := TraitGetString(p, "lifecycle.fail.message")
				b, _ := json.Marshal(map[string]string{"message": msg})
				return b
			},
		},
		{
			FromStatus: "executing", ToStatus: "done",
			Condition: func(p json.RawMessage) bool {
				return TraitBool(p, "lifecycle.executed") && TraitExists(p, "lifecycle.done")
			},
			ActionPayloadFn: func(p json.RawMessage) json.RawMessage {
				msg, _ := TraitGetString(p, "lifecycle.done.message")
				b, _ := json.Marshal(map[string]string{"message": msg})
				return b
			},
		},
		// Bare auto rule: legacy path for non-agent hooks (scripts that just
		// exit 0 without notify). Keep last so the message-bearing rules above
		// take precedence when the agent reported via done_request/fail_request.
		{FromStatus: "executing", ToStatus: "done", Condition: func(p json.RawMessage) bool {
			return TraitBool(p, "lifecycle.executed")
		}},
	}

	// Phase 1 PR-4 (論点4/6): attrs_set / child_added / child_specced, one
	// rule per status in preExecutionStatuses (explicit enumeration, not
	// "*" — see doc comment above). Generated rather than hand-listed 3x5
	// times to keep the FromStatus set for all three actions mechanically
	// identical (a hand-copied list risks the three verbs silently drifting
	// out of sync with each other).
	for _, action := range []string{"attrs_set", "child_added", "child_specced"} {
		for _, status := range preExecutionStatuses {
			rules = append(rules, Rule{Action: action, FromStatus: status, Manual: true})
		}
	}

	return &StateMachine{
		Name:  "default",
		Rules: rules,
	}
}

// preExecutionStatuses is the explicit FromStatus enumeration Phase 1 PR-4's
// attrs_set/child_added/child_specced rules use (論点6-3: never "*" — that
// would let these fire against executing/done/etc on an ordinary
// non-triage task too).
var preExecutionStatuses = []string{"captured", "triaged", "parked", "ready", "working"}
