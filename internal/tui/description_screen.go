package tui

import (
	"encoding/json"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- focus states ---

type descEditFocus int

const (
	descEditFocusEditor descEditFocus = iota
	descEditFocusSave
	descEditFocusCancel
	descEditFocusCount
)

// --- DescriptionScreen ---

// DescriptionScreen is an edit-only screen for the task description.
// It opens directly in edit mode with the textarea focused.
// ctrl+enter or Tab→Save saves; esc or Cancel pops back to the tab view.
type DescriptionScreen struct {
	client      *client.Client
	taskID      string
	taskTitle   string
	taskPayload json.RawMessage

	description string // current description text

	// edit components
	editor    TextAreaModel
	saveBtn   ButtonModel
	cancelBtn ButtonModel

	focusIndex descEditFocus
	errMsg     string
	submitting bool
}

// NewDescriptionScreen creates a DescriptionScreen pre-filled with the task's
// description and immediately focused for editing.
func NewDescriptionScreen(c *client.Client, task *orchestrator.Task) *DescriptionScreen {
	ta := NewTextArea()
	ta.SetLabel("Desc")
	ta.SetHeight(15)
	ta.SetValue(task.Description)
	// Focus immediately so editor.Focused() returns true without waiting for Init.
	// The Cmd returned here (cursor blink) is discarded; Init() re-sends it.
	_ = ta.Focus()

	return &DescriptionScreen{
		client:      c,
		taskID:      task.ID,
		taskTitle:   task.Title,
		taskPayload: task.Payload,
		description: task.Description,
		editor:      ta,
		saveBtn:     NewButton("Save"),
		cancelBtn:   NewButton("Cancel"),
		focusIndex:  descEditFocusEditor,
	}
}

// Init sends the focus command so the textarea cursor blinks from the start.
func (s *DescriptionScreen) Init() tea.Cmd {
	return s.editor.Focus()
}

func (s *DescriptionScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case taskUpdatedMsg:
		s.submitting = false
		if msg.err != nil {
			s.errMsg = "save failed: " + msg.err.Error()
			return s, nil
		}
		// Pop back to TaskDetailScreen which will re-fetch via screenResumedMsg.
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

func (s *DescriptionScreen) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		return func() tea.Msg { return popScreenMsg{} }
	case "ctrl+enter":
		return s.submit()
	case "tab":
		return s.shiftFocus(1)
	case "shift+tab":
		return s.shiftFocus(-1)
	}

	switch s.focusIndex {
	case descEditFocusEditor:
		var cmd tea.Cmd
		s.editor, cmd = s.editor.Update(msg)
		return cmd
	case descEditFocusSave:
		var cmd tea.Cmd
		s.saveBtn, cmd = s.saveBtn.Update(msg)
		return cmd
	case descEditFocusCancel:
		var cmd tea.Cmd
		s.cancelBtn, cmd = s.cancelBtn.Update(msg)
		return cmd
	}
	return nil
}

func (s *DescriptionScreen) shiftFocus(delta int) tea.Cmd {
	s.blurCurrent()
	next := int(s.focusIndex) + delta
	if next < 0 {
		next += int(descEditFocusCount)
	}
	s.focusIndex = descEditFocus(next % int(descEditFocusCount))
	return s.focusCurrent()
}

func (s *DescriptionScreen) blurCurrent() {
	switch s.focusIndex {
	case descEditFocusEditor:
		s.editor.Blur()
	case descEditFocusSave:
		s.saveBtn.Blur()
	case descEditFocusCancel:
		s.cancelBtn.Blur()
	}
}

func (s *DescriptionScreen) focusCurrent() tea.Cmd {
	switch s.focusIndex {
	case descEditFocusEditor:
		return s.editor.Focus()
	case descEditFocusSave:
		return s.saveBtn.Focus()
	case descEditFocusCancel:
		return s.cancelBtn.Focus()
	}
	return nil
}

func (s *DescriptionScreen) submit() tea.Cmd {
	if s.submitting {
		return nil
	}
	s.errMsg = ""
	s.submitting = true
	req := api.UpdateTaskRequest{
		Title:       s.taskTitle,
		Description: s.editor.Value(),
		Payload:     s.taskPayload,
	}
	return updateTaskCmd(s.client, s.taskID, req)
}

func (s *DescriptionScreen) View(width, height int) string {
	var sb strings.Builder

	sb.WriteString(styleTitle.Render("Edit Description: " + truncate(s.taskTitle, 50)))
	sb.WriteByte('\n')
	sb.WriteString(strings.Repeat("─", width))
	sb.WriteByte('\n')

	s.editor.SetWidth(width)
	// Reserve: header(2) + blank before buttons(1) + buttons(1) + error(1) + margin(1)
	editorHeight := max(height-6, 4)
	s.editor.SetHeight(editorHeight)
	sb.WriteString(s.editor.View())

	sb.WriteString("\n  " + s.saveBtn.View() + "    " + s.cancelBtn.View() + "\n")

	if s.errMsg != "" {
		sb.WriteString(styleError.Render("  ! " + s.errMsg))
		sb.WriteByte('\n')
	}

	return sb.String()
}

func (s *DescriptionScreen) ShortHelp() string {
	return "ctrl+enter: save  tab: next  shift+tab: prev  esc: cancel"
}
