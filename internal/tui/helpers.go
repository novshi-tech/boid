package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

// truncate limits a string to maxLen runes, appending "…" if truncated.
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		// pad with spaces
		return s + strings.Repeat(" ", maxLen-len(runes))
	}
	return string(runes[:maxLen-1]) + "…"
}

// openResultMsg is sent when a job pane open attempt completes.
type openResultMsg struct {
	jobID  string
	paneID string
	err    error
}

// clearStatusMsg is sent to clear the status bar message.
type clearStatusMsg struct{}

// openJobCmd opens a job in a tmux pane (reusing an existing pane if alive).
func openJobCmd(jobID, existingPaneID string) tea.Cmd {
	return func() tea.Msg {
		if PaneAlive(existingPaneID) {
			if err := FocusPane(existingPaneID); err == nil {
				return openResultMsg{jobID: jobID, paneID: existingPaneID}
			}
		}
		paneID, err := OpenJobInPane(jobID)
		return openResultMsg{jobID: jobID, paneID: paneID, err: err}
	}
}

// clearStatusAfter returns a Cmd that sends clearStatusMsg after duration d.
func clearStatusAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg {
		return clearStatusMsg{}
	})
}
