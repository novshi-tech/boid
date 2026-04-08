package orchestrator

import (
	"encoding/json"
	"fmt"
)

// TransitionCondition evaluates whether a condition-based transition should fire.
type TransitionCondition func(payload json.RawMessage) bool

type Rule struct {
	Action     string // manual transition trigger (mutually exclusive with Condition)
	FromStatus string // "*" matches any
	ToStatus   string
	Condition  TransitionCondition // auto transition trigger (mutually exclusive with Action)
	Manual     bool                // true if the action is user-initiated (shown in available_actions)
}

type StateMachine struct {
	Name  string
	Rules []Rule
}

// Apply finds an action-based rule matching the action type and current status.
// Condition-based rules are ignored by Apply.
func (sm *StateMachine) Apply(task *Task, action *Action) (*Task, error) {
	for _, r := range sm.Rules {
		if r.Condition != nil {
			continue // skip condition-based rules
		}
		if r.Action == action.Type && (r.FromStatus == "*" || r.FromStatus == string(task.Status)) {
			newTask := *task
			newTask.Status = TaskStatus(r.ToStatus)
			return &newTask, nil
		}
	}
	return nil, fmt.Errorf("no transition for action %q from status %q", action.Type, task.Status)
}

// Advance evaluates condition-based rules for the task's current status and payload.
// Returns the transitioned task and true if a condition was met, or (nil, false) otherwise.
func (sm *StateMachine) Advance(task *Task) (*Task, bool) {
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
			return &newTask, true
		}
	}
	return nil, false
}

// AvailableActions returns the list of manual actions that can be applied to a
// task in the given status. Condition-based (automatic) rules and non-manual
// rules are excluded. Terminal statuses (done, aborted) return an empty list.
func (sm *StateMachine) AvailableActions(status TaskStatus) []string {
	if status == TaskStatusDone || status == TaskStatusAborted {
		return nil
	}
	var actions []string
	seen := map[string]bool{}
	for _, r := range sm.Rules {
		if r.Condition != nil || !r.Manual {
			continue
		}
		if r.FromStatus == "*" || r.FromStatus == string(status) {
			if !seen[r.Action] {
				seen[r.Action] = true
				actions = append(actions, r.Action)
			}
		}
	}
	return actions
}

type TransitionRegistry struct {
	machines map[string]*StateMachine
}

// Get returns the StateMachine for the given transition model name.
func (r *TransitionRegistry) Get(transition string) (*StateMachine, bool) {
	sm, ok := r.machines[transition]
	return sm, ok
}

func (r *TransitionRegistry) Resolve(task *Task) (*StateMachine, error) {
	sm, ok := r.machines[task.Transition]
	if !ok {
		return nil, fmt.Errorf("transition model %q not found", task.Transition)
	}
	return sm, nil
}

func NewDefaultRegistry() *TransitionRegistry {
	return &TransitionRegistry{
		machines: map[string]*StateMachine{
			"one-shot":          OneShotMachine(),
			"one-shot-feedback": OneShotFeedbackMachine(),
			"feedback-loop":     FeedbackLoopMachine(),
		},
	}
}

// defaultRegistry is the package-level registry used by GetMachine.
var defaultRegistry = NewDefaultRegistry()

// GetMachine returns a built-in StateMachine by transition model name.
func GetMachine(transition string) (*StateMachine, bool) {
	return defaultRegistry.Get(transition)
}

func OneShotMachine() *StateMachine {
	return &StateMachine{
		Name: "one-shot",
		Rules: []Rule{
			{Action: "start", FromStatus: "pending", ToStatus: "executing", Manual: true},
			// Condition: tasks ready → auto-advance to done (for triage/plan tasks)
			{FromStatus: "executing", ToStatus: "done", Condition: func(p json.RawMessage) bool {
				return TasksReady(p)
			}},
			// Condition: artifact present → auto-advance to done (for simple impl tasks)
			{FromStatus: "executing", ToStatus: "done", Condition: func(p json.RawMessage) bool {
				return TraitNonNull(p, "artifact")
			}},
			{Action: "done", FromStatus: "executing", ToStatus: "done", Manual: true},
			{Action: "job_failed", FromStatus: "*", ToStatus: "aborted"},
			{Action: "abort", FromStatus: "*", ToStatus: "aborted", Manual: true},
		},
	}
}

// OneShotFeedbackMachine is a one-shot variant with a dedicated reworking state
// for the CI modification loop.
//
// Flow:
//
//	pending → executing → (CI ok or no verification) → done
//	                    → (CI open findings)         → reworking → (CI ok) → done
//	                                                             → (CI open) → reworking (loop)
//
// The executing state runs the initial implementation hook and the
// github-pr-verification gate. When the gate reports open findings the task
// transitions to reworking, where a rework-type instruction drives the agent
// to fix the CI failures. The gate re-runs under reworking and the cycle
// repeats until all findings are resolved.
func OneShotFeedbackMachine() *StateMachine {
	return &StateMachine{
		Name: "one-shot-feedback",
		Rules: []Rule{
			{Action: "start", FromStatus: "pending", ToStatus: "executing", Manual: true},
			// Condition: tasks ready → done (for triage/plan tasks)
			{FromStatus: "executing", ToStatus: "done", Condition: TasksReady},
			// Condition: artifact present AND no unresolved executing-state findings → done.
			{FromStatus: "executing", ToStatus: "done", Condition: func(p json.RawMessage) bool {
				return TraitNonNull(p, "artifact") && !AnyFindingUnresolvedForState("executing")(p)
			}},
			// Condition: artifact present AND executing-state CI findings open →
			// enter reworking to drive the CI fix loop with a dedicated instruction.
			{FromStatus: "executing", ToStatus: "reworking", Condition: func(p json.RawMessage) bool {
				return TraitNonNull(p, "artifact") && AnyFindingUnresolvedForState("executing")(p)
			}},
			// Condition: reworking findings all resolved → done.
			{FromStatus: "reworking", ToStatus: "done", Condition: AllFindingsResolvedForState("reworking")},
			// Condition: reworking findings still open → stay in reworking.
			{FromStatus: "reworking", ToStatus: "reworking", Condition: AnyFindingUnresolvedForState("reworking")},
			{Action: "done", FromStatus: "executing", ToStatus: "done", Manual: true},
			{Action: "done", FromStatus: "reworking", ToStatus: "done", Manual: true},
			{Action: "job_failed", FromStatus: "*", ToStatus: "aborted"},
			{Action: "abort", FromStatus: "*", ToStatus: "aborted", Manual: true},
		},
	}
}

func FeedbackLoopMachine() *StateMachine {
	return &StateMachine{
		Name: "feedback-loop",
		Rules: []Rule{
			{Action: "start", FromStatus: "pending", ToStatus: "executing", Manual: true},
			// Condition: artifact present → auto-advance to verifying
			{FromStatus: "executing", ToStatus: "verifying", Condition: func(p json.RawMessage) bool {
				return TraitNonNull(p, "artifact")
			}},
			// Condition: any finding unresolved → rework (back to executing)
			{FromStatus: "verifying", ToStatus: "executing", Condition: AnyFindingUnresolvedForState("verifying")},
			// Condition: all findings resolved → advance to in_review
			{FromStatus: "verifying", ToStatus: "in_review", Condition: AllFindingsResolvedForState("verifying")},
			{Action: "collect_feedback", FromStatus: "in_review", ToStatus: "collecting_feedback", Manual: true},
			// Condition: any feedback finding unresolved → rework
			{FromStatus: "collecting_feedback", ToStatus: "executing", Condition: AnyFindingUnresolvedForState("collecting_feedback")},
			// Condition: all feedback findings resolved → done
			{FromStatus: "collecting_feedback", ToStatus: "done", Condition: AllFindingsResolvedForState("collecting_feedback")},
			{Action: "job_failed", FromStatus: "*", ToStatus: "aborted"},
			{Action: "abort", FromStatus: "*", ToStatus: "aborted", Manual: true},
		},
	}
}
