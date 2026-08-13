package orchestrator

import (
	"encoding/json"
	"testing"
)

// ---- Phase 1 PR-5b (docs/plans/cross-project-issue-triage.md 決定15/16) ----
//
// The done rule is the whole point of 決定15: "終わった" must be decided by the
// machine, because a done judgment that bounces back to a human costs exactly
// the queue trust the system is built to earn. These pin the two halves of the
// predicate and the canonical-source contract that makes it satisfiable at all
// (決定16).

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

// TestHasCanonicalSource pins 決定16's daemon-side test: "does this card carry
// a source that can report closure at all". The daemon deliberately learns
// this from the PRESENCE of the key rather than from the source type — which
// source types have a terminal state is channel knowledge that stays on the
// workspace side (逆輸入3).
func TestHasCanonicalSource(t *testing.T) {
	cases := []struct {
		detail string
		want   bool
	}{
		{``, false},
		{`{"attrs":{"observed":{}}}`, false},
		{`{"attrs":{"observed":{"source_closed":false}}}`, true},
		{`{"attrs":{"observed":{"source_closed":true}}}`, true},
	}
	for _, tc := range cases {
		if got := HasCanonicalSource(json.RawMessage(tc.detail)); got != tc.want {
			t.Errorf("HasCanonicalSource(%s) = %v, want %v", tc.detail, got, tc.want)
		}
	}
}

func TestShouldAutoDone(t *testing.T) {
	closed := []TaskTriageChild{{ID: "a", Status: TaskTriageChildStatusClosed}}
	mixed := []TaskTriageChild{
		{ID: "a", Status: TaskTriageChildStatusClosed},
		{ID: "b", Status: TaskTriageChildStatusDispatched},
	}
	open := []TaskTriageChild{{ID: "a", Status: TaskTriageChildStatusOpen}}

	cases := []struct {
		name     string
		children []TaskTriageChild
		detail   string
		want     bool
	}{
		{
			// Both halves satisfied — the ordinary end of UC-1.
			name: "all children closed and source closed", children: closed,
			detail: `{"attrs":{"observed":{"source_closed":true}}}`, want: true,
		},
		{
			// 逆輸入2: working covers "nose が手動対応中で子は open のみ" too, so a
			// card that never spawned a task is still done once its source is.
			name: "no children at all, source closed", children: nil,
			detail: `{"attrs":{"observed":{"source_closed":true}}}`, want: true,
		},
		{
			name: "source closed but a child is still dispatched", children: mixed,
			detail: `{"attrs":{"observed":{"source_closed":true}}}`, want: false,
		},
		{
			// An open child is a 残課題 that has not even been specced yet —
			// exactly the "残課題つき完了を正常系とする" loop (逆輸入1), which
			// must go back through triaged, not fall out as done.
			name: "source closed but a child is still open", children: open,
			detail: `{"attrs":{"observed":{"source_closed":true}}}`, want: false,
		},
		{
			name: "children closed but source still open", children: closed,
			detail: `{"attrs":{"observed":{"source_closed":false}}}`, want: false,
		},
		{
			// 決定16 breach: no canonical source at all. Never falls to done —
			// the watchdog surfaces it instead of the machine silently
			// inventing a completion.
			name: "children closed but no canonical source", children: closed,
			detail: `{"attrs":{"summary":"s"}}`, want: false,
		},
	}
	for _, tc := range cases {
		if got := ShouldAutoDone(tc.children, json.RawMessage(tc.detail)); got != tc.want {
			t.Errorf("%s: ShouldAutoDone = %v, want %v", tc.name, got, tc.want)
		}
	}
}
