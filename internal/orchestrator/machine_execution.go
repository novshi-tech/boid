package orchestrator

import "encoding/json"

// NewExecutionMachine returns the state machine governing ordinary
// (session-bearing) tasks — pending/executing/awaiting/done/aborted. This is
// the "process" island the unified NewMachine (pre-PR-B) used to combine
// with the card lifecycle below; see machine.go's package doc comment for
// why the two were split and what stayed unchanged. Selected via
// internal/api's machineFor for any task that does NOT carry a task_triage
// sidecar row.
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
// abort is scoped to the execution-lifecycle statuses (not a "*" wildcard,
// and — since PR-B — not present in NewCardMachine at all): a card cannot be
// aborted, only dropped (drop's coverage lives entirely in NewCardMachine).
// Before the split this was a manually-drawn non-overlap between abort and
// drop inside one shared rule table; splitting the machine object makes it
// structural — abort simply does not exist as a rule anywhere a card's
// status could match.
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
// child_dispatched / child_closed are registered here too (Manual:false,
// FromStatus "*") even though an ordinary execution task is never itself a
// triage card's dispatched child's PARENT — see machine_card.go's doc
// comment for their real writers and rationale (論点9). They are duplicated
// onto BOTH machines (alongside job_failed/progress/done_request/
// fail_request above) purely so StateMachine.Apply/AvailableActions treat
// the action names as "known" regardless of which machine a caller happens
// to be holding — the actual writers call tx.CreateAction directly rather
// than routing through sm.Apply, so which machine object is "the" one
// registering them has no behavioral effect; keeping them on both avoids a
// spurious "no transition for action" surprise if a future caller ever does
// pass them through Apply against whichever machine it was holding.
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
func NewExecutionMachine() *StateMachine {
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
		// wildcard) — see this function's own doc comment above.
		{Action: "abort", FromStatus: "pending", ToStatus: "aborted", Manual: true},
		{Action: "abort", FromStatus: "executing", ToStatus: "aborted", Manual: true},
		{Action: "abort", FromStatus: "awaiting", ToStatus: "aborted", Manual: true},
		{Action: "abort", FromStatus: "done", ToStatus: "aborted", Manual: true},
		{Action: "abort", FromStatus: "aborted", ToStatus: "aborted", Manual: true},

		// Shared with NewCardMachine (see this function's own doc comment
		// and machine_card.go's): registered on both purely so Apply/
		// AvailableActions treat the action name as "known" regardless of
		// which machine a caller is holding. The real writers bypass
		// sm.Apply and call tx.CreateAction directly.
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

	return &StateMachine{
		Name:  ExecutionMachineName,
		Rules: rules,
	}
}
