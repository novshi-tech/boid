package orchestrator

import "encoding/json"

// AwaitingPayload holds the fields of the "awaiting" trait in task.Payload.
//
// Fields written by kits (via boid task notify --ask):
//   - SessionID: the claude --print session ID to resume with --resume
//   - Question: human-readable question text shown to the user
//   - QuestionID: UUID identifying this Q&A turn (for multi-turn tracking)
//
// Fields written by boid (set when the user submits an answer):
//   - PendingAnswer: the user's reply, consumed by the kit on next resume
type AwaitingPayload struct {
	SessionID     string `json:"session_id,omitempty"`
	Question      string `json:"question,omitempty"`
	QuestionID    string `json:"question_id,omitempty"`
	PendingAnswer string `json:"pending_answer,omitempty"`
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
