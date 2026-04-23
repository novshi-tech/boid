package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

func jsonToYAML(data json.RawMessage) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}
	out, err := yaml.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("yaml marshal: %w", err)
	}
	return string(out), nil
}

func yamlToJSON(yamlStr string) (json.RawMessage, error) {
	var v any
	if err := yaml.Unmarshal([]byte(yamlStr), &v); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("json marshal: %w", err)
	}
	return out, nil
}

func mergeSectionIntoPayload(payload json.RawMessage, section string, sectionJSON json.RawMessage) (json.RawMessage, error) {
	raw := make(map[string]json.RawMessage)
	if len(payload) > 0 && strings.TrimSpace(string(payload)) != "null" {
		if err := json.Unmarshal(payload, &raw); err != nil {
			raw = make(map[string]json.RawMessage)
		}
	}
	raw[section] = sectionJSON
	return json.Marshal(raw)
}
