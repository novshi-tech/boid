package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestDetailSuggestion_TopLevel covers docs/plans/cross-project-issue-triage.md:544
// のスキーマ案 — the shape khi's evaluate step is expected to write.
func TestDetailSuggestion_TopLevel(t *testing.T) {
	detail := json.RawMessage(`{"suggestion":{"verb":"wake","action":"re-triage now","reason":"source event fired","basis":"issue #42 reopened"}}`)

	got, ok := orchestrator.DetailSuggestion(detail)
	if !ok {
		t.Fatal("DetailSuggestion: ok = false, want true")
	}
	want := orchestrator.Suggestion{Verb: "wake", Action: "re-triage now", Reason: "source event fired", Basis: "issue #42 reopened"}
	if got != want {
		t.Errorf("DetailSuggestion = %+v, want %+v", got, want)
	}
}

// TestDetailSuggestion_FromAttrs covers the attrs_set fold path: "suggestion"
// is not a promoted key (applyAttrsSetSideEffect, internal/api/
// workflow_triage.go), so when khi writes it via attrs_set it lands under
// detail.attrs instead of at the top level (orchestrator.FoldDetailAttrs).
func TestDetailSuggestion_FromAttrs(t *testing.T) {
	detail := json.RawMessage(`{"attrs":{"suggestion":{"verb":"go","action":"dispatch","reason":"fully specced"}}}`)

	got, ok := orchestrator.DetailSuggestion(detail)
	if !ok {
		t.Fatal("DetailSuggestion: ok = false, want true")
	}
	want := orchestrator.Suggestion{Verb: "go", Action: "dispatch", Reason: "fully specced"}
	if got != want {
		t.Errorf("DetailSuggestion = %+v, want %+v", got, want)
	}
}

// TestDetailSuggestion_TopLevelWinsOverAttrs pins the documented priority
// order: top-level detail.suggestion is read first; detail.attrs.suggestion
// is only a fallback.
func TestDetailSuggestion_TopLevelWinsOverAttrs(t *testing.T) {
	detail := json.RawMessage(`{
		"suggestion": {"verb": "go"},
		"attrs": {"suggestion": {"verb": "drop"}}
	}`)

	got, ok := orchestrator.DetailSuggestion(detail)
	if !ok {
		t.Fatal("DetailSuggestion: ok = false, want true")
	}
	if got.Verb != "go" {
		t.Errorf("DetailSuggestion.Verb = %q, want %q (top-level must win)", got.Verb, "go")
	}
}

// TestDetailSuggestion_EmptyOrAbsent covers "no suggestion yet" — the
// overwhelming majority of triage rows — mirroring DetailChildren's own
// empty/absent test.
func TestDetailSuggestion_EmptyOrAbsent(t *testing.T) {
	for _, detail := range []json.RawMessage{nil, []byte(""), []byte("null"), []byte("{}"), []byte(`{"summary":"x"}`)} {
		got, ok := orchestrator.DetailSuggestion(detail)
		if ok {
			t.Errorf("DetailSuggestion(%q): ok = true, want false", detail)
		}
		if got != (orchestrator.Suggestion{}) {
			t.Errorf("DetailSuggestion(%q) = %+v, want zero value", detail, got)
		}
	}
}

// TestDetailSuggestion_MalformedJSON_ReturnsFalseNotError pins the
// best-effort contract: DetailSuggestion never errors, so a malformed blob
// can never sink whatever row is reading it (rule 5, 隠さない).
func TestDetailSuggestion_MalformedJSON_ReturnsFalseNotError(t *testing.T) {
	for _, detail := range []json.RawMessage{
		[]byte(`not json`),
		[]byte(`{"suggestion": "not an object"}`),
	} {
		got, ok := orchestrator.DetailSuggestion(detail)
		if ok {
			t.Errorf("DetailSuggestion(%q): ok = true, want false", detail)
		}
		if got != (orchestrator.Suggestion{}) {
			t.Errorf("DetailSuggestion(%q) = %+v, want zero value", detail, got)
		}
	}
}
