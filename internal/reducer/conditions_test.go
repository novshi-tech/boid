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

func TestAllSubkeysPassed(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{
			"all passed",
			`{"verification":{"ci":{"passed":true,"findings":[]},"review":{"passed":true,"findings":[]}}}`,
			true,
		},
		{
			"one failed",
			`{"verification":{"ci":{"passed":true,"findings":[]},"review":{"passed":false,"findings":["bug"]}}}`,
			false,
		},
		{
			"empty verification",
			`{"verification":{}}`,
			false, // no sub-keys → not all passed
		},
		{
			"missing verification",
			`{}`,
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reducer.AllSubkeysPassed(json.RawMessage(tc.payload))
			if got != tc.want {
				t.Errorf("AllSubkeysPassed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAnySubkeyFailed(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{
			"one failed",
			`{"verification":{"ci":{"passed":false,"findings":["error"]},"review":{"passed":true,"findings":[]}}}`,
			true,
		},
		{
			"all passed",
			`{"verification":{"ci":{"passed":true,"findings":[]},"review":{"passed":true,"findings":[]}}}`,
			false,
		},
		{
			"missing verification",
			`{}`,
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reducer.AnySubkeyFailed(json.RawMessage(tc.payload))
			if got != tc.want {
				t.Errorf("AnySubkeyFailed = %v, want %v", got, tc.want)
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
