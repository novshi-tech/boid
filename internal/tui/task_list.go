package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

const taskPollInterval = 3 * time.Second

// Status filter names and their cycle order.
var taskFilterCycle = []string{"active", "pending", "done", "aborted", "all"}

// activeStatuses defines which task statuses are considered "active".
var activeStatuses = map[orchestrator.TaskStatus]bool{
	orchestrator.TaskStatusExecuting:          true,
	orchestrator.TaskStatusReworking:          true,
	orchestrator.TaskStatusVerifying:          true,
	orchestrator.TaskStatusInReview:           true,
	orchestrator.TaskStatusCollectingFeedback: true,
}

// --- messages ---

type taskTickMsg struct{}
type tasksMsg struct {
	tasks []*orchestrator.Task
	err   error
}
type projectsMsg struct {
	projects []*orchestrator.Project
	err      error
}

// --- TaskListScreen ---

type TaskListScreen struct {
	shared *SharedState

	tasks        []*orchestrator.Task
	projects     []*orchestrator.Project
	cursor       int
	statusFilter string // active, pending, done, aborted, all
	projectIdx   int    // 0=all, 1..N=project index
	loading      bool
	fetchErr     error
}

func NewTaskListScreen(shared *SharedState) *TaskListScreen {
	return &TaskListScreen{
		shared:       shared,
		statusFilter: "active",
		loading:      true,
	}
}

func (s *TaskListScreen) Init() tea.Cmd {
	return tea.Batch(
		fetchTasksCmd(s.shared.Client, s.statusFilter, s.selectedProjectID()),
		fetchProjectsCmd(s.shared.Client),
		taskTickCmd(),
	)
}

func (s *TaskListScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case taskTickMsg:
		return s, tea.Batch(
			fetchTasksCmd(s.shared.Client, s.statusFilter, s.selectedProjectID()),
			taskTickCmd(),
		)

	case tasksMsg:
		s.loading = false
		if msg.err != nil {
			s.fetchErr = msg.err
		} else {
			s.fetchErr = nil
			s.tasks = msg.tasks
			if s.cursor >= len(s.tasks) && len(s.tasks) > 0 {
				s.cursor = len(s.tasks) - 1
			}
			if len(s.tasks) == 0 {
				s.cursor = 0
			}
		}

	case projectsMsg:
		if msg.err == nil {
			s.projects = msg.projects
		}

	case tea.KeyMsg:
		return s, s.handleKey(msg)
	}

	return s, nil
}

func (s *TaskListScreen) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "j", "down":
		if s.cursor < len(s.tasks)-1 {
			s.cursor++
		}

	case "k", "up":
		if s.cursor > 0 {
			s.cursor--
		}

	case "tab":
		for i, f := range taskFilterCycle {
			if f == s.statusFilter {
				s.statusFilter = taskFilterCycle[(i+1)%len(taskFilterCycle)]
				break
			}
		}
		s.cursor = 0
		s.loading = true
		return fetchTasksCmd(s.shared.Client, s.statusFilter, s.selectedProjectID())

	case "shift+tab":
		for i, f := range taskFilterCycle {
			if f == s.statusFilter {
				s.statusFilter = taskFilterCycle[(i-1+len(taskFilterCycle))%len(taskFilterCycle)]
				break
			}
		}
		s.cursor = 0
		s.loading = true
		return fetchTasksCmd(s.shared.Client, s.statusFilter, s.selectedProjectID())

	case "p":
		total := len(s.projects) + 1 // +1 for "all"
		s.projectIdx = (s.projectIdx + 1) % total
		s.cursor = 0
		s.loading = true
		return fetchTasksCmd(s.shared.Client, s.statusFilter, s.selectedProjectID())

	case "r":
		s.loading = true
		return fetchTasksCmd(s.shared.Client, s.statusFilter, s.selectedProjectID())

	case "enter":
		// Task detail screen (placeholder - to be implemented in a later task)

	case "o":
		// Quick open (placeholder - to be implemented in a later task)

	case "n":
		// New task form (placeholder - to be implemented in a later task)
	}
	return nil
}

func (s *TaskListScreen) selectedProjectID() string {
	if s.projectIdx == 0 || s.projectIdx > len(s.projects) {
		return ""
	}
	return s.projects[s.projectIdx-1].ID
}

func (s *TaskListScreen) selectedProjectName() string {
	if s.projectIdx == 0 || s.projectIdx > len(s.projects) {
		return "all"
	}
	return s.projects[s.projectIdx-1].Meta.Name
}

func (s *TaskListScreen) View() string {
	var sb strings.Builder

	// --- filter bar ---
	sb.WriteString(s.buildTaskFilterBar())
	sb.WriteByte('\n')

	// --- separator ---
	sb.WriteString(strings.Repeat("─", s.shared.Width))
	sb.WriteByte('\n')

	// --- body ---
	bodyHeight := s.shared.Height - 6 // header(2) + filterbar(1) + sep(1) + footer(2)
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	if s.fetchErr != nil {
		sb.WriteString(styleError.Render(fmt.Sprintf("error: %v", s.fetchErr)))
		sb.WriteByte('\n')
	} else if len(s.tasks) == 0 && !s.loading {
		sb.WriteString(styleDim.Render("  no tasks"))
		sb.WriteByte('\n')
	} else {
		visible := s.tasks
		if len(visible) > bodyHeight {
			visible = visible[:bodyHeight]
		}
		for i, task := range visible {
			line := renderTaskLine(task, i == s.cursor, s.shared.Width, s.findProjectName(task.ProjectID))
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}

	// --- separator ---
	sb.WriteString(strings.Repeat("─", s.shared.Width))
	sb.WriteByte('\n')

	// --- footer ---
	sb.WriteString(styleFooter.Render(" enter: detail  o: open job  n: new  tab: filter  p: project  r: refresh  q: quit"))

	return sb.String()
}

func (s *TaskListScreen) buildTaskFilterBar() string {
	var parts []string
	for _, f := range taskFilterCycle {
		label := f
		if f == s.statusFilter {
			parts = append(parts, styleFilterActive.Render("["+label+"]"))
		} else {
			parts = append(parts, styleFilterInactive.Render(" "+label+" "))
		}
	}

	projLabel := styleDim.Render("project: " + s.selectedProjectName())
	filterStr := strings.Join(parts, "  ")

	gap := s.shared.Width - lipglossWidth(filterStr) - lipglossWidth(projLabel)
	if gap < 2 {
		gap = 2
	}
	return filterStr + strings.Repeat(" ", gap) + projLabel
}

func (s *TaskListScreen) findProjectName(projectID string) string {
	for _, p := range s.projects {
		if p.ID == projectID {
			return p.Meta.Name
		}
	}
	return ""
}

// --- rendering ---

func renderTaskLine(task *orchestrator.Task, selected bool, width int, projectName string) string {
	cursor := "  "
	if selected {
		cursor = styleCursor.Render("▸ ")
	}

	dot, statusText := taskStatusDisplay(task.Status)

	title := task.Title
	if title == "" {
		title = "(no title)"
	}

	proj := ""
	if projectName != "" {
		proj = styleDim.Render("[" + truncate(projectName, 10) + "]")
	}

	behavior := styleDim.Render(task.Behavior)
	elapsed := styleDim.Render(formatTaskElapsed(task.CreatedAt))

	line := fmt.Sprintf("%s%s %s  %-24s  %-14s  %-6s  %s",
		cursor, dot, statusText, truncate(title, 24), proj, behavior, elapsed)

	_ = width
	return line
}

func taskStatusDisplay(status orchestrator.TaskStatus) (dot, text string) {
	switch status {
	case orchestrator.TaskStatusExecuting:
		return styleExecuting.Render("●"), styleExecuting.Render("executing")
	case orchestrator.TaskStatusReworking:
		return styleExecuting.Render("●"), styleExecuting.Render("reworking")
	case orchestrator.TaskStatusVerifying:
		return styleVerifying.Render("●"), styleVerifying.Render("verifying")
	case orchestrator.TaskStatusInReview:
		return styleVerifying.Render("●"), styleVerifying.Render("in_review")
	case orchestrator.TaskStatusCollectingFeedback:
		return styleExecuting.Render("●"), styleExecuting.Render("feedback")
	case orchestrator.TaskStatusPending:
		return styleTaskDim.Render("○"), styleTaskDim.Render("pending")
	case orchestrator.TaskStatusDone:
		return styleTaskDim.Render("✓"), styleTaskDim.Render("done")
	case orchestrator.TaskStatusAborted:
		return styleAborted.Render("✗"), styleAborted.Render("aborted")
	default:
		return styleDim.Render("?"), styleDim.Render(string(status))
	}
}

func formatTaskElapsed(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func lipglossWidth(s string) int {
	// Count visible characters (strip ANSI escape codes)
	n := 0
	inEsc := false
	for _, r := range s {
		if r == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		n++
	}
	return n
}

// --- commands ---

func taskTickCmd() tea.Cmd {
	return tea.Tick(taskPollInterval, func(time.Time) tea.Msg {
		return taskTickMsg{}
	})
}

func fetchTasksCmd(c *client.Client, statusFilter, projectID string) tea.Cmd {
	return func() tea.Msg {
		filter := client.TaskListFilter{
			ProjectID: projectID,
		}

		// "active" filter requires fetching all and filtering client-side,
		// because the API only supports single status values.
		switch statusFilter {
		case "active":
			// No status filter - fetch all, filter client-side
		case "all":
			// No status filter - fetch all
		case "pending":
			filter.Status = string(orchestrator.TaskStatusPending)
		case "done":
			filter.Status = string(orchestrator.TaskStatusDone)
		case "aborted":
			filter.Status = string(orchestrator.TaskStatusAborted)
		}

		tasks, err := c.ListTasks(filter)
		if err != nil {
			return tasksMsg{err: err}
		}

		// Client-side filter for "active" statuses
		if statusFilter == "active" {
			var filtered []*orchestrator.Task
			for _, t := range tasks {
				if activeStatuses[t.Status] {
					filtered = append(filtered, t)
				}
			}
			tasks = filtered
		}

		return tasksMsg{tasks: tasks}
	}
}

func fetchProjectsCmd(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		projects, err := c.ListProjects()
		return projectsMsg{projects: projects, err: err}
	}
}
