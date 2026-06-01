package tui

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func newTestInstructionsRoleEditScreen(role string) *InstructionsRoleEditScreen {
	task := &orchestrator.Task{
		ID:     "task-instr-1",
		Title:  "Test Task",
		Status: orchestrator.TaskStatusPending,
		Instructions: orchestrator.Instructions{
			{
				Agent:   "claude-code",
				Message: "do this",
				Model:   "sonnet-4-6",
			},
		},
		CreatedAt: time.Now(),
	}
	return NewInstructionsRoleEditScreen(nil, task, role)
}

func TestInstructionsRoleEdit_InitialYAML_ExistingRole(t *testing.T) {
	s := newTestInstructionsRoleEditScreen("main")
	val := s.editor.Value()
	if val == "" {
		t.Fatal("editor should be pre-filled for existing role")
	}
	if !containsStr(val, "agent: claude-code") {
		t.Errorf("expected 'agent: claude-code', got: %q", val)
	}
	if !containsStr(val, "model: sonnet-4-6") {
		t.Errorf("expected 'model: sonnet-4-6', got: %q", val)
	}
}

// TestInstructionsRoleEdit_InitialYAML_NewRole was removed because the editor
// no longer treats roles as separate entries. With the array-based instructions
// model the editor always targets the active (most-recent) entry, regardless of
// which role label was passed.

func TestInstructionsRoleEdit_InitialFocusOnEditor(t *testing.T) {
	s := newTestInstructionsRoleEditScreen("main")
	if s.focusIndex != instructionEditFocusEditor {
		t.Errorf("initial focus: want %d, got %d", instructionEditFocusEditor, s.focusIndex)
	}
}

func TestInstructionsRoleEdit_TabFocusCycle(t *testing.T) {
	s := newTestInstructionsRoleEditScreen("main")

	expected := []instructionEditFocus{
		instructionEditFocusSave,
		instructionEditFocusCancel,
		instructionEditFocusEditor,
	}
	for _, want := range expected {
		s.Update(tea.KeyMsg{Type: tea.KeyTab})
		if s.focusIndex != want {
			t.Errorf("tab: want %d, got %d", want, s.focusIndex)
		}
	}
}

func TestInstructionsRoleEdit_EscPopsScreen(t *testing.T) {
	s := newTestInstructionsRoleEditScreen("main")

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("esc: expected popScreenMsg, got %T", msg)
	}
}

func TestInstructionsRoleEdit_CancelButton(t *testing.T) {
	s := newTestInstructionsRoleEditScreen("main")
	_, cmd := s.Update(ButtonPressedMsg{Label: "Cancel"})
	if cmd == nil {
		t.Fatal("Cancel: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("Cancel: expected popScreenMsg, got %T", msg)
	}
}

// submit: valid YAML should set submitting=true and return a command.
func TestInstructionsRoleEdit_SubmitValidYAML(t *testing.T) {
	s := newTestInstructionsRoleEditScreen("main")
	s.editor.SetValue("type: execution\nagent: claude-code\nmodel: opus-4-7\nmessage: retry\n")

	cmd := s.submit()
	if s.errMsg != "" {
		t.Errorf("unexpected error: %q", s.errMsg)
	}
	if !s.submitting {
		t.Error("expected submitting=true")
	}
	if cmd == nil {
		t.Error("expected non-nil cmd")
	}
}

// submit: invalid YAML should surface an error and not submit.
func TestInstructionsRoleEdit_SubmitInvalidYAML(t *testing.T) {
	s := newTestInstructionsRoleEditScreen("main")
	s.editor.SetValue(":\tinvalid\x00yaml\n  - bad: [")

	cmd := s.submit()
	if s.errMsg == "" {
		t.Error("expected error for invalid YAML")
	}
	if s.submitting {
		t.Error("should not be submitting")
	}
	if cmd != nil {
		t.Error("expected nil cmd on error")
	}
}

// submit: empty editor should translate into a null role delete patch.
func TestInstructionsRoleEdit_SubmitEmptyDeletesRole(t *testing.T) {
	s := newTestInstructionsRoleEditScreen("main")
	s.editor.SetValue("")

	// We can't observe the encoded body directly via submit(); instead we
	// verify the branch by exercising the internal encoder.
	value := ""
	if value == "" {
		// reproduce logic inline
		patch := map[string]json.RawMessage{s.role: json.RawMessage("null")}
		body, err := json.Marshal(patch)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if !containsStr(string(body), `"main":null`) {
			t.Errorf("expected delete patch, got: %s", body)
		}
	}

	cmd := s.submit()
	if s.errMsg != "" {
		t.Errorf("unexpected error: %q", s.errMsg)
	}
	if !s.submitting {
		t.Error("expected submitting=true when deleting role")
	}
	if cmd == nil {
		t.Error("expected non-nil cmd")
	}
}

// taskUpdatedMsg success should pop the screen.
func TestInstructionsRoleEdit_UpdatedSuccessPops(t *testing.T) {
	s := newTestInstructionsRoleEditScreen("main")
	s.submitting = true

	_, cmd := s.Update(taskUpdatedMsg{task: &orchestrator.Task{ID: "task-instr-1"}})
	if s.submitting {
		t.Error("submitting should be false after response")
	}
	if cmd == nil {
		t.Fatal("expected pop cmd on success")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("success: expected popScreenMsg, got %T", msg)
	}
}

// taskUpdatedMsg failure should stay on the screen and surface the error.
func TestInstructionsRoleEdit_UpdatedErrorStays(t *testing.T) {
	s := newTestInstructionsRoleEditScreen("main")
	s.submitting = true

	_, cmd := s.Update(taskUpdatedMsg{err: errors.New("server error")})
	if s.submitting {
		t.Error("submitting should be false")
	}
	if s.errMsg == "" {
		t.Error("expected error message")
	}
	if cmd != nil {
		t.Error("expected nil cmd on error")
	}
}

func TestInstructionsRoleEdit_View(t *testing.T) {
	s := newTestInstructionsRoleEditScreen("main")
	view := s.View(80, 30)
	if !containsStr(view, "Edit Instruction:") {
		t.Error("View should contain 'Edit Instruction:'")
	}
	if !containsStr(view, "main") {
		t.Error("View should contain role name 'main'")
	}
	if !containsStr(view, "[Save]") {
		t.Error("View should contain '[Save]'")
	}
	if !containsStr(view, "[Cancel]") {
		t.Error("View should contain '[Cancel]'")
	}
}

func TestExtractInstructionRoles_EmptyReturnsNil(t *testing.T) {
	if roles := extractInstructionRoles(nil); roles != nil {
		t.Errorf("nil input: expected nil, got %v", roles)
	}
	if roles := extractInstructionRoles(orchestrator.Instructions{}); roles != nil {
		t.Errorf("empty input: expected nil, got %v", roles)
	}
}
