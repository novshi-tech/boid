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

type payloadEditFocus int

const (
	payloadEditFocusEditor payloadEditFocus = iota
	payloadEditFocusSave
	payloadEditFocusCancel
	payloadEditFocusCount
)

// --- PayloadSectionEditScreen ---

// PayloadSectionEditScreen allows editing a single top-level section of the task payload as YAML.
type PayloadSectionEditScreen struct {
	client          *client.Client
	taskID          string
	taskTitle       string
	taskDescription string
	sectionKey      string
	originalPayload json.RawMessage

	editor    TextAreaModel
	saveBtn   ButtonModel
	cancelBtn ButtonModel

	focusIndex payloadEditFocus
	errMsg     string
	submitting bool
}

// NewPayloadSectionEditScreen creates a new edit screen pre-filled with the section's current YAML.
func NewPayloadSectionEditScreen(c *client.Client, task *orchestrator.Task, sectionKey string) *PayloadSectionEditScreen {
	var initialYAML string
	if len(task.Payload) > 0 && string(task.Payload) != "null" {
		raw := make(map[string]json.RawMessage)
		if err := json.Unmarshal(task.Payload, &raw); err == nil {
			if sectionData, ok := raw[sectionKey]; ok {
				if y, err := jsonToYAML(sectionData); err == nil {
					initialYAML = strings.TrimRight(y, "\n")
				}
			}
		}
	}

	ta := NewTextArea()
	ta.SetLabel(sectionKey)
	ta.SetHeight(12)
	ta.SetValue(initialYAML)

	return &PayloadSectionEditScreen{
		client:          c,
		taskID:          task.ID,
		taskTitle:       task.Title,
		taskDescription: task.Description,
		sectionKey:      sectionKey,
		originalPayload: task.Payload,
		editor:          ta,
		saveBtn:         NewButton("Save"),
		cancelBtn:       NewButton("Cancel"),
		focusIndex:      payloadEditFocusEditor,
	}
}

func (s *PayloadSectionEditScreen) Init() tea.Cmd {
	return s.editor.Focus()
}

func (s *PayloadSectionEditScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
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

func (s *PayloadSectionEditScreen) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		return func() tea.Msg { return popScreenMsg{} }
	case "tab":
		return s.shiftFocus(1)
	case "shift+tab":
		return s.shiftFocus(-1)
	}

	switch s.focusIndex {
	case payloadEditFocusEditor:
		var cmd tea.Cmd
		s.editor, cmd = s.editor.Update(msg)
		return cmd
	case payloadEditFocusSave:
		var cmd tea.Cmd
		s.saveBtn, cmd = s.saveBtn.Update(msg)
		return cmd
	case payloadEditFocusCancel:
		var cmd tea.Cmd
		s.cancelBtn, cmd = s.cancelBtn.Update(msg)
		return cmd
	}
	return nil
}

func (s *PayloadSectionEditScreen) shiftFocus(delta int) tea.Cmd {
	s.blurCurrent()
	next := int(s.focusIndex) + delta
	if next < 0 {
		next += int(payloadEditFocusCount)
	}
	s.focusIndex = payloadEditFocus(next % int(payloadEditFocusCount))
	return s.focusCurrent()
}

func (s *PayloadSectionEditScreen) blurCurrent() {
	switch s.focusIndex {
	case payloadEditFocusEditor:
		s.editor.Blur()
	case payloadEditFocusSave:
		s.saveBtn.Blur()
	case payloadEditFocusCancel:
		s.cancelBtn.Blur()
	}
}

func (s *PayloadSectionEditScreen) focusCurrent() tea.Cmd {
	switch s.focusIndex {
	case payloadEditFocusEditor:
		return s.editor.Focus()
	case payloadEditFocusSave:
		return s.saveBtn.Focus()
	case payloadEditFocusCancel:
		return s.cancelBtn.Focus()
	}
	return nil
}

func (s *PayloadSectionEditScreen) submit() tea.Cmd {
	s.errMsg = ""

	sectionJSON, err := yamlToJSON(s.editor.Value())
	if err != nil {
		s.errMsg = "YAML parse error: " + err.Error()
		return nil
	}

	mergedPayload, err := s.mergeSectionIntoPayload(sectionJSON)
	if err != nil {
		s.errMsg = "payload error: " + err.Error()
		return nil
	}

	s.submitting = true
	req := api.UpdateTaskRequest{
		Title:       s.taskTitle,
		Description: s.taskDescription,
		Payload:     mergedPayload,
	}
	return updateTaskCmd(s.client, s.taskID, req)
}

// mergeSectionIntoPayload replaces sectionKey in the original payload with sectionJSON,
// preserving all other top-level keys.
func (s *PayloadSectionEditScreen) mergeSectionIntoPayload(sectionJSON json.RawMessage) (json.RawMessage, error) {
	raw := make(map[string]json.RawMessage)
	if len(s.originalPayload) > 0 && string(s.originalPayload) != "null" {
		if err := json.Unmarshal(s.originalPayload, &raw); err != nil {
			raw = make(map[string]json.RawMessage)
		}
	}
	raw[s.sectionKey] = sectionJSON
	return json.Marshal(raw)
}

func (s *PayloadSectionEditScreen) View(width, height int) string {
	var sb strings.Builder

	sb.WriteString(styleTitle.Render("Edit Payload: " + s.sectionKey))
	sb.WriteByte('\n')
	sb.WriteString(strings.Repeat("─", width))
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

func (s *PayloadSectionEditScreen) ShortHelp() string {
	return "tab: next  shift+tab: prev  esc: cancel"
}
