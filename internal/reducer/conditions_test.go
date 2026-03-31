package reducer_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/reducer"
)

func TestTraitNonNull(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		trait   string
		want    bool
	}{
		{"present", `{"artifact":{"pr_url":"https://..."}}`, "artifact", true},
		{"null", `{"artifact":null}`, "artifact", false},
		{"missing", `{"prompt":"hello"}`, "artifact", false},
		{"empty object", `{}`, "artifact", false},
		{"string value", `{"prompt":"hello"}`, "prompt", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reducer.TraitNonNull(json.RawMessage(tc.payload), tc.trait)
			if got != tc.want {
				t.Errorf("TraitNonNull(%s, %q) = %v, want %v", tc.payload, tc.trait, got, tc.want)
			}
		})
	}
}

func TestAllFindingsResolvedForState(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		state   string
		want    bool
	}{
		{
			"all resolved for matching state",
			`{"verification":{"ci":{"source_state":"verifying","findings":[{"message":"tests pass","status":"resolved"}]},"lint":{"source_state":"verifying","findings":[{"message":"clean","status":"resolved"}]}}}`,
			"verifying",
			true,
		},
		{
			"one unresolved for matching state",
			`{"verification":{"ci":{"source_state":"verifying","findings":[{"message":"tests pass","status":"resolved"}]},"lint":{"source_state":"verifying","findings":[{"message":"unused import","status":"open"}]}}}`,
			"verifying",
			false,
		},
		{
			"resolved but wrong state",
			`{"verification":{"pr-review":{"source_state":"collecting_feedback","findings":[{"message":"looks good","status":"resolved"}]}}}`,
			"verifying",
			false,
		},
		{
			"empty findings for matching state",
			`{"verification":{"ci":{"source_state":"verifying","findings":[]}}}`,
			"verifying",
			true,
		},
		{
			"mixed states only matching resolved",
			`{"verification":{"ci":{"source_state":"verifying","findings":[{"message":"ok","status":"resolved"}]},"pr-review":{"source_state":"collecting_feedback","findings":[{"message":"fix this","status":"open"}]}}}`,
			"verifying",
			true,
		},
		{
			"empty verification",
			`{"verification":{}}`,
			"verifying",
			false,
		},
		{
			"missing verification",
			`{}`,
			"verifying",
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := reducer.AllFindingsResolvedForState(tc.state)
			got := fn(json.RawMessage(tc.payload))
			if got != tc.want {
				t.Errorf("AllFindingsResolvedForState(%q) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

func TestAnyFindingUnresolvedForState(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		state   string
		want    bool
	}{
		{
			"one unresolved for matching state",
			`{"verification":{"ci":{"source_state":"verifying","findings":[{"message":"test failed","status":"open"}]}}}`,
			"verifying",
			true,
		},
		{
			"all resolved for matching state",
			`{"verification":{"ci":{"source_state":"verifying","findings":[{"message":"tests pass","status":"resolved"}]}}}`,
			"verifying",
			false,
		},
		{
			"unresolved but wrong state",
			`{"verification":{"pr-review":{"source_state":"collecting_feedback","findings":[{"message":"fix this","status":"open"}]}}}`,
			"verifying",
			false,
		},
		{
			"no entries for state",
			`{"verification":{"ci":{"source_state":"verifying","findings":[]}}}`,
			"collecting_feedback",
			false,
		},
		{
			"missing verification",
			`{}`,
			"verifying",
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := reducer.AnyFindingUnresolvedForState(tc.state)
			got := fn(json.RawMessage(tc.payload))
			if got != tc.want {
				t.Errorf("AnyFindingUnresolvedForState(%q) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

func TestTasksReady(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"non-empty array", `{"tasks":[{"title":"foo"}]}`, true},
		{"empty array", `{"tasks":[]}`, false},
		{"null", `{"tasks":null}`, false},
		{"missing", `{}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reducer.TasksReady(json.RawMessage(tc.payload))
			if got != tc.want {
				t.Errorf("TasksReady = %v, want %v", got, tc.want)
			}
		})
	}
}
