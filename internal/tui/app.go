package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/novshi-tech/boid/internal/client"
)

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
	return &App{
		shared: shared,
		stack:  []Screen{NewTaskListScreen(shared)},
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
	if top := m.top(); top != nil {
		return top.Init()
	}
	return nil
}

func (m *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Propagate to all screens.
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
		}
	}

	// Delegate to top screen.
	if top := m.top(); top != nil {
		updated, cmd := top.Update(msg)
		m.stack[len(m.stack)-1] = updated
		return m, cmd
	}
	return m, nil
}

func (m *App) View() string {
	if m.width == 0 {
		return ""
	}

	var sb strings.Builder

	// --- header ---
	badge := styleBadge.Render("[tmux]")
	if !m.shared.TmuxEnabled {
		badge = styleWarn.Render("[no-tmux]")
	}
	title := styleHeader.Render("boid") + styleDim.Render(" ─ tasks")
	header := lipgloss.JoinHorizontal(lipgloss.Top,
		title,
		strings.Repeat(" ", max(0, m.width-lipgloss.Width(title)-lipgloss.Width(badge))),
		badge,
	)
	sb.WriteString(header)
	sb.WriteByte('\n')
	sb.WriteString(strings.Repeat("─", m.width))
	sb.WriteByte('\n')

	// --- body (top screen) ---
	bodyHeight := m.height - 4 // header(2) + footer(2)
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	if top := m.top(); top != nil {
		sb.WriteString(top.View(m.width, bodyHeight))
	}

	// --- footer ---
	sb.WriteString(strings.Repeat("─", m.width))
	sb.WriteByte('\n')
	if top := m.top(); top != nil {
		sb.WriteString(styleFooter.Render(top.ShortHelp()))
	}

	return sb.String()
}
