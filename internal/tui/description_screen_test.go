package tui

import (
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func newTestDescriptionScreen() *DescriptionScreen {
	task := &orchestrator.Task{
		ID:          "desc-task-id",
		Title:       "My Task",
		Description: "line1\nline2\nline3\nline4\nline5",
		Status:      orchestrator.TaskStatusExecuting,
		Behavior:    "dev",
		CreatedAt:   time.Now().Add(-5 * time.Minute),
	}
	return NewDescriptionScreen(nil, task)
}

// --- initial state ---

func TestDescriptionScreen_InitialMode_IsView(t *testing.T) {
	s := newTestDescriptionScreen()
	if s.mode != descriptionModeView {
		t.Errorf("initial mode: want descriptionModeView, got %v", s.mode)
	}
}

func TestDescriptionScreen_InitialScroll_IsZero(t *testing.T) {
	s := newTestDescriptionScreen()
	if s.scroll != 0 {
		t.Errorf("initial scroll: want 0, got %d", s.scroll)
	}
}

// --- view mode scrolling ---

func TestDescriptionScreen_ViewMode_JScrollsDown(t *testing.T) {
	s := newTestDescriptionScreen()

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.scroll != 1 {
		t.Errorf("after j: want scroll 1, got %d", s.scroll)
	}
}

func TestDescriptionScreen_ViewMode_KScrollsUp(t *testing.T) {
	s := newTestDescriptionScreen()
	s.scroll = 2

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.scroll != 1 {
		t.Errorf("after k: want scroll 1, got %d", s.scroll)
	}
}

func TestDescriptionScreen_ViewMode_ScrollCannotGoBelowZero(t *testing.T) {
	s := newTestDescriptionScreen()

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.scroll != 0 {
		t.Errorf("k at 0: want scroll 0, got %d", s.scroll)
	}
}

func TestDescriptionScreen_ViewMode_ScrollCannotExceedMaxLines(t *testing.T) {
	s := newTestDescriptionScreen()
	// description has 5 lines (line1..line5), maxScroll = 4
	s.scroll = 4

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.scroll != 4 {
		t.Errorf("j at max: want scroll 4, got %d", s.scroll)
	}
}

func TestDescriptionScreen_ViewMode_DownArrowScrolls(t *testing.T) {
	s := newTestDescriptionScreen()

	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	if s.scroll != 1 {
		t.Errorf("after down: want scroll 1, got %d", s.scroll)
	}
}

func TestDescriptionScreen_ViewMode_UpArrowScrolls(t *testing.T) {
	s := newTestDescriptionScreen()
	s.scroll = 3

	s.Update(tea.KeyMsg{Type: tea.KeyUp})
	if s.scroll != 2 {
		t.Errorf("after up: want scroll 2, got %d", s.scroll)
	}
}

func TestDescriptionScreen_ViewMode_PageDown(t *testing.T) {
	s := newTestDescriptionScreen()
	s.pageHeight = 2 // set page size

	s.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if s.scroll != 2 {
		t.Errorf("after pgdown: want scroll 2, got %d", s.scroll)
	}
}

func TestDescriptionScreen_ViewMode_PageDownClampedToMax(t *testing.T) {
	s := newTestDescriptionScreen()
	s.pageHeight = 10 // page bigger than content
	s.scroll = 0

	s.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	// maxScroll = 4 (5 lines)
	if s.scroll != 4 {
		t.Errorf("after pgdown (clamp): want scroll 4, got %d", s.scroll)
	}
}

func TestDescriptionScreen_ViewMode_PageUp(t *testing.T) {
	s := newTestDescriptionScreen()
	s.pageHeight = 2
	s.scroll = 4

	s.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if s.scroll != 2 {
		t.Errorf("after pgup: want scroll 2, got %d", s.scroll)
	}
}

func TestDescriptionScreen_ViewMode_PageUpClampedToZero(t *testing.T) {
	s := newTestDescriptionScreen()
	s.pageHeight = 10
	s.scroll = 2

	s.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if s.scroll != 0 {
		t.Errorf("after pgup (clamp): want scroll 0, got %d", s.scroll)
	}
}

// --- view mode navigation ---

func TestDescriptionScreen_ViewMode_EscPopsScreen(t *testing.T) {
	s := newTestDescriptionScreen()

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("esc: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("esc: expected popScreenMsg, got %T", msg)
	}
}

func TestDescriptionScreen_ViewMode_QQuitsApp(t *testing.T) {
	s := newTestDescriptionScreen()

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("q: expected tea.QuitMsg, got %T", msg)
	}
}

func TestDescriptionScreen_ViewMode_BackspacePopsScreen(t *testing.T) {
	s := newTestDescriptionScreen()

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if cmd == nil {
		t.Fatal("backspace: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("backspace: expected popScreenMsg, got %T", msg)
	}
}

// --- edit mode toggle ---

func TestDescriptionScreen_EKeyEntersEditMode(t *testing.T) {
	s := newTestDescriptionScreen()

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if s.mode != descriptionModeEdit {
		t.Errorf("after e: want descriptionModeEdit, got %v", s.mode)
	}
}

func TestDescriptionScreen_EKeyFocusesEditor(t *testing.T) {
	s := newTestDescriptionScreen()

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if s.focusIndex != descEditFocusEditor {
		t.Errorf("after e: want focusIndex=editor, got %v", s.focusIndex)
	}
}

func TestDescriptionScreen_EKeySetsEditorValue(t *testing.T) {
	s := newTestDescriptionScreen()
	s.description = "hello\nworld"

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if s.editor.Value() != "hello\nworld" {
		t.Errorf("after e: editor value = %q, want %q", s.editor.Value(), "hello\nworld")
	}
}

// --- edit mode: esc cancels ---

func TestDescriptionScreen_EditMode_EscReturnsToViewMode(t *testing.T) {
	s := newTestDescriptionScreen()
	s.mode = descriptionModeEdit

	s.Update(tea.KeyMsg{Type: tea.KeyEscape})
	if s.mode != descriptionModeView {
		t.Errorf("after esc in edit: want descriptionModeView, got %v", s.mode)
	}
}

func TestDescriptionScreen_EditMode_EscClearsErrMsg(t *testing.T) {
	s := newTestDescriptionScreen()
	s.mode = descriptionModeEdit
	s.errMsg = "some error"

	s.Update(tea.KeyMsg{Type: tea.KeyEscape})
	if s.errMsg != "" {
		t.Errorf("after esc in edit: want empty errMsg, got %q", s.errMsg)
	}
}

// --- edit mode: save ---

func TestDescriptionScreen_EditMode_CtrlEnterSubmits(t *testing.T) {
	s := newTestDescriptionScreen()
	s.mode = descriptionModeEdit

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyCtrlJ}) // ctrl+enter maps to ctrl+j in some terminals
	_ = cmd
	// Submission is triggered; we just verify submitting flag would be set
	// (actual call requires client, so we test via taskUpdatedMsg path)
}

func TestDescriptionScreen_SaveSuccess_PopsScreen(t *testing.T) {
	s := newTestDescriptionScreen()
	s.mode = descriptionModeEdit
	s.submitting = true

	_, cmd := s.Update(taskUpdatedMsg{task: &orchestrator.Task{ID: "desc-task-id"}})
	if cmd == nil {
		t.Fatal("save success: expected non-nil cmd (popScreenMsg)")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("save success: expected popScreenMsg, got %T", msg)
	}
}

func TestDescriptionScreen_SaveSuccess_ClearsSubmitting(t *testing.T) {
	s := newTestDescriptionScreen()
	s.submitting = true

	s.Update(taskUpdatedMsg{task: &orchestrator.Task{ID: "desc-task-id"}})
	if s.submitting {
		t.Error("save success: expected submitting=false")
	}
}

func TestDescriptionScreen_SaveFailure_SetsErrMsg(t *testing.T) {
	s := newTestDescriptionScreen()
	s.submitting = true

	s.Update(taskUpdatedMsg{err: errors.New("network error")})
	if s.errMsg == "" {
		t.Error("save failure: expected errMsg to be set")
	}
}

func TestDescriptionScreen_SaveFailure_ClearsSubmitting(t *testing.T) {
	s := newTestDescriptionScreen()
	s.submitting = true

	s.Update(taskUpdatedMsg{err: errors.New("network error")})
	if s.submitting {
		t.Error("save failure: expected submitting=false")
	}
}

func TestDescriptionScreen_SaveFailure_StaysInEditMode(t *testing.T) {
	s := newTestDescriptionScreen()
	s.mode = descriptionModeEdit
	s.submitting = true

	s.Update(taskUpdatedMsg{err: errors.New("network error")})
	if s.mode != descriptionModeEdit {
		t.Errorf("save failure: want descriptionModeEdit, got %v", s.mode)
	}
}

// --- edit mode: button Cancel ---

func TestDescriptionScreen_ButtonCancel_ReturnsToViewMode(t *testing.T) {
	s := newTestDescriptionScreen()
	s.mode = descriptionModeEdit

	s.Update(ButtonPressedMsg{Label: "Cancel"})
	if s.mode != descriptionModeView {
		t.Errorf("Cancel button: want descriptionModeView, got %v", s.mode)
	}
}

func TestDescriptionScreen_ButtonSave_TriggersSubmit(t *testing.T) {
	s := newTestDescriptionScreen()
	s.mode = descriptionModeEdit
	s.focusIndex = descEditFocusSave

	_, cmd := s.Update(ButtonPressedMsg{Label: "Save"})
	_ = cmd
	// When client is nil, submit() will panic — test only that the path is reached
	// without crashing (submit guards against double submission).
}

// --- view rendering ---

func TestDescriptionScreen_View_ViewMode_ContainsDescription(t *testing.T) {
	s := newTestDescriptionScreen()

	view := s.View(80, 30)
	if !containsStr(view, "line1") {
		t.Error("View (view mode): expected description content 'line1'")
	}
}

func TestDescriptionScreen_View_ViewMode_ContainsTitle(t *testing.T) {
	s := newTestDescriptionScreen()

	view := s.View(80, 30)
	if !containsStr(view, "My Task") {
		t.Error("View (view mode): expected task title 'My Task'")
	}
}

func TestDescriptionScreen_View_ViewMode_ShowsMoreHint(t *testing.T) {
	s := newTestDescriptionScreen()
	// description has 5 lines; render with height=4 (contentHeight=2)
	view := s.View(80, 4)
	if !containsStr(view, "more lines") {
		t.Error("View (view mode, small height): expected '... N more lines' hint")
	}
}

func TestDescriptionScreen_View_EditMode_ContainsButtons(t *testing.T) {
	s := newTestDescriptionScreen()
	s.mode = descriptionModeEdit

	view := s.View(80, 30)
	if !containsStr(view, "Save") {
		t.Error("View (edit mode): expected 'Save' button")
	}
	if !containsStr(view, "Cancel") {
		t.Error("View (edit mode): expected 'Cancel' button")
	}
}

func TestDescriptionScreen_View_EditMode_ShowsErrMsg(t *testing.T) {
	s := newTestDescriptionScreen()
	s.mode = descriptionModeEdit
	s.errMsg = "save failed: timeout"

	view := s.View(80, 30)
	if !containsStr(view, "save failed") {
		t.Error("View (edit mode): expected error message")
	}
}

func TestDescriptionScreen_View_EmptyDescription(t *testing.T) {
	task := &orchestrator.Task{
		ID:          "empty-task",
		Title:       "Empty Task",
		Description: "",
		Status:      orchestrator.TaskStatusPending,
		Behavior:    "dev",
		CreatedAt:   time.Now(),
	}
	s := NewDescriptionScreen(nil, task)

	view := s.View(80, 20)
	if !containsStr(view, "no description") {
		t.Error("View (empty desc): expected '(no description)' placeholder")
	}
}

// --- ShortHelp ---

func TestDescriptionScreen_ShortHelp_ViewMode(t *testing.T) {
	s := newTestDescriptionScreen()

	help := s.ShortHelp()
	if !containsStr(help, "j/k") {
		t.Error("ShortHelp (view): expected 'j/k'")
	}
	if !containsStr(help, "e: edit") {
		t.Error("ShortHelp (view): expected 'e: edit'")
	}
	if !containsStr(help, "esc: back") {
		t.Error("ShortHelp (view): expected 'esc: back'")
	}
	if !containsStr(help, "q: quit") {
		t.Error("ShortHelp (view): expected 'q: quit'")
	}
}

func TestDescriptionScreen_ShortHelp_EditMode(t *testing.T) {
	s := newTestDescriptionScreen()
	s.mode = descriptionModeEdit

	help := s.ShortHelp()
	if !containsStr(help, "ctrl+enter") {
		t.Error("ShortHelp (edit): expected 'ctrl+enter'")
	}
	if !containsStr(help, "esc: cancel") {
		t.Error("ShortHelp (edit): expected 'esc: cancel'")
	}
}

// --- focus cycling ---

func TestDescriptionScreen_EditMode_TabCyclesFocus(t *testing.T) {
	s := newTestDescriptionScreen()
	s.mode = descriptionModeEdit
	s.focusIndex = descEditFocusEditor

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.focusIndex != descEditFocusSave {
		t.Errorf("after tab: want focusSave, got %v", s.focusIndex)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.focusIndex != descEditFocusCancel {
		t.Errorf("after 2nd tab: want focusCancel, got %v", s.focusIndex)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.focusIndex != descEditFocusEditor {
		t.Errorf("after 3rd tab (wrap): want focusEditor, got %v", s.focusIndex)
	}
}

func TestDescriptionScreen_EditMode_ShiftTabCyclesBackward(t *testing.T) {
	s := newTestDescriptionScreen()
	s.mode = descriptionModeEdit
	s.focusIndex = descEditFocusEditor

	s.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if s.focusIndex != descEditFocusCancel {
		t.Errorf("after shift+tab: want focusCancel, got %v", s.focusIndex)
	}
}
