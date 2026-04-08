package tui

import (
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func newTestEditScreen() *TaskEditScreen {
	task := &orchestrator.Task{
		ID:          "task-edit-1",
		Title:       "Existing Title",
		Description: "Existing Description",
		Status:      orchestrator.TaskStatusPending,
		CreatedAt:   time.Now(),
	}
	return NewTaskEditScreen(nil, task)
}

// --- 初期表示テスト ---

func TestTaskEditInitialValues(t *testing.T) {
	s := newTestEditScreen()

	if got := s.titleField.Value(); got != "Existing Title" {
		t.Errorf("titleField: want %q, got %q", "Existing Title", got)
	}
	if got := s.descArea.Value(); got != "Existing Description" {
		t.Errorf("descArea: want %q, got %q", "Existing Description", got)
	}
}

func TestTaskEditInitialFocusOnTitle(t *testing.T) {
	s := newTestEditScreen()

	if s.focusIndex != editFocusTitle {
		t.Errorf("initial focus: want editFocusTitle(%d), got %d", editFocusTitle, s.focusIndex)
	}
}

// --- フォーカス移動テスト ---

func TestTaskEditTabFocusCycle(t *testing.T) {
	s := newTestEditScreen()

	expected := []editFocus{
		editFocusDescription, editFocusSave, editFocusCancel, editFocusTitle,
	}
	for _, want := range expected {
		s.Update(tea.KeyMsg{Type: tea.KeyTab})
		if s.focusIndex != want {
			t.Errorf("tab: want focus %d, got %d", want, s.focusIndex)
		}
	}
}

func TestTaskEditShiftTabFocusCycle(t *testing.T) {
	s := newTestEditScreen()

	expected := []editFocus{
		editFocusCancel, editFocusSave, editFocusDescription, editFocusTitle,
	}
	for _, want := range expected {
		s.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
		if s.focusIndex != want {
			t.Errorf("shift+tab: want focus %d, got %d", want, s.focusIndex)
		}
	}
}

// --- Save テスト ---

func pressSaveBtn(s *TaskEditScreen) tea.Cmd {
	s.focusIndex = editFocusSave
	s.saveBtn.focused = true
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		return nil
	}
	_, cmd2 := s.Update(cmd())
	return cmd2
}

func TestTaskEditSaveEmptyTitle(t *testing.T) {
	s := newTestEditScreen()
	s.titleField.SetValue("")

	pressSaveBtn(s)
	if s.errMsg == "" {
		t.Error("expected error when saving with empty title")
	}
}

func TestTaskEditSaveBuildsRequest(t *testing.T) {
	s := newTestEditScreen()
	s.titleField.SetValue("Updated Title")
	s.descArea.SetValue("Updated Description")

	cmd := pressSaveBtn(s)

	if s.errMsg != "" {
		t.Errorf("unexpected error: %q", s.errMsg)
	}
	if !s.submitting {
		t.Error("expected submitting=true after valid save")
	}
	if cmd == nil {
		t.Error("expected updateTaskCmd to be returned")
	}
}

// --- taskUpdatedMsg テスト ---

func TestTaskEditUpdatedSuccess(t *testing.T) {
	s := newTestEditScreen()
	s.submitting = true

	_, cmd := s.Update(taskUpdatedMsg{task: &orchestrator.Task{ID: "task-edit-1"}})

	if s.submitting {
		t.Error("submitting should be false after response")
	}
	if s.errMsg != "" {
		t.Errorf("unexpected error: %q", s.errMsg)
	}
	if cmd == nil {
		t.Error("expected popScreen cmd on success")
	}
}

func TestTaskEditUpdatedError(t *testing.T) {
	s := newTestEditScreen()
	s.submitting = true

	_, cmd := s.Update(taskUpdatedMsg{err: errors.New("server error")})

	if s.submitting {
		t.Error("submitting should be false after response")
	}
	if s.errMsg == "" {
		t.Error("expected error message on failure")
	}
	if cmd != nil {
		t.Error("expected nil cmd on error (stay on screen)")
	}
}

// --- Cancel / Esc テスト ---

func TestTaskEditCancelButton(t *testing.T) {
	s := newTestEditScreen()
	s.focusIndex = editFocusCancel
	s.cancelBtn.focused = true

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("cancel button should return cmd")
	}
	_, cmd2 := s.Update(cmd())
	if cmd2 == nil {
		t.Error("ButtonPressedMsg Cancel should return popScreen cmd")
	}
}

func TestTaskEditEscPopsScreen(t *testing.T) {
	s := newTestEditScreen()

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("esc: expected popScreenMsg, got %T", msg)
	}
}

func TestTaskEditButtonPressedCancel(t *testing.T) {
	s := newTestEditScreen()

	_, cmd := s.Update(ButtonPressedMsg{Label: "Cancel"})
	if cmd == nil {
		t.Fatal("ButtonPressedMsg Cancel should return cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("ButtonPressedMsg Cancel: expected popScreenMsg, got %T", msg)
	}
}

func TestTaskEditButtonPressedSave(t *testing.T) {
	s := newTestEditScreen()
	s.titleField.SetValue("My Title")

	_, cmd := s.Update(ButtonPressedMsg{Label: "Save"})

	if s.errMsg != "" {
		t.Errorf("unexpected error: %q", s.errMsg)
	}
	if !s.submitting {
		t.Error("expected submitting=true after ButtonPressedMsg Save")
	}
	if cmd == nil {
		t.Error("expected updateTaskCmd after ButtonPressedMsg Save")
	}
}

// --- View smoke test ---

func TestTaskEditView(t *testing.T) {
	s := newTestEditScreen()
	view := s.View(80, 30)
	if view == "" {
		t.Error("View() returned empty string")
	}
	if !containsStr(view, "Edit Task") {
		t.Error("View should contain 'Edit Task'")
	}
	if !containsStr(view, "[Save]") {
		t.Error("View should contain '[Save]'")
	}
	if !containsStr(view, "[Cancel]") {
		t.Error("View should contain '[Cancel]'")
	}
}
