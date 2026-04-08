package tui

import tea "github.com/charmbracelet/bubbletea"

// CheckboxToggledMsg is dispatched when a CheckboxModel is toggled.
type CheckboxToggledMsg struct {
	Label   string
	Checked bool
}

// CheckboxModel is a boolean toggle UI component.
type CheckboxModel struct {
	label   string
	checked bool
	focused bool
}

// NewCheckbox returns a CheckboxModel with the given label.
func NewCheckbox(label string) CheckboxModel {
	return CheckboxModel{label: label}
}

// Focus marks the checkbox as focused.
func (c *CheckboxModel) Focus() tea.Cmd {
	c.focused = true
	return nil
}

// Blur removes focus from the checkbox.
func (c *CheckboxModel) Blur() { c.focused = false }

// Focused reports whether the checkbox has focus.
func (c CheckboxModel) Focused() bool { return c.focused }

// Value returns the current boolean value.
func (c CheckboxModel) Value() bool { return c.checked }

// SetValue sets the boolean value.
func (c *CheckboxModel) SetValue(v bool) { c.checked = v }

// Update processes a tea.Msg. Enter or Space toggles the checkbox.
func (c CheckboxModel) Update(msg tea.Msg) (CheckboxModel, tea.Cmd) {
	if !c.focused {
		return c, nil
	}
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return c, nil
	}
	switch keyMsg.String() {
	case "enter", " ":
		c.checked = !c.checked
		label, checked := c.label, c.checked
		return c, func() tea.Msg { return CheckboxToggledMsg{Label: label, Checked: checked} }
	}
	return c, nil
}

// View renders the checkbox. Format: "  ▸ [x] label" (focused) or "    [ ] label" (unfocused).
func (c CheckboxModel) View() string {
	mark := " "
	if c.checked {
		mark = "x"
	}
	text := "[" + mark + "] " + c.label
	cursor := " "
	if c.focused {
		cursor = styleCursor.Render("▸")
	}
	return "  " + cursor + " " + text + "\n"
}
