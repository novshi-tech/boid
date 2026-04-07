package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
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
	titleInput    textinput.Model
	descArea      textarea.Model

	focus      formFocus
	errMsg     string
	submitting bool
}

func NewTaskFormScreen(shared *SharedState) *TaskFormScreen {
	ti := textinput.New()
	ti.Placeholder = "Task title"

	ta := textarea.New()
	ta.Placeholder = "Description (optional)"
	ta.SetHeight(4)
	ta.ShowLineNumbers = false

	return &TaskFormScreen{
		shared:        shared,
		projectField:  newSelectField("Project"),
		behaviorField: newSelectField("Behavior"),
		titleInput:    ti,
		descArea:      ta,
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

// moveFocus calls Blur() on the previously focused field and Focus() on the new one.
func (s *TaskFormScreen) moveFocus(newFocus formFocus) tea.Cmd {
	switch s.focus {
	case focusTitle:
		s.titleInput.Blur()
	case focusDescription:
		s.descArea.Blur()
	}
	s.focus = newFocus
	switch newFocus {
	case focusTitle:
		return s.titleInput.Focus()
	case focusDescription:
		return s.descArea.Focus()
	}
	return nil
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

	// Tab / Shift+Tab for focus navigation (blocked while a selector is expanded).
	if !s.projectField.expanded && !s.behaviorField.expanded {
		switch msg.String() {
		case "tab":
			return s.moveFocus((s.focus + 1) % focusCount)
		case "shift+tab":
			return s.moveFocus((s.focus - 1 + focusCount) % focusCount)
		}
	}

	// Backspace: text input フォーカス中は bubbles に委譲（switch へ fall-through）、
	// それ以外は前の画面に戻る。
	if msg.Type == tea.KeyBackspace || msg.Type == tea.KeyCtrlH {
		if s.focus != focusTitle && s.focus != focusDescription {
			return func() tea.Msg { return popScreenMsg{} }
		}
		// fall through to the switch below for text fields
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
		var cmd tea.Cmd
		s.titleInput, cmd = s.titleInput.Update(msg)
		return cmd

	case focusDescription:
		var cmd tea.Cmd
		s.descArea, cmd = s.descArea.Update(msg)
		return cmd

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
	{
		labelStr := fmt.Sprintf("%-10s", "Title:")
		cursor := " "
		if s.focus == focusTitle {
			cursor = styleCursor.Render("▸")
		}
		sb.WriteString("  " + labelStr + " " + cursor + " " + s.titleInput.View() + "\n")
	}

	// Description textarea
	{
		labelStr := fmt.Sprintf("%-10s", "Desc:")
		cursor := " "
		if s.focus == focusDescription {
			cursor = styleCursor.Render("▸")
		}
		sb.WriteString("  " + labelStr + " " + cursor + "\n")
		sb.WriteString(s.descArea.View() + "\n")
	}

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
