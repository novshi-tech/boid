package model_test

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/novshi-tech/boid/internal/model"
)

func TestActiveTraitTypes_Empty(t *testing.T) {
	cases := []json.RawMessage{
		nil,
		json.RawMessage("{}"),
		json.RawMessage("null"),
	}
	for _, raw := range cases {
		traits, err := model.ActiveTraitTypes(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(traits) != 0 {
			t.Fatalf("expected no traits, got %v", traits)
		}
	}
}

func TestActiveTraitTypes_WithTraits(t *testing.T) {
	raw := json.RawMessage(`{"agent_prompt":"hello","pr":"http://example.com"}`)
	traits, err := model.ActiveTraitTypes(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(traits) != 2 {
		t.Fatalf("expected 2 traits, got %d", len(traits))
	}
	names := make([]string, len(traits))
	for i, tr := range traits {
		names[i] = string(tr)
	}
	sort.Strings(names)
	if names[0] != "agent_prompt" || names[1] != "pr" {
		t.Fatalf("unexpected traits: %v", names)
	}
}

func TestActiveTraitTypes_WithNullValues(t *testing.T) {
	raw := json.RawMessage(`{"agent_prompt":"hello","pr":null,"pipeline":"data"}`)
	traits, err := model.ActiveTraitTypes(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(traits) != 2 {
		t.Fatalf("expected 2 traits (null excluded), got %d: %v", len(traits), traits)
	}
	names := make([]string, len(traits))
	for i, tr := range traits {
		names[i] = string(tr)
	}
	sort.Strings(names)
	if names[0] != "agent_prompt" || names[1] != "pipeline" {
		t.Fatalf("unexpected traits: %v", names)
	}
}

func TestMergePayload_BaseOnly(t *testing.T) {
	base := json.RawMessage(`{"a":"1"}`)
	result, err := model.MergePayload(base, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result) != `{"a":"1"}` {
		t.Fatalf("expected base returned, got %s", string(result))
	}
}

func TestMergePayload_UpdateOnly(t *testing.T) {
	update := json.RawMessage(`{"b":"2"}`)
	result, err := model.MergePayload(nil, update)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result) != `{"b":"2"}` {
		t.Fatalf("expected update returned, got %s", string(result))
	}
}

func TestMergePayload_BothEmpty(t *testing.T) {
	result, err := model.MergePayload(nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result) != "{}" {
		t.Fatalf("expected empty object, got %s", string(result))
	}
}

func TestMergePayload_Merge(t *testing.T) {
	base := json.RawMessage(`{"a":"1","b":"2"}`)
	update := json.RawMessage(`{"b":"3","c":"4"}`)
	result, err := model.MergePayload(base, update)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]string
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if m["a"] != "1" {
		t.Fatalf("expected a=1, got %s", m["a"])
	}
	if m["b"] != "3" {
		t.Fatalf("expected b=3 (overwritten), got %s", m["b"])
	}
	if m["c"] != "4" {
		t.Fatalf("expected c=4, got %s", m["c"])
	}
}

func TestMergePayload_NullHandling(t *testing.T) {
	base := json.RawMessage(`{"a":"1","b":"2"}`)
	update := json.RawMessage(`{"b":null,"c":"3"}`)
	result, err := model.MergePayload(base, update)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	// "b" should remain as "2" because null values in update are ignored
	if string(m["b"]) != `"2"` {
		t.Fatalf("expected b to remain \"2\", got %s", string(m["b"]))
	}
	if string(m["c"]) != `"3"` {
		t.Fatalf("expected c=\"3\", got %s", string(m["c"]))
	}
}
