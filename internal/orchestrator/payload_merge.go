package orchestrator

import (
	"encoding/json"
	"sort"
)

// MergeDefaultPayload merges behavior default payload with request payload.
// Request payload takes precedence over default.
// Strategy:
//   - Use default_payload as base
//   - Override with request payload's top-level keys
//   - A null override top-level key means deletion
//   - "instructions" key uses role-level merge via mergeInstructions
func MergeDefaultPayload(defaultPayload, requestPayload json.RawMessage) (json.RawMessage, error) {
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
		if key == "instructions" {
			merged, err := mergeInstructions(base["instructions"], val)
			if err != nil {
				return nil, err
			}
			base[key] = merged
			continue
		}
		base[key] = val
	}

	return json.Marshal(base)
}

// mergeInstructions merges two instructions maps at the role level.
// Override roles replace base roles; override null role means deletion.
func mergeInstructions(base, override json.RawMessage) (json.RawMessage, error) {
	var baseMap map[string]json.RawMessage
	if len(base) > 0 && string(base) != "null" {
		if err := json.Unmarshal(base, &baseMap); err != nil {
			return nil, err
		}
	} else {
		baseMap = make(map[string]json.RawMessage)
	}

	var overMap map[string]json.RawMessage
	if err := json.Unmarshal(override, &overMap); err != nil {
		return nil, err
	}

	for role, overInst := range overMap {
		if string(overInst) == "null" {
			delete(baseMap, role)
			continue
		}
		baseMap[role] = overInst
	}

	return json.Marshal(baseMap)
}

// FilterInstructions extracts instructions matching the given type and consumer,
// sorted by role name for deterministic ordering.
func FilterInstructions(payload json.RawMessage, instType InstructionType, consumer string) []RoutedInstruction {
	if instType == "" || consumer == "" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil
	}
	raw, ok := m["instructions"]
	if !ok || string(raw) == "null" {
		return nil
	}
	var instructions map[string]Instruction
	if err := json.Unmarshal(raw, &instructions); err != nil {
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
			Role:     role,
			Type:     inst.Type,
			Consumer: inst.Consumer,
			Message:  inst.Message,
		})
	}
	return result
}
