package orchestrator

import "encoding/json"

// AwaitingPayload holds the fields of the "awaiting" trait in task.Payload.
//
// The Q&A flow is now uniformly the blocking RPC `boid task ask`: the agent
// stays alive inside a broker connection while the daemon waits for a user
// answer, which is then handed back over the same socket via the in-memory
// BlockingAskRegistry. There is no session-resume path: the legacy
// `notify --ask` → `reopen -m` round trip is no longer wired to claude
// `--resume`, and every dispatch starts a fresh agent process.
//
// Fields written when an `ask` action lands:
//   - Question: human-readable question text shown to the user
//   - QuestionID: UUID identifying this Q&A turn (for multi-turn tracking)
//
// Fields written by boid (set when the user submits an answer):
//   - PendingAnswer: the user's reply (legacy field; the blocking RPC delivers
//     answers in-memory and never sets this, but legacy `notify --ask` paths
//     still surface it as $BOID_USER_ANSWER on the next hook invocation)
//
// SessionID/Mode/Source have been removed: the harness-resume mode they
// described is gone, and persisted records with those fields deserialize
// cleanly (encoding/json ignores unknown keys).
type AwaitingPayload struct {
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
