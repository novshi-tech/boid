package tui

import (
	"fmt"
	"strings"

	"github.com/novshi-tech/boid/internal/api"
)

// renderDescription renders the Description tab content.
// scroll is the current vertical scroll offset (line index).
// height is the available content height.
func renderDescription(detail *api.TaskDetailView, scroll, width, height int) string {
	_ = width
	var sb strings.Builder

	if detail == nil || detail.Task == nil {
		sb.WriteString(styleDim.Render("  (loading...)"))
		sb.WriteByte('\n')
		return sb.String()
	}

	desc := detail.Task.Description
	if desc == "" {
		sb.WriteString(styleDim.Render("  (no description)"))
		sb.WriteByte('\n')
		return sb.String()
	}

	lines := strings.Split(desc, "\n")
	start := scroll
	if start >= len(lines) {
		start = 0
	}
	end := min(start+height, len(lines))
	for _, line := range lines[start:end] {
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	if end < len(lines) {
		sb.WriteString(styleDim.Render(fmt.Sprintf("  ... %d more lines", len(lines)-end)))
		sb.WriteByte('\n')
	}

	return sb.String()
}
