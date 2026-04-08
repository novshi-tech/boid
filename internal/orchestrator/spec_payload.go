package orchestrator

import (
	"encoding/json"
	"fmt"
)

func TraitMergeMode(trait TraitType) MergeMode {
	switch trait {
	case TraitVerification:
		return MergeModeShared
	default:
		return MergeModeExclusive
	}
}

func ActiveTraitTypes(raw json.RawMessage) ([]TraitType, error) {
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return nil, nil
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}

	var traits []TraitType
	for key, value := range payload {
		if string(value) != "null" {
			traits = append(traits, TraitType(key))
		}
	}
	return traits, nil
}

func ValidatePayloadPatch(patch json.RawMessage, allowedTraits []TraitType) error {
	if len(patch) == 0 || string(patch) == "{}" || string(patch) == "null" {
		return nil
	}

	var patchMap map[string]json.RawMessage
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return fmt.Errorf("unmarshal patch: %w", err)
	}

	allowed := make(map[TraitType]bool, len(allowedTraits))
	for _, trait := range allowedTraits {
		allowed[trait] = true
	}

	for key := range patchMap {
		if TraitType(key) == TraitInstructions {
			return fmt.Errorf("trait %q must not be written by a handler", key)
		}
		if !allowed[TraitType(key)] {
			return fmt.Errorf("trait %q not in produces", key)
		}
	}
	return nil
}

func MergePayloadPatch(base, patch json.RawMessage, handlerID string, allowedTraits []TraitType) (json.RawMessage, error) {
	if len(patch) == 0 || string(patch) == "{}" || string(patch) == "null" {
		if len(base) == 0 {
			return json.RawMessage("{}"), nil
		}
		return base, nil
	}

	if allowedTraits != nil {
		if err := ValidatePayloadPatch(patch, allowedTraits); err != nil {
			return nil, err
		}
	}

	var baseMap map[string]json.RawMessage
	if len(base) == 0 || string(base) == "{}" || string(base) == "null" {
		baseMap = make(map[string]json.RawMessage)
	} else if err := json.Unmarshal(base, &baseMap); err != nil {
		return nil, fmt.Errorf("unmarshal base: %w", err)
	}

	var patchMap map[string]json.RawMessage
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return nil, fmt.Errorf("unmarshal patch: %w", err)
	}

	for key, value := range patchMap {
		trait := TraitType(key)
		switch TraitMergeMode(trait) {
		case MergeModeShared:
			var shared map[string]json.RawMessage
			if existing, ok := baseMap[key]; ok && string(existing) != "null" {
				if err := json.Unmarshal(existing, &shared); err != nil {
					shared = make(map[string]json.RawMessage)
				}
			} else {
				shared = make(map[string]json.RawMessage)
			}
			shared[handlerID] = value
			merged, err := json.Marshal(shared)
			if err != nil {
				return nil, fmt.Errorf("marshal shared trait %q: %w", key, err)
			}
			baseMap[key] = merged
		default:
			baseMap[key] = value
		}
	}

	result, err := json.Marshal(baseMap)
	if err != nil {
		return nil, fmt.Errorf("marshal merged: %w", err)
	}
	return result, nil
}

// FilterPayloadByTraits returns a payload containing only the top-level keys
// matching the listed traits. If consumes is empty, an empty payload is returned.
func FilterPayloadByTraits(payload json.RawMessage, consumes []TraitType) json.RawMessage {
	if len(consumes) == 0 {
		return json.RawMessage("{}")
	}
	if len(payload) == 0 || string(payload) == "{}" || string(payload) == "null" {
		return json.RawMessage("{}")
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return json.RawMessage("{}")
	}

	allowed := make(map[TraitType]bool, len(consumes))
	for _, t := range consumes {
		allowed[t.Base()] = true
	}

	filtered := make(map[string]json.RawMessage, len(consumes))
	for key, val := range m {
		if allowed[TraitType(key)] {
			filtered[key] = val
		}
	}

	result, err := json.Marshal(filtered)
	if err != nil {
		return json.RawMessage("{}")
	}
	return result
}

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

	for key, value := range updateMap {
		if string(value) != "null" {
			baseMap[key] = value
		}
	}

	result, err := json.Marshal(baseMap)
	if err != nil {
		return nil, fmt.Errorf("marshal merged: %w", err)
	}
	return result, nil
}
