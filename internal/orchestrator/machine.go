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
	Condition       TransitionCondition                    // auto transition trigger (mutually exclusive with Action)
	Manual          bool                                   // true if the action is user-initiated (shown in available_actions)
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
//	abort  : * → aborted
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
	return &StateMachine{
		Name: "default",
		Rules: []Rule{
			// Manual actions
			{Action: "start",  FromStatus: "pending",   ToStatus: "executing", Manual: true},
			{Action: "done",   FromStatus: "executing", ToStatus: "done",      Manual: true},
			{Action: "done",   FromStatus: "awaiting",  ToStatus: "done",      Manual: true},
			{Action: "fail",   FromStatus: "executing", ToStatus: "aborted",   Manual: true},
			{Action: "reopen", FromStatus: "done",      ToStatus: "executing", Manual: true},
			{Action: "reopen", FromStatus: "aborted",   ToStatus: "executing", Manual: true},
			{Action: "ask",    FromStatus: "executing", ToStatus: "awaiting",  Manual: true},
			{Action: "answer", FromStatus: "awaiting",  ToStatus: "executing", Manual: true},
			{Action: "abort",  FromStatus: "*",         ToStatus: "aborted",   Manual: true},

			// Event-driven (non-manual)
			{Action: "job_failed", FromStatus: "*", ToStatus: "aborted"},

			// Non-transitioning records (created directly by NotifyTask). Registered
			// for completeness; Apply() will accept these actions as valid noops.
			{Action: "progress",     FromStatus: "*"},
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
		},
	}
}
