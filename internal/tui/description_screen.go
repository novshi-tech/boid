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

type descEditFocus int

const (
	descEditFocusEditor descEditFocus = iota
	descEditFocusSave
	descEditFocusCancel
	descEditFocusCount
)

// --- modes ---

type descriptionMode int

const (
	descriptionModeView descriptionMode = iota
	descriptionModeEdit
)

// --- DescriptionScreen ---

// DescriptionScreen shows the full task description and allows inline editing.
// In view mode: j/k scrolls by line, pgup/pgdn scrolls by page, e enters edit mode, esc/q/backspace pops.
// In edit mode: textarea is shown; ctrl+enter or Tab→Save saves; esc cancels back to view mode.
type DescriptionScreen struct {
	client      *client.Client
	taskID      string
	taskTitle   string
	taskPayload json.RawMessage

	description string // current description text

	mode       descriptionMode
	scroll     int // view mode scroll position
	pageHeight int // updated each View() call, used for page scroll

	// edit mode components
	editor    TextAreaModel
	saveBtn   ButtonModel
	cancelBtn ButtonModel

	focusIndex descEditFocus
	errMsg     string
	submitting bool
}

// NewDescriptionScreen creates a DescriptionScreen pre-filled with the task's description.
func NewDescriptionScreen(c *client.Client, task *orchestrator.Task) *DescriptionScreen {
	ta := NewTextArea()
	ta.SetLabel("Desc")
	ta.SetHeight(15)
	ta.SetValue(task.Description)

	return &DescriptionScreen{
		client:      c,
		taskID:      task.ID,
		taskTitle:   task.Title,
		taskPayload: task.Payload,
		description: task.Description,
		editor:      ta,
		saveBtn:     NewButton("Save"),
		cancelBtn:   NewButton("Cancel"),
	}
}

func (s *DescriptionScreen) Init() tea.Cmd { return nil }

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
			s.mode = descriptionModeView
			s.errMsg = ""
			s.editor.Blur()
			return s, nil
		}

	case tea.KeyMsg:
		return s, s.handleKey(msg)
	}
	return s, nil
}

func (s *DescriptionScreen) handleKey(msg tea.KeyMsg) tea.Cmd {
	if s.mode == descriptionModeView {
		return s.handleViewKey(msg)
	}
	return s.handleEditKey(msg)
}

func (s *DescriptionScreen) handleViewKey(msg tea.KeyMsg) tea.Cmd {
	lines := strings.Split(s.description, "\n")
	maxScroll := max(len(lines)-1, 0)
	pageSize := max(s.pageHeight, 1)

	switch msg.String() {
	case "j", "down":
		if s.scroll < maxScroll {
			s.scroll++
		}
	case "k", "up":
		if s.scroll > 0 {
			s.scroll--
		}
	case "pgdown", "ctrl+f":
		s.scroll = min(s.scroll+pageSize, maxScroll)
	case "pgup", "ctrl+b":
		s.scroll = max(s.scroll-pageSize, 0)
	case "e":
		s.mode = descriptionModeEdit
		s.editor.SetValue(s.description)
		s.focusIndex = descEditFocusEditor
		return s.editor.Focus()
	case "esc", "backspace":
		return func() tea.Msg { return popScreenMsg{} }
	case "q":
		return tea.Quit
	}
	return nil
}

func (s *DescriptionScreen) handleEditKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		s.mode = descriptionModeView
		s.errMsg = ""
		s.editor.Blur()
		return nil
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
	if s.mode == descriptionModeEdit {
		return s.viewEdit(width, height)
	}
	return s.viewDescription(width, height)
}

func (s *DescriptionScreen) viewDescription(width, height int) string {
	var sb strings.Builder

	// Header: 2 lines (title + separator)
	sb.WriteString(styleTitle.Render("Description: " + truncate(s.taskTitle, 50)))
	sb.WriteByte('\n')
	sb.WriteString(strings.Repeat("─", width))
	sb.WriteByte('\n')

	contentHeight := max(height-2, 4)
	s.pageHeight = contentHeight

	if s.description == "" {
		sb.WriteString(styleDim.Render("  (no description)"))
		sb.WriteByte('\n')
		return sb.String()
	}

	lines := strings.Split(s.description, "\n")
	start := s.scroll
	if start >= len(lines) {
		start = 0
	}
	end := min(start+contentHeight, len(lines))
	for _, line := range lines[start:end] {
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	if end < len(lines) {
		sb.WriteString(styleDim.Render(fmt.Sprintf("  ... %d more lines", len(lines)-end)))
		sb.WriteByte('\n')
	}

	return sb.String()
}

func (s *DescriptionScreen) viewEdit(width, height int) string {
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
	if s.mode == descriptionModeEdit {
		return "ctrl+enter: save  tab: next  shift+tab: prev  esc: cancel"
	}
	return "j/k: scroll  pgup/pgdn: page  e: edit  esc: back  q: quit"
}
