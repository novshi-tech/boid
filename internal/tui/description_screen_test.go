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

// TestDescriptionScreen_InitiallyFocused verifies that NewDescriptionScreen
// creates the screen with the editor already focused (no extra keypress needed).
func TestDescriptionScreen_InitiallyFocused(t *testing.T) {
	s := newTestDescriptionScreen()
	if !s.editor.Focused() {
		t.Error("NewDescriptionScreen: editor should be focused on creation")
	}
}

func TestDescriptionScreen_InitialFocusIndex_IsEditor(t *testing.T) {
	s := newTestDescriptionScreen()
	if s.focusIndex != descEditFocusEditor {
		t.Errorf("initial focusIndex: want descEditFocusEditor, got %v", s.focusIndex)
	}
}

// --- esc pops screen directly ---

// TestDescriptionScreen_EscInEditMode_PopsScreen verifies that pressing esc
// pops the screen directly back to the tab view.
func TestDescriptionScreen_EscInEditMode_PopsScreen(t *testing.T) {
	s := newTestDescriptionScreen()

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("esc: expected non-nil cmd (popScreenMsg)")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("esc: expected popScreenMsg, got %T", msg)
	}
}

// --- cancel button pops screen ---

// TestDescriptionScreen_CancelButtonPopsScreen verifies that the Cancel button
// pops the screen instead of toggling a (now-removed) view mode.
func TestDescriptionScreen_CancelButtonPopsScreen(t *testing.T) {
	s := newTestDescriptionScreen()

	_, cmd := s.Update(ButtonPressedMsg{Label: "Cancel"})
	if cmd == nil {
		t.Fatal("Cancel button: expected non-nil cmd (popScreenMsg)")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("Cancel button: expected popScreenMsg, got %T", msg)
	}
}

// --- save ---

func TestDescriptionScreen_EditMode_CtrlEnterSubmits(t *testing.T) {
	s := newTestDescriptionScreen()

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyCtrlJ}) // ctrl+enter maps to ctrl+j in some terminals
	_ = cmd
	// Submission is triggered; we just verify the path is reached without panicking.
	// Actual call requires a client, so we test save completion via taskUpdatedMsg.
}

func TestDescriptionScreen_SaveSuccess_PopsScreen(t *testing.T) {
	s := newTestDescriptionScreen()
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

// --- button Save ---

func TestDescriptionScreen_ButtonSave_TriggersSubmit(t *testing.T) {
	s := newTestDescriptionScreen()
	s.focusIndex = descEditFocusSave

	_, cmd := s.Update(ButtonPressedMsg{Label: "Save"})
	_ = cmd
	// When client is nil, submit() will panic — test only that the path is reached
	// without crashing (submit guards against double submission).
}

// --- view rendering ---

func TestDescriptionScreen_View_ContainsDescription(t *testing.T) {
	s := newTestDescriptionScreen()

	view := s.View(80, 30)
	if !containsStr(view, "line1") {
		t.Error("View: expected description content 'line1'")
	}
}

func TestDescriptionScreen_View_ContainsTitle(t *testing.T) {
	s := newTestDescriptionScreen()

	view := s.View(80, 30)
	if !containsStr(view, "My Task") {
		t.Error("View: expected task title 'My Task'")
	}
}

func TestDescriptionScreen_View_ContainsButtons(t *testing.T) {
	s := newTestDescriptionScreen()

	view := s.View(80, 30)
	if !containsStr(view, "Save") {
		t.Error("View: expected 'Save' button")
	}
	if !containsStr(view, "Cancel") {
		t.Error("View: expected 'Cancel' button")
	}
}

func TestDescriptionScreen_View_ShowsErrMsg(t *testing.T) {
	s := newTestDescriptionScreen()
	s.errMsg = "save failed: timeout"

	view := s.View(80, 30)
	if !containsStr(view, "save failed") {
		t.Error("View: expected error message")
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
	// In edit mode the title is always shown.
	if !containsStr(view, "Empty Task") {
		t.Error("View (empty desc): expected task title 'Empty Task'")
	}
}

// --- ShortHelp ---

func TestDescriptionScreen_ShortHelp_ContainsEditHelp(t *testing.T) {
	s := newTestDescriptionScreen()

	help := s.ShortHelp()
	if !containsStr(help, "ctrl+enter") {
		t.Error("ShortHelp: expected 'ctrl+enter'")
	}
	if !containsStr(help, "esc: cancel") {
		t.Error("ShortHelp: expected 'esc: cancel'")
	}
}

// --- focus cycling ---

func TestDescriptionScreen_TabCyclesFocus(t *testing.T) {
	s := newTestDescriptionScreen()
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

func TestDescriptionScreen_ShiftTabCyclesBackward(t *testing.T) {
	s := newTestDescriptionScreen()
	s.focusIndex = descEditFocusEditor

	s.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if s.focusIndex != descEditFocusCancel {
		t.Errorf("after shift+tab: want focusCancel, got %v", s.focusIndex)
	}
}
