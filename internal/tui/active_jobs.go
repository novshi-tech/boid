package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
)

// ActiveJobsScreen displays the list of active (running, interactive) jobs.
type ActiveJobsScreen struct {
	shared *SharedState

	jobs      []api.JobWithContext
	cursor    int
	statusMsg string
	isError   bool
	loading   bool
	fetchErr  error
}

// NewActiveJobsScreen creates a new active jobs screen.
func NewActiveJobsScreen(shared *SharedState) *ActiveJobsScreen {
	return &ActiveJobsScreen{
		shared:  shared,
		loading: true,
	}
}

func (s *ActiveJobsScreen) Init() tea.Cmd {
	return tea.Batch(fetchJobsCmd(s.shared.Client), tickCmd())
}

func (s *ActiveJobsScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		return s, tea.Batch(fetchJobsCmd(s.shared.Client), tickCmd())

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
		return fetchJobsCmd(s.shared.Client)
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

	if s.fetchErr != nil {
		sb.WriteString(styleError.Render(fmt.Sprintf("error: %v", s.fetchErr)))
		sb.WriteByte('\n')
	} else if len(s.jobs) == 0 && !s.loading {
		sb.WriteString(styleDim.Render("  no active jobs"))
		sb.WriteByte('\n')
	} else {
		visible := s.jobs
		if len(visible) > height {
			visible = visible[:height]
		}
		for i, job := range visible {
			line := renderJobLine(job, i == s.cursor, width)
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}

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
	if s.shared.TmuxEnabled {
		return " enter: open   j/k: move   r: refresh   q: quit"
	}
	return " j/k: move   r: refresh   q: quit"
}

// --- commands ---

func tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg {
		return tickMsg{}
	})
}

func fetchJobsCmd(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		interactive := true
		jobs, err := c.ListJobs(api.JobListFilter{
			Status:      "running",
			Interactive: &interactive,
		})
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

// --- helpers ---

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

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func renderJobLine(job api.JobWithContext, selected bool, width int) string {
	cursor := "  "
	if selected {
		cursor = styleCursor.Render("▸ ")
	}

	dot := styleRunning.Render("●")
	id := styleTitle.Render(shortID(job.ID))

	title := job.TaskTitle
	if title == "" {
		title = "(no title)"
	}
	proj := ""
	if job.ProjectName != "" {
		proj = styleDim.Render("[" + job.ProjectName + "]")
	}
	elapsed := styleDim.Render(formatElapsed(job.CreatedAt))

	mid := fmt.Sprintf(" %s  %-8s  %-24s  %-12s  %s",
		dot, id, truncate(title, 24), proj, elapsed)

	line := cursor + mid

	_ = width
	return line
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s + strings.Repeat(" ", maxLen-len(runes))
	}
	return string(runes[:maxLen-1]) + "…"
}
