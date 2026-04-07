package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

// TextAreaModel is a reusable multi-line text input component that follows
// the same API pattern as SelectModel (Focus/Blur/Focused/Update/View/Value).
type TextAreaModel struct {
	label string
	area  textarea.Model
}

// NewTextArea returns a TextAreaModel with default settings.
func NewTextArea() TextAreaModel {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	return TextAreaModel{area: ta}
}

// SetLabel sets the label displayed before the textarea.
func (m *TextAreaModel) SetLabel(label string) { m.label = label }

// SetPlaceholder sets the placeholder text shown when the field is empty.
func (m *TextAreaModel) SetPlaceholder(placeholder string) {
	m.area.Placeholder = placeholder
}

// SetHeight sets the visible height of the textarea.
func (m *TextAreaModel) SetHeight(h int) { m.area.SetHeight(h) }

// Focus marks the component as focused.
func (m *TextAreaModel) Focus() tea.Cmd {
	return m.area.Focus()
}

// Blur removes focus from the component.
func (m *TextAreaModel) Blur() { m.area.Blur() }

// Focused reports whether the component currently has focus.
func (m TextAreaModel) Focused() bool { return m.area.Focused() }

// Value returns the current text value.
func (m TextAreaModel) Value() string { return m.area.Value() }

// Update processes a tea.Msg and delegates key input to the inner textarea.
// Input is ignored when the component is not focused.
func (m TextAreaModel) Update(msg tea.Msg) (TextAreaModel, tea.Cmd) {
	if !m.area.Focused() {
		return m, nil
	}
	var cmd tea.Cmd
	m.area, cmd = m.area.Update(msg)
	return m, cmd
}

// View renders the label line followed by the textarea.
// Format: "  %-10s [cursor]\n[textarea.View()]\n"
func (m TextAreaModel) View() string {
	labelStr := fmt.Sprintf("%-10s", m.label+":")
	cursor := " "
	if m.area.Focused() {
		cursor = styleCursor.Render("▸")
	}
	return "  " + labelStr + " " + cursor + "\n" + m.area.View() + "\n"
}
