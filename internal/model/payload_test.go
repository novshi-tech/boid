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
	raw := json.RawMessage(`{"prompt":"hello","artifact":"http://example.com"}`)
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
	if names[0] != "artifact" || names[1] != "prompt" {
		t.Fatalf("unexpected traits: %v", names)
	}
}

func TestActiveTraitTypes_WithNullValues(t *testing.T) {
	raw := json.RawMessage(`{"prompt":"hello","artifact":null,"tasks":"data"}`)
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
	if names[0] != "prompt" || names[1] != "tasks" {
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

func TestTraitMergeMode(t *testing.T) {
	cases := []struct {
		trait model.TraitType
		want  model.MergeMode
	}{
		{model.TraitPrompt, model.MergeModeExclusive},
		{model.TraitArtifact, model.MergeModeExclusive},
		{model.TraitArtifact, model.MergeModeExclusive},
		{model.TraitTasks, model.MergeModeExclusive},
		{model.TraitVerification, model.MergeModeShared},
	}

	for _, tc := range cases {
		got := model.TraitMergeMode(tc.trait)
		if got != tc.want {
			t.Errorf("TraitMergeMode(%q) = %q, want %q", tc.trait, got, tc.want)
		}
	}
}

func TestValidatePayloadPatch_AllowedTraits(t *testing.T) {
	patch := json.RawMessage(`{"prompt":"hello","artifact":"http://example.com"}`)
	allowed := []model.TraitType{model.TraitPrompt, model.TraitArtifact}

	if err := model.ValidatePayloadPatch(patch, allowed); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidatePayloadPatch_UnauthorizedTrait(t *testing.T) {
	patch := json.RawMessage(`{"prompt":"hello","artifact":"ci"}`)
	allowed := []model.TraitType{model.TraitPrompt}

	err := model.ValidatePayloadPatch(patch, allowed)
	if err == nil {
		t.Fatal("expected error for unauthorized trait")
	}
}

func TestValidatePayloadPatch_EmptyPatch(t *testing.T) {
	for _, patch := range []json.RawMessage{nil, json.RawMessage("{}"), json.RawMessage("null")} {
		if err := model.ValidatePayloadPatch(patch, nil); err != nil {
			t.Fatalf("unexpected error for empty patch: %v", err)
		}
	}
}

func TestMergePayloadPatch_Exclusive(t *testing.T) {
	base := json.RawMessage(`{"prompt":"old"}`)
	patch := json.RawMessage(`{"prompt":"new"}`)
	allowed := []model.TraitType{model.TraitPrompt}

	result, err := model.MergePayloadPatch(base, patch, "hook-1", allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]json.RawMessage
	json.Unmarshal(result, &m)

	if string(m["prompt"]) != `"new"` {
		t.Fatalf("expected prompt=new, got %s", string(m["prompt"]))
	}
}

func TestMergePayloadPatch_Shared(t *testing.T) {
	base := json.RawMessage(`{}`)
	allowed := []model.TraitType{model.TraitVerification}

	// First hook writes
	patch1 := json.RawMessage(`{"verification":{"source_state":"verifying","findings":[{"message":"secure","status":"resolved"}]}}`)
	result, err := model.MergePayloadPatch(base, patch1, "security-review", allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Second hook writes
	patch2 := json.RawMessage(`{"verification":{"source_state":"verifying","findings":[{"message":"bug found","status":"open"}]}}`)
	result, err = model.MergePayloadPatch(result, patch2, "quality-review", allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify namespaced merge
	var m map[string]json.RawMessage
	json.Unmarshal(result, &m)

	var review map[string]json.RawMessage
	if err := json.Unmarshal(m["verification"], &review); err != nil {
		t.Fatalf("unmarshal review: %v", err)
	}

	if _, ok := review["security-review"]; !ok {
		t.Fatal("expected security-review sub-key")
	}
	if _, ok := review["quality-review"]; !ok {
		t.Fatal("expected quality-review sub-key")
	}
}

func TestMergePayloadPatch_UnauthorizedTraitRejected(t *testing.T) {
	base := json.RawMessage(`{}`)
	patch := json.RawMessage(`{"artifact":"ci"}`)
	allowed := []model.TraitType{model.TraitPrompt}

	_, err := model.MergePayloadPatch(base, patch, "hook-1", allowed)
	if err == nil {
		t.Fatal("expected error for unauthorized trait in patch")
	}
}

func TestMergePayloadPatch_EmptyPatch(t *testing.T) {
	base := json.RawMessage(`{"prompt":"hello"}`)

	result, err := model.MergePayloadPatch(base, nil, "hook-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result) != string(base) {
		t.Fatalf("expected base unchanged, got %s", string(result))
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
