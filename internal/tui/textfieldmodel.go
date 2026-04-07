package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// TextFieldModel is a reusable single-line text input component that follows
// the same API pattern as SelectModel (Focus/Blur/Focused/Update/View/Value).
type TextFieldModel struct {
	label string
	input textinput.Model
}

// NewTextField returns a TextFieldModel with default settings.
func NewTextField() TextFieldModel {
	ti := textinput.New()
	return TextFieldModel{input: ti}
}

// SetLabel sets the label displayed before the input field.
func (m *TextFieldModel) SetLabel(label string) { m.label = label }

// SetPlaceholder sets the placeholder text shown when the field is empty.
func (m *TextFieldModel) SetPlaceholder(placeholder string) {
	m.input.Placeholder = placeholder
}

// Focus marks the component as focused.
func (m *TextFieldModel) Focus() tea.Cmd {
	return m.input.Focus()
}

// Blur removes focus from the component.
func (m *TextFieldModel) Blur() { m.input.Blur() }

// Focused reports whether the component currently has focus.
func (m TextFieldModel) Focused() bool { return m.input.Focused() }

// Value returns the current text value.
func (m TextFieldModel) Value() string { return m.input.Value() }

// Update processes a tea.Msg and delegates key input to the inner textinput.
// Input is ignored when the component is not focused.
func (m TextFieldModel) Update(msg tea.Msg) (TextFieldModel, tea.Cmd) {
	if !m.input.Focused() {
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// View renders the component as a single line.
// Format: "  %-10s [cursor] [textinput.View()]\n"
func (m TextFieldModel) View() string {
	labelStr := fmt.Sprintf("%-10s", m.label+":")
	cursor := " "
	if m.input.Focused() {
		cursor = styleCursor.Render("▸")
	}
	return "  " + labelStr + " " + cursor + " " + m.input.View() + "\n"
}
