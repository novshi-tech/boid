package orchestrator

import (
	"encoding/json"
	"testing"
)

// ---- source_closed.go (survivor of 決定15/16's removal, PR-1) ----

func TestSourceClosed(t *testing.T) {
	cases := []struct {
		name   string
		detail string
		want   bool
	}{
		{"absent detail", ``, false},
		{"no attrs", `{}`, false},
		{"no observed", `{"attrs":{"summary":"s"}}`, false},
		{"observed without source_closed", `{"attrs":{"observed":{"at":"2026-08-13"}}}`, false},
		{"explicitly open", `{"attrs":{"observed":{"source_closed":false}}}`, false},
		{"closed", `{"attrs":{"observed":{"source_closed":true}}}`, true},
		{"malformed detail", `not json`, false},
	}
	for _, tc := range cases {
		if got := SourceClosed(json.RawMessage(tc.detail)); got != tc.want {
			t.Errorf("%s: SourceClosed(%s) = %v, want %v", tc.name, tc.detail, got, tc.want)
		}
	}
}
