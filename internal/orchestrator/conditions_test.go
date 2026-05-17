package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestTraitBool_TopLevelAndNested(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		trait   string
		want    bool
	}{
		{"top-level true", `{"flag":true}`, "flag", true},
		{"top-level false", `{"flag":false}`, "flag", false},
		{"missing key", `{}`, "flag", false},
		{"nested true", `{"lifecycle":{"executed":true}}`, "lifecycle.executed", true},
		{"nested false", `{"lifecycle":{"executed":false}}`, "lifecycle.executed", false},
		{"nested missing branch", `{"lifecycle":{}}`, "lifecycle.executed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := orchestrator.TraitBool(json.RawMessage(tc.payload), tc.trait)
			if got != tc.want {
				t.Errorf("TraitBool(%s, %s) = %v, want %v", tc.payload, tc.trait, got, tc.want)
			}
		})
	}
}

func TestTraitExists_PresenceNotValue(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		trait   string
		want    bool
	}{
		{"object present", `{"done":{"message":"ok"}}`, "done", true},
		{"object empty present", `{"done":{}}`, "done", true},
		{"string present", `{"done":""}`, "done", true},
		{"value null absent", `{"done":null}`, "done", false},
		{"key missing", `{}`, "done", false},
		{"nested present", `{"lifecycle":{"done":{"message":"x"}}}`, "lifecycle.done", true},
		{"nested missing branch", `{"lifecycle":{}}`, "lifecycle.done", false},
		{"nested null", `{"lifecycle":{"done":null}}`, "lifecycle.done", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := orchestrator.TraitExists(json.RawMessage(tc.payload), tc.trait)
			if got != tc.want {
				t.Errorf("TraitExists(%s, %s) = %v, want %v", tc.payload, tc.trait, got, tc.want)
			}
		})
	}
}

func TestTraitGetString(t *testing.T) {
	cases := []struct {
		name      string
		payload   string
		trait     string
		wantStr   string
		wantOK    bool
	}{
		{"top-level string", `{"msg":"hello"}`, "msg", "hello", true},
		{"nested string", `{"lifecycle":{"done":{"message":"merged"}}}`, "lifecycle.done.message", "merged", true},
		{"missing key", `{}`, "msg", "", false},
		{"wrong type", `{"msg":42}`, "msg", "", false},
		{"nested missing", `{"lifecycle":{}}`, "lifecycle.done.message", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := orchestrator.TraitGetString(json.RawMessage(tc.payload), tc.trait)
			if ok != tc.wantOK {
				t.Fatalf("TraitGetString(%s, %s) ok=%v, want %v", tc.payload, tc.trait, ok, tc.wantOK)
			}
			if got != tc.wantStr {
				t.Errorf("TraitGetString(%s, %s) = %q, want %q", tc.payload, tc.trait, got, tc.wantStr)
			}
		})
	}
}
