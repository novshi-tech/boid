package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- focus constants ---

type formFocus int

const (
	focusProject formFocus = iota
	focusBehavior
	focusTitle
	focusDescription
	focusSubmit
	focusCancel
	focusCount
)

// --- selectField ---

type selectOption struct {
	Value string
	Label string
}

type selectField struct {
	label    string
	options  []selectOption
	selected int // -1 = unselected
	expanded bool
	cursor   int
}

func newSelectField(label string) selectField {
	return selectField{label: label, selected: -1}
}

func (f *selectField) selectedValue() string {
	if f.selected < 0 || f.selected >= len(f.options) {
		return ""
	}
	return f.options[f.selected].Value
}

func (f *selectField) selectedLabel() string {
	if f.selected < 0 || f.selected >= len(f.options) {
		return ""
	}
	return f.options[f.selected].Label
}

func (f *selectField) handleKey(msg tea.KeyMsg) {
	if !f.expanded {
		if msg.String() == "enter" && len(f.options) > 0 {
			f.expanded = true
			if f.selected >= 0 {
				f.cursor = f.selected
			} else {
				f.cursor = 0
			}
		}
		return
	}
	switch msg.String() {
	case "j", "down":
		if f.cursor < len(f.options)-1 {
			f.cursor++
		}
	case "k", "up":
		if f.cursor > 0 {
			f.cursor--
		}
	case "enter":
		f.selected = f.cursor
		f.expanded = false
	case "esc":
		f.expanded = false
	}
}

// --- simpleInput: single-line text input ---

type simpleInput struct {
	value       []rune
	placeholder string
}

func (t *simpleInput) handleKey(msg tea.KeyMsg) {
	switch msg.Type {
	case tea.KeyBackspace, tea.KeyCtrlH:
		if len(t.value) > 0 {
			t.value = t.value[:len(t.value)-1]
		}
	case tea.KeyRunes, tea.KeySpace:
		t.value = append(t.value, msg.Runes...)
	}
}

func (t *simpleInput) Value() string {
	return string(t.value)
}

// --- simpleTextArea: multi-line text area ---

type simpleTextArea struct {
	lines       []string
	placeholder string
	height      int
}

func newSimpleTextArea(placeholder string, height int) simpleTextArea {
	return simpleTextArea{
		lines:       []string{""},
		placeholder: placeholder,
		height:      height,
	}
}

func (a *simpleTextArea) handleKey(msg tea.KeyMsg) {
	switch msg.Type {
	case tea.KeyEnter:
		a.lines = append(a.lines, "")
	case tea.KeyBackspace, tea.KeyCtrlH:
		last := len(a.lines) - 1
		if len(a.lines[last]) > 0 {
			runes := []rune(a.lines[last])
			a.lines[last] = string(runes[:len(runes)-1])
		} else if last > 0 {
			a.lines = a.lines[:last]
		}
	case tea.KeyRunes, tea.KeySpace:
		last := len(a.lines) - 1
		a.lines[last] += string(msg.Runes)
	}
}

func (a *simpleTextArea) Value() string {
	return strings.Join(a.lines, "\n")
}

// --- messages ---

type projectsLoadedMsg struct {
	projects []*orchestrator.Project
	err      error
}

type behaviorsLoadedMsg struct {
	behaviors []string
	err       error
}

type taskCreatedMsg struct {
	task *orchestrator.Task
	err  error
}

// taskCreatedNotifyMsg is dispatched after a successful task creation so that
// the underlying TaskListScreen can display a confirmation message.
type taskCreatedNotifyMsg struct{}

// --- TaskFormScreen ---

type TaskFormScreen struct {
	shared *SharedState

	projectField  selectField
	behaviorField selectField
	titleInput    simpleInput
	descArea      simpleTextArea

	focus     formFocus
	errMsg    string
	submitting bool
}

func NewTaskFormScreen(shared *SharedState) *TaskFormScreen {
	return &TaskFormScreen{
		shared:        shared,
		projectField:  newSelectField("Project"),
		behaviorField: newSelectField("Behavior"),
		titleInput:    simpleInput{placeholder: "Task title"},
		descArea:      newSimpleTextArea("Description (optional)", 4),
		focus:         focusProject,
	}
}

func (s *TaskFormScreen) Init() tea.Cmd {
	return fetchProjectsForFormCmd(s.shared.Client)
}

func (s *TaskFormScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case projectsLoadedMsg:
		if msg.err == nil {
			opts := make([]selectOption, len(msg.projects))
			for i, p := range msg.projects {
				opts[i] = selectOption{Value: p.ID, Label: p.Meta.Name}
			}
			s.projectField.options = opts
		}

	case behaviorsLoadedMsg:
		if msg.err == nil {
			opts := make([]selectOption, len(msg.behaviors))
			for i, b := range msg.behaviors {
				opts[i] = selectOption{Value: b, Label: b}
			}
			s.behaviorField.options = opts
		}
		s.behaviorField.selected = -1

	case taskCreatedMsg:
		s.submitting = false
		if msg.err != nil {
			s.errMsg = "create failed: " + msg.err.Error()
			return s, nil
		}
		return s, tea.Batch(
			func() tea.Msg { return popScreenMsg{} },
			func() tea.Msg { return taskCreatedNotifyMsg{} },
		)

	case tea.KeyMsg:
		return s, s.handleKey(msg)
	}
	return s, nil
}

func (s *TaskFormScreen) handleKey(msg tea.KeyMsg) tea.Cmd {
	// Esc: close expanded selector first, otherwise pop screen.
	if msg.Type == tea.KeyEsc {
		if s.projectField.expanded {
			s.projectField.expanded = false
			return nil
		}
		if s.behaviorField.expanded {
			s.behaviorField.expanded = false
			return nil
		}
		return func() tea.Msg { return popScreenMsg{} }
	}

	// Backspace: text input フォーカス中はフィールドに委譲、それ以外は pop。
	if msg.Type == tea.KeyBackspace || msg.Type == tea.KeyCtrlH {
		switch s.focus {
		case focusTitle:
			s.titleInput.handleKey(msg)
			return nil
		case focusDescription:
			s.descArea.handleKey(msg)
			return nil
		default:
			return func() tea.Msg { return popScreenMsg{} }
		}
	}

	// Tab / Shift+Tab for focus navigation (blocked while a selector is expanded).
	if !s.projectField.expanded && !s.behaviorField.expanded {
		switch msg.String() {
		case "tab":
			s.focus = (s.focus + 1) % focusCount
			return nil
		case "shift+tab":
			s.focus = (s.focus - 1 + focusCount) % focusCount
			return nil
		}
	}

	switch s.focus {
	case focusProject:
		prevSelected := s.projectField.selected
		s.projectField.handleKey(msg)
		// When project changes, reset behavior and fetch new behaviors.
		if s.projectField.selected != prevSelected && s.projectField.selected >= 0 {
			s.behaviorField.selected = -1
			s.behaviorField.options = nil
			return fetchBehaviorsCmd(s.shared.Client, s.projectField.selectedValue())
		}

	case focusBehavior:
		if s.projectField.selected < 0 {
			return nil // project must be selected first
		}
		s.behaviorField.handleKey(msg)

	case focusTitle:
		s.titleInput.handleKey(msg)

	case focusDescription:
		// Tab / shift+tab are already consumed above; pass everything else.
		s.descArea.handleKey(msg)

	case focusSubmit:
		if msg.String() == "enter" {
			return s.submit()
		}

	case focusCancel:
		if msg.String() == "enter" {
			return func() tea.Msg { return popScreenMsg{} }
		}
	}
	return nil
}

func (s *TaskFormScreen) submit() tea.Cmd {
	if s.projectField.selected < 0 {
		s.errMsg = "project is required"
		return nil
	}
	if s.behaviorField.selected < 0 {
		s.errMsg = "behavior is required"
		return nil
	}
	title := strings.TrimSpace(s.titleInput.Value())
	if title == "" {
		s.errMsg = "title is required"
		return nil
	}
	s.errMsg = ""
	s.submitting = true
	req := api.CreateTaskRequest{
		ProjectID:   s.projectField.selectedValue(),
		Behavior:    s.behaviorField.selectedValue(),
		Title:       title,
		Description: s.descArea.Value(),
	}
	return createTaskCmd(s.shared.Client, req)
}

func (s *TaskFormScreen) View(width, height int) string {
	var sb strings.Builder

	sb.WriteString(styleTitle.Render("New Task"))
	sb.WriteByte('\n')
	sb.WriteString(strings.Repeat("─", width))
	sb.WriteByte('\n')

	// Project selector
	sb.WriteString(s.viewSelectField(&s.projectField, s.focus == focusProject, "(select project)", width))

	// Behavior selector
	behPlaceholder := "(select project first)"
	if s.projectField.selected >= 0 {
		behPlaceholder = "(select behavior)"
	}
	sb.WriteString(s.viewSelectField(&s.behaviorField, s.focus == focusBehavior, behPlaceholder, width))

	// Title input
	sb.WriteString(viewInputLine("Title", s.titleInput.Value(), s.titleInput.placeholder, s.focus == focusTitle, width))

	// Description textarea
	sb.WriteString(s.viewTextArea(width))

	// Buttons
	sb.WriteString(s.viewButtons())

	// Error message
	if s.errMsg != "" {
		sb.WriteString(styleError.Render("  ! " + s.errMsg))
		sb.WriteByte('\n')
	}

	_ = height
	return sb.String()
}

func (s *TaskFormScreen) viewSelectField(f *selectField, focused bool, placeholder string, width int) string {
	var sb strings.Builder

	labelStr := fmt.Sprintf("%-10s", f.label+":")
	cursor := " "
	if focused {
		cursor = styleCursor.Render("▸")
	}

	sel := f.selectedLabel()
	if sel == "" {
		if focused {
			sel = placeholder
		} else {
			sel = styleDim.Render(placeholder)
		}
	} else if focused {
		sel = styleTitle.Render(sel)
	}

	sb.WriteString("  " + labelStr + " " + cursor + " " + sel + "\n")

	// Inline expanded list
	if f.expanded {
		for i, opt := range f.options {
			if i == f.cursor {
				sb.WriteString("             " + styleCursor.Render("▸ "+opt.Label) + "\n")
			} else {
				sb.WriteString("               " + styleDim.Render(opt.Label) + "\n")
			}
		}
	}

	_ = width
	return sb.String()
}

func viewInputLine(label, value, placeholder string, focused bool, width int) string {
	labelStr := fmt.Sprintf("%-10s", label+":")
	cursor := " "
	if focused {
		cursor = styleCursor.Render("▸")
	}

	// prefix: "  " + 10-char label + " X [" = 16 chars; suffix: "]" = 1 char
	inputWidth := width - 16
	if inputWidth < 10 {
		inputWidth = 10
	}

	var displayVal string
	if value == "" {
		if focused {
			displayVal = "█"
		} else {
			displayVal = styleDim.Render(placeholder)
		}
	} else if focused {
		r := []rune(value)
		if len(r) > inputWidth-1 {
			displayVal = string(r[len(r)-(inputWidth-1):]) + "█"
		} else {
			displayVal = value + "█"
		}
	} else {
		displayVal = value
	}

	// Right-pad to inputWidth using visible-char width.
	visible := lipglossWidth(displayVal)
	if visible < inputWidth {
		displayVal += strings.Repeat(" ", inputWidth-visible)
	}

	return "  " + labelStr + " " + cursor + " [" + displayVal + "]\n"
}

func (s *TaskFormScreen) viewTextArea(width int) string {
	var sb strings.Builder

	labelStr := fmt.Sprintf("%-10s", "Desc:")
	cursor := " "
	if s.focus == focusDescription {
		cursor = styleCursor.Render("▸")
	}

	inputWidth := width - 16
	if inputWidth < 10 {
		inputWidth = 10
	}

	h := s.descArea.height
	srcLines := s.descArea.lines

	// Show the last h lines when content overflows.
	start := 0
	if len(srcLines) > h {
		start = len(srcLines) - h
	}
	displayLines := make([]string, h)
	for i, l := range srcLines[start:] {
		if i < h {
			displayLines[i] = l
		}
	}

	// Append cursor block to the last active display line.
	if s.focus == focusDescription {
		lastDisplayIdx := len(srcLines) - 1 - start
		if lastDisplayIdx >= 0 && lastDisplayIdx < h {
			displayLines[lastDisplayIdx] += "█"
		}
	}

	for i, l := range displayLines {
		visible := lipglossWidth(l)
		if visible < inputWidth {
			l += strings.Repeat(" ", inputWidth-visible)
		}
		if i == 0 {
			sb.WriteString("  " + labelStr + " " + cursor + " [" + l + "]\n")
		} else {
			sb.WriteString("  " + fmt.Sprintf("%-10s", "") + "   [" + l + "]\n")
		}
	}

	return sb.String()
}

func (s *TaskFormScreen) viewButtons() string {
	create := "[Create]"
	cancel := "[Cancel]"
	if s.focus == focusSubmit {
		create = styleCursor.Render("[Create]")
	}
	if s.focus == focusCancel {
		cancel = styleCursor.Render("[Cancel]")
	}
	return "\n  " + create + "    " + cancel + "\n"
}

func (s *TaskFormScreen) ShortHelp() string {
	return "tab: next  shift-tab: prev  enter: submit/select  esc: cancel"
}

// --- commands ---

func fetchProjectsForFormCmd(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		projects, err := c.ListProjects()
		return projectsLoadedMsg{projects: projects, err: err}
	}
}

func fetchBehaviorsCmd(c *client.Client, projectID string) tea.Cmd {
	return func() tea.Msg {
		project, err := c.GetProject(projectID)
		if err != nil {
			return behaviorsLoadedMsg{err: err}
		}
		var behaviors []string
		for k := range project.Meta.TaskBehaviors {
			behaviors = append(behaviors, k)
		}
		sort.Strings(behaviors)
		return behaviorsLoadedMsg{behaviors: behaviors}
	}
}

func createTaskCmd(c *client.Client, req api.CreateTaskRequest) tea.Cmd {
	return func() tea.Msg {
		task, err := c.CreateTask(req)
		return taskCreatedMsg{task: task, err: err}
	}
}
