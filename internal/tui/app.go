package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
)

const pollInterval = 2 * time.Second

var filterCycle = []string{"all", "running", "pending", "completed", "failed"}

// --- messages ---

type tickMsg struct{}
type jobsMsg struct {
	jobs []api.JobWithContext
	err  error
}
type openResultMsg struct {
	jobID  string
	paneID string
	err    error
}
type clearStatusMsg struct{}

// --- model ---

// App is the top-level bubbletea model with a navigation stack.
type App struct {
	shared *SharedState

	// Navigation stack
	screens []Screen

	// Legacy fields for job list screen (will be migrated to a Screen later)
	jobs         []api.JobWithContext
	cursor       int
	panes        map[string]string // jobID -> paneID
	statusMsg    string
	isError      bool
	loading      bool
	fetchErr     error
	activeFilter string
}

// NewApp creates a new TUI application model.
func NewApp(c *client.Client, tmuxEnabled bool) *App {
	shared := &SharedState{
		Client:      c,
		TmuxEnabled: tmuxEnabled,
	}
	return &App{
		shared:       shared,
		panes:        make(map[string]string),
		loading:      true,
		activeFilter: "running",
	}
}

// --- bubbletea interface ---

func (m *App) Init() tea.Cmd {
	// Push TaskListScreen as the home screen
	home := NewTaskListScreen(m.shared)
	m.screens = []Screen{home}
	return home.Init()
}

func (m *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.shared.Width = msg.Width
		m.shared.Height = msg.Height

	case PushScreenMsg:
		m.screens = append(m.screens, msg.Screen)
		return m, msg.Screen.Init()

	case PopScreenMsg:
		if len(m.screens) > 1 {
			m.screens = m.screens[:len(m.screens)-1]
		}
		return m, nil

	case tea.KeyMsg:
		// Let q/ctrl+c quit from anywhere
		if msg.String() == "q" || msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}

	// Delegate to current screen
	if len(m.screens) > 0 {
		current := m.screens[len(m.screens)-1]
		newScreen, cmd := current.Update(msg)
		m.screens[len(m.screens)-1] = newScreen
		return m, cmd
	}

	return m, nil
}

func (m *App) View() string {
	if m.shared.Width == 0 {
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
		strings.Repeat(" ", max(0, m.shared.Width-lipgloss.Width(title)-lipgloss.Width(badge))),
		badge,
	)
	sb.WriteString(header)
	sb.WriteByte('\n')
	sb.WriteString(strings.Repeat("─", m.shared.Width))
	sb.WriteByte('\n')

	// --- screen body ---
	if len(m.screens) > 0 {
		sb.WriteString(m.screens[len(m.screens)-1].View())
	}

	return sb.String()
}

func buildFilterBar(active string) string {
	labels := map[string]string{
		"all":       "all",
		"running":   "● running",
		"pending":   "pending",
		"completed": "completed",
		"failed":    "failed",
	}
	var parts []string
	for _, f := range filterCycle {
		label := labels[f]
		if f == active {
			parts = append(parts, styleFilterActive.Render(" "+label+" "))
		} else {
			parts = append(parts, styleFilterInactive.Render(" "+label+" "))
		}
	}
	return strings.Join(parts, "")
}

func buildFooter(tmuxEnabled bool) string {
	if tmuxEnabled {
		return " 1-5/tab: filter   enter: open   j/k: move   r: refresh   q: quit"
	}
	return " 1-5/tab: filter   j/k: move   r: refresh   q: quit"
}

// --- commands ---

func tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg {
		return tickMsg{}
	})
}

func fetchJobsCmd(c *client.Client, filter string) tea.Cmd {
	return func() tea.Msg {
		f := api.JobListFilter{}
		if filter != "all" {
			f.Status = filter
		}
		jobs, err := c.ListJobs(f)
		return jobsMsg{jobs: jobs, err: err}
	}
}

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

func clearStatusAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg {
		return clearStatusMsg{}
	})
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

