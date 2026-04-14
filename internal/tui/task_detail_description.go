package tui

import (
	"fmt"
	"strings"

	"github.com/novshi-tech/boid/internal/api"
)

// wordWrapLine breaks a single line into multiple lines each at most width runes wide.
// If width <= 0 the line is returned unchanged.
func wordWrapLine(line string, width int) []string {
	if width <= 0 {
		return []string{line}
	}
	runes := []rune(line)
	if len(runes) <= width {
		return []string{line}
	}
	var result []string
	for len(runes) > width {
		cut := width
		// find last space before width to break on a word boundary
		for cut > 0 && runes[cut-1] != ' ' {
			cut--
		}
		if cut == 0 {
			// no space found: hard-break at width
			cut = width
		}
		result = append(result, string(runes[:cut]))
		// skip leading spaces on the continuation
		runes = runes[cut:]
		for len(runes) > 0 && runes[0] == ' ' {
			runes = runes[1:]
		}
	}
	if len(runes) > 0 {
		result = append(result, string(runes))
	}
	return result
}

// wrapLines splits desc by newlines and word-wraps each physical line to width.
// If width <= 0, physical lines are returned without wrapping.
func wrapLines(desc string, width int) []string {
	physical := strings.Split(desc, "\n")
	if width <= 0 {
		return physical
	}
	var result []string
	for _, line := range physical {
		result = append(result, wordWrapLine(line, width)...)
	}
	return result
}

// renderDescription renders the Description tab content.
// scroll is the current vertical scroll offset (in wrapped logical lines).
// width is used for word-wrapping; height is the available content height.
func renderDescription(detail *api.TaskDetailView, scroll, width, height int) string {
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

	lines := wrapLines(desc, width)
	start := scroll
	if start >= len(lines) {
		start = 0
	}

	// Determine how many lines can be shown, reserving 1 for the "more" summary
	// when content is truncated so total output never exceeds height lines.
	hasMore := start+height < len(lines)
	var end int
	if hasMore {
		end = start + height - 1 // reserve last slot for summary
	} else {
		end = min(start+height, len(lines))
	}

	for _, line := range lines[start:end] {
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	if hasMore {
		sb.WriteString(styleDim.Render(fmt.Sprintf("  ... %d more lines", len(lines)-end)))
		sb.WriteByte('\n')
	}

	return sb.String()
}
