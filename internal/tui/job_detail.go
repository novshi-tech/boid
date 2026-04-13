package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
)

// JobDetailScreen shows full details and scrollable output for a single job.
type JobDetailScreen struct {
	shared       *SharedState
	job          *api.Job
	outputScroll int
	statusMsg    string
	isError      bool
}

// NewJobDetailScreen creates a new JobDetailScreen for the given job.
func NewJobDetailScreen(shared *SharedState, job *api.Job) *JobDetailScreen {
	return &JobDetailScreen{
		shared: shared,
		job:    job,
	}
}

func (s *JobDetailScreen) Init() tea.Cmd {
	return nil
}

func (s *JobDetailScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return s, s.handleKey(msg)
	case clearStatusMsg:
		s.statusMsg = ""
		s.isError = false
	case openResultMsg:
		if msg.err != nil {
			s.statusMsg = "open failed: " + msg.err.Error()
			s.isError = true
			return s, clearStatusAfter(3 * time.Second)
		}
		if msg.paneID != "" {
			s.shared.Panes[msg.jobID] = msg.paneID
		}
	}
	return s, nil
}

func (s *JobDetailScreen) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "j", "down":
		lines := strings.Split(s.job.Output, "\n")
		if s.outputScroll < len(lines)-1 {
			s.outputScroll++
		}
	case "k", "up":
		if s.outputScroll > 0 {
			s.outputScroll--
		}
	case "o", "enter":
		if !s.shared.TmuxEnabled {
			s.statusMsg = "to open a job, launch `boid tui` inside tmux"
			s.isError = false
			return clearStatusAfter(4 * time.Second)
		}
		return openJobCmd(s.job.ID, s.shared.Panes[s.job.ID])
	case "esc", "backspace", "q":
		return func() tea.Msg { return popScreenMsg{} }
	}
	return nil
}

func (s *JobDetailScreen) View(width, height int) string {
	var sb strings.Builder
	job := s.job

	// ─── Job Detail ───────────────────────────────────────────────
	sb.WriteString(renderSectionHeader("Job Detail", width))
	sb.WriteByte('\n')

	metaLines := []string{
		fmt.Sprintf("  ID:          %s", job.ID),
		fmt.Sprintf("  Role:        %s", job.Role),
	}
	exitStr := ""
	if job.Status != api.JobStatusRunning {
		exitStr = fmt.Sprintf("  ExitCode: %d", job.ExitCode)
	}
	metaLines = append(metaLines,
		fmt.Sprintf("  Status:      %s%s", job.Status, exitStr),
		fmt.Sprintf("  Created:     %s", job.CreatedAt.Format(time.RFC3339)),
		fmt.Sprintf("  Updated:     %s", job.UpdatedAt.Format(time.RFC3339)),
	)
	if job.RuntimeID != "" {
		metaLines = append(metaLines, fmt.Sprintf("  RuntimeID:   %s", job.RuntimeID))
	}
	if job.WorkspacePath != "" {
		metaLines = append(metaLines, fmt.Sprintf("  Workspace:   %s", job.WorkspacePath))
	}
	metaLines = append(metaLines,
		fmt.Sprintf("  Interactive: %v  TTY: %v", job.Interactive, job.TTY),
	)

	for _, l := range metaLines {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}

	// ─── Output ───────────────────────────────────────────────────
	sb.WriteString(renderSectionHeader("Output", width))
	sb.WriteByte('\n')

	// Fixed lines used: 1 (Job Detail header) + len(metaLines) + 1 (Output header)
	// Plus status message lines if present.
	statusLines := 0
	if s.statusMsg != "" {
		statusLines = 2
	}
	fixedLines := 1 + len(metaLines) + 1
	outputHeight := max(height-fixedLines-statusLines, 2)

	if job.Output == "" {
		sb.WriteString(styleDim.Render("  (no output)"))
		sb.WriteByte('\n')
	} else {
		outputLines := strings.Split(job.Output, "\n")
		start := s.outputScroll
		if start >= len(outputLines) {
			start = max(len(outputLines)-1, 0)
		}
		end := min(start+outputHeight, len(outputLines))
		for _, l := range outputLines[start:end] {
			sb.WriteString(l)
			sb.WriteByte('\n')
		}
		if end < len(outputLines) {
			sb.WriteString(styleDim.Render(fmt.Sprintf("  ... %d more lines", len(outputLines)-end)))
			sb.WriteByte('\n')
		}
	}

	// Inline status message
	if s.statusMsg != "" {
		var line string
		if s.isError {
			line = styleError.Render("  ! " + s.statusMsg)
		} else {
			line = styleWarn.Render("  ! " + s.statusMsg)
		}
		sb.WriteByte('\n')
		sb.WriteString(line)
		sb.WriteByte('\n')
	}

	return sb.String()
}

func (s *JobDetailScreen) ShortHelp() string {
	return "j/k: scroll output  o/enter: open in pane  esc/q: back"
}
