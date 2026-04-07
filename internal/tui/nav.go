package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/client"
)

// Screen is the interface that all TUI screens must implement.
type Screen interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (Screen, tea.Cmd)
	View() string
}

// SharedState holds state shared across all screens.
type SharedState struct {
	Client      *client.Client
	TmuxEnabled bool
	Width       int
	Height      int
}

// PushScreenMsg requests the App to push a new screen onto the stack.
type PushScreenMsg struct {
	Screen Screen
}

// PopScreenMsg requests the App to pop the current screen from the stack.
type PopScreenMsg struct{}

// PushScreen returns a command that pushes a screen onto the navigation stack.
func PushScreen(s Screen) tea.Cmd {
	return func() tea.Msg {
		return PushScreenMsg{Screen: s}
	}
}

// PopScreen returns a command that pops the current screen from the navigation stack.
func PopScreen() tea.Cmd {
	return func() tea.Msg {
		return PopScreenMsg{}
	}
}
