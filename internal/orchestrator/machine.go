// Package orchestrator's state-machine layer is split into TWO machines —
// NewExecutionMachine and NewCardMachine (machine_execution.go /
// machine_card.go) — because the pending/executing/awaiting statuses and
// the parked/working/done/dropped card statuses share only their terminal
// statuses and never reach each other. An ordinary task is a session-bearing
// subject (its status tracks a sandboxed process); a card is a judgment
// ledger khi and nose read and annotate, never running a session of its own.
//
// The entity itself is not split — a card's children are ordinary tasks, and
// action log / timeline / Web UI / notify all share one representation.
// Only the StateMachine each task is evaluated against is chosen per task —
// see internal/api's machineFor, which selects NewCardMachine when the task
// carries a task_triage sidecar row and NewExecutionMachine otherwise.
//
// See docs/plans/suggestion-as-state-transition.md and its -impl.md
// companion for the full history of this split.
package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CardMachineName / ExecutionMachineName are the StateMachine.Name values
// NewCardMachine/NewExecutionMachine stamp, so a caller (api.ApplyAction) can
// tell which machine a task resolved to without re-deriving machineFor's
// sidecar-row judgment — comparing sm.Name against this constant is that check.
const (
	CardMachineName      = "card"
	ExecutionMachineName = "execution"
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
			// CloneTaskShallow, not a bare `*task` copy: callers of Apply
			// (workflow_action.go) mutate the returned task's
			// Exec.Instructions/Exec.Payload directly (e.g. reopen's
			// instruction-override, the generic action-payload merge) — a
			// bare struct copy would still share the SAME ExecAttrs/CardAttrs
			// pointer as the input task, so that mutation would alias back
			// into it. See CloneTaskShallow's own doc comment.
			newTask := CloneTaskShallow(task)
			if r.ToStatus != "" {
				newTask.Status = TaskStatus(r.ToStatus)
			}
			return newTask, nil
		}
	}
	return nil, fmt.Errorf("no transition for action %q from status %q", action.Type, task.Status)
}

// AdvanceFull evaluates condition-based rules for the task's current status and payload.
// Returns an AdvanceOutcome (including optional action payload) if a condition was met, or nil otherwise.
//
// Condition/ActionPayloadFn are only ever set on NewExecutionMachine's rules
// (never on NewCardMachine's — see machine_card.go), so task.Exec.Payload
// below is only evaluated when r.Condition != nil, which in practice means
// task is always an execution task here. The nil guard is defense in depth,
// not evidence a card is expected to reach this loop body.
func (sm *StateMachine) AdvanceFull(task *Task) *AdvanceOutcome {
	if task.Exec == nil {
		return nil
	}
	for _, r := range sm.Rules {
		if r.Condition == nil {
			continue // skip action-based rules
		}
		if r.FromStatus != "*" && r.FromStatus != string(task.Status) {
			continue
		}
		if r.Condition(task.Exec.Payload) {
			newTask := CloneTaskShallow(task)
			newTask.Status = TaskStatus(r.ToStatus)
			o := &AdvanceOutcome{Task: newTask}
			if r.ActionPayloadFn != nil {
				o.ActionPayload = r.ActionPayloadFn(task.Exec.Payload)
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
		// child_added / child_specced). These are Manual:true so
		// IsManualAction lets them through ApplyAction, but they must never
		// appear as a Web UI button/available action: unlike a normal manual
		// transition there is no "and then the status changes" story to
		// show the user, and khi sends them itself via `boid action send`,
		// not a button click.
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

// AvailableActionsHint returns a human-readable clause naming which manual
// actions CAN be applied from status right now — e.g. "from status=parked
// you can apply: go, working, drop" — or a "status=X has no further
// transitions available" fallback when AvailableActions(status) is empty.
//
// Single source for "this didn't work, but here's what would have", used
// by every caller that needs to turn a failed/inapplicable action into a
// self-explanatory message instead of a bare rejection: internal/api's
// suggestion-accept 409 (applyAnswered, acceptGo) and the Web UI's
// inapplicable-suggestion notice both call THIS method rather than each
// re-deriving the list by hand.
func (sm *StateMachine) AvailableActionsHint(status TaskStatus) string {
	available := sm.AvailableActions(status)
	if len(available) == 0 {
		return fmt.Sprintf("status=%s has no further transitions available", status)
	}
	return fmt.Sprintf("from status=%s you can apply: %s", status, strings.Join(available, ", "))
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
// available_actions button row. Lets a caller ask "would this actually be
// accepted" before rendering a button, rather than rendering it
// unconditionally and surfacing sm.Apply's rejection as an opaque error.
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

// CanApplyTransitionAction reports whether actionType has a Manual:true,
// STATUS-CHANGING (ToStatus != "") rule matching status — i.e. whether
// sm.Apply(task-in-status, {Type: actionType}) would actually succeed right
// now. This is CanApplyManualAction's narrower sibling: CanApplyManualAction
// also returns true for a non-transitioning rule (attrs_set/noted/answered/
// ...) matching the same action+status pair, which is exactly right for
// gating a dedicated non-transitioning button (its own doc comment's
// example) but wrong for answering "would ACCEPTING this specific verb
// actually flip the task's status from here" — the question a suggestion's
// own verb (orchestrator.IsCardTransitionAction's six-verb set) always
// raises, since every one of those verbs names a real transition, never a
// non-transitioning fact. The card machine rule table admits exactly one
// status per verb (e.g. "done" only fires from "working"), so this lets a
// caller ask "would this verb actually apply from the task's current
// status" before rendering a live Accept button, rather than letting
// sm.Apply reject with an opaque error.
//
// Deliberately does NOT exclude self-loop rules (ToStatus == FromStatus) the
// way AvailableActions does: a self-loop rule still makes sm.Apply SUCCEED
// (no functional status change, but no error either), so hiding it here
// would make this method say "cannot apply" for something that would, in
// fact, apply cleanly. AvailableActions' self-loop filter exists purely for
// its own different job — deciding whether a generic action-bar BUTTON has
// "and then the status changes" a story to tell — not for this method's job
// of predicting sm.Apply's success/failure.
func (sm *StateMachine) CanApplyTransitionAction(actionType string, status TaskStatus) bool {
	for _, r := range sm.Rules {
		if r.Condition != nil || !r.Manual || r.ToStatus == "" {
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
// machine — see workflow_action.go). Event-driven and daemon-recorded facts
// (job_failed, wake_due, child_dispatched, child_closed, progress,
// done_request, fail_request — all Manual:false) must return false here.
// Checking the Rule's own Manual flag, rather than a hand-maintained
// per-name blocklist, removes the risk of forgetting to update a blocklist
// when a new non-manual rule is added.
func (sm *StateMachine) IsManualAction(actionType string) bool {
	for _, r := range sm.Rules {
		if r.Action == actionType && r.Manual {
			return true
		}
	}
	return false
}
