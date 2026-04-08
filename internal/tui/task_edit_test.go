package tui

import (
	"encoding/json"
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

// --- Instructions セクションテスト ---

const testInstructionPayload = `{"instructions":{"main":{"type":"execution","consumer":"claude-code","message":"do this","model":"sonnet","interactive":false},"rework":{"type":"rework","consumer":"claude-code","message":"fix this","model":"haiku","interactive":true}}}`

func newTestEditScreenWithInstructions() *TaskEditScreen {
	task := &orchestrator.Task{
		ID:          "task-edit-2",
		Title:       "Title with instructions",
		Description: "Desc",
		Status:      orchestrator.TaskStatusPending,
		CreatedAt:   time.Now(),
		Payload:     json.RawMessage(testInstructionPayload),
	}
	return NewTaskEditScreen(nil, task)
}

func TestTaskEditInstructionsSectionVisible(t *testing.T) {
	s := newTestEditScreenWithInstructions()
	view := s.View(80, 30)
	if !containsStr(view, "Instructions") {
		t.Error("View should contain 'Instructions' section header")
	}
	if !containsStr(view, "[main]") {
		t.Error("View should contain '[main]' role tab")
	}
	if !containsStr(view, "[rework]") {
		t.Error("View should contain '[rework]' role tab")
	}
}

func TestTaskEditInstructionsSectionHiddenWhenEmpty(t *testing.T) {
	s := newTestEditScreen()
	view := s.View(80, 30)
	if containsStr(view, "Instructions") {
		t.Error("View should NOT contain 'Instructions' when payload has no instructions")
	}
}

func TestTaskEditRoleTabSwitchWithRightKey(t *testing.T) {
	s := newTestEditScreenWithInstructions()
	// roles are sorted: main=0, rework=1
	s.focusIndex = editFocusRoleTab

	if s.activeRole != 0 {
		t.Fatalf("initial activeRole: want 0, got %d", s.activeRole)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyRight})
	if s.activeRole != 1 {
		t.Errorf("after right: want activeRole=1, got %d", s.activeRole)
	}
	// wrap around
	s.Update(tea.KeyMsg{Type: tea.KeyRight})
	if s.activeRole != 0 {
		t.Errorf("wrap right: want activeRole=0, got %d", s.activeRole)
	}
}

func TestTaskEditRoleTabSwitchWithLeftKey(t *testing.T) {
	s := newTestEditScreenWithInstructions()
	s.focusIndex = editFocusRoleTab

	// wrap around to last role
	s.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if s.activeRole != 1 {
		t.Errorf("wrap left: want activeRole=1, got %d", s.activeRole)
	}
}

func TestTaskEditRoleTabSwitchWithHLKeys(t *testing.T) {
	s := newTestEditScreenWithInstructions()
	s.focusIndex = editFocusRoleTab

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if s.activeRole != 1 {
		t.Errorf("l key: want activeRole=1, got %d", s.activeRole)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	if s.activeRole != 0 {
		t.Errorf("h key: want activeRole=0, got %d", s.activeRole)
	}
}

func TestTaskEditInstructionFieldsDisplayed(t *testing.T) {
	s := newTestEditScreenWithInstructions()
	// main role is active (index 0, sorted alphabetically)
	view := s.View(80, 30)
	if !containsStr(view, "sonnet") {
		t.Error("View should show main role model 'sonnet'")
	}
	if !containsStr(view, "claude-code") {
		t.Error("View should show consumer 'claude-code'")
	}
	if !containsStr(view, "do this") {
		t.Error("View should show main role message 'do this'")
	}
}

func TestTaskEditSwitchRoleChangesFields(t *testing.T) {
	s := newTestEditScreenWithInstructions()
	s.focusIndex = editFocusRoleTab

	// Switch to rework role
	s.Update(tea.KeyMsg{Type: tea.KeyRight})

	view := s.View(80, 30)
	if !containsStr(view, "haiku") {
		t.Error("rework role view should show model 'haiku'")
	}
	if !containsStr(view, "fix this") {
		t.Error("rework role view should show message 'fix this'")
	}
}

func TestTaskEditInstructionFocusTabCycle(t *testing.T) {
	s := newTestEditScreenWithInstructions()
	// Focus cycle with instructions: Title → Desc → RoleTab → Model → Consumer → Message → Interactive → Save → Cancel → Title

	expected := []editFocus{
		editFocusDescription,
		editFocusRoleTab,
		editFocusInstModel,
		editFocusInstConsumer,
		editFocusInstMessage,
		editFocusInstInteractive,
		editFocusSave,
		editFocusCancel,
		editFocusTitle,
	}
	for _, want := range expected {
		s.Update(tea.KeyMsg{Type: tea.KeyTab})
		if s.focusIndex != want {
			t.Errorf("tab: want focus %d, got %d", want, s.focusIndex)
		}
	}
}

func TestTaskEditCheckboxToggleInFocus(t *testing.T) {
	s := newTestEditScreenWithInstructions()
	// main role interactive starts as false
	if s.roleEditors[0].interactive.Value() {
		t.Error("initial interactive should be false for main")
	}

	// Focus on interactive field
	s.focusIndex = editFocusInstInteractive
	s.roleEditors[0].interactive.Focus()

	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !s.roleEditors[0].interactive.Value() {
		t.Error("Enter on interactive field should toggle to true")
	}
}

func TestTaskEditSavePayloadJSON(t *testing.T) {
	s := newTestEditScreenWithInstructions()
	s.titleField.SetValue("Updated Title")

	// Modify main role model
	s.focusIndex = editFocusInstModel
	s.roleEditors[0].modelField.Focus()
	s.roleEditors[0].modelField.SetValue("opus")

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

func TestTaskEditBuildPayloadStructure(t *testing.T) {
	s := newTestEditScreenWithInstructions()
	s.roleEditors[0].modelField.SetValue("opus") // main → opus

	payload, err := s.buildPayload()
	if err != nil {
		t.Fatalf("buildPayload error: %v", err)
	}

	var result struct {
		Instructions map[string]orchestrator.Instruction `json:"instructions"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	mainInst, ok := result.Instructions["main"]
	if !ok {
		t.Fatal("expected 'main' key in instructions")
	}
	if mainInst.Model != "opus" {
		t.Errorf("main.model: want 'opus', got %q", mainInst.Model)
	}
	if mainInst.Type != orchestrator.InstructionTypeExecution {
		t.Errorf("main.type: want execution, got %q", mainInst.Type)
	}
	if mainInst.Consumer != "claude-code" {
		t.Errorf("main.consumer: want 'claude-code', got %q", mainInst.Consumer)
	}

	reworkInst, ok := result.Instructions["rework"]
	if !ok {
		t.Fatal("expected 'rework' key in instructions")
	}
	if !reworkInst.Interactive {
		t.Error("rework.interactive: want true")
	}
}

func TestTaskEditBuildPayloadNoInstructions(t *testing.T) {
	s := newTestEditScreen()
	// no instructions: payload should be the original (nil)
	payload, err := s.buildPayload()
	if err != nil {
		t.Fatalf("buildPayload error: %v", err)
	}
	// original payload was nil, so result should also be nil/empty
	if len(payload) != 0 {
		t.Errorf("expected empty payload, got %s", payload)
	}
}
