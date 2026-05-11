package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/timeline"
)

// findAPIJob resolves a timeline event's JobInfo.ID back to the full api.Job
// record kept on the detail view. The timeline package stores only the
// minimum job fields needed for rendering; TUI navigation actions
// (open-in-tmux, drill into JobDetailScreen) need the full record — Interactive,
// Output, etc. — which only api.Job carries.
func (s *TaskDetailScreen) findAPIJob(id string) *api.Job {
	if s.detail == nil || id == "" {
		return nil
	}
	for _, j := range s.detail.Jobs {
		if j != nil && j.ID == id {
			return j
		}
	}
	return nil
}

const (
	activeTaskDetailPollInterval = 1 * time.Second
	idleTaskDetailPollInterval   = 3 * time.Second
)

// tickIntervalForDetail returns activeTaskDetailPollInterval when the task status is active
// (executing), otherwise idleTaskDetailPollInterval.
func tickIntervalForDetail(status orchestrator.TaskStatus) time.Duration {
	if status == orchestrator.TaskStatusExecuting {
		return activeTaskDetailPollInterval
	}
	return idleTaskDetailPollInterval
}

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
type duplicateResultMsg struct {
	newTaskID string
	err       error
}
type duplicateConfirmDeadlineMsg struct{}
type rerunResultMsg struct{ err error }
type rerunConfirmDeadlineMsg struct{}
type titleUpdateResultMsg struct{ err error }

// --- TaskDetailScreen ---

type TaskDetailScreen struct {
	shared      *SharedState
	taskID      string
	projectName string

	detail             *api.TaskDetailView
	activeTab          string
	cursor             int
	timelineCursor     int
	depsCursor         int
	descScroll         int
	descPageHeight     int
	descWidth          int
	payloadCursor      int
	payloadScroll      int
	instructionsCursor int
	instructionsScroll int
	statusMsg          string
	isError            bool
	loading            bool
	fetchErr           error
	abortPending       bool
	deletePending      bool
	duplicatePending   bool
	rerunPending       bool

	titleEditing bool
	titleInput   TextFieldModel
}

func NewTaskDetailScreen(shared *SharedState, taskID, projectName string) *TaskDetailScreen {
	return &TaskDetailScreen{
		shared:      shared,
		taskID:      taskID,
		projectName: projectName,
		activeTab:   tabOverview,
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
		interval := idleTaskDetailPollInterval
		if s.detail != nil && s.detail.Task != nil {
			interval = tickIntervalForDetail(s.detail.Task.Status)
		}
		return s, tea.Batch(
			fetchTaskDetailCmd(s.shared.Client, s.taskID),
			tea.Tick(interval, func(time.Time) tea.Msg { return taskDetailTickMsg{} }),
		)

	case taskBlinkTickMsg:
		return s, nil

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
			if s.detail != nil {
				events := selectableEventsInGroups(buildTreeTimeline(s.detail))
				total := len(events)
				if s.timelineCursor >= total && total > 0 {
					s.timelineCursor = total - 1
				}
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

	case duplicateResultMsg:
		if msg.err != nil {
			s.statusMsg = "duplicate failed: " + msg.err.Error()
			s.isError = true
			return s, clearStatusAfter(4 * time.Second)
		}
		return s, func() tea.Msg {
			return pushScreenMsg{screen: NewTaskDetailScreen(s.shared, msg.newTaskID, s.projectName)}
		}

	case duplicateConfirmDeadlineMsg:
		if s.duplicatePending {
			s.duplicatePending = false
			s.statusMsg = ""
			s.isError = false
		}

	case rerunResultMsg:
		if msg.err != nil {
			s.statusMsg = "rerun failed: " + msg.err.Error()
			s.isError = true
			return s, clearStatusAfter(4 * time.Second)
		}
		s.rerunPending = false
		s.statusMsg = "rerun started"
		s.isError = false
		return s, tea.Batch(
			fetchTaskDetailCmd(s.shared.Client, s.taskID),
			clearStatusAfter(3*time.Second),
		)

	case rerunConfirmDeadlineMsg:
		if s.rerunPending {
			s.rerunPending = false
			s.statusMsg = ""
			s.isError = false
		}

	case titleUpdateResultMsg:
		s.statusMsg = ""
		s.isError = false
		if msg.err != nil {
			s.statusMsg = "save failed: " + msg.err.Error()
			s.isError = true
			return s, clearStatusAfter(4 * time.Second)
		}
		return s, fetchTaskDetailCmd(s.shared.Client, s.taskID)

	case screenResumedMsg:
		s.loading = true
		return s, tea.Batch(
			fetchTaskDetailCmd(s.shared.Client, s.taskID),
			taskDetailTickCmd(),
		)

	case tea.KeyMsg:
		if s.titleEditing {
			return s, s.handleTitleEditKey(msg)
		}
		return s, s.handleKey(msg)
	}

	return s, nil
}

func (s *TaskDetailScreen) handleTitleEditKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		title := strings.TrimSpace(s.titleInput.Value())
		s.titleEditing = false
		s.titleInput.Blur()
		if title == "" {
			return nil
		}
		s.statusMsg = "saving..."
		s.isError = false
		return updateTitleCmd(s.shared.Client, s.taskID, title)
	case "esc":
		s.titleEditing = false
		s.titleInput.Blur()
		return nil
	default:
		var cmd tea.Cmd
		s.titleInput, cmd = s.titleInput.Update(msg)
		return cmd
	}
}

func (s *TaskDetailScreen) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "tab":
		s.activeTab = cycleTab(s.activeTab, 1)

	case "shift+tab":
		s.activeTab = cycleTab(s.activeTab, -1)

	case "j", "down":
		switch s.activeTab {
		case tabOverview:
			events := selectableEventsInGroups(buildTreeTimeline(s.detail))
			total := len(events)
			if s.timelineCursor < total-1 {
				s.timelineCursor++
			}
		case tabDescription:
			if s.detail != nil && s.detail.Task != nil {
				lines := wrapLines(s.detail.Task.Description, s.descWidth)
				if s.descScroll < len(lines)-1 {
					s.descScroll++
				}
			}
		case tabDeps:
			items := depSelectableItems(s.detail)
			if s.depsCursor < len(items)-1 {
				s.depsCursor++
			}
		case tabPayload:
			if s.detail != nil && s.detail.Task != nil {
				sections := extractPayloadSections(s.detail.Task.Payload)
				if s.payloadCursor < len(sections)-1 {
					s.payloadCursor++
					s.payloadScroll = 0
				}
			}
		case tabInstructions:
			if s.detail != nil && s.detail.Task != nil {
				roles := extractInstructionRoles(s.detail.Task.Instructions)
				if s.instructionsCursor < len(roles)-1 {
					s.instructionsCursor++
					s.instructionsScroll = 0
				}
			}
		}

	case "k", "up":
		switch s.activeTab {
		case tabOverview:
			if s.timelineCursor > 0 {
				s.timelineCursor--
			}
		case tabDescription:
			if s.descScroll > 0 {
				s.descScroll--
			}
		case tabDeps:
			if s.depsCursor > 0 {
				s.depsCursor--
			}
		case tabPayload:
			if s.payloadCursor > 0 {
				s.payloadCursor--
				s.payloadScroll = 0
			}
		case tabInstructions:
			if s.instructionsCursor > 0 {
				s.instructionsCursor--
				s.instructionsScroll = 0
			}
		}

	case "pgdown", "ctrl+f":
		if s.activeTab == tabDescription {
			if s.detail != nil && s.detail.Task != nil {
				lines := wrapLines(s.detail.Task.Description, s.descWidth)
				pageSize := max(s.descPageHeight, 1)
				s.descScroll = min(s.descScroll+pageSize, max(len(lines)-1, 0))
			}
		}

	case "pgup", "ctrl+b":
		if s.activeTab == tabDescription {
			pageSize := max(s.descPageHeight, 1)
			s.descScroll = max(s.descScroll-pageSize, 0)
		}

	case "r":
		s.loading = true
		return fetchTaskDetailCmd(s.shared.Client, s.taskID)

	case "enter":
		if s.detail == nil {
			break
		}
		// Deps tab: navigate to the selected dependency / dependent task.
		if s.activeTab == tabDeps {
			items := depSelectableItems(s.detail)
			if s.depsCursor >= 0 && s.depsCursor < len(items) {
				task := items[s.depsCursor]
				// Use ProjectID as project name (resolution is a future improvement).
				projectName := task.ProjectID
				return func() tea.Msg {
					return pushScreenMsg{screen: NewTaskDetailScreen(s.shared, task.ID, projectName)}
				}
			}
			break
		}
		// Payload tab: no-op.
		if s.activeTab == tabPayload {
			break
		}
		// Overview tab: drill into the selected Timeline event (running or completed/failed job).
		if s.activeTab == tabOverview {
			events := selectableEventsInGroups(buildTreeTimeline(s.detail))
			if s.timelineCursor >= 0 && s.timelineCursor < len(events) {
				ev := events[s.timelineCursor]
				if ev.Job != nil {
					if job := s.findAPIJob(ev.Job.ID); job != nil {
						return func() tea.Msg {
							return pushScreenMsg{screen: NewJobDetailScreen(s.shared, job)}
						}
					}
				}
			}
			break
		}

	case "e":
		if s.detail == nil || s.detail.Task == nil {
			break
		}
		if s.activeTab == tabDescription {
			task := s.detail.Task
			return func() tea.Msg {
				return pushScreenMsg{screen: NewDescriptionScreen(s.shared.Client, task)}
			}
		}
		if s.activeTab == tabPayload {
			sections := extractPayloadSections(s.detail.Task.Payload)
			if len(sections) == 0 || s.payloadCursor >= len(sections) {
				break
			}
			sectionKey := sections[s.payloadCursor].key
			task := s.detail.Task
			return func() tea.Msg {
				return pushScreenMsg{screen: NewPayloadSectionEditScreen(s.shared.Client, task, sectionKey)}
			}
		}
		if s.activeTab == tabInstructions {
			roles := extractInstructionRoles(s.detail.Task.Instructions)
			if len(roles) == 0 || s.instructionsCursor >= len(roles) {
				break
			}
			role := roles[s.instructionsCursor].role
			task := s.detail.Task
			return func() tea.Msg {
				return pushScreenMsg{screen: NewInstructionsRoleEditScreen(s.shared.Client, task, role)}
			}
		}
		// Default: start inline title editing.
		s.titleEditing = true
		s.titleInput = NewTextField()
		s.titleInput.SetLabel("edit title")
		s.titleInput.SetValue(s.detail.Task.Title)
		s.statusMsg = ""
		s.isError = false
		return s.titleInput.Focus()

	case "o":
		// Open the selected running job in a tmux pane.
		// Only effective when Overview tab is active and cursor is on a running Timeline event.
		if s.activeTab != tabOverview {
			break
		}
		events := selectableEventsInGroups(buildTreeTimeline(s.detail))
		if s.timelineCursor < 0 || s.timelineCursor >= len(events) {
			break
		}
		ev := events[s.timelineCursor]
		if ev.Job == nil || ev.Job.Status != timeline.JobStatusRunning {
			break
		}
		job := s.findAPIJob(ev.Job.ID)
		if job == nil {
			break
		}
		if !s.shared.TmuxEnabled {
			s.statusMsg = "to open a job, launch `boid tui` inside tmux"
			s.isError = false
			return clearStatusAfter(4 * time.Second)
		}
		return openJobCmd(job.ID, s.shared.Panes[job.ID])

	case "esc", "backspace":
		return func() tea.Msg { return popScreenMsg{} }

	case "q":
		return tea.Quit

	case "d":
		if s.deletePending {
			s.deletePending = false
			return deleteTaskCmd(s.shared.Client, s.taskID)
		}
		s.deletePending = true
		s.statusMsg = "Press d again to delete"
		s.isError = false
		return tea.Tick(s.confirmInterval(), func(time.Time) tea.Msg {
			return deleteConfirmDeadlineMsg{}
		})

	case "D":
		if s.duplicatePending {
			s.duplicatePending = false
			return duplicateTaskCmd(s.shared.Client, s.taskID)
		}
		s.duplicatePending = true
		s.statusMsg = "Press D again to duplicate"
		s.isError = false
		return tea.Tick(s.confirmInterval(), func(time.Time) tea.Msg {
			return duplicateConfirmDeadlineMsg{}
		})

	case "R":
		if s.detail == nil || s.detail.Task == nil {
			break
		}
		status := string(s.detail.Task.Status)
		if status != "done" && status != "aborted" {
			break
		}
		if s.rerunPending {
			s.rerunPending = false
			return rerunTaskCmd(s.shared.Client, s.taskID)
		}
		s.rerunPending = true
		s.statusMsg = "Press R again to rerun"
		s.isError = false
		return tea.Tick(s.confirmInterval(), func(time.Time) tea.Msg {
			return rerunConfirmDeadlineMsg{}
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
			return tea.Tick(s.confirmInterval(), func(time.Time) tea.Msg {
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

	// --- tab bar (1 line) ---
	openFindingsCount := 0
	if s.detail != nil && s.detail.Task != nil {
		openFindingsCount = len(parseOpenFindings(s.detail.Task.Payload))
	}
	sb.WriteString(renderTabBar(s.activeTab, openFindingsCount, width))
	sb.WriteByte('\n')

	// --- sub-header: title + status (1 line) ---
	if s.detail != nil && s.detail.Task != nil {
		task := s.detail.Task
		_, statusText := taskStatusDisplay(task.Status)
		if isBlinkTarget(task.Status) && !s.shared.BlinkOn {
			statusText = styleTaskDim.Render(string(task.Status))
		}
		maxTitleWidth := max(width-lipgloss.Width(statusText)-1, 10)
		titleStr := styleTitle.Render(truncate(task.Title, maxTitleWidth))
		gap := max(width-lipgloss.Width(titleStr)-lipgloss.Width(statusText), 1)
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

	// --- fetch error ---
	if s.fetchErr != nil {
		sb.WriteString(styleError.Render(fmt.Sprintf("  error: %v", s.fetchErr)))
		sb.WriteByte('\n')
		return sb.String()
	}

	// Height budget: tab bar (1) + title (1) + meta (1) = 3 lines used
	statusLines := 0
	if s.statusMsg != "" || s.titleEditing {
		statusLines = 2
	}
	contentHeight := max(height-3-statusLines, 4)

	// --- tab content ---
	switch s.activeTab {
	case tabOverview:
		sb.WriteString(s.renderOverview(width, contentHeight))
	case tabDescription:
		s.descPageHeight = contentHeight
		s.descWidth = width
		sb.WriteString(renderDescription(s.detail, s.descScroll, width, contentHeight))
	case tabDeps:
		sb.WriteString(renderDeps(s.detail, width, contentHeight, s.depsCursor))
	case tabPayload:
		sb.WriteString(renderPayload(s.detail, s.payloadCursor, s.payloadScroll, width, contentHeight))
	case tabInstructions:
		sb.WriteString(renderInstructions(s.detail, s.instructionsCursor, s.instructionsScroll, width, contentHeight))
	}

	// --- inline status message / title edit ---
	if s.titleEditing {
		sb.WriteString(s.titleInput.View())
		sb.WriteString(styleDim.Render("              (Enter: save  Esc: cancel)"))
		sb.WriteByte('\n')
	} else if s.statusMsg != "" {
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

// confirmInterval returns the tick interval for confirm-deadline timers,
// mirroring the polling interval: 1s when the displayed task is active, 3s otherwise.
func (s *TaskDetailScreen) confirmInterval() time.Duration {
	if s.detail != nil && s.detail.Task != nil {
		return tickIntervalForDetail(s.detail.Task.Status)
	}
	return idleTaskDetailPollInterval
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
	parts = append(parts, "d: delete", "D: duplicate")
	if s.detail != nil && s.detail.Task != nil {
		status := string(s.detail.Task.Status)
		if status == "done" || status == "aborted" {
			parts = append(parts, "R: rerun")
		}
	}

	fixed := "tab/shift+tab: switch tab  r: refresh  esc: back  q: quit"

	var tabSpecific string
	switch s.activeTab {
	case tabOverview:
		events := selectableEventsInGroups(buildTreeTimeline(s.detail))
		if s.timelineCursor >= 0 && s.timelineCursor < len(events) {
			ev := events[s.timelineCursor]
			if ev.Job != nil && ev.Job.Status == timeline.JobStatusRunning {
				tabSpecific = "e: edit title  enter: open job detail  o: open in tmux  j/k: scroll cursor"
				break
			}
		}
		tabSpecific = "e: edit title  enter: drill into event  j/k: scroll cursor"
	case tabDescription:
		tabSpecific = "e: edit description  j/k: scroll  pgup/pgdn: page"
	case tabDeps:
		tabSpecific = "enter: jump to task  j/k: move cursor"
	case tabPayload:
		tabSpecific = "e: edit section  j/k: select section"
	case tabInstructions:
		tabSpecific = "e: edit role  j/k: select role"
	}

	return strings.Join(parts, "  ") + "  " + fixed + "  " + tabSpecific
}

// assignKeys assigns a single-character key to each action name.
// The first unused character of the action name is used as the key.
// Key 'd' is reserved for the delete shortcut and cannot be assigned to actions.
//
// "done" は意図的に除外する。executing から done への遷移は single keypress で
// 走らせると、実行中の hook を誤って停止させて worktree/branch ごと破棄してしまう。
// done は lifecycle.executed による自動遷移、あるいは将来導入する専用モーダル経由で
// 行う設計にする。
func assignKeys(actions []string) map[rune]string {
	m := map[rune]string{}
	for _, a := range actions {
		if a == "done" {
			continue
		}
		for _, ch := range a {
			if ch == 'd' || ch == 'o' { // reserved: 'd' for delete, 'o' for tmux open
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
	return tea.Tick(idleTaskDetailPollInterval, func(time.Time) tea.Msg {
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

func duplicateTaskCmd(c *client.Client, taskID string) tea.Cmd {
	return func() tea.Msg {
		task, err := c.DuplicateTask(taskID)
		if err != nil {
			return duplicateResultMsg{err: err}
		}
		return duplicateResultMsg{newTaskID: task.ID}
	}
}

func rerunTaskCmd(c *client.Client, taskID string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.RerunTask(taskID, false)
		return rerunResultMsg{err: err}
	}
}

func updateTitleCmd(c *client.Client, taskID, title string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.UpdateTask(taskID, api.UpdateTaskRequest{Title: title})
		return titleUpdateResultMsg{err: err}
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
