package orchestrator

import (
	"context"
	"encoding/json"
)

// Lifecycle holds computed lifecycle traits derived from action history and
// the current dispatch state. It is never persisted to the payload; it is
// injected transiently before state-machine evaluation.
//
// Done / Fail carry the agent's intent reported via `notify --done` /
// `notify --fail` (recorded as `done_request` / `fail_request` actions). They
// gate the executing→done / executing→aborted auto-advance rules. By keeping
// these as derived traits (rather than firing an immediate state transition
// inside NotifyTask), the runtime is allowed to exit cleanly via SIGUSR1 →
// bash EXIT trap → `boid job done` before the dispatch loop advances the
// state. See docs/plans/lifecycle-accountability.md (Phase 2.c follow-up).
type Lifecycle struct {
	Executed bool         `json:"executed"`
	Done     *DoneReport  `json:"done,omitempty"`
	Fail     *FailReport  `json:"fail,omitempty"`
	Abort    *AbortReason `json:"abort,omitempty"`
}

// DoneReport carries the message the agent reported via `notify --done`.
type DoneReport struct {
	Message string `json:"message"`
}

// FailReport carries the message the agent reported via `notify --fail`.
type FailReport struct {
	Message string `json:"message"`
}

// AbortReason holds metadata derived from the aborted-state transition action.
type AbortReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// LifecycleStore is the minimal interface required to derive lifecycle traits
// from persistent action history.
type LifecycleStore interface {
	ListActionsByTask(taskID string) ([]*Action, error)
}

// DeriveLifecycle computes lifecycle traits from action history.
// hookExecuted indicates whether a hook job completed successfully in the
// current dispatch cycle (used to set lifecycle.executed).
// If store is nil, only Executed is set from hookExecuted.
//
// Done / Fail are scoped to the current executing cycle: every time the task
// (re-)enters executing (start / answer / reopen) the prior intent is cleared,
// so a `done_request` recorded before a `reopen` does not bleed into the new
// cycle. done_request / fail_request are mutually exclusive — recording one
// clears the other.
func DeriveLifecycle(_ context.Context, taskID string, store LifecycleStore, hookExecuted bool) (Lifecycle, error) {
	lc := Lifecycle{Executed: hookExecuted}
	if store == nil {
		return lc, nil
	}
	actions, err := store.ListActionsByTask(taskID)
	if err != nil {
		return lc, err
	}
	for _, a := range actions {
		if a.ToStatus == TaskStatusExecuting {
			lc.Done = nil
			lc.Fail = nil
		}
		switch a.Type {
		case "done_request":
			lc.Done = doneReportFromPayload(a.Payload)
			lc.Fail = nil
		case "fail_request":
			lc.Fail = failReportFromPayload(a.Payload)
			lc.Done = nil
		}
		if a.ToStatus == TaskStatusAborted && lc.Abort == nil {
			lc.Abort = abortReasonFromPayload(a.Payload)
		}
	}
	return lc, nil
}

// injectLifecycle merges the lifecycle block into payload and returns the
// result. The caller must NOT persist the returned payload — lifecycle is
// a transient, computed trait.
func injectLifecycle(payload json.RawMessage, lc Lifecycle) json.RawMessage {
	var m map[string]json.RawMessage
	if len(payload) == 0 || string(payload) == "null" {
		m = make(map[string]json.RawMessage)
	} else if err := json.Unmarshal(payload, &m); err != nil {
		return payload
	}
	lcJSON, err := json.Marshal(lc)
	if err != nil {
		return payload
	}
	m["lifecycle"] = lcJSON
	result, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return result
}

func doneReportFromPayload(payload json.RawMessage) *DoneReport {
	if len(payload) == 0 || string(payload) == "{}" || string(payload) == "null" {
		return &DoneReport{}
	}
	var m struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return &DoneReport{}
	}
	return &DoneReport{Message: m.Message}
}

func failReportFromPayload(payload json.RawMessage) *FailReport {
	if len(payload) == 0 || string(payload) == "{}" || string(payload) == "null" {
		return &FailReport{}
	}
	var m struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return &FailReport{}
	}
	return &FailReport{Message: m.Message}
}

func abortReasonFromPayload(payload json.RawMessage) *AbortReason {
	if len(payload) == 0 || string(payload) == "{}" || string(payload) == "null" {
		return &AbortReason{}
	}
	var m map[string]string
	if err := json.Unmarshal(payload, &m); err != nil {
		return &AbortReason{}
	}
	return &AbortReason{Code: m["code"], Message: m["message"]}
}
