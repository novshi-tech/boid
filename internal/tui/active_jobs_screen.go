package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
)

const pollInterval = 2 * time.Second

var filterCycle = []string{"all", "running", "pending", "completed", "failed"}

// --- screen-local messages ---

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

// --- ActiveJobsScreen ---

// ActiveJobsScreen displays the active jobs list.
type ActiveJobsScreen struct {
	shared       *SharedState
	jobs         []api.JobWithContext
	cursor       int
	statusMsg    string
	isError      bool
	loading      bool
	fetchErr     error
	activeFilter string
}

// NewActiveJobsScreen creates a new ActiveJobsScreen.
func NewActiveJobsScreen(shared *SharedState) *ActiveJobsScreen {
	return &ActiveJobsScreen{
		shared:       shared,
		loading:      true,
		activeFilter: "running",
	}
}

func (s *ActiveJobsScreen) Init() tea.Cmd {
	return tea.Batch(fetchJobsCmd(s.shared.Client, s.activeFilter), tickCmd())
}

func (s *ActiveJobsScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		return s, tea.Batch(fetchJobsCmd(s.shared.Client, s.activeFilter), tickCmd())

	case jobsMsg:
		s.loading = false
		if msg.err != nil {
			s.fetchErr = msg.err
		} else {
			s.fetchErr = nil
			s.jobs = msg.jobs
			if s.cursor >= len(s.jobs) && len(s.jobs) > 0 {
				s.cursor = len(s.jobs) - 1
			}
		}

	case openResultMsg:
		if msg.err != nil {
			s.statusMsg = "open failed: " + msg.err.Error()
			s.isError = true
			return s, clearStatusAfter(3 * time.Second)
		}
		if msg.paneID != "" {
			s.shared.Panes[msg.jobID] = msg.paneID
		}

	case clearStatusMsg:
		s.statusMsg = ""
		s.isError = false

	case tea.KeyMsg:
		return s, s.handleKey(msg)
	}

	return s, nil
}

func (s *ActiveJobsScreen) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "j", "down":
		if s.cursor < len(s.jobs)-1 {
			s.cursor++
		}

	case "k", "up":
		if s.cursor > 0 {
			s.cursor--
		}

	case "r":
		s.loading = true
		return fetchJobsCmd(s.shared.Client, s.activeFilter)

	case "1":
		s.activeFilter = "all"
		s.cursor = 0
		s.loading = true
		return fetchJobsCmd(s.shared.Client, s.activeFilter)

	case "2":
		s.activeFilter = "running"
		s.cursor = 0
		s.loading = true
		return fetchJobsCmd(s.shared.Client, s.activeFilter)

	case "3":
		s.activeFilter = "pending"
		s.cursor = 0
		s.loading = true
		return fetchJobsCmd(s.shared.Client, s.activeFilter)

	case "4":
		s.activeFilter = "completed"
		s.cursor = 0
		s.loading = true
		return fetchJobsCmd(s.shared.Client, s.activeFilter)

	case "5":
		s.activeFilter = "failed"
		s.cursor = 0
		s.loading = true
		return fetchJobsCmd(s.shared.Client, s.activeFilter)

	case "tab":
		for i, f := range filterCycle {
			if f == s.activeFilter {
				s.activeFilter = filterCycle[(i+1)%len(filterCycle)]
				break
			}
		}
		s.cursor = 0
		s.loading = true
		return fetchJobsCmd(s.shared.Client, s.activeFilter)

	case "enter":
		if len(s.jobs) == 0 {
			break
		}
		if !s.shared.TmuxEnabled {
			s.statusMsg = "to open a job, launch `boid tui` inside tmux"
			s.isError = false
			return clearStatusAfter(4 * time.Second)
		}
		job := s.jobs[s.cursor]
		return openJobCmd(job.ID, s.shared.Panes[job.ID])
	}
	return nil
}

func (s *ActiveJobsScreen) View(width, height int) string {
	var sb strings.Builder

	// --- filter bar ---
	sb.WriteString(buildFilterBar(s.activeFilter))
	sb.WriteByte('\n')

	// --- body ---
	bodyHeight := height - 1 // filterbar(1)
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	if s.fetchErr != nil {
		sb.WriteString(styleError.Render(fmt.Sprintf("error: %v", s.fetchErr)))
		sb.WriteByte('\n')
	} else if len(s.jobs) == 0 && !s.loading {
		sb.WriteString(styleDim.Render("  no active jobs"))
		sb.WriteByte('\n')
	} else {
		visible := s.jobs
		if len(visible) > bodyHeight {
			visible = visible[:bodyHeight]
		}
		for i, job := range visible {
			line := renderJobLine(job, i == s.cursor, width)
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}

	// --- status ---
	if s.statusMsg != "" {
		var msg string
		if s.isError {
			msg = styleError.Render("  ! " + s.statusMsg)
		} else {
			msg = styleWarn.Render("  ! " + s.statusMsg)
		}
		sb.WriteByte('\n')
		sb.WriteString(msg)
		sb.WriteByte('\n')
	}

	return sb.String()
}

func (s *ActiveJobsScreen) ShortHelp() string {
	return buildFooter(s.shared.TmuxEnabled)
}

// --- helpers ---

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
