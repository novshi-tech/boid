package tui

import (
	"fmt"
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

// App is the top-level bubbletea model.
type App struct {
	client      *client.Client
	tmuxEnabled bool

	jobs         []api.JobWithContext
	cursor       int
	panes        map[string]string // jobID -> paneID
	statusMsg    string
	isError      bool
	width        int
	height       int
	loading      bool
	fetchErr     error
	activeFilter string
}

// NewApp creates a new TUI application model.
func NewApp(c *client.Client, tmuxEnabled bool) *App {
	return &App{
		client:       c,
		tmuxEnabled:  tmuxEnabled,
		panes:        make(map[string]string),
		loading:      true,
		activeFilter: "running",
	}
}

// --- bubbletea interface ---

func (m *App) Init() tea.Cmd {
	return tea.Batch(fetchJobsCmd(m.client, m.activeFilter), tickCmd())
}

func (m *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		return m, tea.Batch(fetchJobsCmd(m.client, m.activeFilter), tickCmd())

	case jobsMsg:
		m.loading = false
		if msg.err != nil {
			m.fetchErr = msg.err
		} else {
			m.fetchErr = nil
			m.jobs = msg.jobs
			if m.cursor >= len(m.jobs) && len(m.jobs) > 0 {
				m.cursor = len(m.jobs) - 1
			}
		}

	case openResultMsg:
		if msg.err != nil {
			m.statusMsg = "open failed: " + msg.err.Error()
			m.isError = true
			return m, clearStatusAfter(3 * time.Second)
		}
		if msg.paneID != "" {
			m.panes[msg.jobID] = msg.paneID
		}

	case clearStatusMsg:
		m.statusMsg = ""
		m.isError = false

	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}

	return m, nil
}

func (m *App) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "q", "ctrl+c":
		return tea.Quit

	case "j", "down":
		if m.cursor < len(m.jobs)-1 {
			m.cursor++
		}

	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}

	case "r":
		m.loading = true
		return fetchJobsCmd(m.client, m.activeFilter)

	case "1":
		m.activeFilter = "all"
		m.cursor = 0
		m.loading = true
		return fetchJobsCmd(m.client, m.activeFilter)

	case "2":
		m.activeFilter = "running"
		m.cursor = 0
		m.loading = true
		return fetchJobsCmd(m.client, m.activeFilter)

	case "3":
		m.activeFilter = "pending"
		m.cursor = 0
		m.loading = true
		return fetchJobsCmd(m.client, m.activeFilter)

	case "4":
		m.activeFilter = "completed"
		m.cursor = 0
		m.loading = true
		return fetchJobsCmd(m.client, m.activeFilter)

	case "5":
		m.activeFilter = "failed"
		m.cursor = 0
		m.loading = true
		return fetchJobsCmd(m.client, m.activeFilter)

	case "tab":
		for i, f := range filterCycle {
			if f == m.activeFilter {
				m.activeFilter = filterCycle[(i+1)%len(filterCycle)]
				break
			}
		}
		m.cursor = 0
		m.loading = true
		return fetchJobsCmd(m.client, m.activeFilter)

	case "enter":
		if len(m.jobs) == 0 {
			break
		}
		if !m.tmuxEnabled {
			m.statusMsg = "to open a job, launch `boid tui` inside tmux"
			m.isError = false
			return clearStatusAfter(4 * time.Second)
		}
		job := m.jobs[m.cursor]
		return openJobCmd(job.ID, m.panes[job.ID])
	}
	return nil
}

func (m *App) View() string {
	if m.width == 0 {
		return ""
	}

	var sb strings.Builder

	// --- header ---
	badge := styleBadge.Render("[tmux]")
	if !m.tmuxEnabled {
		badge = styleWarn.Render("[no-tmux]")
	}
	title := styleHeader.Render("boid") + styleDim.Render(" ─ active jobs")
	header := lipgloss.JoinHorizontal(lipgloss.Top,
		title,
		strings.Repeat(" ", max(0, m.width-lipgloss.Width(title)-lipgloss.Width(badge))),
		badge,
	)
	sb.WriteString(header)
	sb.WriteByte('\n')
	sb.WriteString(strings.Repeat("─", m.width))
	sb.WriteByte('\n')

	// --- filter bar ---
	sb.WriteString(buildFilterBar(m.activeFilter))
	sb.WriteByte('\n')

	// --- body ---
	bodyHeight := m.height - 6 // header(2) + separator(1) + filterbar(1) + footer(2)
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	if m.fetchErr != nil {
		sb.WriteString(styleError.Render(fmt.Sprintf("error: %v", m.fetchErr)))
		sb.WriteByte('\n')
	} else if len(m.jobs) == 0 && !m.loading {
		sb.WriteString(styleDim.Render("  no active jobs"))
		sb.WriteByte('\n')
	} else {
		visible := m.jobs
		if len(visible) > bodyHeight {
			visible = visible[:bodyHeight]
		}
		for i, job := range visible {
			line := renderJobLine(job, i == m.cursor, m.width)
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}

	// --- status / no-tmux message ---
	if m.statusMsg != "" {
		var msg string
		if m.isError {
			msg = styleError.Render("  ! " + m.statusMsg)
		} else {
			msg = styleWarn.Render("  ! " + m.statusMsg)
		}
		sb.WriteByte('\n')
		sb.WriteString(msg)
		sb.WriteByte('\n')
	}

	// --- footer ---
	sb.WriteString(strings.Repeat("─", m.width))
	sb.WriteByte('\n')
	footer := buildFooter(m.tmuxEnabled)
	sb.WriteString(styleFooter.Render(footer))

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
