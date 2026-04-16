package orchestrator

import (
	"encoding/json"
	"fmt"
	"sort"
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

// MergeDefaultInstructions merges behavior default instructions with request instructions.
// Strategy:
//   - Use defaultInstructions as base
//   - Override with request's roles (role-level replacement)
//   - A null role value in requestInstructions means deletion
//
// requestInstructions is accepted as json.RawMessage so that null role values can be
// distinguished from absent roles.
func MergeDefaultInstructions(defaultInstructions map[string]Instruction, requestInstructions json.RawMessage) (map[string]Instruction, error) {
	base := make(map[string]Instruction, len(defaultInstructions))
	for role, inst := range defaultInstructions {
		base[role] = inst
	}
	if len(requestInstructions) == 0 || string(requestInstructions) == "null" || string(requestInstructions) == "{}" {
		return base, nil
	}
	var override map[string]json.RawMessage
	if err := json.Unmarshal(requestInstructions, &override); err != nil {
		return nil, fmt.Errorf("unmarshal instructions: %w", err)
	}
	for role, raw := range override {
		if string(raw) == "null" {
			delete(base, role)
			continue
		}
		var inst Instruction
		if err := json.Unmarshal(raw, &inst); err != nil {
			return nil, fmt.Errorf("unmarshal instruction %q: %w", role, err)
		}
		base[role] = inst
	}
	return base, nil
}

// defaultMessages maps InstructionType to a fallback message used when
// a role's message field is omitted.
var defaultMessages = map[InstructionType]string{
	InstructionTypeExecution:    "タスクを実行してください",
	InstructionTypeRework:       "verification findings の問題を修正してください",
	InstructionTypeVerification: "成果物を検証してください",
}

// messageFallbackType maps an InstructionType to the type to consult when its
// own message is empty and no default is sufficient. rework falls back to
// execution so the original task description is reused as context.
var messageFallbackType = map[InstructionType]InstructionType{
	InstructionTypeRework: InstructionTypeExecution,
}

// resolveMessage returns the message for a role, applying a fallback chain:
//  1. inst.Message if non-empty
//  2. same-consumer instruction of the fallback type (e.g. rework → execution)
//  3. default message for instType
func resolveMessage(inst Instruction, instType InstructionType, all map[string]Instruction) string {
	if inst.Message != "" {
		return inst.Message
	}
	if fallback, ok := messageFallbackType[instType]; ok {
		for _, fi := range all {
			if fi.Type == fallback && fi.Consumer == inst.Consumer && fi.Message != "" {
				return fi.Message
			}
		}
	}
	return defaultMessages[instType]
}

// FilterInstructions extracts instructions matching the given type and consumer,
// sorted by role name for deterministic ordering.
// When a role's message is empty, a fallback chain is applied (see resolveMessage).
func FilterInstructions(instructions map[string]Instruction, instType InstructionType, consumer string) []RoutedInstruction {
	if instType == "" || consumer == "" || len(instructions) == 0 {
		return nil
	}

	var roles []string
	for role, inst := range instructions {
		if inst.Type == instType && inst.Consumer == consumer {
			roles = append(roles, role)
		}
	}
	if len(roles) == 0 {
		return nil
	}
	sort.Strings(roles)

	result := make([]RoutedInstruction, 0, len(roles))
	for _, role := range roles {
		inst := instructions[role]
		result = append(result, RoutedInstruction{
			Role:        role,
			Type:        inst.Type,
			Consumer:    inst.Consumer,
			Name:        inst.Name,
			Message:     resolveMessage(inst, instType, instructions),
			Interactive: inst.Interactive,
			Model:       inst.Model,
		})
	}
	return result
}
