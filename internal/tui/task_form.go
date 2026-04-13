package tui

import (
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
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
	focusAutoStart
	focusSubmit
	focusCancel
	focusCount
)

// --- key map ---

type taskFormKeyMap struct {
	Tab      key.Binding
	ShiftTab key.Binding
	Enter    key.Binding
	Esc      key.Binding
	Back     key.Binding
	Up       key.Binding
	Down     key.Binding
}

func (k taskFormKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Tab, k.Enter, k.Esc}
}

func (k taskFormKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Tab, k.ShiftTab, k.Enter, k.Esc, k.Back}}
}

var defaultTaskFormKeys = taskFormKeyMap{
	Tab: key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "next"),
	),
	ShiftTab: key.NewBinding(
		key.WithKeys("shift+tab"),
		key.WithHelp("shift-tab", "prev"),
	),
	Enter: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "submit/select"),
	),
	Esc: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "cancel"),
	),
	Back: key.NewBinding(
		key.WithKeys("backspace"),
		key.WithHelp("backspace", "back"),
	),
	Up:   key.NewBinding(key.WithKeys("k", "up")),
	Down: key.NewBinding(key.WithKeys("j", "down")),
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

	projectField   SelectModel
	behaviorField  SelectModel
	titleField     TextFieldModel
	descArea       TextAreaModel
	autoStartField CheckboxModel
	createBtn      ButtonModel
	cancelBtn      ButtonModel

	focus      formFocus
	errMsg     string
	submitting bool

	keys taskFormKeyMap
	help help.Model
}

func NewTaskFormScreen(shared *SharedState) *TaskFormScreen {
	tf := NewTextField()
	tf.SetLabel("Title")
	tf.SetPlaceholder("Task title")

	ta := NewTextArea()
	ta.SetLabel("Desc")
	ta.SetPlaceholder("Description (optional)")
	ta.SetHeight(4)

	pf := NewSelect()
	pf.SetLabel("Project")
	pf.SetPlaceholder("(select project)")
	pf.Focus() // initial focus

	bf := NewSelect()
	bf.SetLabel("Behavior")
	bf.SetPlaceholder("(select project first)")

	s := &TaskFormScreen{
		shared:         shared,
		projectField:   pf,
		behaviorField:  bf,
		titleField:     tf,
		descArea:       ta,
		autoStartField: NewCheckbox("Auto start"),
		createBtn:      NewButton("Create"),
		cancelBtn:      NewButton("Cancel"),
		focus:          focusProject,
		keys:           defaultTaskFormKeys,
		help:           help.New(),
	}
	return s
}

func (s *TaskFormScreen) Init() tea.Cmd {
	return fetchProjectsForFormCmd(s.shared.Client)
}

func (s *TaskFormScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case projectsLoadedMsg:
		if msg.err == nil {
			opts := make([]SelectOption, len(msg.projects))
			for i, p := range msg.projects {
				opts[i] = SelectOption{Value: p.ID, Label: p.Meta.Name}
			}
			s.projectField.SetOptions(opts)
		}

	case behaviorsLoadedMsg:
		if msg.err == nil {
			opts := make([]SelectOption, len(msg.behaviors))
			for i, b := range msg.behaviors {
				opts[i] = SelectOption{Value: b, Label: b}
			}
			s.behaviorField.SetOptions(opts)
		}
		s.behaviorField.ResetSelection()

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

	case ButtonPressedMsg:
		switch msg.Label {
		case "Create":
			return s, s.submit()
		case "Cancel":
			return s, func() tea.Msg { return popScreenMsg{} }
		}

	case tea.KeyMsg:
		return s, s.handleKey(msg)
	}
	return s, nil
}

// moveFocus calls Blur() on the previously focused field and Focus() on the new one.
func (s *TaskFormScreen) moveFocus(newFocus formFocus) tea.Cmd {
	// blur current
	switch s.focus {
	case focusProject:
		s.projectField.Blur()
	case focusBehavior:
		s.behaviorField.Blur()
	case focusTitle:
		s.titleField.Blur()
	case focusDescription:
		s.descArea.Blur()
	case focusAutoStart:
		s.autoStartField.Blur()
	case focusSubmit:
		s.createBtn.Blur()
	case focusCancel:
		s.cancelBtn.Blur()
	}
	s.focus = newFocus
	// focus new
	switch newFocus {
	case focusProject:
		return s.projectField.Focus()
	case focusBehavior:
		return s.behaviorField.Focus()
	case focusTitle:
		return s.titleField.Focus()
	case focusDescription:
		return s.descArea.Focus()
	case focusAutoStart:
		return s.autoStartField.Focus()
	case focusSubmit:
		return s.createBtn.Focus()
	case focusCancel:
		return s.cancelBtn.Focus()
	}
	return nil
}

func (s *TaskFormScreen) handleKey(msg tea.KeyMsg) tea.Cmd {
	// Esc: close expanded selector first; otherwise pop screen.
	if key.Matches(msg, s.keys.Esc) {
		if s.projectField.Expanded() {
			s.projectField, _ = s.projectField.Update(msg)
			return nil
		}
		if s.behaviorField.Expanded() {
			s.behaviorField, _ = s.behaviorField.Update(msg)
			return nil
		}
		return func() tea.Msg { return popScreenMsg{} }
	}

	// Tab / Shift+Tab for focus navigation (blocked while a selector is expanded).
	if !s.projectField.Expanded() && !s.behaviorField.Expanded() {
		if key.Matches(msg, s.keys.Tab) {
			return s.moveFocus((s.focus + 1) % focusCount)
		}
		if key.Matches(msg, s.keys.ShiftTab) {
			return s.moveFocus((s.focus - 1 + focusCount) % focusCount)
		}
	}

	// Backspace: text input フォーカス中は bubbles に委譲、それ以外は前の画面に戻る。
	if msg.Type == tea.KeyBackspace || msg.Type == tea.KeyCtrlH {
		if s.focus != focusTitle && s.focus != focusDescription {
			return func() tea.Msg { return popScreenMsg{} }
		}
		// fall through to the switch below for text fields
	}

	switch s.focus {
	case focusProject:
		prevValue := s.projectField.Value()
		var selectCmd tea.Cmd
		s.projectField, selectCmd = s.projectField.Update(msg)
		newValue := s.projectField.Value()
		if newValue != prevValue && newValue != "" {
			s.behaviorField.ResetSelection()
			s.behaviorField.ClearOptions()
			return tea.Batch(selectCmd, fetchBehaviorsCmd(s.shared.Client, newValue))
		}
		return selectCmd

	case focusBehavior:
		if s.projectField.Value() == "" {
			return nil // project must be selected first
		}
		var cmd tea.Cmd
		s.behaviorField, cmd = s.behaviorField.Update(msg)
		return cmd

	case focusTitle:
		var cmd tea.Cmd
		s.titleField, cmd = s.titleField.Update(msg)
		return cmd

	case focusDescription:
		var cmd tea.Cmd
		s.descArea, cmd = s.descArea.Update(msg)
		return cmd

	case focusAutoStart:
		var cmd tea.Cmd
		s.autoStartField, cmd = s.autoStartField.Update(msg)
		return cmd

	case focusSubmit:
		var cmd tea.Cmd
		s.createBtn, cmd = s.createBtn.Update(msg)
		return cmd

	case focusCancel:
		var cmd tea.Cmd
		s.cancelBtn, cmd = s.cancelBtn.Update(msg)
		return cmd
	}
	return nil
}

func (s *TaskFormScreen) submit() tea.Cmd {
	if s.projectField.Value() == "" {
		s.errMsg = "project is required"
		return nil
	}
	if s.behaviorField.Value() == "" {
		s.errMsg = "behavior is required"
		return nil
	}
	title := strings.TrimSpace(s.titleField.Value())
	if title == "" {
		s.errMsg = "title is required"
		return nil
	}
	s.errMsg = ""
	s.submitting = true
	req := api.CreateTaskRequest{
		ProjectID:   s.projectField.Value(),
		Behavior:    s.behaviorField.Value(),
		Title:       title,
		Description: s.descArea.Value(),
		AutoStart:   s.autoStartField.Value(),
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
	sb.WriteString(s.projectField.View())

	// Behavior selector — placeholder depends on whether a project is selected.
	{
		bf := s.behaviorField
		if s.projectField.Value() == "" {
			bf.SetPlaceholder("(select project first)")
		} else {
			bf.SetPlaceholder("(select behavior)")
		}
		sb.WriteString(bf.View())
	}

	// Title input
	sb.WriteString(s.titleField.View())

	// Description textarea — 画面幅いっぱいに広げる
	s.descArea.SetWidth(width)
	sb.WriteString(s.descArea.View())

	// Auto start checkbox
	sb.WriteString(s.autoStartField.View())

	// Buttons
	sb.WriteString("\n  " + s.createBtn.View() + "    " + s.cancelBtn.View() + "\n")

	// Error message
	if s.errMsg != "" {
		sb.WriteString(styleError.Render("  ! " + s.errMsg))
		sb.WriteByte('\n')
	}

	_ = height
	return sb.String()
}

func (s *TaskFormScreen) ShortHelp() string {
	return s.help.View(s.keys)
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
