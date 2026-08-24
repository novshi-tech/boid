// Package orchestrator's state-machine layer is split into TWO machines —
// NewExecutionMachine and NewCardMachine (machine_execution.go /
// machine_card.go) — rather than the single unified NewMachine this package
// used to export (PR-B, docs/plans/suggestion-as-state-transition-impl.md
// §2, purely a refactor: rule content/names/behavior are unchanged, only
// which machine object each rule lives on).
//
// Why split at all (docs/plans/suggestion-as-state-transition.md §2.5):
// walking the old unified rule table by reachability showed it was always
// two disconnected islands wearing one name. The pending/executing/awaiting
// island and the captured/triaged/parked/ready/working island share only
// their terminal statuses (done/aborted) — a task born in "captured" never
// reaches "executing", and one born in "pending" never reaches "triaged".
// That separation was never a property of the machine itself; it only held
// because FOUR pieces of discipline living entirely outside machine.go
// enforced it by hand:
//
//   - api.ApplyAction peeking at whether a task carries a task_triage
//     sidecar row to route `reopen` (ordinary done/aborted → executing) away
//     from `reopen_triaged` (triage done/aborted → triaged) — the unified
//     machine could not tell a done triage card apart from a done ordinary
//     task by status alone.
//   - the `triage_done` rule being named that instead of plain `done`, purely
//     because IsManualAction shared one namespace across every rule: reusing
//     "done" would have let `boid action send --type done` push a triage
//     card straight to done from a Manual:true rule, letting khi assert
//     completion the daemon never actually evaluated.
//   - the preExecutionStatuses enumeration discipline (論点6-3): every
//     triage-vocabulary rule (attrs_set/child_added/.../answered) had to
//     spell out its FromStatus set by hand instead of using "*", or it would
//     have reached across the island boundary into executing/done/etc on an
//     ordinary task.
//   - abort and drop's scopes being manually kept from overlapping — abort
//     pinned to the execution-lifecycle statuses, drop to the four
//     pre-execution ones — inside the same rule table.
//
// The deeper reason the split holds is not implementation accident: an
// ordinary task is a session-bearing subject (its status tracks whether a
// sandboxed process is running and how it exited); a card is an object khi
// and nose read and annotate (its status is a judgment ledger — whose turn
// it is, whether it needs attention). A card never runs an agent session of
// its own — khi's judgment happens in a separate triggering task, and even
// the Shape button only ever writes back into the card from a workspace-side
// session. That asymmetry is why the exported constructor is
// NewCardMachine, not some execution-flavored name: the card is a note board,
// not a lesser task.
//
// The entity itself is NOT split (docs/plans/suggestion-as-state-transition.md
// §3.8 considered a dedicated `card` table and rejected it): a card's
// children are ordinary tasks whose parent_id crosses whatever boundary a
// second table would draw, and action log / timeline / Web UI / notify all
// already share one representation profitably. Only the StateMachine each
// task is evaluated against is chosen per task — see internal/api's
// machineFor, which selects NewCardMachine when the task carries a
// task_triage sidecar row and NewExecutionMachine otherwise (the same
// sidecar-existence judgment api.resolveReopenVariant already made before
// the split, now shared by both).
//
// Splitting the machine object collapses three of the four bypass
// disciplines above into the type system itself: reopen_triaged can go back
// to being named `done` inside NewCardMachine's own namespace (kept
// `triage_done` regardless in this PR — action HISTORY rows already say
// `triage_done` and are not renamed; only a later PR may switch new writes
// to `done`), the FromStatus-enumeration discipline stops mattering once
// executing/pending/etc. simply do not exist as reachable statuses in
// NewCardMachine's own rule table, and abort/drop's scopes no longer share a
// table to overlap in (abort exists only in NewExecutionMachine; drop only
// in NewCardMachine). The `reopen` sidecar-lookup routing itself (决定17)
// is NOT one of the things the split removes — a done card and a done
// ordinary task are still indistinguishable by status alone, so api still
// has to look at the sidecar to decide whether "reopen" should apply
// NewExecutionMachine's rule or NewCardMachine's; what changes is that this
// same lookup now ALSO selects which machine object governs the whole
// request (machineFor), not just which of two same-named-but-differently-
// scoped rules to fire within one shared table.
//
// "Rule content/names/behavior are unchanged" is deliberately not "behavior
// never changes by one millimeter" (docs/plans/suggestion-as-state-transition.md
// §3.8's own caveat): splitting the machine object means IsManualAction's
// "is this action name known anywhere" check now runs against a SMALLER
// rule table per call site, which moves an error some callers used to get
// from api.ApplyAction's own IsManualAction gate onto whatever happens next
// once the task itself is loaded (e.g. task-not-found now surfaces as 404
// before an unknown/wrong-machine action name would have 400'd) — see
// workflow_action.go's ApplyAction and this PR's own PR description for the
// specific call sites this touches.
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

// CanApplyManualAction reports whether actionType has a Manual:true rule
// matching status — regardless of whether that rule transitions the task
// (ToStatus != "") or not (ToStatus == "") and regardless of whether it
// would be a self-loop. Unlike AvailableActions, which deliberately EXCLUDES
// non-transitioning and self-loop rules because those don't fit the
// generic "action button" UI (see AvailableActions' own doc comment), this
// is the check a caller uses to gate a DEDICATED button for one specific
// non-transitioning manual action — e.g. the Web UI's `answered`
// Accept/Reject buttons, which have their own bespoke rendering (verb
// badge, reason, basis) and their own POST target, not the generic
// available_actions button row.
//
// Added for Opus review finding #3 (2026-08-19 revisit of PR-3): sm.Apply
// already rejects `answered` from done/dropped/aborted (see
// TestCardMachine_TriageVocabulary_FromStatusEnumerated_NotWildcard),
// but nothing let the Web UI ask "would this actually be accepted" BEFORE
// rendering the button — so a triage task's Accept/Reject buttons kept
// showing after it auto-advanced to done, and clicking them redirected to
// an opaque `no transition for action "answered" from status "done"` error.
func (sm *StateMachine) CanApplyManualAction(actionType string, status TaskStatus) bool {
	for _, r := range sm.Rules {
		if r.Condition != nil || !r.Manual {
			continue
		}
		if r.Action == actionType && (r.FromStatus == "*" || r.FromStatus == string(status)) {
			return true
		}
	}
	return false
}

// IsManualAction reports whether actionType has at least one Manual:true
// rule anywhere in THIS machine, regardless of the task's current status.
// This is the single source of truth for "is this action name allowed
// through the public ApplyAction endpoint for a task governed by this
// machine" (internal/api's HTTP handler / brokered action_send / `boid
// action send` CLI all funnel through ApplyAction, which first resolves the
// task's machine via machineFor, then checks IsManualAction against THAT
// machine — see workflow_action.go). Event-driven rules (job_failed) and
// internal-only manual transitions a different method resolves and applies
// on the caller's behalf (wake_triaged/wake_ready/wake_working, dispatch,
// triage_done, reopen_triaged) must return false here — codex review round 2
// found job_failed missing from a hand-maintained blocklist in internal/api,
// which let triaged→(job_failed)→aborted→(reopen)→executing bypass the
// ready-gate entirely. A per-name blocklist requires remembering to update
// it every time a new non-manual rule is added; checking the Rule's own
// Manual flag here removes that whole class of omission.
func (sm *StateMachine) IsManualAction(actionType string) bool {
	for _, r := range sm.Rules {
		if r.Action == actionType && r.Manual {
			return true
		}
	}
	return false
}
