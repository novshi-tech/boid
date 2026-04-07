package tui

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func newTestFormScreen() *TaskFormScreen {
	shared := &SharedState{Panes: make(map[string]string)}
	return NewTaskFormScreen(shared)
}

// --- フォーカス移動テスト ---

func TestFormFocusCycleTab(t *testing.T) {
	s := newTestFormScreen()

	if s.focus != focusProject {
		t.Fatalf("initial focus: want focusProject(%d), got %d", focusProject, s.focus)
	}

	// Tab: project → behavior → title → description → submit → cancel → project
	expected := []formFocus{
		focusBehavior, focusTitle, focusDescription, focusSubmit, focusCancel, focusProject,
	}
	for _, want := range expected {
		s.Update(tea.KeyMsg{Type: tea.KeyTab})
		if s.focus != want {
			t.Errorf("tab: want focus %d, got %d", want, s.focus)
		}
	}
}

func TestFormFocusCycleShiftTab(t *testing.T) {
	s := newTestFormScreen()

	// Shift+Tab from project goes backwards: cancel → submit → description → ...
	expected := []formFocus{
		focusCancel, focusSubmit, focusDescription, focusTitle, focusBehavior, focusProject,
	}
	for _, want := range expected {
		s.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
		if s.focus != want {
			t.Errorf("shift+tab: want focus %d, got %d", want, s.focus)
		}
	}
}

// --- バリデーションテスト ---

func TestFormValidationAllEmpty(t *testing.T) {
	s := newTestFormScreen()
	s.focus = focusSubmit

	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.errMsg == "" {
		t.Error("expected error when submitting empty form")
	}
}

func TestFormValidationNoProject(t *testing.T) {
	s := newTestFormScreen()
	s.behaviorField.options = []selectOption{{Value: "dev", Label: "dev"}}
	s.behaviorField.selected = 0
	s.titleInput.SetValue("My Task")
	s.focus = focusSubmit

	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.errMsg == "" {
		t.Error("expected error: project required")
	}
}

func TestFormValidationNoBehavior(t *testing.T) {
	s := newTestFormScreen()
	s.projectField.options = []selectOption{{Value: "p1", Label: "proj1"}}
	s.projectField.selected = 0
	s.titleInput.SetValue("My Task")
	s.focus = focusSubmit

	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.errMsg == "" {
		t.Error("expected error: behavior required")
	}
}

func TestFormValidationNoTitle(t *testing.T) {
	s := newTestFormScreen()
	s.projectField.options = []selectOption{{Value: "p1", Label: "proj1"}}
	s.projectField.selected = 0
	s.behaviorField.options = []selectOption{{Value: "dev", Label: "dev"}}
	s.behaviorField.selected = 0
	s.focus = focusSubmit

	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.errMsg == "" {
		t.Error("expected error: title required")
	}
}

func TestFormValidationWhitespaceTitle(t *testing.T) {
	s := newTestFormScreen()
	s.projectField.options = []selectOption{{Value: "p1", Label: "proj1"}}
	s.projectField.selected = 0
	s.behaviorField.options = []selectOption{{Value: "dev", Label: "dev"}}
	s.behaviorField.selected = 0
	s.titleInput.SetValue("   ")
	s.focus = focusSubmit

	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.errMsg == "" {
		t.Error("expected error: title is whitespace only")
	}
}

// --- Project 選択変更時に Behavior がリセットされること ---

func TestFormProjectChangeResetsBehavior(t *testing.T) {
	s := newTestFormScreen()
	s.projectField.options = []selectOption{
		{Value: "p1", Label: "proj1"},
		{Value: "p2", Label: "proj2"},
	}
	s.projectField.selected = 0
	s.behaviorField.options = []selectOption{{Value: "dev", Label: "dev"}}
	s.behaviorField.selected = 0

	s.focus = focusProject

	// Enter → expand
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !s.projectField.expanded {
		t.Fatal("expected project selector to expand")
	}

	// j → move cursor to second option
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})

	// Enter → confirm selection of p2
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if s.projectField.selected != 1 {
		t.Errorf("expected project selected=1, got %d", s.projectField.selected)
	}
	if s.behaviorField.selected != -1 {
		t.Errorf("expected behavior reset to -1, got %d", s.behaviorField.selected)
	}
	if s.behaviorField.options != nil {
		t.Error("expected behavior options reset to nil")
	}
	if cmd == nil {
		t.Error("expected fetchBehaviorsCmd to be returned")
	}
}

func TestFormSameProjectNoReset(t *testing.T) {
	s := newTestFormScreen()
	s.projectField.options = []selectOption{
		{Value: "p1", Label: "proj1"},
	}
	s.projectField.selected = 0
	s.behaviorField.options = []selectOption{{Value: "dev", Label: "dev"}}
	s.behaviorField.selected = 0

	s.focus = focusProject

	// Expand and re-select the same project (index 0)
	s.Update(tea.KeyMsg{Type: tea.KeyEnter}) // expand
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}) // confirm same selection

	if s.behaviorField.selected != 0 {
		t.Errorf("same project: behavior should not reset, got selected=%d", s.behaviorField.selected)
	}
	if cmd != nil {
		t.Error("same project: should not trigger fetchBehaviors")
	}
}

// --- selectField 展開/選択テスト ---

func TestSelectFieldExpandOnEnter(t *testing.T) {
	f := newSelectField("Test")
	f.options = []selectOption{{Value: "a", Label: "A"}, {Value: "b", Label: "B"}}

	f.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !f.expanded {
		t.Error("expected field to expand on enter")
	}
}

func TestSelectFieldNoExpandWhenEmpty(t *testing.T) {
	f := newSelectField("Test")
	// no options

	f.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if f.expanded {
		t.Error("should not expand when options are empty")
	}
}

func TestSelectFieldCursorNavigation(t *testing.T) {
	f := newSelectField("Test")
	f.options = []selectOption{
		{Value: "a", Label: "A"},
		{Value: "b", Label: "B"},
		{Value: "c", Label: "C"},
	}
	f.handleKey(tea.KeyMsg{Type: tea.KeyEnter}) // expand

	f.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if f.cursor != 1 {
		t.Errorf("j: want cursor 1, got %d", f.cursor)
	}

	f.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if f.cursor != 2 {
		t.Errorf("j j: want cursor 2, got %d", f.cursor)
	}

	// Can't go past end
	f.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if f.cursor != 2 {
		t.Errorf("j at end: want cursor 2, got %d", f.cursor)
	}

	f.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if f.cursor != 1 {
		t.Errorf("k: want cursor 1, got %d", f.cursor)
	}

	// Can't go below 0
	f.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	f.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if f.cursor != 0 {
		t.Errorf("k at start: want cursor 0, got %d", f.cursor)
	}
}

func TestSelectFieldConfirmSelection(t *testing.T) {
	f := newSelectField("Test")
	f.options = []selectOption{
		{Value: "a", Label: "A"},
		{Value: "b", Label: "B"},
	}
	f.handleKey(tea.KeyMsg{Type: tea.KeyEnter}) // expand
	f.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}) // cursor → B
	f.handleKey(tea.KeyMsg{Type: tea.KeyEnter}) // confirm

	if f.selected != 1 {
		t.Errorf("want selected=1, got %d", f.selected)
	}
	if f.expanded {
		t.Error("selector should be collapsed after selection")
	}
	if f.selectedValue() != "b" {
		t.Errorf("want value 'b', got %q", f.selectedValue())
	}
	if f.selectedLabel() != "B" {
		t.Errorf("want label 'B', got %q", f.selectedLabel())
	}
}

func TestSelectFieldEscCloses(t *testing.T) {
	f := newSelectField("Test")
	f.options = []selectOption{{Value: "a", Label: "A"}}
	f.handleKey(tea.KeyMsg{Type: tea.KeyEnter}) // expand
	if !f.expanded {
		t.Fatal("expected expanded")
	}

	f.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if f.expanded {
		t.Error("esc should close the selector")
	}
	if f.selected != -1 {
		t.Error("esc should not change selection")
	}
}

// --- CreateTask リクエスト構築テスト ---

func TestFormSubmitBuildsRequest(t *testing.T) {
	s := newTestFormScreen()
	s.projectField.options = []selectOption{{Value: "proj-id", Label: "My Project"}}
	s.projectField.selected = 0
	s.behaviorField.options = []selectOption{{Value: "dev", Label: "dev"}}
	s.behaviorField.selected = 0
	s.titleInput.SetValue("Fix the bug")
	s.descArea.SetValue("some detail")
	s.focus = focusSubmit

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if s.errMsg != "" {
		t.Errorf("unexpected error: %q", s.errMsg)
	}
	if !s.submitting {
		t.Error("expected submitting=true after valid submit")
	}
	if cmd == nil {
		t.Error("expected createTaskCmd to be returned")
	}
}

// --- messages ---

func TestFormProjectsLoadedMsg(t *testing.T) {
	s := newTestFormScreen()
	projects := []*orchestrator.Project{
		{ID: "p1", Meta: orchestrator.ProjectMeta{Name: "Alpha"}},
		{ID: "p2", Meta: orchestrator.ProjectMeta{Name: "Beta"}},
	}
	s.Update(projectsLoadedMsg{projects: projects})

	if len(s.projectField.options) != 2 {
		t.Fatalf("want 2 options, got %d", len(s.projectField.options))
	}
	if s.projectField.options[0].Value != "p1" || s.projectField.options[0].Label != "Alpha" {
		t.Errorf("option[0]: got {%q, %q}", s.projectField.options[0].Value, s.projectField.options[0].Label)
	}
	if s.projectField.options[1].Value != "p2" || s.projectField.options[1].Label != "Beta" {
		t.Errorf("option[1]: got {%q, %q}", s.projectField.options[1].Value, s.projectField.options[1].Label)
	}
}

func TestFormBehaviorsLoadedMsg(t *testing.T) {
	s := newTestFormScreen()
	s.behaviorField.selected = 0 // simulate prior selection

	s.Update(behaviorsLoadedMsg{behaviors: []string{"dev", "review"}})

	if len(s.behaviorField.options) != 2 {
		t.Fatalf("want 2 options, got %d", len(s.behaviorField.options))
	}
	if s.behaviorField.selected != -1 {
		t.Errorf("behavior selected should be reset to -1, got %d", s.behaviorField.selected)
	}
}

func TestFormTaskCreatedSuccess(t *testing.T) {
	s := newTestFormScreen()
	s.submitting = true

	_, cmd := s.Update(taskCreatedMsg{task: &orchestrator.Task{ID: "t-new"}})

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

func TestFormTaskCreatedError(t *testing.T) {
	s := newTestFormScreen()
	s.submitting = true

	_, cmd := s.Update(taskCreatedMsg{err: errors.New("server error")})

	if s.submitting {
		t.Error("submitting should be false after response")
	}
	if s.errMsg == "" {
		t.Error("expected error message on failure")
	}
	if cmd != nil {
		t.Error("expected nil cmd on error (stay on form)")
	}
}

// --- Esc の動作テスト ---

func TestFormEscClosesExpandedProject(t *testing.T) {
	s := newTestFormScreen()
	s.projectField.options = []selectOption{{Value: "p1", Label: "p1"}}
	s.projectField.expanded = true

	s.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if s.projectField.expanded {
		t.Error("esc should close expanded project selector")
	}
}

func TestFormEscPopsScreenWhenNothingExpanded(t *testing.T) {
	s := newTestFormScreen()

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Error("esc with nothing expanded should return popScreen cmd")
	}
}

// --- Behavior blocked without project ---

func TestFormBehaviorBlockedWithoutProject(t *testing.T) {
	s := newTestFormScreen()
	s.projectField.selected = -1
	s.behaviorField.options = []selectOption{{Value: "dev", Label: "dev"}}
	s.focus = focusBehavior

	s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if s.behaviorField.expanded {
		t.Error("behavior should not expand when no project selected")
	}
}

// --- Cancel button ---

func TestFormCancelButton(t *testing.T) {
	s := newTestFormScreen()
	s.focus = focusCancel

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Error("cancel button should return popScreen cmd")
	}
}

// --- Backspace の動作テスト ---

// TestFormBackspaceTitleDeletes: Title フォーカス中に backspace → 文字が削除されること
func TestFormBackspaceTitleDeletes(t *testing.T) {
	s := newTestFormScreen()
	s.moveFocus(focusTitle)
	s.titleInput.SetValue("hello")

	s.Update(tea.KeyMsg{Type: tea.KeyBackspace})

	if got := s.titleInput.Value(); got != "hell" {
		t.Errorf("backspace on title: want 'hell', got %q", got)
	}
}

// TestFormBackspaceDescriptionDeletes: Description フォーカス中に backspace → 文字が削除されること
func TestFormBackspaceDescriptionDeletes(t *testing.T) {
	s := newTestFormScreen()
	s.moveFocus(focusDescription)
	s.descArea.SetValue("hello")

	s.Update(tea.KeyMsg{Type: tea.KeyBackspace})

	if got := s.descArea.Value(); got != "hell" {
		t.Errorf("backspace on description: want 'hell', got %q", got)
	}
}

// TestFormBackspaceProjectFocusPops: Project フォーカス中に backspace → 前の画面に戻ること
func TestFormBackspaceProjectFocusPops(t *testing.T) {
	s := newTestFormScreen()
	s.focus = focusProject

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if cmd == nil {
		t.Fatal("backspace on project focus: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("backspace on project focus: expected popScreenMsg, got %T", msg)
	}
}

// TestFormBackspaceSubmitFocusPops: Submit ボタンフォーカス中に backspace → 前の画面に戻ること
func TestFormBackspaceSubmitFocusPops(t *testing.T) {
	s := newTestFormScreen()
	s.focus = focusSubmit

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if cmd == nil {
		t.Fatal("backspace on submit focus: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("backspace on submit focus: expected popScreenMsg, got %T", msg)
	}
}

// --- View smoke test ---

func TestFormView(t *testing.T) {
	s := newTestFormScreen()
	view := s.View(80, 30)
	if view == "" {
		t.Error("View() returned empty string")
	}
	if !containsStr(view, "New Task") {
		t.Error("View should contain 'New Task'")
	}
	if !containsStr(view, "Project") {
		t.Error("View should contain 'Project'")
	}
	if !containsStr(view, "Behavior") {
		t.Error("View should contain 'Behavior'")
	}
	if !containsStr(view, "Title") {
		t.Error("View should contain 'Title'")
	}
	if !containsStr(view, "Desc") {
		t.Error("View should contain 'Desc'")
	}
	if !containsStr(view, "[Create]") {
		t.Error("View should contain '[Create]'")
	}
	if !containsStr(view, "[Cancel]") {
		t.Error("View should contain '[Cancel]'")
	}
}
