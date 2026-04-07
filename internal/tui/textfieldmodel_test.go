package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// --- 基本状態テスト ---

func TestTextFieldModelInitial(t *testing.T) {
	m := NewTextField()
	if m.Focused() {
		t.Error("initial: should not be focused")
	}
	if m.Value() != "" {
		t.Errorf("initial Value: want empty, got %q", m.Value())
	}
}

// --- Focus / Blur ---

func TestTextFieldModelFocusBlur(t *testing.T) {
	m := NewTextField()

	m.Focus()
	if !m.Focused() {
		t.Error("after Focus(): Focused() should be true")
	}

	m.Blur()
	if m.Focused() {
		t.Error("after Blur(): Focused() should be false")
	}
}

// --- View にラベルとカーソルが含まれること ---

func TestTextFieldModelViewContainsLabel(t *testing.T) {
	m := NewTextField()
	m.SetLabel("Title")

	view := m.View()
	if !strings.Contains(view, "Title") {
		t.Errorf("View should contain label 'Title', got %q", view)
	}
}

func TestTextFieldModelViewCursorWhenFocused(t *testing.T) {
	m := NewTextField()
	m.SetLabel("Title")
	m.Focus()

	view := m.View()
	if !strings.Contains(view, "▸") {
		t.Errorf("focused View should contain cursor '▸', got %q", view)
	}
}

func TestTextFieldModelViewNoCursorWhenBlurred(t *testing.T) {
	m := NewTextField()
	m.SetLabel("Title")
	// Blur 状態（デフォルト）

	view := m.View()
	if strings.Contains(view, "▸") {
		t.Errorf("blurred View should not contain cursor '▸', got %q", view)
	}
}

// --- SetPlaceholder ---

func TestTextFieldModelSetPlaceholder(t *testing.T) {
	m := NewTextField()
	m.SetPlaceholder("Task title")

	// placeholder はテキスト入力コンポーネントに設定されるだけで、
	// View には textinput の View() に委譲されるためスモークテストのみ
	_ = m.View()
}

// --- Update が textinput に委譲されること ---

func TestTextFieldModelUpdateDelegates(t *testing.T) {
	m := NewTextField()
	m.Focus()

	// 'a' を入力する
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if m2.Value() != "a" {
		t.Errorf("after typing 'a': want value 'a', got %q", m2.Value())
	}
}

func TestTextFieldModelUpdateIgnoredWhenBlurred(t *testing.T) {
	m := NewTextField()
	// focused=false

	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if m2.Value() != "" {
		t.Errorf("blurred: input should be ignored, got %q", m2.Value())
	}
	if cmd != nil {
		t.Error("blurred: cmd should be nil")
	}
}

// --- View のフォーマットが SelectModel と揃っていること ---

func TestTextFieldModelViewFormat(t *testing.T) {
	m := NewTextField()
	m.SetLabel("Title")

	view := m.View()
	// 先頭が2スペースインデントであること
	if !strings.HasPrefix(view, "  ") {
		t.Errorf("View should start with 2-space indent, got %q", view)
	}
	// 末尾が改行で終わること
	if !strings.HasSuffix(view, "\n") {
		t.Errorf("View should end with newline, got %q", view)
	}
}
