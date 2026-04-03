package orchestrator

import "encoding/json"

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
