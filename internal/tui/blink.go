package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

const blinkInterval = 600 * time.Millisecond

// taskBlinkTickMsg is sent on each blink tick to toggle dot/badge visibility.
type taskBlinkTickMsg struct{}

// taskBlinkCmd schedules the next blink tick.
func taskBlinkCmd() tea.Cmd {
	return tea.Tick(blinkInterval, func(time.Time) tea.Msg {
		return taskBlinkTickMsg{}
	})
}

// isBlinkTarget reports whether status is one whose dot should blink.
func isBlinkTarget(status orchestrator.TaskStatus) bool {
	return status == orchestrator.TaskStatusExecuting
}
