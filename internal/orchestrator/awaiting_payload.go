package orchestrator

import "encoding/json"

// Awaiting Q&A delivery modes. Mode selects how an answer reaches the agent:
//
//   - AwaitingModeSessionResume (default / empty): the agent exited on `notify
//     --ask`; the answer is stored as PendingAnswer and a fresh `claude
//     --resume` hook consumes it on the next dispatch. This is the legacy path
//     and remains the behaviour when Mode is absent (existing records are not
//     broken by the new field).
//   - AwaitingModeBlocking: the agent is still alive, blocked inside a
//     `boid task ask` broker RPC. The answer is handed back to it directly via
//     the in-memory BlockingAskRegistry; no resume hook is dispatched.
const (
	AwaitingModeSessionResume = "session_resume"
	AwaitingModeBlocking      = "blocking"
)

// AwaitingPayload holds the fields of the "awaiting" trait in task.Payload.
//
// Fields written by kits (via boid task notify --ask):
//   - SessionID: the harness session ID used to resume the agent on next invocation
//   - Question: human-readable question text shown to the user
//   - QuestionID: UUID identifying this Q&A turn (for multi-turn tracking)
//
// Fields written by boid (set when the user submits an answer):
//   - PendingAnswer: the user's reply, consumed by the kit on next resume
//
// Mode/Source are optional and omitempty so existing awaiting records (which
// predate these fields) deserialize unchanged. A missing Mode is treated as
// AwaitingModeSessionResume (the historical default). Source is a placeholder
// for future multi-agent messaging (who asked / who should answer); it is not
// consumed by the current Q&A logic.
type AwaitingPayload struct {
	SessionID     string `json:"session_id,omitempty"`
	Question      string `json:"question,omitempty"`
	QuestionID    string `json:"question_id,omitempty"`
	PendingAnswer string `json:"pending_answer,omitempty"`
	Mode          string `json:"mode,omitempty"`
	Source        string `json:"source,omitempty"`
}

// ClearPendingAnswer removes the pending_answer field from the awaiting trait
// while preserving session_id, question, and question_id. This is called after
// a hook is spawned so the answer is not consumed again on the next resume.
// Returns payload unchanged when the awaiting trait is absent or has no answer.
func ClearPendingAnswer(payload json.RawMessage) json.RawMessage {
	if len(payload) == 0 {
		return payload
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return payload
	}
	raw, ok := top[string(TraitAwaiting)]
	if !ok || string(raw) == "null" {
		return payload
	}
	var ap AwaitingPayload
	if err := json.Unmarshal(raw, &ap); err != nil {
		return payload
	}
	if ap.PendingAnswer == "" {
		return payload
	}
	ap.PendingAnswer = ""
	apJSON, err := json.Marshal(ap)
	if err != nil {
		return payload
	}
	top[string(TraitAwaiting)] = apJSON
	out, err := json.Marshal(top)
	if err != nil {
		return payload
	}
	return out
}

// StripAwaitingTrait removes the entire awaiting trait from a payload.
// The awaiting trait is owned exclusively by ApplyAction("ask"/"answer") and
// the lifecycle, so any value carried in a coordinator's FinalPayload (which
// snapshots task.Payload before the hook runs) is necessarily stale and must
// not be merged back over the freshly-persisted DB state on hook completion.
// Returns the payload unchanged when the awaiting trait is absent.
func StripAwaitingTrait(payload json.RawMessage) json.RawMessage {
	if len(payload) == 0 {
		return payload
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return payload
	}
	if _, ok := top[string(TraitAwaiting)]; !ok {
		return payload
	}
	delete(top, string(TraitAwaiting))
	out, err := json.Marshal(top)
	if err != nil {
		return payload
	}
	return out
}

// GetAwaitingPayload reads the awaiting trait from raw payload.
// Returns an empty struct if the trait is absent or malformed.
func GetAwaitingPayload(payload json.RawMessage) AwaitingPayload {
	if len(payload) == 0 {
		return AwaitingPayload{}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return AwaitingPayload{}
	}
	raw, ok := top[string(TraitAwaiting)]
	if !ok || string(raw) == "null" {
		return AwaitingPayload{}
	}
	var ap AwaitingPayload
	_ = json.Unmarshal(raw, &ap)
	return ap
}
