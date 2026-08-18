package components

import "testing"

// TestStatusTabActive_EmptyStatusTreatedAsOpen pins the defensive
// normalization statusTabActive's doc comment claims: an empty
// filter.Status (should not happen once web.go's TaskList normalizes it,
// but this function must not assume that) is treated as "open".
func TestStatusTabActive_EmptyStatusTreatedAsOpen(t *testing.T) {
	if !statusTabActive("", "open") {
		t.Error(`statusTabActive("", "open") = false, want true`)
	}
	if statusTabActive("", "parked") {
		t.Error(`statusTabActive("", "parked") = true, want false`)
	}
}

// TestStatusTabActive_UnknownStatusMatchesNothing is the BD-8 bug this
// function replaced: any status that isn't "closed" or "queue_next" used
// to fall into an else-branch that always marked Open active. An unknown
// or not-yet-added status value (e.g. "parked" before this tab existed)
// must not match any tab.
func TestStatusTabActive_UnknownStatusMatchesNothing(t *testing.T) {
	for _, tab := range []string{"open", "closed", "queue_next", "parked"} {
		if statusTabActive("some-future-status", tab) {
			t.Errorf(`statusTabActive("some-future-status", %q) = true, want false`, tab)
		}
	}
}

func TestStatusTabActive_ExactMatch(t *testing.T) {
	for _, tab := range []string{"open", "closed", "queue_next", "parked"} {
		if !statusTabActive(tab, tab) {
			t.Errorf("statusTabActive(%q, %q) = false, want true", tab, tab)
		}
	}
}
