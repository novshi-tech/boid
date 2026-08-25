package api

import "testing"

// triageSummary backs the one-line badge on the queue/tree rows. The only
// writer a workspace has for task_triage.detail is the attrs_set action, and
// that folds into detail.ATTRS (FoldDetailAttrs, internal/orchestrator/
// card.go) — never the top level. So a reader that looks at
// detail.summary alone can never see anything a workspace wrote, and the badge
// stays empty forever: found while wiring khi's new summary path, where S-1
// ("keep writing the one-line summary separately; the queue row badge reads
// it") turned out not to be satisfiable at all.
//
// The fallback mirrors DetailSuggestion (same file's neighbour in
// internal/orchestrator/card.go), which already reads top-level first
// and falls back to attrs for exactly the same reason.

func TestTriageSummary_TopLevel(t *testing.T) {
	got := triageSummary([]byte(`{"summary":"top"}`))
	if got != "top" {
		t.Fatalf("summary = %q, want %q", got, "top")
	}
}

func TestTriageSummary_FallsBackToAttrs(t *testing.T) {
	// What a workspace can actually produce: attrs_set{"summary": ...}.
	got := triageSummary([]byte(`{"attrs":{"summary":"from attrs","urgency":"now"}}`))
	if got != "from attrs" {
		t.Fatalf("summary = %q, want %q", got, "from attrs")
	}
}

func TestTriageSummary_TopLevelWinsOverAttrs(t *testing.T) {
	// A future writer that places it at the top level should not be shadowed by
	// a stale attrs copy.
	got := triageSummary([]byte(`{"summary":"top","attrs":{"summary":"attrs"}}`))
	if got != "top" {
		t.Fatalf("summary = %q, want %q", got, "top")
	}
}

func TestTriageSummary_EmptyTopLevelDoesNotBlockTheFallback(t *testing.T) {
	got := triageSummary([]byte(`{"summary":"","attrs":{"summary":"attrs"}}`))
	if got != "attrs" {
		t.Fatalf("summary = %q, want %q", got, "attrs")
	}
}

func TestTriageSummary_Absent(t *testing.T) {
	for _, detail := range []string{``, `null`, `{}`, `{"attrs":{}}`, `{"children":[]}`} {
		if got := triageSummary([]byte(detail)); got != "" {
			t.Errorf("triageSummary(%s) = %q, want empty", detail, got)
		}
	}
}

// Never panics, never partially-fabricates a value — same best-effort posture
// the function already had: a malformed blob must not sink the row it lives on.
func TestTriageSummary_Malformed(t *testing.T) {
	for _, detail := range []string{
		`not json`,
		`{"summary":`,
		`{"summary":123}`,             // wrong type at the top level
		`{"attrs":"not an object"}`,   // attrs is not a map
		`{"attrs":{"summary":["a"]}}`, // wrong type inside attrs
	} {
		if got := triageSummary([]byte(detail)); got != "" {
			t.Errorf("triageSummary(%s) = %q, want empty", detail, got)
		}
	}
}

// A wrong-typed value in one location must not blank out a well-formed value in
// the other (the finding that shaped decodeSuggestion's own shape: Opus review,
// 2026-08-18).
func TestTriageSummary_BadTopLevelStillReadsAttrs(t *testing.T) {
	got := triageSummary([]byte(`{"summary":123,"attrs":{"summary":"attrs"}}`))
	if got != "attrs" {
		t.Fatalf("summary = %q, want %q", got, "attrs")
	}
}
