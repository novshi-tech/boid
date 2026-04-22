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

// DefaultMachine returns the single unified state machine used by all tasks.
//
// Manual transitions:
//
//	start       : pending → executing
//	done        : executing / verifying → done
//	reopen      : done → reworking
//	job_failed  : * → aborted
//	abort       : * → aborted
//
// Auto transitions from executing:
//
//	(artifact || tasks) && AnyFindingUnresolvedForState("executing")  → reworking
//	(artifact || tasks) && !AnyFindingUnresolvedForState("executing") → verifying
//	!(artifact || tasks) && lifecycle.executed                        → done
//
// tasks trait と artifact trait は「executing での成果物が揃った」という
// 対称のシグナルとして扱う。plan タスク（tasks を書く）も dev タスク
// （artifact を書く）も同じ executing → verifying パスを辿り、verifying で
// reviewer hook/gate を噛ませる余地を残す。verifying に reviewer が無ければ
// pass-through で done に落ちる。
//
// Auto transitions from verifying:
//
//	AnyFindingUnresolvedForState("verifying")  → reworking
//	!AnyFindingUnresolvedForState("verifying") → done (pass-through when no verify gate)
//
// Auto transitions from reworking:
//
//	!AnyFindingUnresolvedForState("reworking") → verifying (re-enter verification gate)
//	AnyFindingUnresolvedForState("reworking")  → reworking (self-loop until rework fixes all findings)
//
// reworking 判定は source_state=reworking の finding のみを見る。
// verifying-source の open finding (例: mergeable-check) は verifying 再入場時に
// 同じ gate が再実行されて subkey が上書きされる設計なので、reworking 退場を
// ブロックするべきではない。全 source を見る NoUnresolvedFindings() を使うと、
// verifying で書かれた open finding が永久に解消されずデッドロックする。
func DefaultMachine() *StateMachine {
	executionComplete := func(p json.RawMessage) bool {
		return TraitNonNull(p, "artifact") || TasksReady(p)
	}
	return &StateMachine{
		Name: "default",
		Rules: []Rule{
			// Manual actions
			{Action: "start", FromStatus: "pending", ToStatus: "executing", Manual: true},
			{Action: "done", FromStatus: "executing", ToStatus: "done", Manual: true},
			{Action: "done", FromStatus: "verifying", ToStatus: "done", Manual: true},
			{Action: "done", FromStatus: "reworking", ToStatus: "done", Manual: true},
			{Action: "reopen", FromStatus: "done", ToStatus: "reworking", Manual: true},
			{Action: "job_failed", FromStatus: "*", ToStatus: "aborted"},
			{Action: "abort", FromStatus: "*", ToStatus: "aborted", Manual: true},

			// Auto transitions from executing
			{FromStatus: "executing", ToStatus: "reworking", Condition: func(p json.RawMessage) bool {
				return executionComplete(p) && AnyFindingUnresolvedForState("executing")(p)
			}},
			{FromStatus: "executing", ToStatus: "verifying", Condition: func(p json.RawMessage) bool {
				return executionComplete(p) && !AnyFindingUnresolvedForState("executing")(p)
			}},
			// 成果物なしで lifecycle.executed が立っている場合は done（rework 対象なし）
			{FromStatus: "executing", ToStatus: "done", Condition: func(p json.RawMessage) bool {
				return TraitBool(p, "lifecycle.executed") && !executionComplete(p)
			}},

			// Auto transitions from verifying
			{FromStatus: "verifying", ToStatus: "reworking", Condition: AnyFindingUnresolvedForState("verifying")},
			{FromStatus: "verifying", ToStatus: "done", Condition: func(p json.RawMessage) bool {
				return !AnyFindingUnresolvedForState("verifying")(p)
			}},

			// Auto transitions from reworking
			{FromStatus: "reworking", ToStatus: "verifying", Condition: func(p json.RawMessage) bool {
				return !AnyFindingUnresolvedForState("reworking")(p)
			}},
			{FromStatus: "reworking", ToStatus: "reworking", Condition: AnyFindingUnresolvedForState("reworking")},
		},
	}
}
