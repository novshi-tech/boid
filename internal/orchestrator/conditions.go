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
