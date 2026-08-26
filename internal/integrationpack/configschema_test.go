package integrationpack

import "testing"

// TestConfigSchema_Validate pins the v0 minimal JSON-Schema subset (docs/
// plans/signal-ingest-detailed-design.md §6.2 item 4: "type/object/
// properties/required、値型は string/number/boolean のみ") — the validator a
// connector's declared config gets checked against (Q19).
func TestConfigSchema_Validate(t *testing.T) {
	schema := ConfigSchema{
		Type: "object",
		Properties: map[string]PropertySchema{
			"jql":          {Type: "string"},
			"max_results":  {Type: "number"},
			"include_done": {Type: "boolean"},
		},
		Required: []string{"jql"},
	}

	t.Run("valid value passes", func(t *testing.T) {
		value := map[string]any{
			"jql":          "assignee = currentUser()",
			"max_results":  float64(50),
			"include_done": true,
		}
		if err := schema.Validate(value); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("missing required field rejected", func(t *testing.T) {
		value := map[string]any{"max_results": float64(50)}
		if err := schema.Validate(value); err == nil {
			t.Error("want error for missing required field, got nil")
		}
	})

	t.Run("wrong type for declared property rejected", func(t *testing.T) {
		value := map[string]any{"jql": 123}
		if err := schema.Validate(value); err == nil {
			t.Error("want error for jql not being a string, got nil")
		}
	})

	t.Run("number property accepts int and float64", func(t *testing.T) {
		for _, v := range []any{int(5), int64(5), float64(5)} {
			value := map[string]any{"jql": "x", "max_results": v}
			if err := schema.Validate(value); err != nil {
				t.Errorf("max_results=%T(%v): unexpected error: %v", v, v, err)
			}
		}
	})

	t.Run("boolean property rejects non-bool", func(t *testing.T) {
		value := map[string]any{"jql": "x", "include_done": "yes"}
		if err := schema.Validate(value); err == nil {
			t.Error("want error for include_done not being a boolean, got nil")
		}
	})

	t.Run("undeclared property is ignored (v0 does not enforce additionalProperties)", func(t *testing.T) {
		value := map[string]any{"jql": "x", "extra_undeclared_field": "whatever"}
		if err := schema.Validate(value); err != nil {
			t.Errorf("unexpected error for an undeclared property: %v", err)
		}
	})

	t.Run("empty schema (no properties/required) accepts anything", func(t *testing.T) {
		empty := ConfigSchema{Type: "object"}
		if err := empty.Validate(map[string]any{"anything": "goes"}); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if err := empty.Validate(nil); err != nil {
			t.Errorf("unexpected error for nil value: %v", err)
		}
	})
}
