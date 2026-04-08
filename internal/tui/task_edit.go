package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- focus constants ---

type editFocus int

const (
	editFocusTitle editFocus = iota
	editFocusDescription
	editFocusSave
	editFocusCancel
	editFocusCount
)

// --- key map ---

type taskEditKeyMap struct {
	Tab      key.Binding
	ShiftTab key.Binding
	Enter    key.Binding
	Esc      key.Binding
}

func (k taskEditKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Tab, k.Enter, k.Esc}
}

func (k taskEditKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Tab, k.ShiftTab, k.Enter, k.Esc}}
}

var defaultTaskEditKeys = taskEditKeyMap{
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
		key.WithHelp("enter", "save/select"),
	),
	Esc: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "cancel"),
	),
}

// --- messages ---

type taskUpdatedMsg struct {
	task *orchestrator.Task
	err  error
}

// --- TaskEditScreen ---

type TaskEditScreen struct {
	client *client.Client
	taskID string

	titleField TextFieldModel
	descArea   TextAreaModel
	saveBtn    ButtonModel
	cancelBtn  ButtonModel

	focusIndex editFocus
	errMsg     string
	submitting bool

	keys taskEditKeyMap
	help help.Model
}

func NewTaskEditScreen(c *client.Client, task *orchestrator.Task) *TaskEditScreen {
	tf := NewTextField()
	tf.SetLabel("Title")
	tf.SetValue(task.Title)

	ta := NewTextArea()
	ta.SetLabel("Desc")
	ta.SetHeight(4)
	ta.SetValue(task.Description)

	return &TaskEditScreen{
		client:     c,
		taskID:     task.ID,
		titleField: tf,
		descArea:   ta,
		saveBtn:    NewButton("Save"),
		cancelBtn:  NewButton("Cancel"),
		focusIndex: editFocusTitle,
		keys:       defaultTaskEditKeys,
		help:       help.New(),
	}
}

func (s *TaskEditScreen) Init() tea.Cmd {
	return s.titleField.Focus()
}

func (s *TaskEditScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case taskUpdatedMsg:
		s.submitting = false
		if msg.err != nil {
			s.errMsg = "update failed: " + msg.err.Error()
			return s, nil
		}
		return s, func() tea.Msg { return popScreenMsg{} }

	case ButtonPressedMsg:
		switch msg.Label {
		case "Save":
			return s, s.submit()
		case "Cancel":
			return s, func() tea.Msg { return popScreenMsg{} }
		}

	case tea.KeyMsg:
		return s, s.handleKey(msg)
	}
	return s, nil
}

func (s *TaskEditScreen) moveFocus(newFocus editFocus) tea.Cmd {
	switch s.focusIndex {
	case editFocusTitle:
		s.titleField.Blur()
	case editFocusDescription:
		s.descArea.Blur()
	case editFocusSave:
		s.saveBtn.Blur()
	case editFocusCancel:
		s.cancelBtn.Blur()
	}
	s.focusIndex = newFocus
	switch newFocus {
	case editFocusTitle:
		return s.titleField.Focus()
	case editFocusDescription:
		return s.descArea.Focus()
	case editFocusSave:
		return s.saveBtn.Focus()
	case editFocusCancel:
		return s.cancelBtn.Focus()
	}
	return nil
}

func (s *TaskEditScreen) handleKey(msg tea.KeyMsg) tea.Cmd {
	if key.Matches(msg, s.keys.Esc) {
		return func() tea.Msg { return popScreenMsg{} }
	}
	if key.Matches(msg, s.keys.Tab) {
		return s.moveFocus((s.focusIndex + 1) % editFocusCount)
	}
	if key.Matches(msg, s.keys.ShiftTab) {
		return s.moveFocus((s.focusIndex - 1 + editFocusCount) % editFocusCount)
	}

	switch s.focusIndex {
	case editFocusTitle:
		var cmd tea.Cmd
		s.titleField, cmd = s.titleField.Update(msg)
		return cmd
	case editFocusDescription:
		var cmd tea.Cmd
		s.descArea, cmd = s.descArea.Update(msg)
		return cmd
	case editFocusSave:
		var cmd tea.Cmd
		s.saveBtn, cmd = s.saveBtn.Update(msg)
		return cmd
	case editFocusCancel:
		var cmd tea.Cmd
		s.cancelBtn, cmd = s.cancelBtn.Update(msg)
		return cmd
	}
	return nil
}

func (s *TaskEditScreen) submit() tea.Cmd {
	title := strings.TrimSpace(s.titleField.Value())
	if title == "" {
		s.errMsg = "title is required"
		return nil
	}
	s.errMsg = ""
	s.submitting = true
	req := api.UpdateTaskRequest{
		Title:       title,
		Description: s.descArea.Value(),
	}
	return updateTaskCmd(s.client, s.taskID, req)
}

func (s *TaskEditScreen) View(width, height int) string {
	var sb strings.Builder

	sb.WriteString(styleTitle.Render("Edit Task"))
	sb.WriteByte('\n')
	sb.WriteString(strings.Repeat("─", width))
	sb.WriteByte('\n')

	sb.WriteString(s.titleField.View())
	sb.WriteString(s.descArea.View())

	sb.WriteString("\n  " + s.saveBtn.View() + "    " + s.cancelBtn.View() + "\n")

	if s.errMsg != "" {
		sb.WriteString(styleError.Render("  ! " + s.errMsg))
		sb.WriteByte('\n')
	}

	_ = height
	return sb.String()
}

func (s *TaskEditScreen) ShortHelp() string {
	return s.help.View(s.keys)
}

// --- commands ---

func updateTaskCmd(c *client.Client, taskID string, req api.UpdateTaskRequest) tea.Cmd {
	return func() tea.Msg {
		task, err := c.UpdateTask(taskID, req)
		return taskUpdatedMsg{task: task, err: err}
	}
}
