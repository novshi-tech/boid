package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
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

// truncate limits s to at most maxWidth display cells, appending "…" if truncated.
// No padding is added; use fitCell for fixed-width column output.
// Correctly handles ANSI escape sequences and wide characters.
func truncate(s string, maxWidth int) string {
	if xansi.StringWidth(s) <= maxWidth {
		return s
	}
	return xansi.GraphemeWidth.Truncate(s, maxWidth, "…")
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

// taskUpdatedMsg is sent when a task update API call completes.
type taskUpdatedMsg struct {
	task *orchestrator.Task
	err  error
}

// updateTaskCmd sends an UpdateTask request to the server.
func updateTaskCmd(c *client.Client, taskID string, req api.UpdateTaskRequest) tea.Cmd {
	return func() tea.Msg {
		task, err := c.UpdateTask(taskID, req)
		return taskUpdatedMsg{task: task, err: err}
	}
}
