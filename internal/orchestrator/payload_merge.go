package orchestrator

import (
	"encoding/json"
	"fmt"
)

// MergeDefaultPayload merges behavior default payload with request payload.
// Request payload takes precedence over default.
// Strategy:
//   - Use default_payload as base
//   - Override with request payload's top-level keys
//   - A null override top-level key means deletion
//
// "instructions" is no longer allowed in payload; use MergeDefaultInstructions instead.
func MergeDefaultPayload(defaultPayload, requestPayload json.RawMessage) (json.RawMessage, error) {
	if err := RejectPayloadInstructions(defaultPayload); err != nil {
		return nil, fmt.Errorf("default_payload: %w", err)
	}
	if err := RejectPayloadInstructions(requestPayload); err != nil {
		return nil, fmt.Errorf("request payload: %w", err)
	}

	if len(defaultPayload) == 0 || string(defaultPayload) == "null" {
		if len(requestPayload) == 0 {
			return json.RawMessage("{}"), nil
		}
		return requestPayload, nil
	}
	if len(requestPayload) == 0 || string(requestPayload) == "{}" || string(requestPayload) == "null" {
		return defaultPayload, nil
	}

	var base map[string]json.RawMessage
	if err := json.Unmarshal(defaultPayload, &base); err != nil {
		return nil, err
	}

	var override map[string]json.RawMessage
	if err := json.Unmarshal(requestPayload, &override); err != nil {
		return nil, err
	}

	for key, val := range override {
		if string(val) == "null" {
			delete(base, key)
			continue
		}
		base[key] = val
	}

	return json.Marshal(base)
}

// RejectPayloadInstructions returns an error if payload contains an "instructions" top-level key.
// instructions moved out of payload into Task.Instructions; accepting it here would silently drop it.
func RejectPayloadInstructions(payload json.RawMessage) error {
	if len(payload) == 0 || string(payload) == "{}" || string(payload) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil
	}
	if _, ok := m["instructions"]; ok {
		return fmt.Errorf(`"instructions" must be provided at top level, not inside payload`)
	}
	return nil
}

// MergeDefaultInstructions builds the initial instruction list for a new task.
// Strategy:
//   - Take the behavior default's "main" entry (if any) as the seed
//   - If requestInstructions is provided, use it instead of the default
//
// requestInstructions accepts the new array form `[{...}, ...]` and the legacy
// single-instruction map form `{"main": {...}}` for backward compatibility.
func MergeDefaultInstructions(defaultInstructions map[string]Instruction, requestInstructions json.RawMessage) (Instructions, error) {
	var base Instructions
	if main, ok := defaultInstructions["main"]; ok {
		base = Instructions{main}
	}
	if len(requestInstructions) == 0 || string(requestInstructions) == "null" || string(requestInstructions) == "{}" || string(requestInstructions) == "[]" {
		return base, nil
	}
	var override Instructions
	if err := json.Unmarshal(requestInstructions, &override); err != nil {
		return nil, fmt.Errorf("unmarshal instructions: %w", err)
	}
	if len(override) == 0 {
		return base, nil
	}
	return override, nil
}

// AppendInstruction returns a new instruction list with `inst` appended. The
// caller is responsible for filling in fields not derived from the previous
// active entry. Used by `boid task reopen` to record a new context message
// while preserving history.
func AppendInstruction(existing Instructions, inst Instruction) Instructions {
	out := make(Instructions, len(existing)+1)
	copy(out, existing)
	out[len(existing)] = inst
	return out
}

// defaultMessages maps InstructionType to a fallback message used when
// a role's message field is omitted.
var defaultMessages = map[InstructionType]string{
	InstructionTypeExecution: "タスクを実行してください",
}

// resolveMessage returns the message for an instruction, falling back to the
// type's default when the message field is empty.
func resolveMessage(inst Instruction, instType InstructionType) string {
	if inst.Message != "" {
		return inst.Message
	}
	return defaultMessages[instType]
}

// FilterInstructions returns the active routed instruction for the given
// consumer. Only the most recent entry in the history is considered (older
// entries are kept for audit but do not drive dispatch). Returns nil when
// type/consumer is empty or the active entry does not match.
func FilterInstructions(instructions Instructions, instType InstructionType, consumer string) []RoutedInstruction {
	if instType == "" || consumer == "" || len(instructions) == 0 {
		return nil
	}
	active := instructions[len(instructions)-1]
	if active.Type != "" && active.Type != instType {
		return nil
	}
	if active.Consumer != consumer {
		return nil
	}
	return []RoutedInstruction{{
		Role:        active.Name,
		Type:        instType,
		Consumer:    active.Consumer,
		Name:        active.Name,
		Message:     resolveMessage(active, instType),
		Interactive: active.Interactive,
		Model:       active.Model,
	}}
}
