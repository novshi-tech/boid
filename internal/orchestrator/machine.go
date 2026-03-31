package orchestrator

import (
	"encoding/json"
	"fmt"

	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/internal/project"
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

func (r *Registry) Resolve(meta *project.ProjectMeta, behavior string) (*StateMachine, error) {
	b, ok := meta.TaskBehaviors[behavior]
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
			// Condition: tasks ready → auto-advance to done (for triage/plan tasks)
			{FromStatus: "executing", ToStatus: "done", Condition: func(p json.RawMessage) bool {
				return TasksReady(p)
			}},
			// Condition: artifact present → auto-advance to done (for simple impl tasks)
			{FromStatus: "executing", ToStatus: "done", Condition: func(p json.RawMessage) bool {
				return TraitNonNull(p, "artifact")
			}},
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
			// Condition: artifact present → auto-advance to verifying
			{FromStatus: "executing", ToStatus: "verifying", Condition: func(p json.RawMessage) bool {
				return TraitNonNull(p, "artifact")
			}},
			// Condition: any finding unresolved → rework (back to executing)
			{FromStatus: "verifying", ToStatus: "executing", Condition: AnyFindingUnresolvedForState("verifying")},
			// Condition: all findings resolved → advance to in_review
			{FromStatus: "verifying", ToStatus: "in_review", Condition: AllFindingsResolvedForState("verifying")},
			{Action: "collect_feedback", FromStatus: "in_review", ToStatus: "collecting_feedback"},
			// Condition: any feedback finding unresolved → rework
			{FromStatus: "collecting_feedback", ToStatus: "executing", Condition: AnyFindingUnresolvedForState("collecting_feedback")},
			// Condition: all feedback findings resolved → done
			{FromStatus: "collecting_feedback", ToStatus: "done", Condition: AllFindingsResolvedForState("collecting_feedback")},
			{Action: "job_failed", FromStatus: "*", ToStatus: "aborted"},
			{Action: "abort", FromStatus: "*", ToStatus: "aborted"},
		},
	}
}
