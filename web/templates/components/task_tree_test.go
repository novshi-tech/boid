package components

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestChildrenPreviewLabel(t *testing.T) {
	cases := []struct {
		name     string
		children []orchestrator.TaskTriageChild
		want     string
	}{
		{"none", nil, ""},
		{
			"specced only",
			[]orchestrator.TaskTriageChild{{Status: orchestrator.TaskTriageChildStatusSpecced}},
			"1 specced",
		},
		{
			"open only",
			[]orchestrator.TaskTriageChild{{Status: orchestrator.TaskTriageChildStatusOpen}},
			"1 open",
		},
		{
			"mixed",
			[]orchestrator.TaskTriageChild{
				{Status: orchestrator.TaskTriageChildStatusSpecced},
				{Status: orchestrator.TaskTriageChildStatusSpecced},
				{Status: orchestrator.TaskTriageChildStatusOpen},
			},
			"2 specced, 1 open",
		},
		{
			"only dispatched/closed falls back to a bare count",
			[]orchestrator.TaskTriageChild{
				{Status: orchestrator.TaskTriageChildStatusDispatched},
				{Status: orchestrator.TaskTriageChildStatusClosed},
			},
			"2 children",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := childrenPreviewLabel(c.children)
			if got != c.want {
				t.Errorf("childrenPreviewLabel(%+v) = %q, want %q", c.children, got, c.want)
			}
		})
	}
}

func makeTreeTestTask(id string) *orchestrator.Task {
	return &orchestrator.Task{
		ID:        id,
		Title:     "task " + id,
		Status:    orchestrator.TaskStatusParked,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

// TestTaskTreeRow_RendersSuggestionVerbBadgeAndReason is a render test for
// the verb badge + reason line taskTreeRow adds for BD-8 (Opus review
// finding, 2026-08-18: this had no render test at all, so a typo in the
// class name or field used — e.g. "badge-verb-"+item.Suggestion.Action
// instead of .Verb — would have shipped green).
func TestTaskTreeRow_RendersSuggestionVerbBadgeAndReason(t *testing.T) {
	item := TreeItem{
		Task:       makeTreeTestTask("t1"),
		Suggestion: orchestrator.Suggestion{Verb: "reopen", Reason: "source event fired"},
	}

	var buf bytes.Buffer
	if err := TaskTree([]TreeItem{item}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, `class="badge badge-verb-reopen"`) {
		t.Errorf("expected verb badge with class badge-verb-reopen, got: %s", html)
	}
	if !strings.Contains(html, `>reopen<`) {
		t.Errorf("expected visible verb label \"reopen\", got: %s", html)
	}
	if !strings.Contains(html, "source event fired") {
		t.Errorf("expected reason text, got: %s", html)
	}
}

// TestTaskTreeRow_NoSuggestion_RendersNoVerbBadge covers the (overwhelming
// majority) zero-value case: no badge, no reason line, no "badge-verb-"
// class fragment at all.
func TestTaskTreeRow_NoSuggestion_RendersNoVerbBadge(t *testing.T) {
	item := TreeItem{Task: makeTreeTestTask("t1")}

	var buf bytes.Buffer
	if err := TaskTree([]TreeItem{item}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if strings.Contains(html, "badge-verb-") {
		t.Errorf("expected no verb badge for an empty Suggestion, got: %s", html)
	}
}

// TestVerbBadgeClass_KnownVerbs pins each of card machine v2's six
// transition verbs (orchestrator.IsCardTransitionAction; docs/plans/
// suggestion-as-state-transition.md §3.1 — suggestion.verb is boid's own
// state-machine vocabulary now, not the old free-form go/shape/manual/park/
// drop/wake set) to its own class. Deliberately does not derive the
// expected list from knownSuggestionVerbs itself — a test that reads the
// same map it's checking can't catch a typo inside that map.
func TestVerbBadgeClass_KnownVerbs(t *testing.T) {
	for _, verb := range []string{"go", "working", "park", "drop", "done", "reopen"} {
		got := VerbBadgeClass(verb)
		want := "badge-verb-" + verb
		if got != want {
			t.Errorf("VerbBadgeClass(%q) = %q, want %q", verb, got, want)
		}
	}
}

// TestVerbBadgeClass_UnknownVerbFallsBackToNeutral is the BD-8 残件4
// regression: suggestion.verb is not vocabulary-checked anywhere upstream
// (see knownSuggestionVerbs' doc comment for why it must not be added to
// orchestrator's promotedAttrVocabulary), so an unrecognized verb must map
// to the neutral fallback class rather than to a "badge-verb-<unknown
// word>" class that has no matching CSS rule (and would render with no
// color at all).
func TestVerbBadgeClass_UnknownVerbFallsBackToNeutral(t *testing.T) {
	for _, verb := range []string{"", "unknown-future-verb", "Go", "WAKE", "go "} {
		if got := VerbBadgeClass(verb); got != "badge-verb-unknown" {
			t.Errorf("VerbBadgeClass(%q) = %q, want %q", verb, got, "badge-verb-unknown")
		}
	}
}

// TestTaskTreeRow_UnknownVerb_StillRendersTextWithNeutralClass is the
// render-level regression for the queue/Parked row half of BD-8 残件4: an
// unrecognized verb must still show its literal text (rule 5, 隠さない) —
// only the badge's color/class falls back to neutral.
func TestTaskTreeRow_UnknownVerb_StillRendersTextWithNeutralClass(t *testing.T) {
	item := TreeItem{
		Task:       makeTreeTestTask("t1"),
		Suggestion: orchestrator.Suggestion{Verb: "mystery", Reason: "unclear"},
	}

	var buf bytes.Buffer
	if err := TaskTree([]TreeItem{item}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, `class="badge badge-verb-unknown"`) {
		t.Errorf("expected neutral badge-verb-unknown class, got: %s", html)
	}
	if !strings.Contains(html, ">mystery<") {
		t.Errorf("expected the unknown verb's literal text to still render, got: %s", html)
	}
}
