package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
)

type gateReplayResultMsg struct {
	result *api.ReplayGateResult
	err    error
}

type replayConfirmDeadlineMsg struct{}

// JobDetailScreen shows full details and scrollable output for a single job.
type JobDetailScreen struct {
	shared        *SharedState
	job           *api.Job
	outputScroll  int
	statusMsg     string
	isError       bool
	replayPending bool
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
	case gateReplayResultMsg:
		s.replayPending = false
		if msg.err != nil {
			s.statusMsg = "replay failed: " + msg.err.Error()
			s.isError = true
			return s, clearStatusAfter(4 * time.Second)
		}
		return s, func() tea.Msg { return popScreenMsg{} }
	case replayConfirmDeadlineMsg:
		if s.replayPending {
			s.replayPending = false
			s.statusMsg = ""
			s.isError = false
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
	case "R":
		job := s.job
		if job.Role != "gate" && job.Role != "entry_gate" && job.Role != "exit_gate" {
			s.statusMsg = "replay is only for gate jobs"
			s.isError = false
			return clearStatusAfter(3 * time.Second)
		}
		if job.ExecutionState == "" {
			s.statusMsg = "replay unavailable: legacy job has no execution_state"
			s.isError = false
			return clearStatusAfter(3 * time.Second)
		}
		if s.replayPending {
			s.replayPending = false
			return replayGateCmd(s.shared.Client, job.TaskID, job.HandlerID, job.ExecutionState)
		}
		s.replayPending = true
		s.statusMsg = "Press R again to replay"
		s.isError = false
		return tea.Tick(3*time.Second, func(time.Time) tea.Msg {
			return replayConfirmDeadlineMsg{}
		})
	case "esc", "backspace":
		return func() tea.Msg { return popScreenMsg{} }
	case "q":
		return tea.Quit
	}
	return nil
}

func replayGateCmd(c *client.Client, taskID, gateID, status string) tea.Cmd {
	return func() tea.Msg {
		res, err := c.ReplayGate(taskID, gateID, status)
		return gateReplayResultMsg{result: res, err: err}
	}
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
		fmt.Sprintf("  Interactive: %v", job.Interactive),
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
	return "j/k: scroll output  o/enter: open in pane  R: replay (gate only)  esc: back  q: quit"
}
