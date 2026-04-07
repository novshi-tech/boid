package tui

import tea "github.com/charmbracelet/bubbletea"

// ButtonPressedMsg is dispatched when a focused ButtonModel receives Enter.
type ButtonPressedMsg struct {
	Label string
}

// ButtonModel is a simple focusable button component following the bubbles
// Init/Update/View pattern.
type ButtonModel struct {
	label   string
	focused bool
}

// NewButton returns a ButtonModel with the given label.
func NewButton(label string) ButtonModel {
	return ButtonModel{label: label}
}

// Focus marks the button as focused.
func (b *ButtonModel) Focus() tea.Cmd {
	b.focused = true
	return nil
}

// Blur removes focus from the button.
func (b *ButtonModel) Blur() { b.focused = false }

// Focused reports whether the button currently has focus.
func (b ButtonModel) Focused() bool { return b.focused }

// Update processes a tea.Msg. When focused and Enter is pressed,
// a ButtonPressedMsg is dispatched. All other input is ignored.
func (b ButtonModel) Update(msg tea.Msg) (ButtonModel, tea.Cmd) {
	if !b.focused {
		return b, nil
	}
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return b, nil
	}
	if keyMsg.String() == "enter" {
		label := b.label
		return b, func() tea.Msg { return ButtonPressedMsg{Label: label} }
	}
	return b, nil
}

// View renders the button. Focused buttons are highlighted with styleCursor.
func (b ButtonModel) View() string {
	text := "[" + b.label + "]"
	if b.focused {
		return styleCursor.Render(text)
	}
	return text
}
