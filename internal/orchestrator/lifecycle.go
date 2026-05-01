package orchestrator

import (
	"context"
	"encoding/json"
)

// Lifecycle holds computed lifecycle traits derived from action history and
// the current dispatch state. It is never persisted to the payload; it is
// injected transiently before state-machine evaluation.
type Lifecycle struct {
	Executed bool         `json:"executed"`
	Abort    *AbortReason `json:"abort,omitempty"`
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
