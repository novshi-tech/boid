package reducer

import "encoding/json"

// TraitNonNull returns true if the given trait key exists and is not null in the payload.
func TraitNonNull(payload json.RawMessage, trait string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return false
	}
	v, ok := m[trait]
	return ok && string(v) != "null"
}

// AllFindingsResolvedForState returns a condition that is true when all verification
// subkeys matching the given source_state have no open findings.
// Returns false if no subkeys match the given state.
func AllFindingsResolvedForState(sourceState string) TransitionCondition {
	return func(payload json.RawMessage) bool {
		sub := verificationSubkeys(payload)
		found := false
		for _, entry := range sub {
			if entry.SourceState != sourceState {
				continue
			}
			found = true
			for _, f := range entry.Findings {
				if f.Status != "resolved" {
					return false
				}
			}
		}
		return found
	}
}

// AnyFindingUnresolvedForState returns a condition that is true when any verification
// subkey matching the given source_state has a non-resolved finding.
func AnyFindingUnresolvedForState(sourceState string) TransitionCondition {
	return func(payload json.RawMessage) bool {
		for _, entry := range verificationSubkeys(payload) {
			if entry.SourceState != sourceState {
				continue
			}
			for _, f := range entry.Findings {
				if f.Status != "resolved" {
					return true
				}
			}
		}
		return false
	}
}

// TasksReady returns true if the "tasks" trait is a non-empty array.
func TasksReady(payload json.RawMessage) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return false
	}
	raw, ok := m["tasks"]
	if !ok || string(raw) == "null" {
		return false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return false
	}
	return len(arr) > 0
}

// Finding represents a single verification finding with a lifecycle status.
type Finding struct {
	Message string `json:"message"`
	Status  string `json:"status"` // "open" or "resolved"
}

type verificationEntry struct {
	SourceState string    `json:"source_state"`
	Findings    []Finding `json:"findings"`
}

func verificationSubkeys(payload json.RawMessage) map[string]verificationEntry {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil
	}
	raw, ok := m["verification"]
	if !ok || string(raw) == "null" {
		return nil
	}
	var sub map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sub); err != nil {
		return nil
	}
	result := make(map[string]verificationEntry, len(sub))
	for k, v := range sub {
		var entry verificationEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			continue
		}
		result[k] = entry
	}
	return result
}
