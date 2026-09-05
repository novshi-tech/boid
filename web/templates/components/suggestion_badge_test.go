package components

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestVerbBadgeClass_KnownVerbs pins each of card machine v2's six
// transition verbs (orchestrator.IsCardTransitionAction; docs/plans/
// suggestion-as-state-transition.md §3.1 — suggestion.verb is boid's own
// state-machine vocabulary now, not the old free-form go/shape/manual/park/
// drop/wake set) to its own class. Deliberately does not derive the
// expected list from knownSuggestionVerbs itself — a test that reads the
// same map it's checking can't catch a typo inside that map.
func TestVerbBadgeClass_KnownVerbs(t *testing.T) {
	for _, verb := range []string{"go", "start", "park", "drop", "complete", "reopen"} {
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

// TestVerbBadgeClass_RetiredSpellingsFallBackToNeutral pins
// knownSuggestionVerbs' own doc comment: this map does not normalize a
// retired verb spelling (unlike the write-side orchestrator.NormalizeCardVerb)
// — a suggestion still carrying "working"/"done" would render as unknown
// here, not as start/complete. In production the §8 data migration keeps a
// real stored suggestion from ever reaching this state; this test only
// pins the (deliberate) rendering behavior if one somehow did.
func TestVerbBadgeClass_RetiredSpellingsFallBackToNeutral(t *testing.T) {
	for _, verb := range []string{"working", "done"} {
		if got := VerbBadgeClass(verb); got != "badge-verb-unknown" {
			t.Errorf("VerbBadgeClass(%q) = %q, want %q", verb, got, "badge-verb-unknown")
		}
	}
}

// TestSuggestionInapplicable_AllVerbStatusCombinations mirrors
// orchestrator.TestCardMachineV2_CanApplyTransitionAction_PinsExactlyEightEdges
// at the components-package level: exactly 9 of the 24 (verb, status)
// combinations are applicable.
func TestSuggestionInapplicable_AllVerbStatusCombinations(t *testing.T) {
	applicable := map[string]map[orchestrator.TaskStatus]bool{
		"go":       {orchestrator.TaskStatusParked: true, orchestrator.TaskStatusWorking: true},
		"start":    {orchestrator.TaskStatusParked: true},
		"drop":     {orchestrator.TaskStatusParked: true},
		"park":     {orchestrator.TaskStatusWorking: true},
		"complete": {orchestrator.TaskStatusParked: true, orchestrator.TaskStatusWorking: true},
		"reopen":   {orchestrator.TaskStatusDone: true, orchestrator.TaskStatusDropped: true},
	}
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusWorking,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusDropped,
	}

	for verb, byStatus := range applicable {
		for _, status := range statuses {
			wantInapplicable := !byStatus[status]
			got := SuggestionInapplicable(verb, status)
			if got != wantInapplicable {
				t.Errorf("SuggestionInapplicable(%s, %s) = %v, want %v", verb, status, got, wantInapplicable)
			}
		}
	}
}

// TestSuggestionInapplicable_EmptyVerb_NeverInapplicable pins that an empty
// verb (the zero-value "no suggestion" case) never counts as inapplicable —
// callers gate on Suggestion.Verb != "" separately (the list row, task detail
// page) and must not ALSO get an inapplicable badge for a task with no
// suggestion at all.
func TestSuggestionInapplicable_EmptyVerb_NeverInapplicable(t *testing.T) {
	for _, status := range []orchestrator.TaskStatus{
		orchestrator.TaskStatusParked, orchestrator.TaskStatusWorking,
		orchestrator.TaskStatusDone, orchestrator.TaskStatusDropped,
	} {
		if SuggestionInapplicable("", status) {
			t.Errorf("SuggestionInapplicable(\"\", %s) = true, want false", status)
		}
	}
}
