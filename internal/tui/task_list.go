package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

const taskPollInterval = 3 * time.Second

// statusWidth is the fixed column width for the STATUS column.
// Ambiguous-width characters (●, ✓, ✗, ○) render as 2 cells in most
// terminals, so "● executing" = 2+1+9 = 12 cells.
const statusWidth = 12

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
type workspacesMsg struct {
	workspaces []*orchestrator.WorkspaceSummary
	err        error
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
	kind   string   // "project", "behavior", or "workspace"
	labels []string // display labels
	values []string // internal values ("" for all)
	cursor int
}

// --- TaskListScreen ---

type TaskListScreen struct {
	shared *SharedState

	table               table.Model
	tasks               []*orchestrator.Task
	displayTasks        []*orchestrator.Task // tasks filtered by searchQuery
	projects            []*orchestrator.Project
	workspaces          []*orchestrator.WorkspaceSummary
	stateClosed         bool            // false=open, true=closed
	selectedProjectID   string          // "" = all
	selectedWorkspaceID string          // "" = all
	behaviorFilter      string          // "" = all
	searchMode          bool            // true while typing a search query
	searchQuery         string          // retained query ("" = no filter)
	searchInput         textinput.Model // text input widget for search mode
	popup               popupSelector
	loading             bool
	fetchErr            error
	statusMsg           string
	isError             bool
	mini                miniSelector
	titleWidth          int // current TITLE column width; default 24
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
	si := textinput.New()
	return &TaskListScreen{
		shared:      shared,
		table:       t,
		loading:     true,
		titleWidth:  24,
		searchInput: si,
	}
}

func (s *TaskListScreen) Init() tea.Cmd {
	return tea.Batch(
		fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter, nil),
		fetchProjectsCmd(s.shared.Client),
		fetchWorkspacesCmd(s.shared.Client),
		taskTickCmd(),
	)
}

func (s *TaskListScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case taskTickMsg:
		return s, tea.Batch(
			fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter, s.wsProjectIDs()),
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

	case workspacesMsg:
		if msg.err == nil {
			s.workspaces = msg.workspaces
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
		return s, fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter, s.wsProjectIDs())

	case screenResumedMsg:
		return s, fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter, s.wsProjectIDs())

	case taskCreatedNotifyMsg:
		s.statusMsg = "task created"
		s.isError = false
		return s, tea.Batch(fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter, s.wsProjectIDs()), clearStatusAfter(3*time.Second))

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
	if s.searchMode {
		return s.handleSearchKey(msg)
	}

	switch msg.String() {
	case "j", "down":
		if len(s.displayTasks) > 0 {
			s.table.MoveDown(1)
		}

	case "k", "up":
		if len(s.displayTasks) > 0 {
			s.table.MoveUp(1)
		}

	case "tab":
		s.stateClosed = !s.stateClosed
		s.table.SetCursor(0)
		s.loading = true
		return fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter, s.wsProjectIDs())

	case "w":
		s.popup = s.buildWorkspacePopup()

	case "p":
		s.popup = s.buildProjectPopup()

	case "b":
		s.popup = s.buildBehaviorPopup()

	case "/":
		s.searchMode = true
		s.searchInput.SetValue(s.searchQuery)
		return s.searchInput.Focus()

	case "r":
		s.loading = true
		return fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter, s.wsProjectIDs())

	case "enter":
		if len(s.displayTasks) == 0 {
			break
		}
		task := s.displayTasks[s.table.Cursor()]
		projectName := s.findProjectName(task.ProjectID)
		return PushScreen(NewTaskDetailScreen(s.shared, task.ID, projectName))

	case "s":
		if len(s.displayTasks) == 0 {
			break
		}
		task := s.displayTasks[s.table.Cursor()]
		if task.Status != orchestrator.TaskStatusPending {
			break
		}
		s.statusMsg = "starting..."
		s.isError = false
		return applyActionCmd(s.shared.Client, task.ID, "start")

	case "o":
		if len(s.displayTasks) == 0 {
			break
		}
		task := s.displayTasks[s.table.Cursor()]
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

// handleSearchKey processes key events when search mode is active.
func (s *TaskListScreen) handleSearchKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		s.searchMode = false
		s.searchQuery = ""
		s.searchInput.Blur()
		s.searchInput.SetValue("")
		s.syncTableRows()
		s.table.SetCursor(0)
	case "enter":
		s.searchMode = false
		s.searchQuery = s.searchInput.Value()
		s.searchInput.Blur()
	default:
		var cmd tea.Cmd
		s.searchInput, cmd = s.searchInput.Update(msg)
		s.searchQuery = s.searchInput.Value()
		s.syncTableRows()
		if len(s.displayTasks) > 0 {
			s.table.SetCursor(0)
		}
		return cmd
	}
	return nil
}

// buildWorkspacePopup constructs a popupSelector for workspace selection.
func (s *TaskListScreen) buildWorkspacePopup() popupSelector {
	labels := []string{"(all)"}
	values := []string{""}
	cursor := 0
	for i, w := range s.workspaces {
		labels = append(labels, w.ID)
		values = append(values, w.ID)
		if w.ID == s.selectedWorkspaceID {
			cursor = i + 1
		}
	}
	return popupSelector{
		active: true,
		kind:   "workspace",
		labels: labels,
		values: values,
		cursor: cursor,
	}
}

// buildProjectPopup constructs a popupSelector for project selection.
// When a workspace is selected, only shows projects belonging to that workspace.
func (s *TaskListScreen) buildProjectPopup() popupSelector {
	labels := []string{"(all)"}
	values := []string{""}
	cursor := 0
	idx := 1
	for _, p := range s.projects {
		if s.selectedWorkspaceID != "" && p.WorkspaceID != s.selectedWorkspaceID {
			continue
		}
		labels = append(labels, p.Meta.Name)
		values = append(values, p.ID)
		if p.ID == s.selectedProjectID {
			cursor = idx
		}
		idx++
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
		case "workspace":
			s.selectedWorkspaceID = val
			// Reset project if it no longer belongs to the selected workspace.
			if val != "" && s.selectedProjectID != "" {
				belongs := false
				for _, p := range s.projects {
					if p.ID == s.selectedProjectID && p.WorkspaceID == val {
						belongs = true
						break
					}
				}
				if !belongs {
					s.selectedProjectID = ""
				}
			}
		case "project":
			s.selectedProjectID = val
		case "behavior":
			s.behaviorFilter = val
		}
		s.table.SetCursor(0)
		s.loading = true
		return fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter, s.wsProjectIDs())
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

// wsProjectIDs returns a set of project IDs that belong to the selected workspace,
// or nil if no workspace is selected.
func (s *TaskListScreen) wsProjectIDs() map[string]bool {
	if s.selectedWorkspaceID == "" {
		return nil
	}
	ids := map[string]bool{}
	for _, p := range s.projects {
		if p.WorkspaceID == s.selectedWorkspaceID {
			ids[p.ID] = true
		}
	}
	return ids
}

// workspaceName returns the display name for the current workspace filter.
func (s *TaskListScreen) workspaceName() string {
	if s.selectedWorkspaceID == "" {
		return "all"
	}
	return s.selectedWorkspaceID
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
	if s.searchMode {
		return "esc: cancel  enter: confirm"
	}
	return "enter: detail  s: start  o: open job  n: new  tab: state  /: search  w: workspace  p: project  b: behavior  r: refresh  q: quit"
}

func (s *TaskListScreen) buildTaskFilterBar(width int) string {
	stateLabel := "open"
	if s.stateClosed {
		stateLabel = "closed"
	}

	wsLabel := s.workspaceName()
	projLabel := s.projectName()
	behLabel := s.behaviorFilter
	if behLabel == "" {
		behLabel = "all"
	}

	stateChip := styleFilterActive.Render("state: " + stateLabel)
	wsChip := styleDim.Render("ws: " + wsLabel)
	projChip := styleDim.Render("proj: " + projLabel)
	behChip := styleDim.Render("behavior: " + behLabel)

	bar := stateChip + "    " + wsChip + "    " + projChip + "    " + behChip

	if s.searchMode {
		bar += "    " + styleFilterActive.Render("q: ") + s.searchInput.View()
	} else if s.searchQuery != "" {
		bar += "    " + styleFilterActive.Render("q: "+s.searchQuery)
	}

	_ = width
	return bar
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
// the table. Fixed widths: STATUS(12), PROJECT(12), BEHAVIOR(10), AGE(6).
// The remainder (minus separator overhead) is assigned to TITLE with a minimum
// of 20. The calculated TITLE width is stored in s.titleWidth for use by
// syncTableRows.
func (s *TaskListScreen) recalcColumns(width int) {
	const (
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

// buildTreeOrder takes a flat list of tasks and returns them in DFS tree order
// (parent before children) along with a depth map keyed by task ID.
// The original input order is preserved for roots and siblings.
func buildTreeOrder(tasks []*orchestrator.Task) ([]*orchestrator.Task, map[string]int) {
	idSet := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		idSet[t.ID] = true
	}

	children := make(map[string][]*orchestrator.Task)
	var roots []*orchestrator.Task
	for _, t := range tasks {
		if t.ParentID == "" || !idSet[t.ParentID] {
			roots = append(roots, t)
		} else {
			children[t.ParentID] = append(children[t.ParentID], t)
		}
	}

	result := make([]*orchestrator.Task, 0, len(tasks))
	depths := make(map[string]int, len(tasks))

	var dfs func(t *orchestrator.Task, depth int)
	dfs = func(t *orchestrator.Task, depth int) {
		result = append(result, t)
		depths[t.ID] = depth
		for _, child := range children[t.ID] {
			dfs(child, depth+1)
		}
	}
	for _, root := range roots {
		dfs(root, 0)
	}

	return result, depths
}

// progressBadge returns a compact progress string for a parent task
// (e.g. "3/7" or "3/7 [!1]"). Returns "" when TotalChildCount == 0.
func progressBadge(task *orchestrator.Task) string {
	if task.TotalChildCount == 0 {
		return ""
	}
	s := fmt.Sprintf("%d/%d", task.DoneChildCount, task.TotalChildCount)
	if task.AbortedChildCount > 0 {
		s += fmt.Sprintf(" [!%d]", task.AbortedChildCount)
	}
	return s
}

// applyStateFilter filters tasks by open/closed state, taking child counts into account.
// Open mode: keeps tasks that are open OR have open children.
// Closed mode: keeps tasks that are closed AND have no open children.
func applyStateFilter(tasks []*orchestrator.Task, stateClosed bool) []*orchestrator.Task {
	var filtered []*orchestrator.Task
	for _, t := range tasks {
		if stateClosed {
			if closedStatuses[t.Status] && t.OpenChildCount == 0 {
				filtered = append(filtered, t)
			}
		} else {
			if openStatuses[t.Status] || t.OpenChildCount > 0 {
				filtered = append(filtered, t)
			}
		}
	}
	return filtered
}

// syncTableRows applies the searchQuery filter to s.tasks, builds the tree order,
// stores the result in s.displayTasks, then converts to table rows and updates the table.
func (s *TaskListScreen) syncTableRows() {
	// Filter by searchQuery (title, case-insensitive).
	var base []*orchestrator.Task
	if s.searchQuery == "" {
		base = s.tasks
	} else {
		q := strings.ToLower(s.searchQuery)
		filtered := make([]*orchestrator.Task, 0, len(s.tasks))
		for _, t := range s.tasks {
			if strings.Contains(strings.ToLower(t.Title), q) {
				filtered = append(filtered, t)
			}
		}
		base = filtered
	}

	// Build tree order (DFS: parent before children).
	ordered, depths := buildTreeOrder(base)
	s.displayTasks = ordered

	rows := make([]table.Row, len(s.displayTasks))
	for i, task := range s.displayTasks {
		// STATUS セル: 素の文字列でビルドしてから色を適用する
		rawDot, rawStatusText := taskStatusRaw(task.Status)
		rawStatusCell := truncate(rawDot+" "+rawStatusText, statusWidth)

		title := task.Title
		if title == "" {
			title = "(no title)"
		}

		// TITLE セル: インデント + タイトル + 進捗バッジを素の文字列で構築し、
		// truncate 後に色を適用する
		depth := depths[task.ID]
		indent := strings.Repeat("  ", depth)
		rawTitle := indent + title
		progress := progressBadge(task)

		var rawTitleCell string
		if progress != "" {
			progressPart := " " + progress
			maxTitle := s.titleWidth - len([]rune(progressPart))
			if maxTitle < 1 {
				maxTitle = 1
			}
			titlePart := strings.TrimRight(truncate(rawTitle, maxTitle), " ")
			rawTitleCell = truncate(titlePart+progressPart, s.titleWidth)
		} else {
			rawTitleCell = truncate(rawTitle, s.titleWidth)
		}

		// ステータスと blocked 状態に応じたスタイルを適用する
		st := taskCellStyle(task)
		statusCell := st.Render(rawStatusCell)
		titleCell := st.Render(rawTitleCell)

		projectCell := ""
		if name := s.findProjectName(task.ProjectID); name != "" {
			projectCell = truncate(name, 12)
		}

		rows[i] = table.Row{
			statusCell,
			titleCell,
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

// taskStatusRaw は ANSI コードを含まない素の dot アイコンとステータステキストを返す。
// truncate 後に色を適用する用途で使用する。
func taskStatusRaw(status orchestrator.TaskStatus) (dot, text string) {
	switch status {
	case orchestrator.TaskStatusExecuting:
		return "●", "executing"
	case orchestrator.TaskStatusReworking:
		return "●", "reworking"
	case orchestrator.TaskStatusVerifying:
		return "●", "verifying"
	case orchestrator.TaskStatusPending:
		return "○", "pending"
	case orchestrator.TaskStatusDone:
		return "✓", "done"
	case orchestrator.TaskStatusAborted:
		return "✗", "aborted"
	default:
		return "?", string(status)
	}
}

// taskCellStyle はタスクのステータスと blocked 状態に基づいて lipgloss スタイルを返す。
// STATUS セルと TITLE セルの両方に適用する。
func taskCellStyle(task *orchestrator.Task) lipgloss.Style {
	switch task.Status {
	case orchestrator.TaskStatusExecuting, orchestrator.TaskStatusReworking:
		return styleExecuting.Bold(true)
	case orchestrator.TaskStatusVerifying:
		return styleVerifying
	case orchestrator.TaskStatusDone:
		return styleTaskDim
	case orchestrator.TaskStatusAborted:
		return styleAborted
	case orchestrator.TaskStatusPending:
		if task.Blocked {
			return styleDim
		}
		return lipgloss.NewStyle()
	default:
		return lipgloss.NewStyle()
	}
}

// taskStatusDisplay は後方互換のため ANSI 色付きの dot とテキストを返す。
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

// fetchTasksCmd fetches tasks from the server applying server-side project filter,
// then filters client-side by state, behavior, and workspace (via workspaceProjectIDs).
// workspaceProjectIDs is a set of project IDs belonging to the selected workspace;
// nil means no workspace filter.
func fetchTasksCmd(c *client.Client, stateClosed bool, projectID, behaviorFilter string, workspaceProjectIDs map[string]bool) tea.Cmd {
	return func() tea.Msg {
		filter := client.TaskListFilter{
			ProjectID: projectID,
		}

		tasks, err := c.ListTasks(filter)
		if err != nil {
			return tasksMsg{err: err}
		}

		// Filter by open/closed state client-side (child counts included).
		filtered := applyStateFilter(tasks, stateClosed)

		// Filter by workspace (only when no specific project is already selected).
		if projectID == "" && len(workspaceProjectIDs) > 0 {
			var byWorkspace []*orchestrator.Task
			for _, t := range filtered {
				if workspaceProjectIDs[t.ProjectID] {
					byWorkspace = append(byWorkspace, t)
				}
			}
			filtered = byWorkspace
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

func fetchWorkspacesCmd(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		workspaces, err := c.ListWorkspaces()
		return workspacesMsg{workspaces: workspaces, err: err}
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
