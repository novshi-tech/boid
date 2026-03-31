package model

import (
	"encoding/json"
	"fmt"
)

// MergeMode defines how a trait's payload_patch is merged.
type MergeMode string

const (
	// MergeModeExclusive replaces the trait value entirely.
	MergeModeExclusive MergeMode = "exclusive"
	// MergeModeShared namespaces values by hook/gate ID under the trait key.
	MergeModeShared MergeMode = "shared"
)

// TraitMergeMode returns the merge mode for a given trait type.
func TraitMergeMode(t TraitType) MergeMode {
	switch t {
	case TraitVerification:
		return MergeModeShared
	default:
		return MergeModeExclusive
	}
}

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

// ValidatePayloadPatch checks that all keys in patch are in the allowed traits list.
func ValidatePayloadPatch(patch json.RawMessage, allowedTraits []TraitType) error {
	if len(patch) == 0 || string(patch) == "{}" || string(patch) == "null" {
		return nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(patch, &m); err != nil {
		return fmt.Errorf("unmarshal patch: %w", err)
	}

	allowed := make(map[TraitType]bool, len(allowedTraits))
	for _, t := range allowedTraits {
		allowed[t] = true
	}

	for k := range m {
		if !allowed[TraitType(k)] {
			return fmt.Errorf("trait %q not in requires_traits", k)
		}
	}
	return nil
}

// MergePayloadPatch merges a payload patch into the base payload, respecting
// trait-based write restrictions and merge modes.
// For exclusive traits, the value replaces base[trait].
// For shared traits, the value is placed at base[trait][hookID].
// If allowedTraits is nil, validation is skipped (all traits allowed).
func MergePayloadPatch(base, patch json.RawMessage, hookID string, allowedTraits []TraitType) (json.RawMessage, error) {
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
	} else {
		if err := json.Unmarshal(base, &baseMap); err != nil {
			return nil, fmt.Errorf("unmarshal base: %w", err)
		}
	}

	var patchMap map[string]json.RawMessage
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return nil, fmt.Errorf("unmarshal patch: %w", err)
	}

	for k, v := range patchMap {
		trait := TraitType(k)
		switch TraitMergeMode(trait) {
		case MergeModeShared:
			// Namespace by hookID: base[trait][hookID] = value
			var sub map[string]json.RawMessage
			if existing, ok := baseMap[k]; ok && string(existing) != "null" {
				if err := json.Unmarshal(existing, &sub); err != nil {
					sub = make(map[string]json.RawMessage)
				}
			} else {
				sub = make(map[string]json.RawMessage)
			}
			sub[hookID] = v
			merged, err := json.Marshal(sub)
			if err != nil {
				return nil, fmt.Errorf("marshal shared trait %q: %w", k, err)
			}
			baseMap[k] = merged
		default:
			// Exclusive: replace entirely
			baseMap[k] = v
		}
	}

	result, err := json.Marshal(baseMap)
	if err != nil {
		return nil, fmt.Errorf("marshal merged: %w", err)
	}
	return result, nil
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
