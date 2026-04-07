package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
)

const taskDetailPollInterval = 3 * time.Second

// --- messages ---

type taskDetailTickMsg struct{}
type taskDetailMsg struct {
	detail *api.TaskDetailView
	err    error
}
type applyActionResultMsg struct{ err error }
type abortConfirmDeadlineMsg struct{}
type deleteResultMsg struct{ err error }
type deleteConfirmDeadlineMsg struct{}

// --- TaskDetailScreen ---

type TaskDetailScreen struct {
	shared      *SharedState
	taskID      string
	projectName string

	detail        *api.TaskDetailView
	cursor        int
	descScroll    int
	statusMsg     string
	isError       bool
	loading       bool
	fetchErr      error
	abortPending  bool
	deletePending bool
}

func NewTaskDetailScreen(shared *SharedState, taskID, projectName string) *TaskDetailScreen {
	return &TaskDetailScreen{
		shared:      shared,
		taskID:      taskID,
		projectName: projectName,
		loading:     true,
	}
}

func (s *TaskDetailScreen) Init() tea.Cmd {
	return tea.Batch(
		fetchTaskDetailCmd(s.shared.Client, s.taskID),
		taskDetailTickCmd(),
	)
}

func (s *TaskDetailScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case taskDetailTickMsg:
		return s, tea.Batch(
			fetchTaskDetailCmd(s.shared.Client, s.taskID),
			taskDetailTickCmd(),
		)

	case taskDetailMsg:
		s.loading = false
		if msg.err != nil {
			s.fetchErr = msg.err
		} else {
			s.fetchErr = nil
			s.detail = msg.detail
			if s.detail != nil && s.cursor >= len(s.detail.Jobs) && len(s.detail.Jobs) > 0 {
				s.cursor = len(s.detail.Jobs) - 1
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

	case applyActionResultMsg:
		if msg.err != nil {
			s.statusMsg = "action failed: " + msg.err.Error()
			s.isError = true
			return s, clearStatusAfter(4 * time.Second)
		}
		s.abortPending = false
		s.statusMsg = ""
		return s, fetchTaskDetailCmd(s.shared.Client, s.taskID)

	case abortConfirmDeadlineMsg:
		if s.abortPending {
			s.abortPending = false
			s.statusMsg = ""
			s.isError = false
		}

	case deleteResultMsg:
		if msg.err != nil {
			s.statusMsg = "delete failed: " + msg.err.Error()
			s.isError = true
			return s, clearStatusAfter(4 * time.Second)
		}
		return s, func() tea.Msg { return popScreenMsg{} }

	case deleteConfirmDeadlineMsg:
		if s.deletePending {
			s.deletePending = false
			s.statusMsg = ""
			s.isError = false
		}

	case tea.KeyMsg:
		return s, s.handleKey(msg)
	}

	return s, nil
}

func (s *TaskDetailScreen) handleKey(msg tea.KeyMsg) tea.Cmd {
	jobCount := 0
	if s.detail != nil {
		jobCount = len(s.detail.Jobs)
	}

	switch msg.String() {
	case "j", "down":
		if s.cursor < jobCount-1 {
			s.cursor++
		}

	case "k", "up":
		if s.cursor > 0 {
			s.cursor--
		}

	case "r":
		s.loading = true
		return fetchTaskDetailCmd(s.shared.Client, s.taskID)

	case "enter":
		if s.detail == nil || len(s.detail.Jobs) == 0 {
			break
		}
		if !s.shared.TmuxEnabled {
			s.statusMsg = "to open a job, launch `boid tui` inside tmux"
			s.isError = false
			return clearStatusAfter(4 * time.Second)
		}
		job := s.detail.Jobs[s.cursor]
		return openJobCmd(job.ID, s.shared.Panes[job.ID])

	case "esc", "backspace":
		return func() tea.Msg { return popScreenMsg{} }

	case "d":
		if s.deletePending {
			s.deletePending = false
			return deleteTaskCmd(s.shared.Client, s.taskID)
		}
		s.deletePending = true
		s.statusMsg = "Press d again to delete"
		s.isError = false
		return tea.Tick(3*time.Second, func(time.Time) tea.Msg {
			return deleteConfirmDeadlineMsg{}
		})

	default:
		if len(msg.Runes) != 1 {
			break
		}
		ch := msg.Runes[0]
		km := assignKeys(s.availableActions())
		action, ok := km[ch]
		if !ok {
			break
		}
		if action == "abort" {
			if s.abortPending {
				s.abortPending = false
				s.statusMsg = "aborting..."
				s.isError = false
				return applyActionCmd(s.shared.Client, s.taskID, "abort")
			}
			s.abortPending = true
			s.statusMsg = "Press " + string(ch) + " again to abort"
			s.isError = false
			return tea.Tick(3*time.Second, func(time.Time) tea.Msg {
				return abortConfirmDeadlineMsg{}
			})
		}
		s.statusMsg = actionLoadingMsg(action)
		s.isError = false
		return applyActionCmd(s.shared.Client, s.taskID, action)
	}
	return nil
}

func (s *TaskDetailScreen) View(width, height int) string {
	var sb strings.Builder

	// --- sub-header: title + status (1 line) ---
	if s.detail != nil && s.detail.Task != nil {
		task := s.detail.Task
		titleStr := styleTitle.Render(truncate(task.Title, 50))
		_, statusText := taskStatusDisplay(task.Status)
		gap := max(width-lipglossWidth(titleStr)-lipglossWidth(statusText), 1)
		sb.WriteString(titleStr)
		sb.WriteString(strings.Repeat(" ", gap))
		sb.WriteString(statusText)
		sb.WriteByte('\n')

		// --- sub-header line 2: project / behavior / age ---
		projStr := styleDim.Render("project: " + s.projectName)
		behaviorStr := styleDim.Render("behavior: " + task.Behavior)
		ageStr := styleDim.Render("age: " + formatTaskElapsed(task.CreatedAt))
		sb.WriteString(projStr + "  " + behaviorStr + "  " + ageStr)
		sb.WriteByte('\n')
	} else {
		sb.WriteString(styleDim.Render("Loading..."))
		sb.WriteByte('\n')
		sb.WriteByte('\n')
	}

	// --- separator ---
	sb.WriteString(strings.Repeat("─", width))
	sb.WriteByte('\n')

	// Compute height budget: 2 sub-header + 1 sep already used
	remaining := max(height-3, 4)

	// Jobs section: "Jobs:" label + up to half the remaining for job rows
	// Description section: "Description:" label + separator + rest
	overhead := 3 // jobLabel + sep + descLabel

	jobRowsMax := max((remaining-overhead)/2, 1)
	descHeight := max(remaining-overhead-jobRowsMax, 1)

	// --- jobs section ---
	sb.WriteString(styleDim.Render("Jobs:"))
	sb.WriteByte('\n')

	if s.fetchErr != nil {
		sb.WriteString(styleError.Render(fmt.Sprintf("error: %v", s.fetchErr)))
		sb.WriteByte('\n')
	} else if s.detail == nil || len(s.detail.Jobs) == 0 {
		if !s.loading {
			sb.WriteString(styleDim.Render("  no jobs"))
			sb.WriteByte('\n')
		}
	} else {
		jobs := s.detail.Jobs
		jobScroll := 0
		if s.cursor >= jobRowsMax {
			jobScroll = s.cursor - jobRowsMax + 1
		}
		end := min(jobScroll+jobRowsMax, len(jobs))
		for i := jobScroll; i < end; i++ {
			sb.WriteString(renderDetailJobLine(jobs[i], i == s.cursor, width))
			sb.WriteByte('\n')
		}
	}

	// --- separator ---
	sb.WriteString(strings.Repeat("─", width))
	sb.WriteByte('\n')

	// --- description section ---
	sb.WriteString(styleDim.Render("Description:"))
	sb.WriteByte('\n')

	if s.detail != nil && s.detail.Task != nil && s.detail.Task.Description != "" {
		lines := strings.Split(s.detail.Task.Description, "\n")
		start := s.descScroll
		if start >= len(lines) {
			start = 0
		}
		end := min(start+descHeight, len(lines))
		for _, line := range lines[start:end] {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}

	// --- inline status message ---
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

func (s *TaskDetailScreen) availableActions() []string {
	if s.detail == nil {
		return nil
	}
	return s.detail.AvailableActions
}

func (s *TaskDetailScreen) ShortHelp() string {
	km := assignKeys(s.availableActions())
	// Reverse map: action → key (for ordered output)
	rev := map[string]rune{}
	for ch, action := range km {
		rev[action] = ch
	}
	var parts []string
	for _, action := range s.availableActions() {
		if ch, ok := rev[action]; ok {
			parts = append(parts, string(ch)+": "+action)
		}
	}
	parts = append(parts, "d: delete")
	fixed := "j/k: move  enter: open job  r: refresh  esc: back"
	return strings.Join(parts, "  ") + "  " + fixed
}

// assignKeys assigns a single-character key to each action name.
// The first unused character of the action name is used as the key.
// Key 'd' is reserved for the delete shortcut and cannot be assigned to actions.
func assignKeys(actions []string) map[rune]string {
	m := map[rune]string{}
	for _, a := range actions {
		for _, ch := range a {
			if ch == 'd' { // reserved for delete
				continue
			}
			if _, used := m[ch]; !used {
				m[ch] = a
				break
			}
		}
	}
	return m
}

// --- job line rendering ---

func renderDetailJobLine(job *api.Job, selected bool, width int) string {
	cursor := "  "
	if selected {
		cursor = styleCursor.Render("▸ ")
	}

	var dot string
	switch job.Status {
	case api.JobStatusRunning:
		dot = styleRunning.Render("●")
	case api.JobStatusCompleted:
		dot = styleCompleted.Render("✓")
	case api.JobStatusFailed:
		dot = styleFailed.Render("✗")
	default:
		dot = stylePending.Render("○")
	}

	statusStr := fmt.Sprintf("%-9s", string(job.Status))
	idStr := styleDim.Render(shortID(job.ID))

	roleStr := ""
	if job.Role != "" {
		roleStr = styleDim.Render("[" + job.Role + "]")
	}

	var elapsed string
	if job.Status == api.JobStatusCompleted || job.Status == api.JobStatusFailed {
		elapsed = styleDim.Render("done")
	} else {
		elapsed = styleDim.Render(formatElapsed(job.CreatedAt))
	}

	_ = width
	return fmt.Sprintf("%s%s %-9s  %-8s  %-18s  %s",
		cursor, dot, statusStr, idStr, roleStr, elapsed)
}

// --- commands ---

func taskDetailTickCmd() tea.Cmd {
	return tea.Tick(taskDetailPollInterval, func(time.Time) tea.Msg {
		return taskDetailTickMsg{}
	})
}

func fetchTaskDetailCmd(c *client.Client, taskID string) tea.Cmd {
	return func() tea.Msg {
		detail, err := c.GetTaskDetail(taskID)
		return taskDetailMsg{detail: detail, err: err}
	}
}

func applyActionCmd(c *client.Client, taskID, actionType string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.ApplyAction(taskID, api.ApplyActionRequest{Type: actionType})
		return applyActionResultMsg{err: err}
	}
}

func deleteTaskCmd(c *client.Client, taskID string) tea.Cmd {
	return func() tea.Msg {
		err := c.DeleteTask(taskID)
		return deleteResultMsg{err: err}
	}
}

func actionLoadingMsg(action string) string {
	switch action {
	case "start":
		return "starting..."
	default:
		return action + "..."
	}
}
