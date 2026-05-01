package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- focus states ---

type instructionEditFocus int

const (
	instructionEditFocusEditor instructionEditFocus = iota
	instructionEditFocusSave
	instructionEditFocusCancel
	instructionEditFocusCount
)

// InstructionsRoleEditScreen lets the user edit a single instruction role as
// YAML. The role itself acts as the key in the API's role-wise partial update:
// submit sends `{role: <parsed instruction>}` via the Instructions field of
// UpdateTaskRequest. Leaving the editor empty sends `{role: null}`, which the
// server treats as a deletion of that role.
type InstructionsRoleEditScreen struct {
	client    *client.Client
	taskID    string
	taskTitle string
	role      string

	editor    TextAreaModel
	saveBtn   ButtonModel
	cancelBtn ButtonModel

	focusIndex instructionEditFocus
	errMsg     string
	submitting bool
}

// NewInstructionsRoleEditScreen seeds the editor with the active
// instruction's YAML representation. The role parameter is preserved as a
// label only; the editor always targets the active (most-recent) entry.
func NewInstructionsRoleEditScreen(c *client.Client, task *orchestrator.Task, role string) *InstructionsRoleEditScreen {
	var initialYAML string
	if active := task.Instructions.Active(); active != nil {
		if y, err := instructionToYAML(*active); err == nil {
			initialYAML = strings.TrimRight(y, "\n")
		}
	}

	ta := NewTextArea()
	ta.SetLabel(role)
	ta.SetHeight(12)
	ta.SetValue(initialYAML)

	return &InstructionsRoleEditScreen{
		client:     c,
		taskID:     task.ID,
		taskTitle:  task.Title,
		role:       role,
		editor:     ta,
		saveBtn:    NewButton("Save"),
		cancelBtn:  NewButton("Cancel"),
		focusIndex: instructionEditFocusEditor,
	}
}

func (s *InstructionsRoleEditScreen) Init() tea.Cmd {
	return s.editor.Focus()
}

func (s *InstructionsRoleEditScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
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

func (s *InstructionsRoleEditScreen) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		return func() tea.Msg { return popScreenMsg{} }
	case "tab":
		return s.shiftFocus(1)
	case "shift+tab":
		return s.shiftFocus(-1)
	}

	switch s.focusIndex {
	case instructionEditFocusEditor:
		var cmd tea.Cmd
		s.editor, cmd = s.editor.Update(msg)
		return cmd
	case instructionEditFocusSave:
		var cmd tea.Cmd
		s.saveBtn, cmd = s.saveBtn.Update(msg)
		return cmd
	case instructionEditFocusCancel:
		var cmd tea.Cmd
		s.cancelBtn, cmd = s.cancelBtn.Update(msg)
		return cmd
	}
	return nil
}

func (s *InstructionsRoleEditScreen) shiftFocus(delta int) tea.Cmd {
	s.blurCurrent()
	next := int(s.focusIndex) + delta
	if next < 0 {
		next += int(instructionEditFocusCount)
	}
	s.focusIndex = instructionEditFocus(next % int(instructionEditFocusCount))
	return s.focusCurrent()
}

func (s *InstructionsRoleEditScreen) blurCurrent() {
	switch s.focusIndex {
	case instructionEditFocusEditor:
		s.editor.Blur()
	case instructionEditFocusSave:
		s.saveBtn.Blur()
	case instructionEditFocusCancel:
		s.cancelBtn.Blur()
	}
}

func (s *InstructionsRoleEditScreen) focusCurrent() tea.Cmd {
	switch s.focusIndex {
	case instructionEditFocusEditor:
		return s.editor.Focus()
	case instructionEditFocusSave:
		return s.saveBtn.Focus()
	case instructionEditFocusCancel:
		return s.cancelBtn.Focus()
	}
	return nil
}

func (s *InstructionsRoleEditScreen) submit() tea.Cmd {
	s.errMsg = ""

	value := strings.TrimSpace(s.editor.Value())

	// empty editor → delete role via `{role: null}` partial
	var patch map[string]json.RawMessage
	if value == "" {
		patch = map[string]json.RawMessage{s.role: json.RawMessage("null")}
	} else {
		raw, err := yamlToJSON(value)
		if err != nil {
			s.errMsg = "YAML parse error: " + err.Error()
			return nil
		}
		// Round-trip through orchestrator.Instruction so malformed entries are
		// caught before we hit the API.
		var inst orchestrator.Instruction
		if err := json.Unmarshal(raw, &inst); err != nil {
			s.errMsg = fmt.Sprintf("instruction parse error: %v", err)
			return nil
		}
		normalized, err := json.Marshal(inst)
		if err != nil {
			s.errMsg = "instruction encode error: " + err.Error()
			return nil
		}
		patch = map[string]json.RawMessage{s.role: normalized}
	}

	body, err := json.Marshal(patch)
	if err != nil {
		s.errMsg = "encode error: " + err.Error()
		return nil
	}

	s.submitting = true
	req := api.UpdateTaskRequest{Instructions: body}
	return updateTaskCmd(s.client, s.taskID, req)
}

func (s *InstructionsRoleEditScreen) View(width, height int) string {
	var sb strings.Builder

	sb.WriteString(styleTitle.Render("Edit Instruction: " + s.role))
	sb.WriteByte('\n')
	sb.WriteString(strings.Repeat("─", width))
	sb.WriteByte('\n')

	sb.WriteString(styleDim.Render("  (clear the editor to delete this role)"))
	sb.WriteByte('\n')

	s.editor.SetWidth(width)
	sb.WriteString(s.editor.View())

	sb.WriteString("\n  " + s.saveBtn.View() + "    " + s.cancelBtn.View() + "\n")

	if s.errMsg != "" {
		sb.WriteString(styleError.Render("  ! " + s.errMsg))
		sb.WriteByte('\n')
	}

	_ = height
	return sb.String()
}

func (s *InstructionsRoleEditScreen) ShortHelp() string {
	return "tab: next  shift+tab: prev  esc: cancel"
}
