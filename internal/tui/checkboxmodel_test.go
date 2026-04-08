package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestCheckboxInitial(t *testing.T) {
	c := NewCheckbox("interactive")
	if c.Value() {
		t.Error("initial value should be false")
	}
	if c.Focused() {
		t.Error("initial focused should be false")
	}
}

func TestCheckboxSetValue(t *testing.T) {
	c := NewCheckbox("interactive")
	c.SetValue(true)
	if !c.Value() {
		t.Error("SetValue(true): want true, got false")
	}
	c.SetValue(false)
	if c.Value() {
		t.Error("SetValue(false): want false, got true")
	}
}

func TestCheckboxFocusBlur(t *testing.T) {
	c := NewCheckbox("interactive")
	c.Focus()
	if !c.Focused() {
		t.Error("Focus(): want focused=true")
	}
	c.Blur()
	if c.Focused() {
		t.Error("Blur(): want focused=false")
	}
}

func TestCheckboxToggleWithEnter(t *testing.T) {
	c := NewCheckbox("interactive")
	c.Focus()

	c2, cmd := c.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !c2.Value() {
		t.Error("Enter: expected checked=true after toggle")
	}
	if cmd == nil {
		t.Error("Enter: expected non-nil cmd")
	}
	msg := cmd()
	toggled, ok := msg.(CheckboxToggledMsg)
	if !ok {
		t.Fatalf("Enter: expected CheckboxToggledMsg, got %T", msg)
	}
	if toggled.Label != "interactive" {
		t.Errorf("Label: want %q, got %q", "interactive", toggled.Label)
	}
	if !toggled.Checked {
		t.Error("CheckboxToggledMsg.Checked: want true")
	}

	// Toggle back
	c3, _ := c2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if c3.Value() {
		t.Error("second Enter: expected checked=false")
	}
}

func TestCheckboxToggleWithSpace(t *testing.T) {
	c := NewCheckbox("interactive")
	c.Focus()

	c2, cmd := c.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	if !c2.Value() {
		t.Error("Space: expected checked=true after toggle")
	}
	if cmd == nil {
		t.Error("Space: expected non-nil cmd")
	}
}

func TestCheckboxNoToggleWhenUnfocused(t *testing.T) {
	c := NewCheckbox("interactive")
	// not focused

	c2, cmd := c.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if c2.Value() {
		t.Error("unfocused Enter: should not toggle")
	}
	if cmd != nil {
		t.Error("unfocused Enter: expected nil cmd")
	}
}

func TestCheckboxOtherKeyNoToggle(t *testing.T) {
	c := NewCheckbox("interactive")
	c.Focus()

	c2, cmd := c.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if c2.Value() {
		t.Error("other key: should not toggle")
	}
	if cmd != nil {
		t.Error("other key: expected nil cmd")
	}
}

func TestCheckboxViewUnchecked(t *testing.T) {
	c := NewCheckbox("interactive")
	view := c.View()
	if !containsStr(view, "[ ] interactive") {
		t.Errorf("unchecked view: want '[ ] interactive', got %q", view)
	}
}

func TestCheckboxViewChecked(t *testing.T) {
	c := NewCheckbox("interactive")
	c.SetValue(true)
	view := c.View()
	if !containsStr(view, "[x] interactive") {
		t.Errorf("checked view: want '[x] interactive', got %q", view)
	}
}
