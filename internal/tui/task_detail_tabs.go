package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const (
	tabOverview     = "overview"
	tabDescription  = "description"
	tabDeps         = "deps"
	tabInstructions = "instructions"
	tabPayload      = "payload"
)

type tabDef struct {
	id    string
	label string
}

var detailTabs = []tabDef{
	{tabOverview, "Overview"},
	{tabDescription, "Description"},
	{tabDeps, "Deps"},
	{tabInstructions, "Instructions"},
	{tabPayload, "Payload"},
}

// cycleTab returns the tab id that is delta steps away from current in detailTabs.
// delta=+1 moves forward; delta=-1 moves backward. Wraps around at the ends.
func cycleTab(current string, delta int) string {
	idx := 0
	for i, t := range detailTabs {
		if t.id == current {
			idx = i
			break
		}
	}
	n := len(detailTabs)
	next := ((idx+delta)%n + n) % n
	return detailTabs[next].id
}

// renderTabBar returns a separator line with tab labels.
// The active tab is highlighted; others are dimmed.
func renderTabBar(activeTab string, width int) string {
	var parts []string
	for _, t := range detailTabs {
		if t.id == activeTab {
			parts = append(parts, styleFilterActive.Render(t.label))
		} else {
			parts = append(parts, styleFilterInactive.Render(t.label))
		}
	}
	inner := strings.Join(parts, "  ")
	prefix := "─── "
	prefixWidth := len([]rune(prefix))
	innerWidth := lipgloss.Width(inner)
	fillLen := max(width-prefixWidth-innerWidth-1, 0)
	return styleDim.Render(prefix) + inner + styleDim.Render(" "+strings.Repeat("─", fillLen))
}
