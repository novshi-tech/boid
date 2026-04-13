package tui

import (
	"strings"
)

const (
	tabOverview = "overview"
	tabTimeline = "timeline"
	tabDeps     = "deps"
	tabPayload  = "payload"
)

type tabDef struct {
	id    string
	label string
}

var detailTabs = []tabDef{
	{tabOverview, "[O]verview"},
	{tabTimeline, "[T]imeline"},
	{tabDeps, "[D]eps"},
	{tabPayload, "[P]ayload"},
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
	innerWidth := lipglossWidth(inner)
	fillLen := max(width-prefixWidth-innerWidth-1, 0)
	return styleDim.Render(prefix) + inner + styleDim.Render(" "+strings.Repeat("─", fillLen))
}
