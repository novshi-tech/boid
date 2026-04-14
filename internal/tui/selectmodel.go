package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// SelectOption is a single item in a SelectModel dropdown.
type SelectOption struct {
	Value string
	Label string
}

// SelectChangedMsg is dispatched when the selected value changes.
type SelectChangedMsg struct {
	Value string
}

// SelectModel is a reusable dropdown select component that follows the
// bubbles/textinput API pattern (Init/Update/View, Focus/Blur).
type SelectModel struct {
	label       string
	placeholder string
	options     []SelectOption
	selected    int // -1 = nothing selected
	expanded    bool
	cursor      int
	focused     bool
}

// NewSelect returns a SelectModel with no selection.
func NewSelect() SelectModel {
	return SelectModel{selected: -1}
}

// SetLabel sets the label displayed before the selector.
func (m *SelectModel) SetLabel(label string) { m.label = label }

// SetPlaceholder sets the text shown when nothing is selected.
func (m *SelectModel) SetPlaceholder(placeholder string) { m.placeholder = placeholder }

// SetOptions replaces the available options.
func (m *SelectModel) SetOptions(opts []SelectOption) { m.options = opts }

// ResetSelection clears the current selection (sets selected to -1).
func (m *SelectModel) ResetSelection() { m.selected = -1 }

// ClearOptions removes all options and resets the selection.
func (m *SelectModel) ClearOptions() {
	m.options = nil
	m.selected = -1
}

// Focus marks the component as focused. Implements the bubbles focus pattern.
func (m *SelectModel) Focus() tea.Cmd {
	m.focused = true
	return nil
}

// Blur removes focus from the component.
func (m *SelectModel) Blur() { m.focused = false }

// Focused reports whether the component currently has focus.
func (m SelectModel) Focused() bool { return m.focused }

// Expanded reports whether the dropdown list is currently visible.
func (m SelectModel) Expanded() bool { return m.expanded }

// Value returns the Value field of the selected option, or "" if none.
func (m SelectModel) Value() string {
	if m.selected < 0 || m.selected >= len(m.options) {
		return ""
	}
	return m.options[m.selected].Value
}

// SelectedLabel returns the Label field of the selected option, or "".
func (m SelectModel) SelectedLabel() string {
	if m.selected < 0 || m.selected >= len(m.options) {
		return ""
	}
	return m.options[m.selected].Label
}

// Update processes a tea.Msg and returns the updated model plus an optional
// command. When the selected value changes, a SelectChangedMsg is dispatched.
// Key input is ignored when the component is not focused.
func (m SelectModel) Update(msg tea.Msg) (SelectModel, tea.Cmd) {
	if !m.focused {
		return m, nil
	}
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}

	if !m.expanded {
		// Enter opens the dropdown (only when options are available).
		if keyMsg.String() == "enter" && len(m.options) > 0 {
			m.expanded = true
			if m.selected >= 0 {
				m.cursor = m.selected
			} else {
				m.cursor = 0
			}
		}
		return m, nil
	}

	// Expanded — handle navigation keys.
	switch keyMsg.String() {
	case "j", "down":
		if m.cursor < len(m.options)-1 {
			m.cursor++
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "enter":
		prev := m.selected
		m.selected = m.cursor
		m.expanded = false
		if m.selected != prev {
			val := m.Value()
			return m, func() tea.Msg { return SelectChangedMsg{Value: val} }
		}
	case "esc":
		m.expanded = false
	}
	return m, nil
}

// View renders the component as a single line (plus inline option list when
// expanded). The format mirrors the existing viewSelectField layout.
func (m SelectModel) View() string {
	var sb strings.Builder

	labelStr := fmt.Sprintf("%-12s", m.label+":")
	cursor := " "
	if m.focused {
		cursor = styleCursor.Render("▸")
	}

	ph := m.placeholder
	if ph == "" {
		ph = "(select)"
	}

	sel := m.SelectedLabel()
	if sel == "" {
		if m.focused {
			sel = ph
		} else {
			sel = styleDim.Render(ph)
		}
	} else if m.focused {
		sel = styleTitle.Render(sel)
	}

	sb.WriteString("  " + labelStr + " " + cursor + " " + sel + "\n")

	if m.expanded {
		for i, opt := range m.options {
			if i == m.cursor {
				sb.WriteString("               " + styleCursor.Render("▸ "+opt.Label) + "\n")
			} else {
				sb.WriteString("                 " + styleDim.Render(opt.Label) + "\n")
			}
		}
	}

	return sb.String()
}
