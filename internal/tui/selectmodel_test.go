package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// --- 基本状態テスト ---

func TestSelectModelInitial(t *testing.T) {
	m := NewSelect()
	if m.Value() != "" {
		t.Errorf("initial Value: want empty, got %q", m.Value())
	}
	if m.SelectedLabel() != "" {
		t.Errorf("initial SelectedLabel: want empty, got %q", m.SelectedLabel())
	}
	if m.Focused() {
		t.Error("initial: should not be focused")
	}
	if m.Expanded() {
		t.Error("initial: should not be expanded")
	}
}

// --- Focus / Blur ---

func TestSelectModelFocusBlur(t *testing.T) {
	m := NewSelect()

	m.Focus()
	if !m.Focused() {
		t.Error("after Focus(): Focused() should be true")
	}

	m.Blur()
	if m.Focused() {
		t.Error("after Blur(): Focused() should be false")
	}
}

// --- 非フォーカス時はキー無視 ---

func TestSelectModelIgnoresKeysWhenBlurred(t *testing.T) {
	m := NewSelect()
	m.SetOptions([]SelectOption{{Value: "a", Label: "A"}})
	// focused=false (default)

	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m2.Expanded() {
		t.Error("blurred: should not expand on enter")
	}
	if cmd != nil {
		t.Error("blurred: cmd should be nil")
	}
}

// --- 展開 / 閉じる ---

func TestSelectModelExpandOnEnter(t *testing.T) {
	m := NewSelect()
	m.SetOptions([]SelectOption{{Value: "a", Label: "A"}, {Value: "b", Label: "B"}})
	m.Focus()

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m2.Expanded() {
		t.Error("expected expanded after Enter")
	}
}

func TestSelectModelNoExpandWhenEmpty(t *testing.T) {
	m := NewSelect()
	// options empty
	m.Focus()

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m2.Expanded() {
		t.Error("should not expand when options are empty")
	}
}

func TestSelectModelEscClosesExpanded(t *testing.T) {
	m := NewSelect()
	m.SetOptions([]SelectOption{{Value: "a", Label: "A"}})
	m.Focus()

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // expand
	if !m2.Expanded() {
		t.Fatal("expected expanded")
	}

	m3, _ := m2.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m3.Expanded() {
		t.Error("esc should close the selector")
	}
	if m3.Value() != "" {
		t.Error("esc should not change selection")
	}
}

// --- カーソル移動 ---

func TestSelectModelCursorNavigation(t *testing.T) {
	m := NewSelect()
	m.SetOptions([]SelectOption{
		{Value: "a", Label: "A"},
		{Value: "b", Label: "B"},
		{Value: "c", Label: "C"},
	})
	m.Focus()
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // expand

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.cursor != 1 {
		t.Errorf("j: want cursor 1, got %d", m.cursor)
	}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.cursor != 2 {
		t.Errorf("j j: want cursor 2, got %d", m.cursor)
	}

	// 末尾を超えない
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.cursor != 2 {
		t.Errorf("j at end: want cursor 2, got %d", m.cursor)
	}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m.cursor != 1 {
		t.Errorf("k: want cursor 1, got %d", m.cursor)
	}

	// 先頭を下回らない
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m.cursor != 0 {
		t.Errorf("k at start: want cursor 0, got %d", m.cursor)
	}

	// down キーも動作する
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 1 {
		t.Errorf("down: want cursor 1, got %d", m.cursor)
	}

	// up キーも動作する
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.cursor != 0 {
		t.Errorf("up: want cursor 0, got %d", m.cursor)
	}
}

// --- 選択確定 ---

func TestSelectModelConfirmSelection(t *testing.T) {
	m := NewSelect()
	m.SetOptions([]SelectOption{
		{Value: "a", Label: "A"},
		{Value: "b", Label: "B"},
	})
	m.Focus()

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // expand
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}) // cursor → B
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // confirm

	if m2.selected != 1 {
		t.Errorf("want selected=1, got %d", m2.selected)
	}
	if m2.Expanded() {
		t.Error("selector should collapse after selection")
	}
	if m2.Value() != "b" {
		t.Errorf("want value 'b', got %q", m2.Value())
	}
	if m2.SelectedLabel() != "B" {
		t.Errorf("want label 'B', got %q", m2.SelectedLabel())
	}
	if cmd == nil {
		t.Error("expected SelectChangedMsg cmd after value change")
	}
	msg := cmd()
	sc, ok := msg.(SelectChangedMsg)
	if !ok {
		t.Fatalf("expected SelectChangedMsg, got %T", msg)
	}
	if sc.Value != "b" {
		t.Errorf("SelectChangedMsg.Value: want 'b', got %q", sc.Value)
	}
}

// --- 同じ値の再選択では SelectChangedMsg を返さない ---

func TestSelectModelNoChangeMsgWhenSameSelected(t *testing.T) {
	m := NewSelect()
	m.SetOptions([]SelectOption{{Value: "a", Label: "A"}})
	m.Focus()

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // expand
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // confirm (index 0)

	// 再度選択
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // expand again
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // confirm same

	if cmd != nil {
		t.Error("re-selecting same value should not emit SelectChangedMsg")
	}
}

// --- カーソルが選択済み位置から始まること ---

func TestSelectModelCursorStartsAtSelected(t *testing.T) {
	m := NewSelect()
	m.SetOptions([]SelectOption{
		{Value: "a", Label: "A"},
		{Value: "b", Label: "B"},
		{Value: "c", Label: "C"},
	})
	m.Focus()
	// 先に index=2 を選択
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})                      // expand
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}) // cursor 1
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}) // cursor 2
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})                      // confirm index=2

	// 再展開
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m2.cursor != 2 {
		t.Errorf("cursor should start at selected index 2, got %d", m2.cursor)
	}
}

// --- SetLabel / SetPlaceholder / View スモークテスト ---

func TestSelectModelView(t *testing.T) {
	m := NewSelect()
	m.SetLabel("Status")
	m.SetPlaceholder("(pick one)")
	m.SetOptions([]SelectOption{{Value: "open", Label: "Open"}, {Value: "closed", Label: "Closed"}})
	m.Focus()

	view := m.View()
	if !containsStr(view, "Status") {
		t.Error("View should contain label 'Status'")
	}
	if !containsStr(view, "(pick one)") {
		t.Error("View should contain placeholder when nothing selected")
	}

	// 展開後
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	view2 := m2.View()
	if !containsStr(view2, "Open") {
		t.Error("expanded View should contain options")
	}
	if !containsStr(view2, "Closed") {
		t.Error("expanded View should contain all options")
	}
}
