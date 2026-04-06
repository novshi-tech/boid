package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/novshi-tech/boid/internal/client"
)

// --- model ---

// App is the top-level bubbletea model with a screen stack.
type App struct {
	shared *SharedState
	stack  []Screen
	width  int
	height int
}

// NewApp creates a new TUI application model.
func NewApp(c *client.Client, tmuxEnabled bool) *App {
	shared := &SharedState{
		Client:      c,
		TmuxEnabled: tmuxEnabled,
		Panes:       make(map[string]string),
	}
	initial := NewActiveJobsScreen(shared)
	return &App{
		shared: shared,
		stack:  []Screen{initial},
	}
}

func (m *App) top() Screen {
	if len(m.stack) == 0 {
		return nil
	}
	return m.stack[len(m.stack)-1]
}

// --- bubbletea interface ---

func (m *App) Init() tea.Cmd {
	if s := m.top(); s != nil {
		return s.Init()
	}
	return nil
}

func (m *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		var cmds []tea.Cmd
		for i, s := range m.stack {
			updated, cmd := s.Update(msg)
			m.stack[i] = updated
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)

	case pushScreenMsg:
		m.stack = append(m.stack, msg.screen)
		return m, msg.screen.Init()

	case popScreenMsg:
		if len(m.stack) > 1 {
			m.stack = m.stack[:len(m.stack)-1]
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc", "backspace":
			if len(m.stack) > 1 {
				m.stack = m.stack[:len(m.stack)-1]
				return m, nil
			}
		}
	}

	// Delegate to top screen
	if s := m.top(); s != nil {
		updated, cmd := s.Update(msg)
		m.stack[len(m.stack)-1] = updated
		return m, cmd
	}
	return m, nil
}

func (m *App) View() string {
	if m.width == 0 {
		return ""
	}

	s := m.top()
	if s == nil {
		return ""
	}

	var sb strings.Builder

	// --- header ---
	badge := styleBadge.Render("[tmux]")
	if !m.shared.TmuxEnabled {
		badge = styleWarn.Render("[no-tmux]")
	}
	title := styleHeader.Render("boid")
	header := lipgloss.JoinHorizontal(lipgloss.Top,
		title,
		strings.Repeat(" ", max(0, m.width-lipgloss.Width(title)-lipgloss.Width(badge))),
		badge,
	)
	sb.WriteString(header)
	sb.WriteByte('\n')
	sb.WriteString(strings.Repeat("─", m.width))
	sb.WriteByte('\n')

	// --- body ---
	bodyHeight := max(1, m.height-4) // header(1) + separator(1) + footer separator(1) + footer(1)
	sb.WriteString(s.View(m.width, bodyHeight))

	// --- footer ---
	sb.WriteString(strings.Repeat("─", m.width))
	sb.WriteByte('\n')
	sb.WriteString(styleFooter.Render(s.ShortHelp()))

	return sb.String()
}
