package tui

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

const testPayloadForEdit = `{
	"instructions": {"main": {"type": "execution", "consumer": "claude-code", "message": "do this", "model": "sonnet"}},
	"artifacts": {"output": "result"}
}`

func newTestPayloadSectionEditScreen(sectionKey string) *PayloadSectionEditScreen {
	task := &orchestrator.Task{
		ID:          "task-payload-1",
		Title:       "Test Task",
		Description: "Test Desc",
		Status:      orchestrator.TaskStatusExecuting,
		Payload:     json.RawMessage(testPayloadForEdit),
		CreatedAt:   time.Now(),
	}
	return NewPayloadSectionEditScreen(nil, task, sectionKey)
}

// --- 初期化テスト ---

func TestPayloadSectionEditScreen_InitialYAML(t *testing.T) {
	s := newTestPayloadSectionEditScreen("instructions")
	// Should pre-fill with the YAML representation of the instructions section
	val := s.editor.Value()
	if val == "" {
		t.Error("editor should be pre-filled with YAML for instructions section")
	}
	if !containsStr(val, "main:") {
		t.Errorf("editor should contain 'main:' key, got: %q", val)
	}
}

func TestPayloadSectionEditScreen_InitialFocusOnEditor(t *testing.T) {
	s := newTestPayloadSectionEditScreen("instructions")
	if s.focusIndex != payloadEditFocusEditor {
		t.Errorf("initial focus: want payloadEditFocusEditor(%d), got %d",
			payloadEditFocusEditor, s.focusIndex)
	}
}

func TestPayloadSectionEditScreen_SectionKey(t *testing.T) {
	s := newTestPayloadSectionEditScreen("artifacts")
	if s.sectionKey != "artifacts" {
		t.Errorf("sectionKey: want 'artifacts', got %q", s.sectionKey)
	}
}

// --- フォーカス移動テスト ---

func TestPayloadSectionEdit_TabFocusCycle(t *testing.T) {
	s := newTestPayloadSectionEditScreen("instructions")

	// editor → save → cancel → editor
	expected := []payloadEditFocus{
		payloadEditFocusSave,
		payloadEditFocusCancel,
		payloadEditFocusEditor,
	}
	for _, want := range expected {
		s.Update(tea.KeyMsg{Type: tea.KeyTab})
		if s.focusIndex != want {
			t.Errorf("tab: want focus %d, got %d", want, s.focusIndex)
		}
	}
}

func TestPayloadSectionEdit_ShiftTabFocusCycle(t *testing.T) {
	s := newTestPayloadSectionEditScreen("instructions")

	// editor → cancel → save → editor
	expected := []payloadEditFocus{
		payloadEditFocusCancel,
		payloadEditFocusSave,
		payloadEditFocusEditor,
	}
	for _, want := range expected {
		s.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
		if s.focusIndex != want {
			t.Errorf("shift+tab: want focus %d, got %d", want, s.focusIndex)
		}
	}
}

// --- Esc テスト ---

func TestPayloadSectionEdit_EscPopsScreen(t *testing.T) {
	s := newTestPayloadSectionEditScreen("instructions")

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("esc: expected popScreenMsg, got %T", msg)
	}
}

// --- Cancel ボタンテスト ---

func TestPayloadSectionEdit_CancelButton(t *testing.T) {
	s := newTestPayloadSectionEditScreen("instructions")

	_, cmd := s.Update(ButtonPressedMsg{Label: "Cancel"})
	if cmd == nil {
		t.Fatal("Cancel button: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("Cancel button: expected popScreenMsg, got %T", msg)
	}
}

// --- 保存テスト ---

func TestPayloadSectionEdit_SaveValidYAML(t *testing.T) {
	s := newTestPayloadSectionEditScreen("instructions")
	s.editor.SetValue("main:\n  type: execution\n  model: opus\n")

	cmd := s.submit()
	if s.errMsg != "" {
		t.Errorf("valid YAML: unexpected error: %q", s.errMsg)
	}
	if !s.submitting {
		t.Error("valid YAML: expected submitting=true")
	}
	if cmd == nil {
		t.Error("valid YAML: expected updateTaskCmd")
	}
}

func TestPayloadSectionEdit_SaveInvalidYAML(t *testing.T) {
	s := newTestPayloadSectionEditScreen("instructions")
	s.editor.SetValue(":\tinvalid\x00yaml\n  - bad: [")

	cmd := s.submit()
	if s.errMsg == "" {
		t.Error("invalid YAML: expected error message")
	}
	if s.submitting {
		t.Error("invalid YAML: should not be submitting")
	}
	if cmd != nil {
		t.Error("invalid YAML: expected nil cmd")
	}
}

func TestPayloadSectionEdit_SaveButton(t *testing.T) {
	s := newTestPayloadSectionEditScreen("artifacts")
	s.editor.SetValue("output: my_result\n")

	_, cmd := s.Update(ButtonPressedMsg{Label: "Save"})
	if s.errMsg != "" {
		t.Errorf("Save button: unexpected error: %q", s.errMsg)
	}
	if !s.submitting {
		t.Error("Save button: expected submitting=true")
	}
	if cmd == nil {
		t.Error("Save button: expected updateTaskCmd")
	}
}

// --- taskUpdatedMsg テスト ---

func TestPayloadSectionEdit_UpdatedSuccess(t *testing.T) {
	s := newTestPayloadSectionEditScreen("instructions")
	s.submitting = true

	_, cmd := s.Update(taskUpdatedMsg{task: &orchestrator.Task{ID: "task-payload-1"}})

	if s.submitting {
		t.Error("submitting should be false after response")
	}
	if s.errMsg != "" {
		t.Errorf("unexpected error: %q", s.errMsg)
	}
	if cmd == nil {
		t.Error("expected popScreen cmd on success")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("success: expected popScreenMsg, got %T", msg)
	}
}

func TestPayloadSectionEdit_UpdatedError(t *testing.T) {
	s := newTestPayloadSectionEditScreen("instructions")
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

// --- mergeSectionIntoPayload テスト ---

func TestMergeSectionIntoPayload_ReplacesSection(t *testing.T) {
	s := newTestPayloadSectionEditScreen("artifacts")
	newVal := json.RawMessage(`{"output":"updated"}`)

	merged, err := s.mergeSectionIntoPayload(newVal)
	if err != nil {
		t.Fatalf("mergeSectionIntoPayload error: %v", err)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(merged, &result); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}

	// artifacts should be updated
	var artifacts map[string]any
	if err := json.Unmarshal(result["artifacts"], &artifacts); err != nil {
		t.Fatalf("unmarshal artifacts: %v", err)
	}
	if artifacts["output"] != "updated" {
		t.Errorf("artifacts.output: want 'updated', got %v", artifacts["output"])
	}

	// instructions should be preserved
	if _, ok := result["instructions"]; !ok {
		t.Error("instructions key should be preserved in merged payload")
	}
}

func TestMergeSectionIntoPayload_AddsNewSection(t *testing.T) {
	s := newTestPayloadSectionEditScreen("new_section")
	s.originalPayload = json.RawMessage(`{"existing": "data"}`)
	newVal := json.RawMessage(`{"key": "value"}`)

	merged, err := s.mergeSectionIntoPayload(newVal)
	if err != nil {
		t.Fatalf("mergeSectionIntoPayload error: %v", err)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(merged, &result); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}
	if _, ok := result["new_section"]; !ok {
		t.Error("new_section should be added to merged payload")
	}
	if _, ok := result["existing"]; !ok {
		t.Error("existing key should be preserved")
	}
}

// --- View smoke test ---

func TestPayloadSectionEdit_ViewRenders(t *testing.T) {
	s := newTestPayloadSectionEditScreen("instructions")
	view := s.View(80, 30)
	if view == "" {
		t.Error("View() returned empty string")
	}
	if !containsStr(view, "Edit Payload:") {
		t.Error("View should contain 'Edit Payload:'")
	}
	if !containsStr(view, "instructions") {
		t.Error("View should contain section key 'instructions'")
	}
	if !containsStr(view, "[Save]") {
		t.Error("View should contain '[Save]'")
	}
	if !containsStr(view, "[Cancel]") {
		t.Error("View should contain '[Cancel]'")
	}
}

func TestPayloadSectionEdit_ViewShowsError(t *testing.T) {
	s := newTestPayloadSectionEditScreen("instructions")
	s.errMsg = "YAML parse error: bad indent"
	view := s.View(80, 30)
	if !containsStr(view, "YAML parse error") {
		t.Error("View should display error message")
	}
}

func TestPayloadSectionEdit_ShortHelp(t *testing.T) {
	s := newTestPayloadSectionEditScreen("instructions")
	help := s.ShortHelp()
	if help == "" {
		t.Error("ShortHelp should not be empty")
	}
	if !containsStr(help, "esc") {
		t.Error("ShortHelp should mention esc")
	}
}
