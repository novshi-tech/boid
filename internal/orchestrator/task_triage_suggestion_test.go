package orchestrator_test

import (
	"encoding/json"
	"strings"
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
// workflow_card.go), so when khi writes it via attrs_set it lands under
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

// TestDetailSuggestion_MalformedAttrsDoesNotBlankValidTopLevel is a
// regression test for an Opus review finding (2026-08-18): an earlier
// version decoded both the top-level "suggestion" key and
// "attrs.suggestion" off a single json.Unmarshal call into one struct. A
// type mismatch on the attrs copy (e.g. khi writing a bare string instead
// of an object via attrs_set — plausible from an LLM) made that single
// Unmarshal call fail, which wiped out an otherwise well-formed top-level
// suggestion too. Each candidate must be decoded independently.
func TestDetailSuggestion_MalformedAttrsDoesNotBlankValidTopLevel(t *testing.T) {
	detail := json.RawMessage(`{"suggestion":{"verb":"go"},"attrs":{"suggestion":"just a string"}}`)

	got, ok := orchestrator.DetailSuggestion(detail)
	if !ok {
		t.Fatal("DetailSuggestion: ok = false, want true (top-level suggestion is well-formed)")
	}
	if got.Verb != "go" {
		t.Errorf("DetailSuggestion.Verb = %q, want %q", got.Verb, "go")
	}
}

// TestDetailSuggestion_NonObjectAttrsDoesNotBlankValidTopLevel covers the
// sibling case: "attrs" itself is not even an object.
func TestDetailSuggestion_NonObjectAttrsDoesNotBlankValidTopLevel(t *testing.T) {
	detail := json.RawMessage(`{"suggestion":{"verb":"go"},"attrs":"oops"}`)

	got, ok := orchestrator.DetailSuggestion(detail)
	if !ok {
		t.Fatal("DetailSuggestion: ok = false, want true (top-level suggestion is well-formed)")
	}
	if got.Verb != "go" {
		t.Errorf("DetailSuggestion.Verb = %q, want %q", got.Verb, "go")
	}
}

// TestDetailSuggestion_MalformedTopLevelStillFallsBackToAttrs is the
// mirror direction: a bad top-level "suggestion" must not prevent reading
// a well-formed one under attrs.
func TestDetailSuggestion_MalformedTopLevelStillFallsBackToAttrs(t *testing.T) {
	detail := json.RawMessage(`{"suggestion":"not an object","attrs":{"suggestion":{"verb":"wake"}}}`)

	got, ok := orchestrator.DetailSuggestion(detail)
	if !ok {
		t.Fatal("DetailSuggestion: ok = false, want true (attrs.suggestion is well-formed)")
	}
	if got.Verb != "wake" {
		t.Errorf("DetailSuggestion.Verb = %q, want %q", got.Verb, "wake")
	}
}

// TestDetailSuggestion_EmptyTopLevelObjectFallsBackToAttrs is the regression
// test for the empty-object-wins bug (BD-8 post-merge review, 2026-08-18):
// decodeSuggestion used to return (Suggestion{}, true) for `{}` (a
// successful Unmarshal, just of nothing), so an empty top-level object
// pre-empted DetailSuggestion's fallback to attrs — a real suggestion under
// attrs.suggestion would never be read. Same shape for a top-level object
// whose fields don't match the Suggestion struct at all (e.g. a differently
// named schema attempt): it decodes to the zero value too and must not win
// either.
func TestDetailSuggestion_EmptyTopLevelObjectFallsBackToAttrs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		detail json.RawMessage
	}{
		{"empty object", json.RawMessage(`{"suggestion":{},"attrs":{"suggestion":{"verb":"wake"}}}`)},
		{"unrecognized fields", json.RawMessage(`{"suggestion":{"v":"wake"},"attrs":{"suggestion":{"verb":"wake"}}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := orchestrator.DetailSuggestion(tc.detail)
			if !ok {
				t.Fatal("DetailSuggestion: ok = false, want true (attrs.suggestion is well-formed)")
			}
			if got.Verb != "wake" {
				t.Errorf("DetailSuggestion.Verb = %q, want %q (must fall back to attrs, not stop at the empty-decoding top-level object)", got.Verb, "wake")
			}
		})
	}
}

// ---- DetailSuggestionRaw (PR #987 review, MEDIUM 9 — this function had no
// dedicated test at all before). accept(verb) (internal/api/
// suggestion_accept.go) is DetailSuggestionRaw's one caller: it needs MORE
// than Suggestion's four fixed fields expose (specifically park's
// params.wake_at/wake_task_id), so it reads the raw bytes of whichever
// candidate DetailSuggestion itself would have picked, rather than the
// decoded-and-narrowed Suggestion struct. ----

// TestDetailSuggestionRaw_TopLevel mirrors TestDetailSuggestion_TopLevel:
// same candidate, but the caller gets the raw bytes back (including a key
// Suggestion's fixed struct doesn't have — "params" here).
func TestDetailSuggestionRaw_TopLevel(t *testing.T) {
	detail := json.RawMessage(`{"suggestion":{"verb":"park","params":{"wake_at":"2026-09-01T00:00:00Z"}}}`)

	raw, ok := orchestrator.DetailSuggestionRaw(detail)
	if !ok {
		t.Fatal("DetailSuggestionRaw: ok = false, want true")
	}
	var got struct {
		Verb   string `json:"verb"`
		Params struct {
			WakeAt string `json:"wake_at"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if got.Verb != "park" || got.Params.WakeAt != "2026-09-01T00:00:00Z" {
		t.Errorf("decoded raw = %+v, want verb=park params.wake_at=2026-09-01T00:00:00Z", got)
	}
}

// TestDetailSuggestionRaw_FromAttrs mirrors TestDetailSuggestion_FromAttrs —
// the attrs_set fold path is where a suggestion actually lands in practice.
func TestDetailSuggestionRaw_FromAttrs(t *testing.T) {
	detail := json.RawMessage(`{"attrs":{"suggestion":{"verb":"reopen"}}}`)

	raw, ok := orchestrator.DetailSuggestionRaw(detail)
	if !ok {
		t.Fatal("DetailSuggestionRaw: ok = false, want true")
	}
	if !strings.Contains(string(raw), `"verb":"reopen"`) {
		t.Errorf("raw = %s, want it to contain verb=reopen", raw)
	}
}

// TestDetailSuggestionRaw_TopLevelWinsOverAttrs mirrors DetailSuggestion's
// own priority order — both functions must agree on WHICH candidate wins,
// or a caller reading Suggestion for display and DetailSuggestionRaw for
// the real params could disagree about which suggestion is "current".
func TestDetailSuggestionRaw_TopLevelWinsOverAttrs(t *testing.T) {
	detail := json.RawMessage(`{
		"suggestion": {"verb": "go"},
		"attrs": {"suggestion": {"verb": "drop"}}
	}`)

	raw, ok := orchestrator.DetailSuggestionRaw(detail)
	if !ok {
		t.Fatal("DetailSuggestionRaw: ok = false, want true")
	}
	var got struct {
		Verb string `json:"verb"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if got.Verb != "go" {
		t.Errorf("raw = %s, want the top-level suggestion (verb=go) to win", raw)
	}
}

// TestDetailSuggestionRaw_EmptyOrAbsent mirrors DetailSuggestion's own
// "nothing to accept" case.
func TestDetailSuggestionRaw_EmptyOrAbsent(t *testing.T) {
	for _, detail := range []json.RawMessage{nil, []byte(""), []byte("null"), []byte("{}"), []byte(`{"summary":"x"}`)} {
		raw, ok := orchestrator.DetailSuggestionRaw(detail)
		if ok {
			t.Errorf("DetailSuggestionRaw(%q): ok = true, want false", detail)
		}
		if raw != nil {
			t.Errorf("DetailSuggestionRaw(%q) = %q, want nil", detail, raw)
		}
	}
}

// TestDetailSuggestionRaw_MalformedJSON_ReturnsFalseNotError mirrors
// DetailSuggestion's own best-effort contract: never error, so a malformed
// blob can never sink a caller reading it (rule 5, 隠さない).
func TestDetailSuggestionRaw_MalformedJSON_ReturnsFalseNotError(t *testing.T) {
	for _, detail := range []json.RawMessage{
		[]byte(`not json`),
		[]byte(`{"suggestion": "not an object"}`),
	} {
		raw, ok := orchestrator.DetailSuggestionRaw(detail)
		if ok {
			t.Errorf("DetailSuggestionRaw(%q): ok = true, want false", detail)
		}
		if raw != nil {
			t.Errorf("DetailSuggestionRaw(%q) = %q, want nil", detail, raw)
		}
	}
}
