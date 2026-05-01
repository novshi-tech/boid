package orchestrator

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
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
		if !allowed[TraitType(key)] {
			return fmt.Errorf("trait %q not in produces", key)
		}
	}
	return nil
}

// RejectReservedPayloadKeys returns an error if the payload contains writes to
// the artifact.children.* namespace (which is managed by virtual evaluation only).
func RejectReservedPayloadKeys(payload json.RawMessage) error {
	if len(payload) == 0 || string(payload) == "{}" || string(payload) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil
	}
	artifactRaw, ok := m["artifact"]
	if !ok {
		return nil
	}
	var artifact map[string]json.RawMessage
	if err := json.Unmarshal(artifactRaw, &artifact); err != nil {
		return nil
	}
	if _, ok := artifact["children"]; ok {
		return fmt.Errorf("artifact.children.* is reserved")
	}
	return nil
}

// mergeObjectsShallow は base / patch が両方 JSON object のときに、 patch の
// top-level key を base に被せた結果を返す。 どちらか一方でも object でない
// (scalar / array / null) ときは ok=false を返し、 呼び出し側に whole-value
// overwrite を促す。 ネストは 1 段のみで、 同名 sub-key は patch 側が勝つ。
func mergeObjectsShallow(base, patch json.RawMessage) (json.RawMessage, bool) {
	var baseObj map[string]json.RawMessage
	if err := json.Unmarshal(base, &baseObj); err != nil || baseObj == nil {
		return nil, false
	}
	var patchObj map[string]json.RawMessage
	if err := json.Unmarshal(patch, &patchObj); err != nil || patchObj == nil {
		return nil, false
	}
	maps.Copy(baseObj, patchObj)
	merged, err := json.Marshal(baseObj)
	if err != nil {
		return nil, false
	}
	return merged, true
}

func MergePayloadPatch(base, patch json.RawMessage, handlerID string, allowedTraits []TraitType) (json.RawMessage, error) {
	if len(patch) == 0 || string(patch) == "{}" || string(patch) == "null" {
		if len(base) == 0 {
			return json.RawMessage("{}"), nil
		}
		return base, nil
	}
	if err := RejectReservedPayloadKeys(patch); err != nil {
		return nil, err
	}

	var allowed map[TraitType]bool
	if allowedTraits != nil {
		allowed = make(map[TraitType]bool, len(allowedTraits))
		for _, trait := range allowedTraits {
			allowed[trait] = true
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
		if allowed != nil && !allowed[trait] {
			slog.Warn("dropping payload_patch trait not in produces", "trait", key, "handler_id", handlerID)
			continue
		}
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
			// MergeModeExclusive: base/patch が両方 object のときは sub-key 単位で
			// merge する。 別フェーズや別 handler が同じ trait の異なる sub-key を
			// 書く合法ケースを守るため。 一方が scalar/array なら whole-value 上書き。
			if existing, ok := baseMap[key]; ok && len(existing) > 0 {
				if merged, ok := mergeObjectsShallow(existing, value); ok {
					baseMap[key] = merged
					continue
				}
			}
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
