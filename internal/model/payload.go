package model

import (
	"encoding/json"
	"fmt"
)

// ActiveTraitTypes returns the TraitType keys from a JSON payload where
// the value is not null.
func ActiveTraitTypes(raw json.RawMessage) ([]TraitType, error) {
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return nil, nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}

	var traits []TraitType
	for k, v := range m {
		if string(v) != "null" {
			traits = append(traits, TraitType(k))
		}
	}
	return traits, nil
}

// MergePayload merges update into base. Non-null fields in update overwrite base.
func MergePayload(base, update json.RawMessage) (json.RawMessage, error) {
	if len(update) == 0 || string(update) == "{}" || string(update) == "null" {
		if len(base) == 0 {
			return json.RawMessage("{}"), nil
		}
		return base, nil
	}
	if len(base) == 0 || string(base) == "{}" || string(base) == "null" {
		return update, nil
	}

	var baseMap map[string]json.RawMessage
	if err := json.Unmarshal(base, &baseMap); err != nil {
		return nil, fmt.Errorf("unmarshal base: %w", err)
	}

	var updateMap map[string]json.RawMessage
	if err := json.Unmarshal(update, &updateMap); err != nil {
		return nil, fmt.Errorf("unmarshal update: %w", err)
	}

	for k, v := range updateMap {
		if string(v) != "null" {
			baseMap[k] = v
		}
	}

	result, err := json.Marshal(baseMap)
	if err != nil {
		return nil, fmt.Errorf("marshal merged: %w", err)
	}
	return result, nil
}
