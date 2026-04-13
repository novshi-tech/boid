package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

const taskPollInterval = 3 * time.Second

// openStatuses defines which task statuses are considered "open".
var openStatuses = map[orchestrator.TaskStatus]bool{
	orchestrator.TaskStatusExecuting: true,
	orchestrator.TaskStatusReworking: true,
	orchestrator.TaskStatusVerifying: true,
	orchestrator.TaskStatusPending:   true,
}

// closedStatuses defines which task statuses are considered "closed".
var closedStatuses = map[orchestrator.TaskStatus]bool{
	orchestrator.TaskStatusDone:    true,
	orchestrator.TaskStatusAborted: true,
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
type quickOpenResultMsg struct {
	taskID string
	jobs   []*api.Job
	err    error
}

// --- miniSelector ---

type miniSelector struct {
	jobs   []*api.Job
	cursor int
	active bool
}

// --- popupSelector ---

// popupSelector is a small modal for selecting a value from a list.
// labels[0] is always "(all)" with values[0] = "".
type popupSelector struct {
	active bool
	kind   string   // "project" or "behavior"
	labels []string // display labels
	values []string // internal values ("" for all)
	cursor int
}

// --- TaskListScreen ---

type TaskListScreen struct {
	shared *SharedState

	table             table.Model
	tasks             []*orchestrator.Task
	projects          []*orchestrator.Project
	stateClosed       bool   // false=open, true=closed
	selectedProjectID string // "" = all
	behaviorFilter    string // "" = all
	popup             popupSelector
	loading           bool
	fetchErr          error
	statusMsg         string
	isError           bool
	mini              miniSelector
	titleWidth        int // current TITLE column width; default 24
}

func NewTaskListScreen(shared *SharedState) *TaskListScreen {
	cols := []table.Column{
		{Title: "STATUS", Width: 11},
		{Title: "TITLE", Width: 24},
		{Title: "PROJECT", Width: 12},
		{Title: "BEHAVIOR", Width: 10},
		{Title: "AGE", Width: 6},
	}
	t := table.New(
		table.WithColumns(cols),
		table.WithFocused(true),
		table.WithStyles(table.Styles{
			Header:   styleTableHeader,
			Cell:     styleTableCell,
			Selected: styleTableSelected,
		}),
	)
	return &TaskListScreen{
		shared:     shared,
		table:      t,
		loading:    true,
		titleWidth: 24,
	}
}

func (s *TaskListScreen) Init() tea.Cmd {
	return tea.Batch(
		fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter),
		fetchProjectsCmd(s.shared.Client),
		taskTickCmd(),
	)
}

func (s *TaskListScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case taskTickMsg:
		return s, tea.Batch(
			fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter),
			taskTickCmd(),
		)

	case tasksMsg:
		s.loading = false
		if msg.err != nil {
			s.fetchErr = msg.err
		} else {
			s.fetchErr = nil
			s.tasks = msg.tasks
			s.syncTableRows()
		}

	case projectsMsg:
		if msg.err == nil {
			s.projects = msg.projects
		}

	case quickOpenResultMsg:
		s.statusMsg = ""
		if msg.err != nil {
			s.statusMsg = "error: " + msg.err.Error()
			s.isError = true
			return s, clearStatusAfter(3 * time.Second)
		}
		switch len(msg.jobs) {
		case 0:
			s.statusMsg = "no active job"
			s.isError = false
			return s, clearStatusAfter(3 * time.Second)
		case 1:
			if !s.shared.TmuxEnabled {
				s.statusMsg = "to open a job, launch `boid tui` inside tmux"
				s.isError = false
				return s, clearStatusAfter(4 * time.Second)
			}
			return s, openJobCmd(msg.jobs[0].ID, s.shared.Panes[msg.jobs[0].ID])
		default:
			s.mini = miniSelector{jobs: msg.jobs, cursor: 0, active: true}
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

	case applyActionResultMsg:
		if msg.err != nil {
			s.statusMsg = "action failed: " + msg.err.Error()
			s.isError = true
			return s, clearStatusAfter(4 * time.Second)
		}
		s.statusMsg = ""
		return s, fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter)

	case screenResumedMsg:
		return s, fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter)

	case taskCreatedNotifyMsg:
		s.statusMsg = "task created"
		s.isError = false
		return s, tea.Batch(fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter), clearStatusAfter(3*time.Second))

	case clearStatusMsg:
		s.statusMsg = ""
		s.isError = false

	case tea.WindowSizeMsg:
		s.recalcColumns(msg.Width)
		s.table.SetWidth(msg.Width)
		bodyH := msg.Height - 2
		if bodyH < 1 {
			bodyH = 1
		}
		s.table.SetHeight(bodyH)
		s.syncTableRows()

	case tea.KeyMsg:
		return s, s.handleKey(msg)
	}

	return s, nil
}

func (s *TaskListScreen) handleKey(msg tea.KeyMsg) tea.Cmd {
	if s.popup.active {
		return s.handlePopupKey(msg)
	}
	if s.mini.active {
		return s.handleMiniKey(msg)
	}

	switch msg.String() {
	case "j", "down":
		if len(s.tasks) > 0 {
			s.table.MoveDown(1)
		}

	case "k", "up":
		if len(s.tasks) > 0 {
			s.table.MoveUp(1)
		}

	case "tab":
		s.stateClosed = !s.stateClosed
		s.table.SetCursor(0)
		s.loading = true
		return fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter)

	case "p":
		s.popup = s.buildProjectPopup()

	case "b":
		s.popup = s.buildBehaviorPopup()

	case "r":
		s.loading = true
		return fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter)

	case "enter":
		if len(s.tasks) == 0 {
			break
		}
		task := s.tasks[s.table.Cursor()]
		projectName := s.findProjectName(task.ProjectID)
		return PushScreen(NewTaskDetailScreen(s.shared, task.ID, projectName))

	case "s":
		if len(s.tasks) == 0 {
			break
		}
		task := s.tasks[s.table.Cursor()]
		if task.Status != orchestrator.TaskStatusPending {
			break
		}
		s.statusMsg = "starting..."
		s.isError = false
		return applyActionCmd(s.shared.Client, task.ID, "start")

	case "o":
		if len(s.tasks) == 0 {
			break
		}
		task := s.tasks[s.table.Cursor()]
		s.statusMsg = "loading..."
		s.isError = false
		return fetchQuickOpenCmd(s.shared.Client, task.ID)

	case "n":
		return PushScreen(NewTaskFormScreen(s.shared))

	case "q":
		return tea.Quit
	}
	return nil
}

// buildProjectPopup constructs a popupSelector for project selection.
func (s *TaskListScreen) buildProjectPopup() popupSelector {
	labels := []string{"(all)"}
	values := []string{""}
	cursor := 0
	for i, p := range s.projects {
		labels = append(labels, p.Meta.Name)
		values = append(values, p.ID)
		if p.ID == s.selectedProjectID {
			cursor = i + 1
		}
	}
	return popupSelector{
		active: true,
		kind:   "project",
		labels: labels,
		values: values,
		cursor: cursor,
	}
}

// buildBehaviorPopup constructs a popupSelector for behavior selection.
func (s *TaskListScreen) buildBehaviorPopup() popupSelector {
	behaviors := distinctBehaviors(s.tasks)
	labels := []string{"(all)"}
	values := []string{""}
	cursor := 0
	for i, b := range behaviors {
		labels = append(labels, b)
		values = append(values, b)
		if b == s.behaviorFilter {
			cursor = i + 1
		}
	}
	return popupSelector{
		active: true,
		kind:   "behavior",
		labels: labels,
		values: values,
		cursor: cursor,
	}
}

// handlePopupKey processes key events when the popup selector is active.
func (s *TaskListScreen) handlePopupKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "j", "down":
		if s.popup.cursor < len(s.popup.labels)-1 {
			s.popup.cursor++
		}
	case "k", "up":
		if s.popup.cursor > 0 {
			s.popup.cursor--
		}
	case "enter":
		val := s.popup.values[s.popup.cursor]
		kind := s.popup.kind
		s.popup.active = false
		switch kind {
		case "project":
			s.selectedProjectID = val
		case "behavior":
			s.behaviorFilter = val
		}
		s.table.SetCursor(0)
		s.loading = true
		return fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter)
	case "esc":
		s.popup.active = false
	}
	return nil
}

// distinctBehaviors returns a sorted slice of unique non-empty behavior values
// from the given tasks. Suitable for building the behavior selector options.
func distinctBehaviors(tasks []*orchestrator.Task) []string {
	seen := map[string]bool{}
	var behaviors []string
	for _, t := range tasks {
		if t.Behavior != "" && !seen[t.Behavior] {
			seen[t.Behavior] = true
			behaviors = append(behaviors, t.Behavior)
		}
	}
	sort.Strings(behaviors)
	return behaviors
}

// projectName returns the display name for the current project filter.
func (s *TaskListScreen) projectName() string {
	if s.selectedProjectID == "" {
		return "all"
	}
	name := s.findProjectName(s.selectedProjectID)
	if name == "" {
		return s.selectedProjectID
	}
	return name
}

func (s *TaskListScreen) View(width, height int) string {
	var sb strings.Builder

	// --- filter bar ---
	sb.WriteString(s.buildTaskFilterBar(width))
	sb.WriteByte('\n')

	// --- separator ---
	sb.WriteString(strings.Repeat("─", width))
	sb.WriteByte('\n')

	// --- popup overlay ---
	if s.popup.active {
		sb.WriteString(renderPopupSelector(s.popup, width))
		return sb.String()
	}

	// --- body ---
	bodyHeight := height - 2 // filterbar(1) + sep(1)
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
		s.recalcColumns(width)
		s.table.SetWidth(width)
		s.table.SetHeight(bodyHeight)
		sb.WriteString(s.table.View())
		sb.WriteByte('\n')
	}

	// --- mini selector (above footer) ---
	if s.mini.active {
		sb.WriteString(renderMiniSelector(s.mini, width))
		sb.WriteByte('\n')
	}

	// --- status message ---
	if s.statusMsg != "" {
		var line string
		if s.isError {
			line = styleError.Render("  ! " + s.statusMsg)
		} else {
			line = styleDim.Render("  " + s.statusMsg)
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}

	return sb.String()
}

func (s *TaskListScreen) ShortHelp() string {
	return "enter: detail  s: start  o: open job  n: new  tab: state  p: project  b: behavior  r: refresh  q: quit"
}

func (s *TaskListScreen) buildTaskFilterBar(width int) string {
	stateLabel := "open"
	if s.stateClosed {
		stateLabel = "closed"
	}

	projLabel := s.projectName()
	behLabel := s.behaviorFilter
	if behLabel == "" {
		behLabel = "all"
	}

	stateChip := styleFilterActive.Render("state: " + stateLabel)
	projChip := styleDim.Render("proj: " + projLabel)
	behChip := styleDim.Render("behavior: " + behLabel)

	_ = width
	return stateChip + "    " + projChip + "    " + behChip
}

// renderPopupSelector renders the popup selector list.
func renderPopupSelector(p popupSelector, width int) string {
	var sb strings.Builder
	title := "Select " + p.kind + ":"
	sb.WriteString("  " + styleTitle.Render(title) + "\n")
	for i, label := range p.labels {
		if i == p.cursor {
			sb.WriteString("    " + styleFilterActive.Render("▸ "+label) + "\n")
		} else {
			sb.WriteString("      " + styleDim.Render(label) + "\n")
		}
	}
	_ = width
	return sb.String()
}

func (s *TaskListScreen) findProjectName(projectID string) string {
	for _, p := range s.projects {
		if p.ID == projectID {
			return p.Meta.Name
		}
	}
	return ""
}

// recalcColumns recalculates column widths based on terminal width and updates
// the table. Fixed widths: STATUS(11), PROJECT(12), BEHAVIOR(10), AGE(6).
// The remainder (minus separator overhead) is assigned to TITLE with a minimum
// of 20. The calculated TITLE width is stored in s.titleWidth for use by
// syncTableRows.
func (s *TaskListScreen) recalcColumns(width int) {
	const (
		statusWidth   = 11
		projectWidth  = 12
		behaviorWidth = 10
		ageWidth      = 6
		minTitle      = 20
		numCols       = 5
	)
	fixedTotal := statusWidth + projectWidth + behaviorWidth + ageWidth
	separators := numCols + 1 // approximate separator overhead from bubbles/table
	titleWidth := width - fixedTotal - separators
	if titleWidth < minTitle {
		titleWidth = minTitle
	}
	s.titleWidth = titleWidth
	cols := []table.Column{
		{Title: "STATUS", Width: statusWidth},
		{Title: "TITLE", Width: titleWidth},
		{Title: "PROJECT", Width: projectWidth},
		{Title: "BEHAVIOR", Width: behaviorWidth},
		{Title: "AGE", Width: ageWidth},
	}
	s.table.SetColumns(cols)
}

// stripANSI removes ANSI escape sequences from s.
func stripANSI(s string) string {
	var b strings.Builder
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
		b.WriteRune(r)
	}
	return b.String()
}

// syncTableRows converts s.tasks to table rows and updates the table.
func (s *TaskListScreen) syncTableRows() {
	rows := make([]table.Row, len(s.tasks))
	for i, task := range s.tasks {
		dot, statusText := taskStatusDisplay(task.Status)
		statusCell := stripANSI(dot) + " " + stripANSI(statusText)

		title := task.Title
		if title == "" {
			title = "(no title)"
		}

		projectCell := ""
		if name := s.findProjectName(task.ProjectID); name != "" {
			projectCell = truncate(name, 12)
		}

		rows[i] = table.Row{
			statusCell,
			truncate(title, s.titleWidth),
			projectCell,
			task.Behavior,
			formatTaskElapsed(task.CreatedAt),
		}
	}
	s.table.SetRows(rows)
	// Fix cursor if it became negative due to SetCursor being called with empty rows.
	if len(rows) > 0 && s.table.Cursor() < 0 {
		s.table.SetCursor(0)
	}
}

// --- rendering ---

func taskStatusDisplay(status orchestrator.TaskStatus) (dot, text string) {
	switch status {
	case orchestrator.TaskStatusExecuting:
		return styleExecuting.Render("●"), styleExecuting.Render("executing")
	case orchestrator.TaskStatusReworking:
		return styleExecuting.Render("●"), styleExecuting.Render("reworking")
	case orchestrator.TaskStatusVerifying:
		return styleVerifying.Render("●"), styleVerifying.Render("verifying")
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

func fetchTasksCmd(c *client.Client, stateClosed bool, projectID, behaviorFilter string) tea.Cmd {
	return func() tea.Msg {
		filter := client.TaskListFilter{
			ProjectID: projectID,
		}

		tasks, err := c.ListTasks(filter)
		if err != nil {
			return tasksMsg{err: err}
		}

		// Filter by open/closed state client-side.
		var filtered []*orchestrator.Task
		for _, t := range tasks {
			if stateClosed {
				if closedStatuses[t.Status] {
					filtered = append(filtered, t)
				}
			} else {
				if openStatuses[t.Status] {
					filtered = append(filtered, t)
				}
			}
		}

		// Filter by behavior.
		if behaviorFilter != "" {
			var byBehavior []*orchestrator.Task
			for _, t := range filtered {
				if t.Behavior == behaviorFilter {
					byBehavior = append(byBehavior, t)
				}
			}
			filtered = byBehavior
		}

		return tasksMsg{tasks: filtered}
	}
}

func fetchProjectsCmd(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		projects, err := c.ListProjects()
		return projectsMsg{projects: projects, err: err}
	}
}

func fetchQuickOpenCmd(c *client.Client, taskID string) tea.Cmd {
	return func() tea.Msg {
		detail, err := c.GetTaskDetail(taskID)
		if err != nil {
			return quickOpenResultMsg{taskID: taskID, err: err}
		}
		var running []*api.Job
		for _, j := range detail.Jobs {
			if j.Status == api.JobStatusRunning {
				running = append(running, j)
			}
		}
		return quickOpenResultMsg{taskID: taskID, jobs: running}
	}
}

// handleMiniKey processes key events when the mini selector is active.
func (s *TaskListScreen) handleMiniKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "j", "right":
		if s.mini.cursor < len(s.mini.jobs)-1 {
			s.mini.cursor++
		}
	case "k", "left":
		if s.mini.cursor > 0 {
			s.mini.cursor--
		}
	case "enter":
		job := s.mini.jobs[s.mini.cursor]
		s.mini.active = false
		if !s.shared.TmuxEnabled {
			s.statusMsg = "to open a job, launch `boid tui` inside tmux"
			s.isError = false
			return clearStatusAfter(4 * time.Second)
		}
		return openJobCmd(job.ID, s.shared.Panes[job.ID])
	case "esc":
		s.mini.active = false
	}
	return nil
}

// renderMiniSelector renders the horizontal job selector row.
func renderMiniSelector(m miniSelector, width int) string {
	var sb strings.Builder
	sb.WriteString(styleDim.Render("Select job: "))
	for i, job := range m.jobs {
		label := shortID(job.ID)
		if job.Role != "" {
			label += " [" + job.Role + "]"
		}
		if i == m.cursor {
			sb.WriteString(styleFilterActive.Render("▸ " + label))
		} else {
			sb.WriteString(styleFilterInactive.Render("  " + label))
		}
		if i < len(m.jobs)-1 {
			sb.WriteString("   ")
		}
	}
	_ = width
	return sb.String()
}
