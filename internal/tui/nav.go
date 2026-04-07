package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/client"
)

// Screen represents a single screen in the navigation stack.
type Screen interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (Screen, tea.Cmd)
	View(width, height int) string
	ShortHelp() string // footer key-binding summary
}

// SharedState holds state shared across all screens.
type SharedState struct {
	Client      *client.Client
	TmuxEnabled bool
	Panes       map[string]string // jobID -> paneID
}

// pushScreenMsg tells the App to push a new screen onto the stack.
type pushScreenMsg struct{ screen Screen }

// popScreenMsg tells the App to pop the top screen from the stack.
type popScreenMsg struct{}

// screenResumedMsg is sent to the screen that becomes visible after a pop.
type screenResumedMsg struct{}

// PushScreen returns a tea.Cmd that pushes a screen onto the navigation stack.
func PushScreen(s Screen) tea.Cmd {
	return func() tea.Msg { return pushScreenMsg{s} }
}

// PopScreen is a tea.Msg that pops the top screen from the navigation stack.
func PopScreen() tea.Msg { return popScreenMsg{} }
