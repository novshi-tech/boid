package orchestrator

import (
	"encoding/json"
	"fmt"
)

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
//   - Take the behavior's default_instruction (if any) as the seed
//   - If requestInstructions is provided, use it instead of the default
//
// requestInstructions accepts the array form `[{...}, ...]` and the legacy
// single-map form `{"main": {...}}` (handled by Instructions.UnmarshalJSON).
func MergeDefaultInstructions(defaultInstruction *Instruction, requestInstructions json.RawMessage) (Instructions, error) {
	var base Instructions
	if defaultInstruction != nil {
		base = Instructions{*defaultInstruction}
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
// agent. Only the most recent entry in the history is considered (older
// entries are kept for audit but do not drive dispatch). Returns nil when
// type/agent is empty or the active entry does not match.
func FilterInstructions(instructions Instructions, instType InstructionType, agent string) []RoutedInstruction {
	if instType == "" || agent == "" || len(instructions) == 0 {
		return nil
	}
	active := instructions[len(instructions)-1]
	if active.Type != "" && active.Type != instType {
		return nil
	}
	if active.Agent != agent {
		return nil
	}
	return []RoutedInstruction{{
		Role:    active.Name,
		Type:    instType,
		Agent:   active.Agent,
		Name:    active.Name,
		Message: resolveMessage(active, instType),
		Model:   active.Model,
	}}
}
