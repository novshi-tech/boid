package tui

import (
	"testing"
)

// TestDetailTabs_Order verifies the tab order is Overview → Description → Deps → Payload.
func TestDetailTabs_Order(t *testing.T) {
	want := []string{tabOverview, tabDescription, tabDeps, tabPayload}
	if len(detailTabs) != len(want) {
		t.Fatalf("detailTabs: want %d tabs, got %d", len(want), len(detailTabs))
	}
	for i, w := range want {
		if detailTabs[i].id != w {
			t.Errorf("detailTabs[%d].id = %q, want %q", i, detailTabs[i].id, w)
		}
	}
}

// TestDetailTabs_NoTimeline verifies the Timeline tab no longer exists.
func TestDetailTabs_NoTimeline(t *testing.T) {
	for _, tab := range detailTabs {
		if tab.id == "timeline" {
			t.Errorf("Timeline tab should be removed but found: %+v", tab)
		}
	}
}

// TestDetailTabs_DefaultIsOverview verifies the default tab is Overview.
func TestDetailTabs_DefaultIsOverview(t *testing.T) {
	s := newTestTaskDetailScreen()
	if s.activeTab != tabOverview {
		t.Errorf("default tab: want %q, got %q", tabOverview, s.activeTab)
	}
}

// TestCycleTab_ForwardSequence verifies full forward cycle: Overview → Description → Deps → Payload → Overview.
func TestCycleTab_ForwardSequence(t *testing.T) {
	sequence := []string{tabOverview, tabDescription, tabDeps, tabPayload, tabOverview}
	current := tabOverview
	for i := 1; i < len(sequence); i++ {
		next := cycleTab(current, 1)
		if next != sequence[i] {
			t.Errorf("cycleTab(%q, +1): want %q, got %q", current, sequence[i], next)
		}
		current = next
	}
}

// TestCycleTab_BackwardSequence verifies full backward cycle: Overview → Payload → Deps → Description → Overview.
func TestCycleTab_BackwardSequence(t *testing.T) {
	sequence := []string{tabOverview, tabPayload, tabDeps, tabDescription, tabOverview}
	current := tabOverview
	for i := 1; i < len(sequence); i++ {
		next := cycleTab(current, -1)
		if next != sequence[i] {
			t.Errorf("cycleTab(%q, -1): want %q, got %q", current, sequence[i], next)
		}
		current = next
	}
}

// TestShortHelp_TabSpecific verifies that ShortHelp differs per active tab.
func TestShortHelp_TabSpecific(t *testing.T) {
	cases := []struct {
		tab     string
		want    string
		notWant string
	}{
		{tabOverview, "e: edit title", "e: edit description"},
		{tabDescription, "e: edit description", "e: edit title"},
		{tabDeps, "enter: jump to task", "e: edit title"},
		{tabPayload, "e: edit section", "e: edit title"},
	}
	for _, tc := range cases {
		s := newTestTaskDetailScreen()
		s.activeTab = tc.tab
		help := s.ShortHelp()
		if !containsStr(help, tc.want) {
			t.Errorf("ShortHelp(%q): expected %q, got %q", tc.tab, tc.want, help)
		}
		if tc.notWant != "" && containsStr(help, tc.notWant) {
			t.Errorf("ShortHelp(%q): should not contain %q, got %q", tc.tab, tc.notWant, help)
		}
	}
}

// TestShortHelp_AlwaysContainsTabKey verifies tab/shift+tab is always present.
func TestShortHelp_AlwaysContainsTabKey(t *testing.T) {
	tabs := []string{tabOverview, tabDescription, tabDeps, tabPayload}
	for _, tab := range tabs {
		s := newTestTaskDetailScreen()
		s.activeTab = tab
		help := s.ShortHelp()
		if !containsStr(help, "tab") {
			t.Errorf("ShortHelp(%q): expected 'tab' in help, got %q", tab, help)
		}
	}
}
