package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// --- 基本状態テスト ---

func TestButtonModelInitial(t *testing.T) {
	b := NewButton("OK")
	if b.Focused() {
		t.Error("initial: should not be focused")
	}
}

// --- Focus / Blur ---

func TestButtonModelFocusBlur(t *testing.T) {
	b := NewButton("OK")

	b.Focus()
	if !b.Focused() {
		t.Error("after Focus(): Focused() should be true")
	}

	b.Blur()
	if b.Focused() {
		t.Error("after Blur(): Focused() should be false")
	}
}

// --- Enter でメッセージ発火 ---

func TestButtonModelEnterFiresMsg(t *testing.T) {
	b := NewButton("Create")
	b.Focus()

	_, cmd := b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("focused button: Enter should return a cmd")
	}
	msg := cmd()
	bp, ok := msg.(ButtonPressedMsg)
	if !ok {
		t.Fatalf("expected ButtonPressedMsg, got %T", msg)
	}
	if bp.Label != "Create" {
		t.Errorf("ButtonPressedMsg.Label: want 'Create', got %q", bp.Label)
	}
}

// --- 非フォーカス時は Enter を無視 ---

func TestButtonModelIgnoresEnterWhenBlurred(t *testing.T) {
	b := NewButton("Create")
	// focused=false (default)

	_, cmd := b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("blurred button: Enter should not return a cmd")
	}
}

// --- その他のキーは無視 ---

func TestButtonModelIgnoresOtherKeys(t *testing.T) {
	b := NewButton("Create")
	b.Focus()

	_, cmd := b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if cmd != nil {
		t.Error("non-enter key should return nil cmd")
	}
}

// --- View ---

func TestButtonModelViewFocused(t *testing.T) {
	b := NewButton("Create")
	b.Focus()
	view := b.View()
	if !containsStr(view, "[Create]") {
		t.Errorf("focused View should contain '[Create]', got %q", view)
	}
}

func TestButtonModelViewBlurred(t *testing.T) {
	b := NewButton("Create")
	view := b.View()
	if !containsStr(view, "[Create]") {
		t.Errorf("blurred View should contain '[Create]', got %q", view)
	}
}
