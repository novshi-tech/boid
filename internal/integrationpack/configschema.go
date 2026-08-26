package integrationpack

import "fmt"

// ConfigSchema is a connector's declared configSchema — the v0 minimal
// JSON-Schema subset docs/plans/signal-ingest-detailed-design.md §6.2 item
// 4 specifies: "type/object/properties/required、値型は string/number/
// boolean のみ". Hand-rolled rather than pulling in a real JSON Schema
// library — CLAUDE.md's "外部ライブラリは最小限" rule, and the subset this
// package needs is narrow enough that a full implementation (refs, oneOf/
// anyOf, format, ...) would be pure unused surface. Constructed by
// parseConfigSchema (manifest.go), which already validates the schema
// document ITSELF (root type, property types, required references) — this
// type's own Validate method checks a candidate VALUE against an
// already-valid schema.
type ConfigSchema struct {
	// Type is always "object" for a schema parseConfigSchema produced (v0
	// supports no other root type) — kept as a field (rather than assumed)
	// so a hand-built ConfigSchema in a test can still be introspected.
	Type string
	// Properties maps each declared property name to its (string/number/
	// boolean) leaf type. A property not listed here is NOT rejected by
	// Validate — v0 does not implement JSON Schema's additionalProperties:
	// false; see Validate's own doc comment.
	Properties map[string]PropertySchema
	// Required lists property names that must be present in a validated
	// value. Every entry is guaranteed (by parseConfigSchema) to also
	// appear in Properties.
	Required []string
}

// PropertySchema is one ConfigSchema.Properties entry's leaf type.
type PropertySchema struct {
	// Type is one of "string", "number", "boolean" (v0's supported leaf
	// types — signal-ingest-detailed-design.md §6.2 item 4).
	Type string
}

// Validate checks value — a connector config already decoded from JSON/YAML
// into Go's generic any shapes (map[string]any, with string/bool/float64/
// int/int64 leaves) — against s:
//
//   - every name in s.Required must be present in value
//   - for every key in value that ALSO appears in s.Properties, the
//     value's Go type must match the declared leaf type (string→string,
//     boolean→bool, number→any of int/int32/int64/float32/float64 — both
//     yaml.v3's and encoding/json's own decoded numeric shapes are
//     accepted, since callers may come from either)
//   - a key in value with NO corresponding s.Properties entry is NOT an
//     error — v0 does not implement JSON Schema's additionalProperties:
//     false (deliberately out of scope; see this type's own doc comment)
//
// A nil value is treated as an empty object — Validate then only fails if
// s.Required is non-empty.
func (s ConfigSchema) Validate(value map[string]any) error {
	for _, req := range s.Required {
		if _, ok := value[req]; !ok {
			return fmt.Errorf("integrationpack: config validation: missing required field %q", req)
		}
	}
	for key, v := range value {
		prop, ok := s.Properties[key]
		if !ok {
			continue
		}
		if err := prop.validateValue(key, v); err != nil {
			return err
		}
	}
	return nil
}

func (p PropertySchema) validateValue(key string, v any) error {
	switch p.Type {
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("integrationpack: config validation: field %q must be a string, got %T", key, v)
		}
	case "number":
		switch v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		default:
			return fmt.Errorf("integrationpack: config validation: field %q must be a number, got %T", key, v)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("integrationpack: config validation: field %q must be a boolean, got %T", key, v)
		}
	default:
		// Unreachable on a schema parseConfigSchema produced (it already
		// rejects any other property type) — kept as a defensive error
		// rather than a panic for a hand-built ConfigSchema (e.g. a test)
		// that skipped that validation.
		return fmt.Errorf("integrationpack: config validation: field %q: unsupported schema type %q (v0 supports string/number/boolean only)", key, p.Type)
	}
	return nil
}
