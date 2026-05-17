package orchestrator

import (
	"encoding/json"
	"strings"
)

// TraitBool returns true if the given trait key exists and its value is the
// JSON literal true. The key may be a dot-separated path (e.g.
// "lifecycle.executed") for nested object access.
func TraitBool(payload json.RawMessage, trait string) bool {
	head, tail, nested := strings.Cut(trait, ".")
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return false
	}
	v, ok := m[head]
	if !ok {
		return false
	}
	if !nested {
		return string(v) == "true"
	}
	return TraitBool(v, tail)
}

// TraitExists reports whether the dotted-path key resolves to a present,
// non-null JSON value in payload. Unlike TraitBool it does not require the
// value to be the JSON literal true; any concrete value (object, array,
// string, number, true) counts as "exists". Used by state-machine condition
// rules that gate on the presence of a sibling trait (e.g.
// `lifecycle.done` set by `done_request`) regardless of its body shape.
func TraitExists(payload json.RawMessage, trait string) bool {
	head, tail, nested := strings.Cut(trait, ".")
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return false
	}
	v, ok := m[head]
	if !ok {
		return false
	}
	if string(v) == "null" {
		return false
	}
	if !nested {
		return true
	}
	return TraitExists(v, tail)
}

// TraitGetString reads the string value at a dotted path. Returns ("", false)
// if the path is missing or the value is not a JSON string. Used by auto-rule
// ActionPayloadFns to extract user-facing message text from lifecycle traits.
func TraitGetString(payload json.RawMessage, trait string) (string, bool) {
	head, tail, nested := strings.Cut(trait, ".")
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return "", false
	}
	v, ok := m[head]
	if !ok {
		return "", false
	}
	if !nested {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", false
		}
		return s, true
	}
	return TraitGetString(v, tail)
}
