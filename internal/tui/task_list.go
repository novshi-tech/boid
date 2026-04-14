package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

const (
	activeTaskPollInterval = 1 * time.Second
	idleTaskPollInterval   = 3 * time.Second
)

// statusWidth is the fixed column width for the STATUS column.
// "● executing" = 1+1+9 = 11 visible runes; ansi.StringWidth reports ● as 1 cell
// via uniseg/GraphemeWidth, so we use 11 to match the actual measured width.
const (
	statusWidth = 11
	colProject  = 12
	colBehavior = 10
	colAge      = 6
	minTitle    = 20
)

// tableRow holds the pre-rendered cell strings for one table row.
// Indices: 0=TITLE, 1=STATUS, 2=PROJECT, 3=BEHAVIOR, 4=AGE.
type tableRow [5]string

// activeStatuses defines which task statuses are considered "active" for poll interval decisions.
// Active tasks are being processed by the system, so faster polling improves responsiveness.
var activeStatuses = map[orchestrator.TaskStatus]bool{
	orchestrator.TaskStatusExecuting: true,
	orchestrator.TaskStatusReworking: true,
	orchestrator.TaskStatusVerifying: true,
}

// tickIntervalForTasks returns activeTaskPollInterval if any task in the slice is active,
// otherwise idleTaskPollInterval.
func tickIntervalForTasks(tasks []*orchestrator.Task) time.Duration {
	for _, t := range tasks {
		if activeStatuses[t.Status] {
			return activeTaskPollInterval
		}
	}
	return idleTaskPollInterval
}

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

	// Custom table rendering (replaces bubbles/table).
	cursor          int        // selected row index into displayTasks
	viewStart       int        // first visible row index (scroll offset)
	tableBodyHeight int        // visible data rows (excluding header); set in renderTable
	tableRows       []tableRow // pre-rendered cell strings, built by syncTableRows

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
	titleWidth          int  // current TITLE column width; default 24
	blinkOn             bool // toggles every 600ms for executing/reworking/verifying dot blink
}

func NewTaskListScreen(shared *SharedState) *TaskListScreen {
	si := textinput.New()
	return &TaskListScreen{
		shared:      shared,
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
		tickTaskList(s.displayTasks),
		taskBlinkCmd(),
	)
}

func (s *TaskListScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case taskTickMsg:
		return s, tea.Batch(
			fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter, s.wsProjectIDs()),
			tickTaskList(s.displayTasks),
		)

	case taskBlinkTickMsg:
		s.blinkOn = !s.blinkOn
		s.syncTableRows()
		return s, taskBlinkCmd()

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
		return s, tea.Batch(
			fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter, s.wsProjectIDs()),
			taskBlinkCmd(),
		)

	case taskCreatedNotifyMsg:
		s.statusMsg = "task created"
		s.isError = false
		return s, tea.Batch(fetchTasksCmd(s.shared.Client, s.stateClosed, s.selectedProjectID, s.behaviorFilter, s.wsProjectIDs()), clearStatusAfter(3*time.Second))

	case clearStatusMsg:
		s.statusMsg = ""
		s.isError = false

	case tea.WindowSizeMsg:
		s.recalcColumns(msg.Width)
		bodyH := msg.Height - 2
		if bodyH < 1 {
			bodyH = 1
		}
		s.tableBodyHeight = bodyH - 1 // subtract header row
		if s.tableBodyHeight < 0 {
			s.tableBodyHeight = 0
		}
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
		s.moveCursor(1)

	case "k", "up":
		s.moveCursor(-1)

	case "tab":
		s.stateClosed = !s.stateClosed
		s.cursor = 0
		s.viewStart = 0
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
		task := s.displayTasks[s.cursor]
		projectName := s.findProjectName(task.ProjectID)
		return PushScreen(NewTaskDetailScreen(s.shared, task.ID, projectName))

	case "s":
		if len(s.displayTasks) == 0 {
			break
		}
		task := s.displayTasks[s.cursor]
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
		task := s.displayTasks[s.cursor]
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
		s.cursor = 0
		s.viewStart = 0
	case "enter":
		s.searchMode = false
		s.searchQuery = s.searchInput.Value()
		s.searchInput.Blur()
	default:
		var cmd tea.Cmd
		s.searchInput, cmd = s.searchInput.Update(msg)
		s.searchQuery = s.searchInput.Value()
		s.syncTableRows()
		s.cursor = 0
		s.viewStart = 0
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
		s.cursor = 0
		s.viewStart = 0
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
		sb.WriteString(s.renderTable(bodyHeight, width))
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
	// 5 columns joined with " " → 4 separators.
	fixedTotal := statusWidth + colProject + colBehavior + colAge
	titleWidth := width - fixedTotal - 4
	if titleWidth < minTitle {
		titleWidth = minTitle
	}
	s.titleWidth = titleWidth
}

// selectedBgSGR is the SGR sequence for the selected-row background (color 237).
// Kept in sync with styleTableSelected in style.go.
const selectedBgSGR = "\x1b[48;5;237m"

// reinforceSelectedBg re-applies the selected-row background after every SGR
// reset (\x1b[0m or \x1b[m) in s. This preserves an outer background style
// applied over a string that contains internal resets (e.g., colored cells),
// which would otherwise cancel the outer background mid-string.
func reinforceSelectedBg(s string) string {
	s = strings.ReplaceAll(s, "\x1b[0m", "\x1b[0m"+selectedBgSGR)
	s = strings.ReplaceAll(s, "\x1b[m", "\x1b[m"+selectedBgSGR)
	return s
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
// (parent before children) along with a prefix map keyed by task ID.
// The prefix encodes the tree connector ("├─", "└─", "│ ", "  ") for display.
// Roots have prefix "". The original input order is preserved for roots and siblings.
func buildTreeOrder(tasks []*orchestrator.Task) ([]*orchestrator.Task, map[string]string) {
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
	prefixes := make(map[string]string, len(tasks))

	// isLastChild[id] は、そのタスクが同一親の子の中で末尾かどうか。
	isLastChild := make(map[string]bool, len(tasks))
	for _, sibs := range children {
		if len(sibs) > 0 {
			isLastChild[sibs[len(sibs)-1].ID] = true
		}
	}

	// ancestorIsLast は depth≥1 の祖先について「末子かどうか」を順に格納したスタック。
	// depth=0 のルートは prefix に影響しないので含めない。
	var dfs func(t *orchestrator.Task, depth int, ancestorIsLast []bool)
	dfs = func(t *orchestrator.Task, depth int, ancestorIsLast []bool) {
		result = append(result, t)

		if depth == 0 {
			prefixes[t.ID] = ""
		} else {
			var sb strings.Builder
			for _, last := range ancestorIsLast {
				if last {
					sb.WriteString("  ")
				} else {
					sb.WriteString("│ ")
				}
			}
			if isLastChild[t.ID] {
				sb.WriteString("└─")
			} else {
				sb.WriteString("├─")
			}
			prefixes[t.ID] = sb.String()
		}

		// depth≥1 のノードのみ自身の末子フラグを子のスタックに追加する。
		var childStack []bool
		if depth > 0 {
			childStack = make([]bool, len(ancestorIsLast)+1)
			copy(childStack, ancestorIsLast)
			childStack[len(ancestorIsLast)] = isLastChild[t.ID]
		}

		for _, child := range children[t.ID] {
			dfs(child, depth+1, childStack)
		}
	}
	for _, root := range roots {
		dfs(root, 0, nil)
	}

	return result, prefixes
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
// Open mode: keeps tasks that are open OR have open children (pass 1), then also keeps
// closed children whose parent is in the pass-1 result (pass 2).
// Closed mode: keeps tasks that are closed AND have no open children.
func applyStateFilter(tasks []*orchestrator.Task, stateClosed bool) []*orchestrator.Task {
	if stateClosed {
		var filtered []*orchestrator.Task
		for _, t := range tasks {
			if closedStatuses[t.Status] && t.OpenChildCount == 0 {
				filtered = append(filtered, t)
			}
		}
		return filtered
	}

	// Open mode — pass 1: current logic.
	firstPassIDs := make(map[string]bool)
	var firstPass []*orchestrator.Task
	for _, t := range tasks {
		if openStatuses[t.Status] || t.OpenChildCount > 0 {
			firstPass = append(firstPass, t)
			firstPassIDs[t.ID] = true
		}
	}

	// Collect IDs of open parents: tasks in pass 1 that have children.
	openParentIDs := make(map[string]bool)
	for _, t := range firstPass {
		if t.TotalChildCount > 0 {
			openParentIDs[t.ID] = true
		}
	}

	if len(openParentIDs) == 0 {
		return firstPass
	}

	// Pass 2: preserve original input order, adding closed children of open parents.
	result := make([]*orchestrator.Task, 0, len(firstPass))
	for _, t := range tasks {
		if firstPassIDs[t.ID] {
			result = append(result, t)
		} else if openParentIDs[t.ParentID] && closedStatuses[t.Status] {
			result = append(result, t)
		}
	}
	return result
}

// syncTableRows applies the searchQuery filter to s.tasks, builds the tree order,
// stores the result in s.displayTasks, then converts to table rows and updates the table.
// カーソル行は無着色モード（ANSI なし）で構築し、table.Selected.Reverse が正しく効くようにする。
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
	ordered, prefixes := buildTreeOrder(base)
	s.displayTasks = ordered

	rows := make([]tableRow, len(s.displayTasks))
	for i, task := range s.displayTasks {
		rawDot, rawStatusText := taskStatusRaw(task.Status)

		title := task.Title
		if title == "" {
			title = "(no title)"
		}

		prefix := prefixes[task.ID]
		progress := progressBadge(task)
		isParent := task.TotalChildCount > 0

		// TITLE セルの content 部分を truncate する（prefix 幅を除いた範囲で）。
		// lipgloss.Width で表示幅ベースに計算する。
		prefixWidth := lipgloss.Width(prefix)
		availableWidth := s.titleWidth - prefixWidth
		if availableWidth < 1 {
			availableWidth = 1
		}
		var truncatedContent string
		if progress != "" {
			progressPart := " " + progress
			progressWidth := lipgloss.Width(progressPart)
			maxTitle := availableWidth - progressWidth
			if maxTitle < 1 {
				maxTitle = 1
			}
			titlePart := truncate(title, maxTitle)
			truncatedContent = truncate(titlePart+progressPart, availableWidth)
		} else {
			truncatedContent = truncate(title, availableWidth)
		}

		projectName := s.findProjectName(task.ProjectID)

		// 行全体を dim にするか（blocked pending / done / aborted）
		isDimRow := task.Status == orchestrator.TaskStatusDone ||
			task.Status == orchestrator.TaskStatusAborted ||
			(task.Status == orchestrator.TaskStatusPending && task.Blocked)

		// STATUS セル: ドットのみ着色、親タスクのテキストは Bold+Underline（dim 行は除く）。
		// aborted は dim 行でもドットを赤のまま維持する。
		var dotStyle lipgloss.Style
		if isDimRow && task.Status != orchestrator.TaskStatusAborted {
			dotStyle = styleTaskDim
		} else if isBlinkTarget(task.Status) && !s.blinkOn {
			dotStyle = styleTaskDim
		} else {
			dotStyle = taskCellStyle(task)
		}
		dot := dotStyle.Render(rawDot)
		var statusText string
		if !isDimRow && isParent {
			statusText = styleTreeParent.Render(rawStatusText)
		} else {
			statusText = rawStatusText
		}
		statusCell := dot + " " + statusText

		// TITLE セル: prefix を styleDim で着色し、content は親なら Bold+Underline（dim 行は除く）。
		var content string
		if isDimRow {
			content = styleDim.Render(truncatedContent)
		} else if isParent {
			content = styleTreeParent.Render(truncatedContent)
		} else {
			content = truncatedContent
		}
		var titleCell string
		if prefix != "" {
			titleCell = styleDim.Render(prefix) + content
		} else {
			titleCell = content
		}

		// PROJECT / BEHAVIOR / AGE セル: 親タスクは Bold+Underline（dim 行は除く）。
		var projectCell, behaviorCell, ageCell string
		if isDimRow {
			projectCell = styleDim.Render(projectName)
			behaviorCell = styleDim.Render(task.Behavior)
			ageCell = styleDim.Render(formatTaskElapsed(task.CreatedAt))
		} else if isParent {
			projectCell = styleTreeParent.Render(projectName)
			behaviorCell = styleTreeParent.Render(task.Behavior)
			ageCell = styleTreeParent.Render(formatTaskElapsed(task.CreatedAt))
		} else {
			projectCell = projectName
			behaviorCell = task.Behavior
			ageCell = formatTaskElapsed(task.CreatedAt)
		}

		rows[i] = tableRow{titleCell, statusCell, projectCell, behaviorCell, ageCell}
	}

	s.tableRows = rows

	// Clip cursor to valid range.
	n := len(rows)
	if n == 0 {
		s.cursor = 0
		s.viewStart = 0
	} else if s.cursor >= n {
		s.cursor = n - 1
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
// STATUS セルのドット着色専用。Bold は親判定（styleTreeParent）側で行う。
func taskCellStyle(task *orchestrator.Task) lipgloss.Style {
	switch task.Status {
	case orchestrator.TaskStatusExecuting, orchestrator.TaskStatusReworking:
		return styleExecuting
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

// --- custom table rendering ---

// fitCell adjusts content to exactly colWidth display cells.
// If content is shorter, spaces are appended. If wider, the tail is truncated
// with "…". Correctly handles ANSI escape sequences via lipgloss.Width and
// xansi.GraphemeWidth.Truncate.
func fitCell(content string, colWidth int) string {
	w := lipgloss.Width(content)
	if w < colWidth {
		return content + strings.Repeat(" ", colWidth-w)
	}
	if w > colWidth {
		return xansi.GraphemeWidth.Truncate(content, colWidth, "…")
	}
	return content
}

// moveCursor moves the cursor by delta rows and adjusts the scroll offset.
func (s *TaskListScreen) moveCursor(delta int) {
	n := len(s.displayTasks)
	if n == 0 {
		return
	}
	s.cursor += delta
	if s.cursor < 0 {
		s.cursor = 0
	} else if s.cursor >= n {
		s.cursor = n - 1
	}
	if s.tableBodyHeight > 0 {
		if s.cursor < s.viewStart {
			s.viewStart = s.cursor
		} else if s.cursor >= s.viewStart+s.tableBodyHeight {
			s.viewStart = s.cursor - s.tableBodyHeight + 1
		}
	}
}

// renderTableHeader renders the header row with fixed column widths.
func (s *TaskListScreen) renderTableHeader() string {
	cells := []string{
		fitCell(styleTableHeader.Render("TITLE"), s.titleWidth),
		fitCell(styleTableHeader.Render("STATUS"), statusWidth),
		fitCell(styleTableHeader.Render("PROJECT"), colProject),
		fitCell(styleTableHeader.Render("BEHAVIOR"), colBehavior),
		fitCell(styleTableHeader.Render("AGE"), colAge),
	}
	return strings.Join(cells, " ")
}

// renderDataRow renders a single data row. The cursor row is highlighted with
// Reverse style padded to lineWidth cells so the highlight spans the full line.
func (s *TaskListScreen) renderDataRow(i, lineWidth int) string {
	r := s.tableRows[i]
	cells := []string{
		fitCell(r[0], s.titleWidth),
		fitCell(r[1], statusWidth),
		fitCell(r[2], colProject),
		fitCell(r[3], colBehavior),
		fitCell(r[4], colAge),
	}
	line := strings.Join(cells, " ")
	if i == s.cursor {
		// Re-apply the selected-row background after every internal SGR
		// reset so the highlight survives through embedded ANSI codes
		// while keeping cell foreground colors intact.
		reinforced := reinforceSelectedBg(line)
		if w := lipgloss.Width(reinforced); w < lineWidth {
			reinforced += strings.Repeat(" ", lineWidth-w)
		}
		return styleTableSelected.Render(reinforced)
	}
	return line
}

// renderTable renders the full table (header + visible data rows) for the given
// body height and updates s.tableBodyHeight for scroll calculations.
func (s *TaskListScreen) renderTable(bodyHeight, lineWidth int) string {
	var sb strings.Builder

	// Header row.
	sb.WriteString(s.renderTableHeader())
	sb.WriteByte('\n')

	// Data rows: bodyHeight minus header row.
	dataH := bodyHeight - 1
	if dataH < 0 {
		dataH = 0
	}
	s.tableBodyHeight = dataH

	// Clamp viewStart so cursor is always visible.
	if s.cursor < s.viewStart {
		s.viewStart = s.cursor
	} else if dataH > 0 && s.cursor >= s.viewStart+dataH {
		s.viewStart = s.cursor - dataH + 1
	}
	if s.viewStart < 0 {
		s.viewStart = 0
	}

	end := s.viewStart + dataH
	if end > len(s.tableRows) {
		end = len(s.tableRows)
	}
	for i := s.viewStart; i < end; i++ {
		sb.WriteString(s.renderDataRow(i, lineWidth))
		sb.WriteByte('\n')
	}

	return sb.String()
}

// --- commands ---

func tickTaskList(tasks []*orchestrator.Task) tea.Cmd {
	return tea.Tick(tickIntervalForTasks(tasks), func(time.Time) tea.Msg {
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
