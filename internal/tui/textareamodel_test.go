package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// --- 基本状態テスト ---

func TestTextAreaModelInitial(t *testing.T) {
	m := NewTextArea()
	if m.Focused() {
		t.Error("initial: should not be focused")
	}
	if m.Value() != "" {
		t.Errorf("initial Value: want empty, got %q", m.Value())
	}
}

// --- Focus / Blur ---

func TestTextAreaModelFocusBlur(t *testing.T) {
	m := NewTextArea()

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

func TestTextAreaModelViewContainsLabel(t *testing.T) {
	m := NewTextArea()
	m.SetLabel("Desc")

	view := m.View()
	if !strings.Contains(view, "Desc") {
		t.Errorf("View should contain label 'Desc', got %q", view)
	}
}

func TestTextAreaModelViewCursorWhenFocused(t *testing.T) {
	m := NewTextArea()
	m.SetLabel("Desc")
	m.Focus()

	view := m.View()
	if !strings.Contains(view, "▸") {
		t.Errorf("focused View should contain cursor '▸', got %q", view)
	}
}

func TestTextAreaModelViewNoCursorWhenBlurred(t *testing.T) {
	m := NewTextArea()
	m.SetLabel("Desc")
	// Blur 状態（デフォルト）

	view := m.View()
	if strings.Contains(view, "▸") {
		t.Errorf("blurred View should not contain cursor '▸', got %q", view)
	}
}

// --- SetPlaceholder / SetHeight / SetWidth ---

func TestTextAreaModelSetWidth(t *testing.T) {
	m := NewTextArea()
	m.SetWidth(80)
	// クラッシュしないこと・View が呼べること
	_ = m.View()
}

func TestTextAreaModelSetPlaceholder(t *testing.T) {
	m := NewTextArea()
	m.SetPlaceholder("Description (optional)")
	// スモークテスト
	_ = m.View()
}

func TestTextAreaModelSetHeight(t *testing.T) {
	m := NewTextArea()
	m.SetHeight(4)
	// クラッシュしないこと
	_ = m.View()
}

// --- Update が textarea に委譲されること ---

func TestTextAreaModelUpdateDelegates(t *testing.T) {
	m := NewTextArea()
	m.Focus()

	// 'a' を入力する
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if m2.Value() != "a" {
		t.Errorf("after typing 'a': want value 'a', got %q", m2.Value())
	}
}

func TestTextAreaModelUpdateIgnoredWhenBlurred(t *testing.T) {
	m := NewTextArea()
	// focused=false

	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if m2.Value() != "" {
		t.Errorf("blurred: input should be ignored, got %q", m2.Value())
	}
	if cmd != nil {
		t.Error("blurred: cmd should be nil")
	}
}

// --- View のフォーマット: ラベル行と textarea 行が分かれていること ---

func TestTextAreaModelViewFormat(t *testing.T) {
	m := NewTextArea()
	m.SetLabel("Desc")

	view := m.View()
	// 先頭が2スペースインデントであること
	if !strings.HasPrefix(view, "  ") {
		t.Errorf("View should start with 2-space indent, got %q", view)
	}
	lines := strings.Split(view, "\n")
	// ラベル行と textarea 行の間に改行があること（最低2行）
	if len(lines) < 2 {
		t.Errorf("View should have at least 2 lines (label + textarea), got %d", len(lines))
	}
}
