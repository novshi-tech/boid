package tui

import (
	"encoding/json"
	"fmt"
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

type editFocus int

const (
	editFocusTitle editFocus = iota
	editFocusDescription
	editFocusRoleTab       // role tab navigation
	editFocusInstModel     // instruction model field
	editFocusInstConsumer  // instruction consumer field
	editFocusInstMessage   // instruction message area
	editFocusInstInteractive // instruction interactive checkbox
	editFocusSave
	editFocusCancel
	editFocusCount
)

// instructionEditor holds the editable fields for one instruction role.
type instructionEditor struct {
	role          string
	instType      orchestrator.InstructionType
	modelField    TextFieldModel
	consumerField TextFieldModel
	messageArea   TextAreaModel
	interactive   CheckboxModel
}

// --- key map ---

type taskEditKeyMap struct {
	Tab      key.Binding
	ShiftTab key.Binding
	Enter    key.Binding
	Esc      key.Binding
	Left     key.Binding
	Right    key.Binding
}

func (k taskEditKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Tab, k.Enter, k.Esc}
}

func (k taskEditKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Tab, k.ShiftTab, k.Left, k.Right, k.Enter, k.Esc}}
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
	Left: key.NewBinding(
		key.WithKeys("left", "h"),
		key.WithHelp("←/h", "prev role"),
	),
	Right: key.NewBinding(
		key.WithKeys("right", "l"),
		key.WithHelp("→/l", "next role"),
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

	// instruction editors (one per role; empty when task has no instructions)
	roleEditors     []instructionEditor
	activeRole      int
	originalPayload json.RawMessage

	saveBtn   ButtonModel
	cancelBtn ButtonModel

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

	roleEditors := parseInstructions(task.Payload)

	return &TaskEditScreen{
		client:          c,
		taskID:          task.ID,
		titleField:      tf,
		descArea:        ta,
		roleEditors:     roleEditors,
		activeRole:      0,
		originalPayload: task.Payload,
		saveBtn:         NewButton("Save"),
		cancelBtn:       NewButton("Cancel"),
		focusIndex:      editFocusTitle,
		keys:            defaultTaskEditKeys,
		help:            help.New(),
	}
}

// parseInstructions reads instructions from the payload JSON and returns
// a sorted slice of instructionEditors. Returns nil when no instructions exist.
func parseInstructions(payload json.RawMessage) []instructionEditor {
	if len(payload) == 0 {
		return nil
	}
	var raw struct {
		Instructions map[string]orchestrator.Instruction `json:"instructions"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil || len(raw.Instructions) == 0 {
		return nil
	}

	roles := make([]string, 0, len(raw.Instructions))
	for role := range raw.Instructions {
		roles = append(roles, role)
	}
	sort.Strings(roles)

	editors := make([]instructionEditor, 0, len(roles))
	for _, role := range roles {
		inst := raw.Instructions[role]

		mf := NewTextField()
		mf.SetLabel("Model")
		mf.SetValue(inst.Model)

		cf := NewTextField()
		cf.SetLabel("Consumer")
		cf.SetValue(inst.Consumer)

		ma := NewTextArea()
		ma.SetLabel("Message")
		ma.SetHeight(4)
		ma.SetValue(inst.Message)

		cb := NewCheckbox("Interactive")
		cb.SetValue(inst.Interactive)

		editors = append(editors, instructionEditor{
			role:          role,
			instType:      inst.Type,
			modelField:    mf,
			consumerField: cf,
			messageArea:   ma,
			interactive:   cb,
		})
	}
	return editors
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

// isInstructionFocus returns true for focus states inside the instructions section.
func isInstructionFocus(f editFocus) bool {
	return f >= editFocusRoleTab && f <= editFocusInstInteractive
}

// nextFocus returns the next focus state, skipping instruction states when no editors exist.
func (s *TaskEditScreen) nextFocus() editFocus {
	next := (s.focusIndex + 1) % editFocusCount
	if len(s.roleEditors) == 0 {
		for isInstructionFocus(next) {
			next = (next + 1) % editFocusCount
		}
	}
	return next
}

// prevFocus returns the previous focus state, skipping instruction states when no editors exist.
func (s *TaskEditScreen) prevFocus() editFocus {
	prev := (s.focusIndex - 1 + editFocusCount) % editFocusCount
	if len(s.roleEditors) == 0 {
		for isInstructionFocus(prev) {
			prev = (prev - 1 + editFocusCount) % editFocusCount
		}
	}
	return prev
}

func (s *TaskEditScreen) blurCurrent() {
	switch s.focusIndex {
	case editFocusTitle:
		s.titleField.Blur()
	case editFocusDescription:
		s.descArea.Blur()
	case editFocusRoleTab:
		// tab itself has no blur state
	case editFocusInstModel:
		if len(s.roleEditors) > 0 {
			s.roleEditors[s.activeRole].modelField.Blur()
		}
	case editFocusInstConsumer:
		if len(s.roleEditors) > 0 {
			s.roleEditors[s.activeRole].consumerField.Blur()
		}
	case editFocusInstMessage:
		if len(s.roleEditors) > 0 {
			s.roleEditors[s.activeRole].messageArea.Blur()
		}
	case editFocusInstInteractive:
		if len(s.roleEditors) > 0 {
			s.roleEditors[s.activeRole].interactive.Blur()
		}
	case editFocusSave:
		s.saveBtn.Blur()
	case editFocusCancel:
		s.cancelBtn.Blur()
	}
}

func (s *TaskEditScreen) focusCurrent() tea.Cmd {
	switch s.focusIndex {
	case editFocusTitle:
		return s.titleField.Focus()
	case editFocusDescription:
		return s.descArea.Focus()
	case editFocusRoleTab:
		return nil
	case editFocusInstModel:
		if len(s.roleEditors) > 0 {
			return s.roleEditors[s.activeRole].modelField.Focus()
		}
	case editFocusInstConsumer:
		if len(s.roleEditors) > 0 {
			return s.roleEditors[s.activeRole].consumerField.Focus()
		}
	case editFocusInstMessage:
		if len(s.roleEditors) > 0 {
			return s.roleEditors[s.activeRole].messageArea.Focus()
		}
	case editFocusInstInteractive:
		if len(s.roleEditors) > 0 {
			return s.roleEditors[s.activeRole].interactive.Focus()
		}
	case editFocusSave:
		return s.saveBtn.Focus()
	case editFocusCancel:
		return s.cancelBtn.Focus()
	}
	return nil
}

func (s *TaskEditScreen) moveFocus(newFocus editFocus) tea.Cmd {
	s.blurCurrent()
	s.focusIndex = newFocus
	return s.focusCurrent()
}

func (s *TaskEditScreen) handleKey(msg tea.KeyMsg) tea.Cmd {
	if key.Matches(msg, s.keys.Esc) {
		return func() tea.Msg { return popScreenMsg{} }
	}
	if key.Matches(msg, s.keys.Tab) {
		return s.moveFocus(s.nextFocus())
	}
	if key.Matches(msg, s.keys.ShiftTab) {
		return s.moveFocus(s.prevFocus())
	}

	// Role tab: left/right switches active role
	if s.focusIndex == editFocusRoleTab && len(s.roleEditors) > 0 {
		if key.Matches(msg, s.keys.Left) {
			s.activeRole = (s.activeRole - 1 + len(s.roleEditors)) % len(s.roleEditors)
			return nil
		}
		if key.Matches(msg, s.keys.Right) {
			s.activeRole = (s.activeRole + 1) % len(s.roleEditors)
			return nil
		}
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
	case editFocusInstModel:
		if len(s.roleEditors) > 0 {
			var cmd tea.Cmd
			s.roleEditors[s.activeRole].modelField, cmd = s.roleEditors[s.activeRole].modelField.Update(msg)
			return cmd
		}
	case editFocusInstConsumer:
		if len(s.roleEditors) > 0 {
			var cmd tea.Cmd
			s.roleEditors[s.activeRole].consumerField, cmd = s.roleEditors[s.activeRole].consumerField.Update(msg)
			return cmd
		}
	case editFocusInstMessage:
		if len(s.roleEditors) > 0 {
			var cmd tea.Cmd
			s.roleEditors[s.activeRole].messageArea, cmd = s.roleEditors[s.activeRole].messageArea.Update(msg)
			return cmd
		}
	case editFocusInstInteractive:
		if len(s.roleEditors) > 0 {
			var cmd tea.Cmd
			s.roleEditors[s.activeRole].interactive, cmd = s.roleEditors[s.activeRole].interactive.Update(msg)
			return cmd
		}
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

	payload, err := s.buildPayload()
	if err != nil {
		s.errMsg = "payload error: " + err.Error()
		s.submitting = false
		return nil
	}

	req := api.UpdateTaskRequest{
		Title:       title,
		Description: s.descArea.Value(),
		Payload:     payload,
	}
	return updateTaskCmd(s.client, s.taskID, req)
}

// buildPayload merges edited instructions back into the original payload JSON.
func (s *TaskEditScreen) buildPayload() (json.RawMessage, error) {
	if len(s.roleEditors) == 0 {
		return s.originalPayload, nil
	}

	// Parse existing payload to preserve unknown fields.
	raw := make(map[string]json.RawMessage)
	if len(s.originalPayload) > 0 && string(s.originalPayload) != "null" {
		if err := json.Unmarshal(s.originalPayload, &raw); err != nil {
			raw = make(map[string]json.RawMessage)
		}
	}

	instructions := make(map[string]orchestrator.Instruction, len(s.roleEditors))
	for _, ed := range s.roleEditors {
		instructions[ed.role] = orchestrator.Instruction{
			Type:        ed.instType,
			Consumer:    ed.consumerField.Value(),
			Message:     ed.messageArea.Value(),
			Model:       ed.modelField.Value(),
			Interactive: ed.interactive.Value(),
		}
	}

	instrJSON, err := json.Marshal(instructions)
	if err != nil {
		return nil, err
	}
	raw["instructions"] = instrJSON

	return json.Marshal(raw)
}

func (s *TaskEditScreen) View(width, height int) string {
	var sb strings.Builder

	sb.WriteString(styleTitle.Render("Edit Task"))
	sb.WriteByte('\n')
	sb.WriteString(strings.Repeat("─", width))
	sb.WriteByte('\n')

	sb.WriteString(s.titleField.View())
	// Description — 画面幅いっぱいに広げる
	s.descArea.SetWidth(width)
	sb.WriteString(s.descArea.View())

	// Instructions section (hidden when no instructions)
	if len(s.roleEditors) > 0 {
		sb.WriteByte('\n')
		sb.WriteString(styleTitle.Render("Instructions"))
		sb.WriteByte('\n')
		sb.WriteString(strings.Repeat("─", width))
		sb.WriteByte('\n')

		// Role tabs
		sb.WriteString("  ")
		for i, ed := range s.roleEditors {
			tab := "[" + ed.role + "]"
			if i == s.activeRole {
				if s.focusIndex == editFocusRoleTab {
					tab = styleCursor.Render(tab)
				} else {
					tab = styleTitle.Render(tab)
				}
			} else {
				tab = styleDim.Render(tab)
			}
			if i > 0 {
				sb.WriteString(" ")
			}
			sb.WriteString(tab)
		}
		sb.WriteByte('\n')
		sb.WriteByte('\n')

		// Active role type (read-only)
		ed := s.roleEditors[s.activeRole]
		sb.WriteString(fmt.Sprintf("  %-10s   %s\n", "Type:", string(ed.instType)))

		sb.WriteString(ed.modelField.View())
		sb.WriteString(ed.consumerField.View())
		// instruction message — 画面幅いっぱいに広げる
		s.roleEditors[s.activeRole].messageArea.SetWidth(width)
		sb.WriteString(s.roleEditors[s.activeRole].messageArea.View())
		sb.WriteString(ed.interactive.View())
	}

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
