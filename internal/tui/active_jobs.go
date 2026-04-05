package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/api"
)

// formatElapsed returns a human-readable elapsed time string (MM:SS or HH:MM:SS).
func formatElapsed(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// shortID returns the first 8 characters of a job ID.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// renderJobList renders the active job list component.
// It returns the lines to display (without the header/footer).
func renderJobLine(job api.JobWithContext, selected bool, width int) string {
	cursor := "  "
	if selected {
		cursor = styleCursor.Render("▸ ")
	}

	dot := styleRunning.Render("●")
	id := styleTitle.Render(shortID(job.ID))

	title := job.TaskTitle
	if title == "" {
		title = "(no title)"
	}
	proj := ""
	if job.ProjectName != "" {
		proj = styleDim.Render("[" + job.ProjectName + "]")
	}
	elapsed := styleDim.Render(formatElapsed(job.CreatedAt))

	// Build the line content
	mid := fmt.Sprintf(" %s  %-8s  %-24s  %-12s  %s",
		dot, id, truncate(title, 24), proj, elapsed)

	line := cursor + mid

	_ = width // reserved for future padding
	return line
}

// truncate limits a string to maxLen runes, appending "…" if truncated.
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		// pad with spaces
		return s + strings.Repeat(" ", maxLen-len(runes))
	}
	return string(runes[:maxLen-1]) + "…"
}
