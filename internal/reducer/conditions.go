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

// AllSubkeysPassed returns true if all sub-keys under "verification" have passed=true.
// Returns false if verification is missing, empty, or any sub-key has passed=false.
func AllSubkeysPassed(payload json.RawMessage) bool {
	sub := verificationSubkeys(payload)
	if len(sub) == 0 {
		return false
	}
	for _, entry := range sub {
		if !entry.Passed {
			return false
		}
	}
	return true
}

// AnySubkeyFailed returns true if any sub-key under "verification" has passed=false.
func AnySubkeyFailed(payload json.RawMessage) bool {
	for _, entry := range verificationSubkeys(payload) {
		if !entry.Passed {
			return true
		}
	}
	return false
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

type verificationEntry struct {
	Passed bool `json:"passed"`
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
