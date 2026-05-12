package tui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- helpers ---

func makeAwaitingTask(question, questionID, pendingAnswer string) *orchestrator.Task {
	ap := orchestrator.AwaitingPayload{
		Question:      question,
		QuestionID:    questionID,
		PendingAnswer: pendingAnswer,
	}
	apJSON, _ := json.Marshal(ap)
	payload, _ := json.Marshal(map[string]json.RawMessage{
		string(orchestrator.TraitAwaiting): apJSON,
	})
	return &orchestrator.Task{
		ID:        "task-awaiting",
		Title:     "Awaiting Task",
		Status:    orchestrator.TaskStatusAwaiting,
		Behavior:  "dev",
		Payload:   payload,
		CreatedAt: time.Now().Add(-2 * time.Minute),
	}
}

func newTestAnswerScreen(readOnly bool) *TaskAnswerScreen {
	pendingAnswer := ""
	if readOnly {
		pendingAnswer = "already answered"
	}
	task := makeAwaitingTask("Is this correct?", "q-001", pendingAnswer)
	ap := orchestrator.GetAwaitingPayload(task.Payload)
	return NewTaskAnswerScreen(nil, task, ap)
}

// --- tests ---

// TestTaskAnswerScreen_Submit_CallsAnswerTask verifies that Submit with non-empty text
// returns a non-nil cmd (which will invoke AnswerTask on the client).
func TestTaskAnswerScreen_Submit_CallsAnswerTask(t *testing.T) {
	task := makeAwaitingTask("Is this correct?", "q-123", "")
	ap := orchestrator.GetAwaitingPayload(task.Payload)
	// nil client is fine here; we only check the cmd is non-nil, not execute it
	s := NewTaskAnswerScreen((*client.Client)(nil), task, ap)

	s.editor.SetValue("yes, this is correct")

	cmd := s.submit()
	if cmd == nil {
		t.Fatal("submit with non-empty answer: expected non-nil cmd")
	}
}

// TestTaskAnswerScreen_Submit_EmptyAnswer_InlineError verifies that Submit with empty text
// sets errMsg and returns nil (no server call).
func TestTaskAnswerScreen_Submit_EmptyAnswer_InlineError(t *testing.T) {
	task := makeAwaitingTask("Question?", "q-1", "")
	ap := orchestrator.GetAwaitingPayload(task.Payload)
	s := NewTaskAnswerScreen(nil, task, ap)

	// editor value is empty
	cmd := s.submit()
	if cmd != nil {
		t.Errorf("submit with empty answer: expected nil cmd, got non-nil")
	}
	if s.errMsg == "" {
		t.Error("submit with empty answer: expected errMsg to be set")
	}
	if !strings.Contains(s.errMsg, "empty") {
		t.Errorf("errMsg should mention 'empty', got %q", s.errMsg)
	}
}

// TestTaskAnswerScreen_EscCancels verifies that esc returns popScreenMsg.
func TestTaskAnswerScreen_EscCancels(t *testing.T) {
	s := newTestAnswerScreen(false)

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("esc: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("esc: expected popScreenMsg, got %T", msg)
	}
}

// TestTaskAnswerScreen_ReadOnly_NoTextarea verifies that read-only mode doesn't render textarea.
func TestTaskAnswerScreen_ReadOnly_NoTextarea(t *testing.T) {
	s := newTestAnswerScreen(true)

	view := s.View(80, 30)
	// In read-only mode, the textarea component is not rendered (no focusable area).
	// The pending answer value is shown via styleDim.Render, not via s.editor.View().
	if !containsStr(view, "already answered") {
		t.Error("read-only view: expected pending answer text to appear")
	}
}

// TestTaskAnswerScreen_ReadOnly_Submit_IsNoOp verifies that submit in read-only returns nothing.
func TestTaskAnswerScreen_ReadOnly_Submit_IsNoOp(t *testing.T) {
	s := newTestAnswerScreen(true)

	// No matter what key is pressed (except esc), no submit cmd fires.
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		// Enter in read-only just passes through handleKey, which returns nil for read-only
		msg := cmd()
		if _, ok := msg.(answerResultMsg); ok {
			t.Error("read-only mode: Enter should not trigger answerResultMsg")
		}
	}
}

// TestTaskAnswerScreen_AnswerResult_Success_PopScreen verifies success response pops screen.
func TestTaskAnswerScreen_AnswerResult_Success_PopScreen(t *testing.T) {
	s := newTestAnswerScreen(false)

	_, cmd := s.Update(answerResultMsg{err: nil})
	if cmd == nil {
		t.Fatal("answerResultMsg success: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("answerResultMsg success: expected popScreenMsg, got %T", msg)
	}
}

// TestTaskAnswerScreen_AnswerResult_Error_SetsErrMsg verifies error response sets errMsg.
func TestTaskAnswerScreen_AnswerResult_Error_SetsErrMsg(t *testing.T) {
	s := newTestAnswerScreen(false)

	s.Update(answerResultMsg{err: errors.New("server error")})
	if s.errMsg == "" {
		t.Error("answerResultMsg error: expected errMsg to be set")
	}
	if !strings.Contains(s.errMsg, "server error") {
		t.Errorf("errMsg should contain 'server error', got %q", s.errMsg)
	}
	if s.submitting {
		t.Error("answerResultMsg error: expected submitting=false")
	}
}

// TestTaskAnswerScreen_View_ShowsQuestion verifies the question text appears in the view.
func TestTaskAnswerScreen_View_ShowsQuestion(t *testing.T) {
	task := makeAwaitingTask("Should we proceed?", "q-2", "")
	ap := orchestrator.GetAwaitingPayload(task.Payload)
	s := NewTaskAnswerScreen(nil, task, ap)

	view := s.View(80, 30)
	if !containsStr(view, "Question from agent") {
		t.Error("view: expected 'Question from agent' header")
	}
	if !containsStr(view, "Should we proceed?") {
		t.Error("view: expected question text")
	}
}

// TestTaskAnswerScreen_TabCycles_Focus verifies Tab cycles through editor→submit→cancel.
func TestTaskAnswerScreen_TabCycles_Focus(t *testing.T) {
	s := newTestAnswerScreen(false)

	// Initial focus is on editor
	if s.focusIndex != answerFocusEditor {
		t.Errorf("initial focus: want editor, got %d", s.focusIndex)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.focusIndex != answerFocusSubmit {
		t.Errorf("after tab: want submit, got %d", s.focusIndex)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.focusIndex != answerFocusCancel {
		t.Errorf("after tab: want cancel, got %d", s.focusIndex)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.focusIndex != answerFocusEditor {
		t.Errorf("after tab wrap: want editor, got %d", s.focusIndex)
	}
}

// TestTaskAnswerScreen_CancelButton_PopsScreen verifies Cancel button pops screen.
func TestTaskAnswerScreen_CancelButton_PopsScreen(t *testing.T) {
	s := newTestAnswerScreen(false)

	_, cmd := s.Update(ButtonPressedMsg{Label: "Cancel"})
	if cmd == nil {
		t.Fatal("Cancel button: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("Cancel button: expected popScreenMsg, got %T", msg)
	}
}

// TestTaskAnswerScreen_ShortHelp_ReadOnly verifies ShortHelp for read-only mode.
func TestTaskAnswerScreen_ShortHelp_ReadOnly(t *testing.T) {
	s := newTestAnswerScreen(true)
	help := s.ShortHelp()
	if !containsStr(help, "esc") {
		t.Errorf("read-only ShortHelp: expected 'esc', got %q", help)
	}
	if containsStr(help, "tab") {
		t.Errorf("read-only ShortHelp: should not include 'tab', got %q", help)
	}
}

// TestTaskAnswerScreen_ShortHelp_EditMode verifies ShortHelp for edit mode.
func TestTaskAnswerScreen_ShortHelp_EditMode(t *testing.T) {
	s := newTestAnswerScreen(false)
	help := s.ShortHelp()
	if !containsStr(help, "tab") {
		t.Errorf("edit mode ShortHelp: expected 'tab', got %q", help)
	}
	if !containsStr(help, "esc") {
		t.Errorf("edit mode ShortHelp: expected 'esc', got %q", help)
	}
}

// TestWrapText_ShortLine verifies that short lines are not wrapped.
func TestWrapText_ShortLine(t *testing.T) {
	got := wrapText("hello world", 80)
	if got != "hello world" {
		t.Errorf("wrapText short: got %q", got)
	}
}

// TestWrapText_LongLine verifies that long lines are wrapped at word boundaries.
func TestWrapText_LongLine(t *testing.T) {
	input := "one two three four five six"
	got := wrapText(input, 10)
	lines := strings.Split(got, "\n")
	for _, line := range lines {
		if len([]rune(line)) > 10 {
			t.Errorf("wrapText: line %q exceeds width 10", line)
		}
	}
}
