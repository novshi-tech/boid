package reducer

import (
	"encoding/json"
	"fmt"

	"github.com/novshi-tech/boid/internal/model"
)

// TransitionCondition evaluates whether a condition-based transition should fire.
type TransitionCondition func(payload json.RawMessage) bool

type Rule struct {
	Action     string              // manual transition trigger (mutually exclusive with Condition)
	FromStatus string              // "*" matches any
	ToStatus   string
	Condition  TransitionCondition // auto transition trigger (mutually exclusive with Action)
}

type StateMachine struct {
	Name  string
	Rules []Rule
}

// Apply finds an action-based rule matching the action type and current status.
// Condition-based rules are ignored by Apply.
func (sm *StateMachine) Apply(task *model.Task, action *model.Action) (*model.Task, error) {
	for _, r := range sm.Rules {
		if r.Condition != nil {
			continue // skip condition-based rules
		}
		if r.Action == action.Type && (r.FromStatus == "*" || r.FromStatus == string(task.Status)) {
			newTask := *task
			newTask.Status = model.TaskStatus(r.ToStatus)
			return &newTask, nil
		}
	}
	return nil, fmt.Errorf("no transition for action %q from status %q", action.Type, task.Status)
}

// Advance evaluates condition-based rules for the task's current status and payload.
// Returns the transitioned task and true if a condition was met, or (nil, false) otherwise.
func (sm *StateMachine) Advance(task *model.Task) (*model.Task, bool) {
	for _, r := range sm.Rules {
		if r.Condition == nil {
			continue // skip action-based rules
		}
		if r.FromStatus != "*" && r.FromStatus != string(task.Status) {
			continue
		}
		if r.Condition(task.Payload) {
			newTask := *task
			newTask.Status = model.TaskStatus(r.ToStatus)
			return &newTask, true
		}
	}
	return nil, false
}

type Registry struct {
	machines map[string]*StateMachine
}

func (r *Registry) Resolve(project *model.ProjectMeta, behavior string) (*StateMachine, error) {
	b, ok := project.TaskBehaviors[behavior]
	if !ok {
		return nil, fmt.Errorf("behavior %q not found", behavior)
	}
	sm, ok := r.machines[b.Transition]
	if !ok {
		return nil, fmt.Errorf("transition model %q not found", b.Transition)
	}
	return sm, nil
}

func NewDefaultRegistry() *Registry {
	return &Registry{
		machines: map[string]*StateMachine{
			"one-shot":      OneShotMachine(),
			"feedback-loop": FeedbackLoopMachine(),
		},
	}
}

func OneShotMachine() *StateMachine {
	return &StateMachine{
		Name: "one-shot",
		Rules: []Rule{
			{Action: "start", FromStatus: "pending", ToStatus: "executing"},
			{Action: "done", FromStatus: "executing", ToStatus: "done"},
			{Action: "job_completed", FromStatus: "executing", ToStatus: "done"},
			{Action: "job_failed", FromStatus: "*", ToStatus: "aborted"},
			{Action: "abort", FromStatus: "*", ToStatus: "aborted"},
		},
	}
}

func FeedbackLoopMachine() *StateMachine {
	return &StateMachine{
		Name: "feedback-loop",
		Rules: []Rule{
			{Action: "start", FromStatus: "pending", ToStatus: "executing"},
			{Action: "verify", FromStatus: "executing", ToStatus: "verifying"},
			{Action: "job_completed", FromStatus: "executing", ToStatus: "verifying"},
			{Action: "review", FromStatus: "verifying", ToStatus: "in_review"},
			{Action: "job_completed", FromStatus: "verifying", ToStatus: "in_review"},
			{Action: "collect_feedback", FromStatus: "in_review", ToStatus: "collecting_feedback"},
			{Action: "job_completed", FromStatus: "in_review", ToStatus: "collecting_feedback"},
			{Action: "rework", FromStatus: "collecting_feedback", ToStatus: "executing"},
			{Action: "done", FromStatus: "collecting_feedback", ToStatus: "done"},
			{Action: "job_failed", FromStatus: "*", ToStatus: "aborted"},
			{Action: "abort", FromStatus: "*", ToStatus: "aborted"},
		},
	}
}
