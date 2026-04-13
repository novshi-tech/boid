package tui

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestMain(m *testing.M) {
	// Force TrueColor so lipgloss emits ANSI codes regardless of TTY.
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

func newTestTaskListScreen() *TaskListScreen {
	shared := &SharedState{Panes: make(map[string]string)}
	return NewTaskListScreen(shared)
}

// --- state toggle tests ---

func TestStateToggle_Tab(t *testing.T) {
	s := newTestTaskListScreen()
	// Default is open (stateClosed=false)
	if s.stateClosed {
		t.Fatal("initial stateClosed should be false")
	}

	// tab -> closed
	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if !s.stateClosed {
		t.Error("after tab: stateClosed should be true")
	}

	// tab -> open again
	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.stateClosed {
		t.Error("after tab tab: stateClosed should be false")
	}
}

func TestStateToggle_TabResetsCursor(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(10)
	s.syncTableRows()
	s.table.SetCursor(5)
	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.table.Cursor() != 0 {
		t.Errorf("expected cursor 0 after state toggle, got %d", s.table.Cursor())
	}
}

// --- open/closed status maps tests ---

func TestOpenStatuses(t *testing.T) {
	want := map[orchestrator.TaskStatus]bool{
		orchestrator.TaskStatusExecuting: true,
		orchestrator.TaskStatusReworking: true,
		orchestrator.TaskStatusVerifying: true,
		orchestrator.TaskStatusPending:   true,
	}

	if len(openStatuses) != len(want) {
		t.Fatalf("openStatuses has %d entries, want %d", len(openStatuses), len(want))
	}
	for status := range want {
		if !openStatuses[status] {
			t.Errorf("expected %q in openStatuses", status)
		}
	}

	notOpen := []orchestrator.TaskStatus{
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
	}
	for _, status := range notOpen {
		if openStatuses[status] {
			t.Errorf("expected %q NOT in openStatuses", status)
		}
	}
}

func TestClosedStatuses(t *testing.T) {
	want := map[orchestrator.TaskStatus]bool{
		orchestrator.TaskStatusDone:    true,
		orchestrator.TaskStatusAborted: true,
	}

	if len(closedStatuses) != len(want) {
		t.Fatalf("closedStatuses has %d entries, want %d", len(closedStatuses), len(want))
	}
	for status := range want {
		if !closedStatuses[status] {
			t.Errorf("expected %q in closedStatuses", status)
		}
	}

	notClosed := []orchestrator.TaskStatus{
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusReworking,
		orchestrator.TaskStatusVerifying,
		orchestrator.TaskStatusPending,
	}
	for _, status := range notClosed {
		if closedStatuses[status] {
			t.Errorf("expected %q NOT in closedStatuses", status)
		}
	}
}

// --- cursor movement tests ---

func TestCursorMovement(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(5)
	s.syncTableRows()

	// Move down
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.table.Cursor() != 1 {
		t.Errorf("after j: want cursor 1, got %d", s.table.Cursor())
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.table.Cursor() != 2 {
		t.Errorf("after j j: want cursor 2, got %d", s.table.Cursor())
	}

	// Move up
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.table.Cursor() != 1 {
		t.Errorf("after k: want cursor 1, got %d", s.table.Cursor())
	}

	// Can't go below 0
	s.table.SetCursor(0)
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.table.Cursor() != 0 {
		t.Errorf("cursor should not go below 0, got %d", s.table.Cursor())
	}

	// Can't go above len-1
	s.table.SetCursor(4)
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.table.Cursor() != 4 {
		t.Errorf("cursor should not exceed task count, got %d", s.table.Cursor())
	}
}

func TestCursorMovementArrowKeys(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(3)
	s.syncTableRows()

	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	if s.table.Cursor() != 1 {
		t.Errorf("after down: want cursor 1, got %d", s.table.Cursor())
	}

	s.Update(tea.KeyMsg{Type: tea.KeyUp})
	if s.table.Cursor() != 0 {
		t.Errorf("after up: want cursor 0, got %d", s.table.Cursor())
	}
}

// --- project modal tests ---

func TestProjectModal_PKey_Opens(t *testing.T) {
	s := newTestTaskListScreen()
	s.projects = []*orchestrator.Project{
		{ID: "p1", Meta: orchestrator.ProjectMeta{Name: "proj1"}},
		{ID: "p2", Meta: orchestrator.ProjectMeta{Name: "proj2"}},
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if !s.popup.active {
		t.Error("p key: popup should be active")
	}
	if s.popup.kind != "project" {
		t.Errorf("p key: popup kind should be 'project', got %q", s.popup.kind)
	}
	// labels: "(all)", "proj1", "proj2"
	if len(s.popup.labels) != 3 {
		t.Errorf("p key: want 3 popup labels (all + 2 projects), got %d", len(s.popup.labels))
	}
	if s.popup.labels[0] != "(all)" {
		t.Errorf("p key: first label should be '(all)', got %q", s.popup.labels[0])
	}
}

func TestProjectModal_EnterConfirms(t *testing.T) {
	s := newTestTaskListScreen()
	s.projects = []*orchestrator.Project{
		{ID: "p1", Meta: orchestrator.ProjectMeta{Name: "proj1"}},
		{ID: "p2", Meta: orchestrator.ProjectMeta{Name: "proj2"}},
	}

	// Open modal and move cursor to proj1 (index 1)
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.popup.cursor != 1 {
		t.Fatalf("expected popup cursor 1, got %d", s.popup.cursor)
	}

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.popup.active {
		t.Error("enter: popup should be closed")
	}
	if s.selectedProjectID != "p1" {
		t.Errorf("enter: selectedProjectID should be 'p1', got %q", s.selectedProjectID)
	}
	if cmd == nil {
		t.Error("enter: expected fetchTasksCmd")
	}
}

func TestProjectModal_EnterAll(t *testing.T) {
	s := newTestTaskListScreen()
	s.projects = []*orchestrator.Project{
		{ID: "p1", Meta: orchestrator.ProjectMeta{Name: "proj1"}},
	}
	s.selectedProjectID = "p1"

	// Open modal — cursor should be at p1 (index 1)
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	// Move cursor to (all) at index 0
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if s.selectedProjectID != "" {
		t.Errorf("selecting (all) should set selectedProjectID to '', got %q", s.selectedProjectID)
	}
}

func TestProjectModal_EscCancels(t *testing.T) {
	s := newTestTaskListScreen()
	s.projects = []*orchestrator.Project{
		{ID: "p1", Meta: orchestrator.ProjectMeta{Name: "proj1"}},
	}
	s.selectedProjectID = "p1"

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	// Move cursor to (all)
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	s.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if s.popup.active {
		t.Error("esc: popup should be closed")
	}
	if s.selectedProjectID != "p1" {
		t.Errorf("esc: selectedProjectID should remain 'p1', got %q", s.selectedProjectID)
	}
}

// --- behavior modal tests ---

func TestBehaviorModal_BKey_Opens(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = []*orchestrator.Task{
		{ID: "t1", Title: "T1", Status: orchestrator.TaskStatusExecuting, Behavior: "dev", CreatedAt: time.Now()},
		{ID: "t2", Title: "T2", Status: orchestrator.TaskStatusExecuting, Behavior: "review", CreatedAt: time.Now()},
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	if !s.popup.active {
		t.Error("b key: popup should be active")
	}
	if s.popup.kind != "behavior" {
		t.Errorf("b key: popup kind should be 'behavior', got %q", s.popup.kind)
	}
	// labels: "(all)", "dev", "review" (sorted)
	if len(s.popup.labels) != 3 {
		t.Errorf("b key: want 3 popup labels, got %d", len(s.popup.labels))
	}
}

func TestBehaviorModal_EnterConfirms(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = []*orchestrator.Task{
		{ID: "t1", Title: "T1", Status: orchestrator.TaskStatusExecuting, Behavior: "dev", CreatedAt: time.Now()},
		{ID: "t2", Title: "T2", Status: orchestrator.TaskStatusExecuting, Behavior: "review", CreatedAt: time.Now()},
	}

	// Open modal, move to "dev" (index 1 after sort)
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if s.popup.active {
		t.Error("enter: popup should be closed")
	}
	if s.behaviorFilter != "dev" {
		t.Errorf("enter: behaviorFilter should be 'dev', got %q", s.behaviorFilter)
	}
	if cmd == nil {
		t.Error("enter: expected fetchTasksCmd")
	}
}

func TestBehaviorModal_EscCancels(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = []*orchestrator.Task{
		{ID: "t1", Title: "T1", Status: orchestrator.TaskStatusExecuting, Behavior: "dev", CreatedAt: time.Now()},
	}
	s.behaviorFilter = "dev"

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	s.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if s.popup.active {
		t.Error("esc: popup should be closed")
	}
	if s.behaviorFilter != "dev" {
		t.Errorf("esc: behaviorFilter should remain 'dev', got %q", s.behaviorFilter)
	}
}

// --- popup navigation tests ---

func TestPopupCursorNavigation(t *testing.T) {
	s := newTestTaskListScreen()
	s.projects = []*orchestrator.Project{
		{ID: "p1", Meta: orchestrator.ProjectMeta{Name: "proj1"}},
		{ID: "p2", Meta: orchestrator.ProjectMeta{Name: "proj2"}},
		{ID: "p3", Meta: orchestrator.ProjectMeta{Name: "proj3"}},
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	// cursor starts at 0 (all)
	if s.popup.cursor != 0 {
		t.Fatalf("initial popup cursor should be 0, got %d", s.popup.cursor)
	}

	// j moves down
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.popup.cursor != 1 {
		t.Errorf("j: want cursor 1, got %d", s.popup.cursor)
	}

	// down arrow moves down
	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	if s.popup.cursor != 2 {
		t.Errorf("down: want cursor 2, got %d", s.popup.cursor)
	}

	// k moves up
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.popup.cursor != 1 {
		t.Errorf("k: want cursor 1, got %d", s.popup.cursor)
	}

	// up arrow moves up
	s.Update(tea.KeyMsg{Type: tea.KeyUp})
	if s.popup.cursor != 0 {
		t.Errorf("up: want cursor 0, got %d", s.popup.cursor)
	}

	// Can't go below 0
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.popup.cursor != 0 {
		t.Errorf("k at start: cursor should stay 0, got %d", s.popup.cursor)
	}

	// Move to end and check can't go past
	s.popup.cursor = 3
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.popup.cursor != 3 {
		t.Errorf("j at end: cursor should stay 3, got %d", s.popup.cursor)
	}
}

func TestPopupBlocksNormalKeys(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(5)
	s.syncTableRows()
	s.projects = []*orchestrator.Project{
		{ID: "p1", Meta: orchestrator.ProjectMeta{Name: "proj1"}},
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	// j should move popup cursor, not task cursor
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.table.Cursor() != 0 {
		t.Errorf("popup active: task cursor should not move, got %d", s.table.Cursor())
	}
	if s.popup.cursor != 1 {
		t.Errorf("popup active: popup cursor should be 1, got %d", s.popup.cursor)
	}
}

// --- distinctBehaviors tests ---

func TestDistinctBehaviors_Empty(t *testing.T) {
	result := distinctBehaviors(nil)
	if len(result) != 0 {
		t.Errorf("nil tasks: expected empty slice, got %v", result)
	}
}

func TestDistinctBehaviors_Sorted(t *testing.T) {
	tasks := []*orchestrator.Task{
		{Behavior: "review"},
		{Behavior: "dev"},
		{Behavior: "dev"},
		{Behavior: "audit"},
		{Behavior: ""},
	}
	result := distinctBehaviors(tasks)
	want := []string{"audit", "dev", "review"}
	if len(result) != len(want) {
		t.Fatalf("want %v, got %v", want, result)
	}
	for i, v := range want {
		if result[i] != v {
			t.Errorf("index %d: want %q, got %q", i, v, result[i])
		}
	}
}

// --- filter bar view tests ---

func TestTaskListView_FilterBarNewFormat(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(2)
	s.syncTableRows()

	view := s.View(120, 40)
	if view == "" {
		t.Error("View() returned empty string")
	}
	// Should contain new filter chip labels
	if !containsStr(view, "state: open") {
		t.Error("View should contain 'state: open' filter chip")
	}
	if !containsStr(view, "ws: all") {
		t.Error("View should contain 'ws: all' filter chip")
	}
	if !containsStr(view, "proj: all") {
		t.Error("View should contain 'proj: all' filter chip")
	}
	if !containsStr(view, "behavior: all") {
		t.Error("View should contain 'behavior: all' filter chip")
	}
}

func TestTaskListView_StateClosedFilterBar(t *testing.T) {
	s := newTestTaskListScreen()
	s.stateClosed = true

	view := s.View(120, 40)
	if !containsStr(view, "state: closed") {
		t.Error("View should contain 'state: closed' when stateClosed=true")
	}
}

func TestTaskListView_PopupRendered(t *testing.T) {
	s := newTestTaskListScreen()
	s.projects = []*orchestrator.Project{
		{ID: "p1", Meta: orchestrator.ProjectMeta{Name: "proj1"}},
	}
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})

	view := s.View(120, 40)
	if !containsStr(view, "Select project:") {
		t.Error("View should contain 'Select project:' when popup is active")
	}
	if !containsStr(view, "(all)") {
		t.Error("View should contain '(all)' option in popup")
	}
	if !containsStr(view, "proj1") {
		t.Error("View should contain 'proj1' option in popup")
	}
}

// --- workspace selector tests ---

func TestWorkspaceModal_WKey_Opens(t *testing.T) {
	s := newTestTaskListScreen()
	s.workspaces = []*orchestrator.WorkspaceSummary{
		{ID: "ws1"},
		{ID: "ws2"},
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	if !s.popup.active {
		t.Error("w key: popup should be active")
	}
	if s.popup.kind != "workspace" {
		t.Errorf("w key: popup kind should be 'workspace', got %q", s.popup.kind)
	}
	// labels: "(all)", "ws1", "ws2"
	if len(s.popup.labels) != 3 {
		t.Errorf("w key: want 3 popup labels (all + 2 workspaces), got %d", len(s.popup.labels))
	}
	if s.popup.labels[0] != "(all)" {
		t.Errorf("w key: first label should be '(all)', got %q", s.popup.labels[0])
	}
}

func TestWorkspaceModal_EnterConfirms(t *testing.T) {
	s := newTestTaskListScreen()
	s.workspaces = []*orchestrator.WorkspaceSummary{
		{ID: "ws1"},
		{ID: "ws2"},
	}

	// Open modal and move cursor to ws1 (index 1)
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.popup.cursor != 1 {
		t.Fatalf("expected popup cursor 1, got %d", s.popup.cursor)
	}

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.popup.active {
		t.Error("enter: popup should be closed")
	}
	if s.selectedWorkspaceID != "ws1" {
		t.Errorf("enter: selectedWorkspaceID should be 'ws1', got %q", s.selectedWorkspaceID)
	}
	if cmd == nil {
		t.Error("enter: expected fetchTasksCmd")
	}
}

func TestWorkspaceModal_EnterAll(t *testing.T) {
	s := newTestTaskListScreen()
	s.workspaces = []*orchestrator.WorkspaceSummary{
		{ID: "ws1"},
	}
	s.selectedWorkspaceID = "ws1"

	// Open modal — cursor should be at ws1 (index 1)
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	// Move cursor back to (all) at index 0
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if s.selectedWorkspaceID != "" {
		t.Errorf("selecting (all) should set selectedWorkspaceID to '', got %q", s.selectedWorkspaceID)
	}
}

func TestWorkspaceModal_EscCancels(t *testing.T) {
	s := newTestTaskListScreen()
	s.workspaces = []*orchestrator.WorkspaceSummary{
		{ID: "ws1"},
	}
	s.selectedWorkspaceID = "ws1"

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	s.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if s.popup.active {
		t.Error("esc: popup should be closed")
	}
	if s.selectedWorkspaceID != "ws1" {
		t.Errorf("esc: selectedWorkspaceID should remain 'ws1', got %q", s.selectedWorkspaceID)
	}
}

func TestWorkspaceModal_ResetsProjectOnWorkspaceChange(t *testing.T) {
	s := newTestTaskListScreen()
	s.workspaces = []*orchestrator.WorkspaceSummary{
		{ID: "ws1"},
		{ID: "ws2"},
	}
	s.projects = []*orchestrator.Project{
		{ID: "p1", WorkspaceID: "ws1", Meta: orchestrator.ProjectMeta{Name: "proj1"}},
		{ID: "p2", WorkspaceID: "ws2", Meta: orchestrator.ProjectMeta{Name: "proj2"}},
	}
	s.selectedProjectID = "p1"
	s.selectedWorkspaceID = "ws1"

	// Switch to ws2 — p1 does not belong to ws2, so project should reset.
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	s.popup.cursor = 2 // ws2 is index 2
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if s.selectedWorkspaceID != "ws2" {
		t.Errorf("selectedWorkspaceID should be 'ws2', got %q", s.selectedWorkspaceID)
	}
	if s.selectedProjectID != "" {
		t.Errorf("selectedProjectID should be reset to '' when project not in new workspace, got %q", s.selectedProjectID)
	}
}

func TestWorkspaceModal_KeepsProjectWhenSameWorkspace(t *testing.T) {
	s := newTestTaskListScreen()
	s.workspaces = []*orchestrator.WorkspaceSummary{
		{ID: "ws1"},
	}
	s.projects = []*orchestrator.Project{
		{ID: "p1", WorkspaceID: "ws1", Meta: orchestrator.ProjectMeta{Name: "proj1"}},
	}
	s.selectedProjectID = "p1"

	// Select ws1 — p1 belongs to ws1, so project should remain.
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	s.popup.cursor = 1 // ws1
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if s.selectedWorkspaceID != "ws1" {
		t.Errorf("selectedWorkspaceID should be 'ws1', got %q", s.selectedWorkspaceID)
	}
	if s.selectedProjectID != "p1" {
		t.Errorf("selectedProjectID should remain 'p1' when project belongs to new workspace, got %q", s.selectedProjectID)
	}
}

func TestProjectPopup_FilteredByWorkspace(t *testing.T) {
	s := newTestTaskListScreen()
	s.projects = []*orchestrator.Project{
		{ID: "p1", WorkspaceID: "ws1", Meta: orchestrator.ProjectMeta{Name: "proj1"}},
		{ID: "p2", WorkspaceID: "ws2", Meta: orchestrator.ProjectMeta{Name: "proj2"}},
	}
	s.selectedWorkspaceID = "ws1"

	// Open project popup — only proj1 should appear.
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if !s.popup.active {
		t.Fatal("p key: popup should be active")
	}
	// labels: "(all)", "proj1" — proj2 filtered out
	if len(s.popup.labels) != 2 {
		t.Errorf("want 2 labels (all + proj1), got %d: %v", len(s.popup.labels), s.popup.labels)
	}
	if s.popup.labels[1] != "proj1" {
		t.Errorf("second label should be 'proj1', got %q", s.popup.labels[1])
	}
}

func TestWorkspaceFilterBarChip(t *testing.T) {
	s := newTestTaskListScreen()
	s.selectedWorkspaceID = "ws1"
	view := s.View(120, 40)
	if !containsStr(view, "ws: ws1") {
		t.Error("View should contain 'ws: ws1' when workspace is selected")
	}
}

func TestWorkspaceFilterBar_AllWhenNoWorkspace(t *testing.T) {
	s := newTestTaskListScreen()
	view := s.View(120, 40)
	if !containsStr(view, "ws: all") {
		t.Error("View should contain 'ws: all' when no workspace selected")
	}
}

func TestWorkspaceMsg_Stored(t *testing.T) {
	s := newTestTaskListScreen()
	ws := []*orchestrator.WorkspaceSummary{{ID: "ws1"}, {ID: "ws2"}}
	s.Update(workspacesMsg{workspaces: ws})
	if len(s.workspaces) != 2 {
		t.Errorf("expected 2 workspaces, got %d", len(s.workspaces))
	}
}

func TestWorkspaceMsg_ErrorIgnored(t *testing.T) {
	s := newTestTaskListScreen()
	s.Update(workspacesMsg{err: fmt.Errorf("network error")})
	if len(s.workspaces) != 0 {
		t.Errorf("expected workspaces unchanged on error, got %d", len(s.workspaces))
	}
}

// --- task status display tests ---

func TestTaskStatusDisplayContainsANSI(t *testing.T) {
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusReworking,
		orchestrator.TaskStatusVerifying,
	}
	for _, s := range statuses {
		dot, text := taskStatusDisplay(s)
		if !strings.Contains(dot, "\x1b") {
			t.Errorf("status %q: dot should contain ANSI code, got %q", s, dot)
		}
		if !strings.Contains(text, "\x1b") {
			t.Errorf("status %q: text should contain ANSI code, got %q", s, text)
		}
	}
}

func TestTaskStatusDisplay(t *testing.T) {
	tests := []struct {
		status     orchestrator.TaskStatus
		wantIcon   string
		wantStatus string
	}{
		{orchestrator.TaskStatusExecuting, "●", "executing"},
		{orchestrator.TaskStatusReworking, "●", "reworking"},
		{orchestrator.TaskStatusVerifying, "●", "verifying"},
		{orchestrator.TaskStatusPending, "○", "pending"},
		{orchestrator.TaskStatusDone, "✓", "done"},
		{orchestrator.TaskStatusAborted, "✗", "aborted"},
	}

	for _, tc := range tests {
		dot, text := taskStatusDisplay(tc.status)
		if !containsStr(dot, tc.wantIcon) {
			t.Errorf("status %q: dot should contain %q, got %q", tc.status, tc.wantIcon, dot)
		}
		if !containsStr(text, tc.wantStatus) {
			t.Errorf("status %q: text should contain %q, got %q", tc.status, tc.wantStatus, text)
		}
	}
}

func TestFormatTaskElapsed(t *testing.T) {
	now := time.Now()

	tests := []struct {
		created time.Time
		want    string
	}{
		{now.Add(-30 * time.Second), "30s"},
		{now.Add(-5 * time.Minute), "5m"},
		{now.Add(-2 * time.Hour), "2h"},
	}

	for _, tc := range tests {
		got := formatTaskElapsed(tc.created)
		if got != tc.want {
			t.Errorf("formatTaskElapsed(%v ago): want %q, got %q", time.Since(tc.created), tc.want, got)
		}
	}
}

// --- quick open tests ---

func TestQuickOpenKeyNoTasks(t *testing.T) {
	s := newTestTaskListScreen()
	// No tasks: o key should do nothing (no statusMsg set)
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if s.statusMsg != "" {
		t.Errorf("o with no tasks: want empty statusMsg, got %q", s.statusMsg)
	}
}

func TestQuickOpenKeySetLoadingMsg(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(1)
	s.syncTableRows()
	// o key should set "loading..." immediately
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if s.statusMsg != "loading..." {
		t.Errorf("o with task: want statusMsg %q, got %q", "loading...", s.statusMsg)
	}
}

func TestQuickOpenResultNoJobs(t *testing.T) {
	s := newTestTaskListScreen()
	_, cmd := s.Update(quickOpenResultMsg{taskID: "t1", jobs: nil})
	if s.statusMsg != "no active job" {
		t.Errorf("0 jobs: want statusMsg %q, got %q", "no active job", s.statusMsg)
	}
	if s.mini.active {
		t.Error("0 jobs: mini selector should not be active")
	}
	if cmd == nil {
		t.Error("0 jobs: expected clearStatus cmd")
	}
}

func TestQuickOpenResultOneJobTmuxDisabled(t *testing.T) {
	s := newTestTaskListScreen() // TmuxEnabled=false
	jobs := makeDummyJobs(1)
	_, cmd := s.Update(quickOpenResultMsg{taskID: "t1", jobs: jobs})
	if s.mini.active {
		t.Error("1 job: mini selector should not be active")
	}
	if cmd == nil {
		t.Error("1 job: expected a cmd")
	}
}

func TestQuickOpenResultOneJobTmuxEnabled(t *testing.T) {
	s := newTestTaskListScreen()
	s.shared.TmuxEnabled = true
	jobs := makeDummyJobs(1)
	_, cmd := s.Update(quickOpenResultMsg{taskID: "t1", jobs: jobs})
	if s.mini.active {
		t.Error("1 job tmux: mini selector should not be active")
	}
	if cmd == nil {
		t.Error("1 job tmux: expected openJobCmd")
	}
}

func TestQuickOpenResultMultipleJobs(t *testing.T) {
	s := newTestTaskListScreen()
	jobs := makeDummyJobs(3)
	_, cmd := s.Update(quickOpenResultMsg{taskID: "t1", jobs: jobs})
	if !s.mini.active {
		t.Error("multiple jobs: mini selector should be active")
	}
	if s.mini.cursor != 0 {
		t.Errorf("multiple jobs: want cursor 0, got %d", s.mini.cursor)
	}
	if len(s.mini.jobs) != 3 {
		t.Errorf("multiple jobs: want 3 jobs in mini, got %d", len(s.mini.jobs))
	}
	if cmd != nil {
		t.Error("multiple jobs: expected nil cmd (no action yet)")
	}
}

// --- mini selector tests ---

func TestMiniSelectorCursorMovement(t *testing.T) {
	s := newTestTaskListScreen()
	s.mini = miniSelector{jobs: makeDummyJobs(3), cursor: 0, active: true}

	// right key moves cursor forward
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.mini.cursor != 1 {
		t.Errorf("j: want cursor 1, got %d", s.mini.cursor)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyRight})
	if s.mini.cursor != 2 {
		t.Errorf("right: want cursor 2, got %d", s.mini.cursor)
	}
	// Can't go past end
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.mini.cursor != 2 {
		t.Errorf("j at end: cursor should stay 2, got %d", s.mini.cursor)
	}

	// k/left moves cursor back
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.mini.cursor != 1 {
		t.Errorf("k: want cursor 1, got %d", s.mini.cursor)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if s.mini.cursor != 0 {
		t.Errorf("left: want cursor 0, got %d", s.mini.cursor)
	}
	// Can't go below 0
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.mini.cursor != 0 {
		t.Errorf("k at start: cursor should stay 0, got %d", s.mini.cursor)
	}
}

func TestMiniSelectorEsc(t *testing.T) {
	s := newTestTaskListScreen()
	s.mini = miniSelector{jobs: makeDummyJobs(2), cursor: 1, active: true}

	s.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if s.mini.active {
		t.Error("esc: mini selector should be closed")
	}
}

func TestMiniSelectorEnterTmuxDisabled(t *testing.T) {
	s := newTestTaskListScreen() // TmuxEnabled=false
	s.mini = miniSelector{jobs: makeDummyJobs(2), cursor: 1, active: true}

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.mini.active {
		t.Error("enter: mini selector should be closed")
	}
	if cmd == nil {
		t.Error("enter: expected a cmd (clearStatus)")
	}
}

func TestMiniSelectorEnterTmuxEnabled(t *testing.T) {
	s := newTestTaskListScreen()
	s.shared.TmuxEnabled = true
	s.mini = miniSelector{jobs: makeDummyJobs(2), cursor: 0, active: true}

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.mini.active {
		t.Error("enter: mini selector should be closed")
	}
	if cmd == nil {
		t.Error("enter tmux: expected openJobCmd")
	}
}

// --- start keybinding tests ---

func TestStartKey_SetsLoadingMsg(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = []*orchestrator.Task{
		{ID: "task-1", Title: "Pending", Status: orchestrator.TaskStatusPending, Behavior: "dev", CreatedAt: time.Now()},
	}
	s.syncTableRows()
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if s.statusMsg != "starting..." {
		t.Errorf("s on pending task: want statusMsg %q, got %q", "starting...", s.statusMsg)
	}
	if s.isError {
		t.Error("s on pending task: expected isError=false")
	}
}

func TestStartKey_PendingTaskInList(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = []*orchestrator.Task{
		{ID: "task-1", Title: "Pending", Status: orchestrator.TaskStatusPending, Behavior: "dev", CreatedAt: time.Now()},
	}
	s.syncTableRows()
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd == nil {
		t.Error("s on pending task: expected non-nil cmd (applyActionCmd)")
	}
}

func TestStartKey_NonPendingTaskInList(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = []*orchestrator.Task{
		{ID: "task-1", Title: "Running", Status: orchestrator.TaskStatusExecuting, Behavior: "dev", CreatedAt: time.Now()},
	}
	s.syncTableRows()
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd != nil {
		t.Error("s on executing task: expected nil cmd")
	}
}

func TestStartKey_NoTasksInList(t *testing.T) {
	s := newTestTaskListScreen()
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd != nil {
		t.Error("s with no tasks: expected nil cmd")
	}
}

func TestApplyActionResult_Success_RefreshesTaskList(t *testing.T) {
	s := newTestTaskListScreen()
	_, cmd := s.Update(applyActionResultMsg{err: nil})
	if cmd == nil {
		t.Error("success result: expected fetchTasksCmd")
	}
	if s.statusMsg != "" {
		t.Errorf("success result: expected empty statusMsg, got %q", s.statusMsg)
	}
}

func TestApplyActionResult_Error_SetsStatusMsgInList(t *testing.T) {
	s := newTestTaskListScreen()
	s.Update(applyActionResultMsg{err: fmt.Errorf("server error")})
	if s.statusMsg == "" {
		t.Error("error result: expected statusMsg to be set")
	}
	if !s.isError {
		t.Error("error result: expected isError=true")
	}
}

func TestMiniSelectorBlocksNormalKeys(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(3)
	s.syncTableRows()
	s.mini = miniSelector{jobs: makeDummyJobs(2), cursor: 0, active: true}

	// j should move mini cursor, not task cursor
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.table.Cursor() != 0 {
		t.Errorf("mini active: task cursor should not move, got %d", s.table.Cursor())
	}
	if s.mini.cursor != 1 {
		t.Errorf("mini active: mini cursor should be 1, got %d", s.mini.cursor)
	}
}

// --- scroll tests ---

func TestViewScrollFollowsCursorDown(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(10)
	s.syncTableRows()

	// bodyHeight = height - 2 (filterbar + sep) = 5 - 2 = 3
	// table header takes 1 line → 2 data rows visible.
	// Set table height so MoveDown can scroll correctly.
	s.table.SetHeight(3)

	// Move cursor to index 3 by pressing j 3 times.
	for i := 0; i < 3; i++ {
		s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	}
	view := s.View(120, 5)

	// Task 0 should NOT be visible (scrolled away).
	if containsStr(view, "Task 0") {
		t.Error("Task 0 should not be visible when cursor=3 and bodyHeight=3")
	}
	// Task 3 (cursor) should be visible.
	if !containsStr(view, "Task 3") {
		t.Error("Task 3 (cursor) should be visible")
	}
}

func TestViewScrollFollowsCursorUp(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(10)
	s.syncTableRows()
	s.table.SetHeight(3)

	// Move cursor to 5 by pressing j 5 times.
	for i := 0; i < 5; i++ {
		s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	}
	view := s.View(120, 5)
	if containsStr(view, "Task 0") {
		t.Error("Task 0 should not be visible when cursor=5")
	}

	// Move cursor back to 0 by pressing k 5 times.
	for i := 0; i < 5; i++ {
		s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	}
	view = s.View(120, 5)
	if !containsStr(view, "Task 0") {
		t.Error("Task 0 should be visible when cursor=0")
	}
	if containsStr(view, "Task 5") {
		t.Error("Task 5 should not be visible when cursor=0 and bodyHeight=3")
	}
}

func TestViewCursorHighlightedCorrectlyWhenScrolled(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(10)
	s.syncTableRows()
	s.table.SetHeight(3)
	// Move cursor to 4 by pressing j 4 times.
	for i := 0; i < 4; i++ {
		s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	}
	view := s.View(120, 5)
	// Task 4 should be visible in the view.
	if !containsStr(view, "Task 4") {
		t.Error("Task 4 should be visible when cursor=4")
	}
}

// --- helpers ---

func makeDummyTasks(n int) []*orchestrator.Task {
	tasks := make([]*orchestrator.Task, n)
	for i := range tasks {
		tasks[i] = &orchestrator.Task{
			ID:        fmt.Sprintf("task-%d", i),
			Title:     fmt.Sprintf("Task %d", i),
			Status:    orchestrator.TaskStatusExecuting,
			Behavior:  "dev",
			CreatedAt: time.Now().Add(-time.Duration(i) * time.Minute),
		}
	}
	return tasks
}

func makeDummyJobs(n int) []*api.Job {
	jobs := make([]*api.Job, n)
	roles := []string{"main", "verification", "review"}
	for i := range jobs {
		role := ""
		if i < len(roles) {
			role = roles[i]
		}
		jobs[i] = &api.Job{
			ID:        fmt.Sprintf("job-%08d", i),
			Status:    api.JobStatusRunning,
			Role:      role,
			CreatedAt: time.Now().Add(-time.Duration(i) * time.Minute),
		}
	}
	return jobs
}

// --- syncTableRows ANSI stripping tests ---

func TestSyncTableRows_StatusCellNoANSI(t *testing.T) {
	s := newTestTaskListScreen()
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusReworking,
		orchestrator.TaskStatusVerifying,
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
	}
	for _, status := range statuses {
		s.tasks = []*orchestrator.Task{
			{ID: "t1", Title: "Test", Status: status, Behavior: "dev", CreatedAt: time.Now()},
		}
		s.syncTableRows()
		rows := s.table.Rows()
		if len(rows) == 0 {
			t.Fatalf("status %q: no rows after syncTableRows", status)
		}
		statusCell := rows[0][0]
		if strings.Contains(statusCell, "\x1b") {
			t.Errorf("status %q: STATUS cell contains ANSI code: %q", status, statusCell)
		}
	}
}

// --- BEHAVIOR column initial width tests ---

func TestBehaviorColumnInitialWidth(t *testing.T) {
	s := newTestTaskListScreen()
	const wantWidth = 10 // must match behaviorWidth in recalcColumns
	cols := s.table.Columns()
	for _, c := range cols {
		if c.Title == "BEHAVIOR" {
			if c.Width != wantWidth {
				t.Errorf("BEHAVIOR initial width: want %d, got %d", wantWidth, c.Width)
			}
			return
		}
	}
	t.Error("BEHAVIOR column not found in initial columns")
}

// --- recalcColumns tests ---

func TestRecalcColumns_NormalWidth(t *testing.T) {
	s := newTestTaskListScreen()
	s.recalcColumns(100)

	// TITLE should get the remainder: 100 - (11+12+10+6) - 6 = 55
	if s.titleWidth <= 20 {
		t.Errorf("width=100: expected titleWidth > 20, got %d", s.titleWidth)
	}

	// Check that column slice was updated (TITLE column should match titleWidth).
	cols := s.table.Columns()
	var titleCol int = -1
	for _, c := range cols {
		if c.Title == "TITLE" {
			titleCol = c.Width
		}
	}
	if titleCol != s.titleWidth {
		t.Errorf("width=100: TITLE column width %d != titleWidth %d", titleCol, s.titleWidth)
	}
}

func TestRecalcColumns_SmallWidth(t *testing.T) {
	s := newTestTaskListScreen()
	s.recalcColumns(10)

	if s.titleWidth != 20 {
		t.Errorf("very small width: expected titleWidth=20 (minimum), got %d", s.titleWidth)
	}
}

// --- q key tests ---

// TestQKey_ReturnsQuit verifies q returns tea.Quit when mini selector is not active.
func TestQKey_ReturnsQuit(t *testing.T) {
	s := newTestTaskListScreen()

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q key: expected non-nil cmd (tea.Quit)")
	}
	result := cmd()
	if _, ok := result.(tea.QuitMsg); !ok {
		t.Errorf("q key: expected QuitMsg, got %T", result)
	}
}

// TestQKey_NoQuit_WhenMiniActive verifies q does not quit when mini selector is active.
func TestQKey_NoQuit_WhenMiniActive(t *testing.T) {
	s := newTestTaskListScreen()
	s.mini = miniSelector{jobs: makeDummyJobs(2), cursor: 0, active: true}

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd != nil {
		result := cmd()
		if _, ok := result.(tea.QuitMsg); ok {
			t.Error("q when mini active: should not return QuitMsg")
		}
	}
}

// --- search mode tests ---

func TestSearchMode_SlashOpens(t *testing.T) {
	s := newTestTaskListScreen()
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if !s.searchMode {
		t.Error("/ key: searchMode should be true")
	}
}

func TestSearchMode_EscClearsAndExits(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(5)
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	s.searchInput.SetValue("Task")
	s.searchQuery = "Task"
	s.syncTableRows()

	s.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if s.searchMode {
		t.Error("esc: searchMode should be false")
	}
	if s.searchQuery != "" {
		t.Errorf("esc: searchQuery should be cleared, got %q", s.searchQuery)
	}
}

func TestSearchMode_EnterKeepsQueryAndExits(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(5)
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	s.searchInput.SetValue("Task 1")
	s.searchQuery = "Task 1"

	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.searchMode {
		t.Error("enter: searchMode should be false")
	}
	if s.searchQuery != "Task 1" {
		t.Errorf("enter: searchQuery should be retained as 'Task 1', got %q", s.searchQuery)
	}
}

func TestSearchMode_SlashReopensWithRetainedQuery(t *testing.T) {
	s := newTestTaskListScreen()
	// Simulate retained query (not in search mode)
	s.searchQuery = "hello"
	s.searchInput.SetValue("hello")

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if !s.searchMode {
		t.Error("/: should re-enter searchMode")
	}
	if s.searchInput.Value() != "hello" {
		t.Errorf("/: searchInput should carry retained query 'hello', got %q", s.searchInput.Value())
	}
}

func TestSearchMode_IncrementalFilter(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = []*orchestrator.Task{
		{ID: "t1", Title: "Alpha Task", Status: orchestrator.TaskStatusExecuting, CreatedAt: time.Now()},
		{ID: "t2", Title: "Beta Work", Status: orchestrator.TaskStatusExecuting, CreatedAt: time.Now()},
		{ID: "t3", Title: "Alpha Work", Status: orchestrator.TaskStatusExecuting, CreatedAt: time.Now()},
	}
	s.syncTableRows()

	// Set query directly and re-sync (simulates typing)
	s.searchMode = true
	s.searchQuery = "alpha"
	s.searchInput.SetValue("alpha")
	s.syncTableRows()

	if len(s.displayTasks) != 2 {
		t.Errorf("filter 'alpha': want 2 displayTasks, got %d", len(s.displayTasks))
	}
}

func TestSearchMode_CaseInsensitive(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = []*orchestrator.Task{
		{ID: "t1", Title: "Alpha Task", Status: orchestrator.TaskStatusExecuting, CreatedAt: time.Now()},
		{ID: "t2", Title: "Beta Work", Status: orchestrator.TaskStatusExecuting, CreatedAt: time.Now()},
	}
	s.searchQuery = "ALPHA"
	s.syncTableRows()

	if len(s.displayTasks) != 1 {
		t.Errorf("filter 'ALPHA': want 1 displayTask, got %d", len(s.displayTasks))
	}
	if s.displayTasks[0].ID != "t1" {
		t.Errorf("filter 'ALPHA': want task t1, got %s", s.displayTasks[0].ID)
	}
}

func TestSearchMode_ChipShownWhenQueryNonEmpty(t *testing.T) {
	s := newTestTaskListScreen()
	s.searchQuery = "hello"

	view := s.View(120, 40)
	if !containsStr(view, "q: hello") {
		t.Error("View should contain 'q: hello' chip when query is non-empty")
	}
}

func TestSearchMode_ChipHiddenWhenQueryEmpty(t *testing.T) {
	s := newTestTaskListScreen()

	view := s.View(120, 40)
	if containsStr(view, "q:") {
		t.Error("View should not contain 'q:' chip when query is empty")
	}
}

func TestSearchMode_BlocksNormalKeys(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(5)
	s.syncTableRows()
	s.table.SetCursor(0)

	// Enter search mode
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if !s.searchMode {
		t.Fatal("/ key should enter search mode")
	}

	// 'j' in search mode should type 'j' into the search field, NOT do table MoveDown.
	// Normal table navigation would move cursor from 0 → 1.
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.table.Cursor() == 1 {
		t.Error("search mode: j should not navigate the task table (cursor moved to 1)")
	}
	// Confirm the key went to the search input.
	if s.searchInput.Value() != "j" {
		t.Errorf("search mode: searchInput should contain 'j', got %q", s.searchInput.Value())
	}
}

func TestSearchMode_ShortHelp(t *testing.T) {
	s := newTestTaskListScreen()

	normal := s.ShortHelp()
	if !containsStr(normal, "/: search") {
		t.Error("normal mode ShortHelp should mention /: search")
	}

	s.searchMode = true
	search := s.ShortHelp()
	if !containsStr(search, "esc: cancel") {
		t.Error("search mode ShortHelp should mention esc: cancel")
	}
	if !containsStr(search, "enter: confirm") {
		t.Error("search mode ShortHelp should mention enter: confirm")
	}
}

func TestSearchMode_ANDWithOtherFilters(t *testing.T) {
	s := newTestTaskListScreen()
	// Simulate fetchTasksCmd already filtered to "dev" behavior tasks only
	s.tasks = []*orchestrator.Task{
		{ID: "t1", Title: "Alpha Dev", Status: orchestrator.TaskStatusExecuting, Behavior: "dev", CreatedAt: time.Now()},
		{ID: "t3", Title: "Beta Dev", Status: orchestrator.TaskStatusExecuting, Behavior: "dev", CreatedAt: time.Now()},
	}
	s.searchQuery = "alpha"
	s.syncTableRows()

	if len(s.displayTasks) != 1 {
		t.Errorf("AND filter: want 1 displayTask, got %d", len(s.displayTasks))
	}
	if s.displayTasks[0].ID != "t1" {
		t.Errorf("AND filter: want task t1, got %s", s.displayTasks[0].ID)
	}
}

func TestSearchMode_EmptyQueryShowsAll(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(5)
	s.searchQuery = ""
	s.syncTableRows()

	if len(s.displayTasks) != 5 {
		t.Errorf("empty query: want 5 displayTasks, got %d", len(s.displayTasks))
	}
}

func TestSearchMode_CursorResetOnQueryChange(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(5)
	s.syncTableRows()
	s.table.SetCursor(3)

	// Enter search mode and type 't' — all "Task N" titles contain 't', so 5 tasks remain
	// visible and the cursor should reset to 0.
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if s.table.Cursor() != 0 {
		t.Errorf("typing in search: cursor should reset to 0, got %d", s.table.Cursor())
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && searchStr(s, substr)
}

func searchStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- buildTreeOrder tests ---

func TestBuildTreeOrder_FlatList(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "a", CreatedAt: time.Now()},
		{ID: "b", CreatedAt: time.Now()},
	}
	ordered, depths := buildTreeOrder(tasks)
	if len(ordered) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(ordered))
	}
	if ordered[0].ID != "a" || ordered[1].ID != "b" {
		t.Errorf("want [a, b], got [%s, %s]", ordered[0].ID, ordered[1].ID)
	}
	if depths["a"] != 0 || depths["b"] != 0 {
		t.Errorf("root tasks should have depth 0, got a=%d b=%d", depths["a"], depths["b"])
	}
}

func TestBuildTreeOrder_TwoLevel(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "p", ParentID: "", CreatedAt: time.Now()},
		{ID: "c1", ParentID: "p", CreatedAt: time.Now()},
		{ID: "c2", ParentID: "p", CreatedAt: time.Now()},
	}
	ordered, depths := buildTreeOrder(tasks)
	if len(ordered) != 3 {
		t.Fatalf("want 3 tasks, got %d", len(ordered))
	}
	if ordered[0].ID != "p" {
		t.Errorf("first should be p, got %s", ordered[0].ID)
	}
	if ordered[1].ID != "c1" || ordered[2].ID != "c2" {
		t.Errorf("children should follow parent in input order: got %s, %s", ordered[1].ID, ordered[2].ID)
	}
	if depths["p"] != 0 || depths["c1"] != 1 || depths["c2"] != 1 {
		t.Errorf("depths: want p=0,c1=1,c2=1; got p=%d,c1=%d,c2=%d", depths["p"], depths["c1"], depths["c2"])
	}
}

func TestBuildTreeOrder_ThreeLevel(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "p", ParentID: ""},
		{ID: "c", ParentID: "p"},
		{ID: "gc", ParentID: "c"},
	}
	ordered, depths := buildTreeOrder(tasks)
	if len(ordered) != 3 {
		t.Fatalf("want 3, got %d", len(ordered))
	}
	if ordered[0].ID != "p" || ordered[1].ID != "c" || ordered[2].ID != "gc" {
		t.Errorf("want order p,c,gc; got %s,%s,%s", ordered[0].ID, ordered[1].ID, ordered[2].ID)
	}
	if depths["p"] != 0 || depths["c"] != 1 || depths["gc"] != 2 {
		t.Errorf("depths: want p=0,c=1,gc=2; got p=%d,c=%d,gc=%d", depths["p"], depths["c"], depths["gc"])
	}
}

func TestBuildTreeOrder_OrphanedParent(t *testing.T) {
	// "c" has a parent not in the set → treated as root
	tasks := []*orchestrator.Task{
		{ID: "c", ParentID: "missing"},
	}
	ordered, depths := buildTreeOrder(tasks)
	if len(ordered) != 1 {
		t.Fatalf("want 1 task, got %d", len(ordered))
	}
	if depths["c"] != 0 {
		t.Errorf("orphaned task should have depth 0, got %d", depths["c"])
	}
}

func TestBuildTreeOrder_SiblingOrder(t *testing.T) {
	// Siblings appear in input order
	tasks := []*orchestrator.Task{
		{ID: "p", ParentID: ""},
		{ID: "c2", ParentID: "p"},
		{ID: "c1", ParentID: "p"},
	}
	ordered, _ := buildTreeOrder(tasks)
	if len(ordered) != 3 {
		t.Fatalf("want 3, got %d", len(ordered))
	}
	if ordered[1].ID != "c2" || ordered[2].ID != "c1" {
		t.Errorf("siblings should preserve input order: got %s, %s", ordered[1].ID, ordered[2].ID)
	}
}

func TestBuildTreeOrder_MultipleRoots(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "r1", ParentID: ""},
		{ID: "r1c", ParentID: "r1"},
		{ID: "r2", ParentID: ""},
	}
	ordered, depths := buildTreeOrder(tasks)
	if len(ordered) != 3 {
		t.Fatalf("want 3, got %d", len(ordered))
	}
	// r1 → r1c, then r2
	if ordered[0].ID != "r1" || ordered[1].ID != "r1c" || ordered[2].ID != "r2" {
		t.Errorf("want r1,r1c,r2; got %s,%s,%s", ordered[0].ID, ordered[1].ID, ordered[2].ID)
	}
	if depths["r2"] != 0 {
		t.Errorf("r2 should have depth 0, got %d", depths["r2"])
	}
}

// --- progressBadge tests ---

func TestProgressBadge_NoChildren(t *testing.T) {
	task := &orchestrator.Task{TotalChildCount: 0}
	if got := progressBadge(task); got != "" {
		t.Errorf("want empty badge for childless task, got %q", got)
	}
}

func TestProgressBadge_WithChildren(t *testing.T) {
	task := &orchestrator.Task{TotalChildCount: 7, DoneChildCount: 3}
	if got := progressBadge(task); got != "3/7" {
		t.Errorf("want '3/7', got %q", got)
	}
}

func TestProgressBadge_WithAborted(t *testing.T) {
	task := &orchestrator.Task{TotalChildCount: 7, DoneChildCount: 3, AbortedChildCount: 1}
	if got := progressBadge(task); got != "3/7 [!1]" {
		t.Errorf("want '3/7 [!1]', got %q", got)
	}
}

func TestProgressBadge_ZeroAborted(t *testing.T) {
	task := &orchestrator.Task{TotalChildCount: 5, DoneChildCount: 5, AbortedChildCount: 0}
	if got := progressBadge(task); got != "5/5" {
		t.Errorf("want '5/5', got %q", got)
	}
}

// --- syncTableRows tree/progress tests ---

func TestSyncTableRows_Indent_Depth1(t *testing.T) {
	s := newTestTaskListScreen()
	s.recalcColumns(120)
	parent := &orchestrator.Task{ID: "p", Title: "Parent", Status: orchestrator.TaskStatusExecuting, CreatedAt: time.Now()}
	child := &orchestrator.Task{ID: "c", Title: "Child", ParentID: "p", Status: orchestrator.TaskStatusExecuting, CreatedAt: time.Now()}
	s.tasks = []*orchestrator.Task{parent, child}
	s.syncTableRows()

	rows := s.table.Rows()
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	parentTitle := rows[0][1]
	if strings.HasPrefix(parentTitle, "  ") {
		t.Errorf("parent row should not have 2-space indent, got %q", parentTitle)
	}
	childTitle := rows[1][1]
	if !strings.HasPrefix(childTitle, "  ") {
		t.Errorf("child row should start with 2-space indent, got %q", childTitle)
	}
	if !containsStr(childTitle, "Child") {
		t.Errorf("child title should contain 'Child', got %q", childTitle)
	}
}

func TestSyncTableRows_Indent_Depth2(t *testing.T) {
	s := newTestTaskListScreen()
	s.recalcColumns(120)
	parent := &orchestrator.Task{ID: "p", Title: "Parent", Status: orchestrator.TaskStatusExecuting, CreatedAt: time.Now()}
	child := &orchestrator.Task{ID: "c", Title: "Child", ParentID: "p", Status: orchestrator.TaskStatusExecuting, CreatedAt: time.Now()}
	grandchild := &orchestrator.Task{ID: "gc", Title: "GrandChild", ParentID: "c", Status: orchestrator.TaskStatusExecuting, CreatedAt: time.Now()}
	s.tasks = []*orchestrator.Task{parent, child, grandchild}
	s.syncTableRows()

	rows := s.table.Rows()
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	gcTitle := rows[2][1]
	if !strings.HasPrefix(gcTitle, "    ") {
		t.Errorf("grandchild should have 4-space indent, got %q", gcTitle)
	}
}

func TestSyncTableRows_ProgressInTitle(t *testing.T) {
	s := newTestTaskListScreen()
	s.recalcColumns(120)
	parent := &orchestrator.Task{
		ID:              "p",
		Title:           "Parent",
		Status:          orchestrator.TaskStatusExecuting,
		TotalChildCount: 7,
		DoneChildCount:  3,
		CreatedAt:       time.Now(),
	}
	s.tasks = []*orchestrator.Task{parent}
	s.syncTableRows()

	rows := s.table.Rows()
	titleCell := rows[0][1]
	if !containsStr(titleCell, "3/7") {
		t.Errorf("parent title should contain progress '3/7', got %q", titleCell)
	}
}

func TestSyncTableRows_ProgressWithAbortedBadge(t *testing.T) {
	s := newTestTaskListScreen()
	s.recalcColumns(120)
	parent := &orchestrator.Task{
		ID:                "p",
		Title:             "Parent",
		Status:            orchestrator.TaskStatusExecuting,
		TotalChildCount:   7,
		DoneChildCount:    3,
		AbortedChildCount: 1,
		CreatedAt:         time.Now(),
	}
	s.tasks = []*orchestrator.Task{parent}
	s.syncTableRows()

	rows := s.table.Rows()
	titleCell := rows[0][1]
	if !containsStr(titleCell, "[!1]") {
		t.Errorf("parent title should contain aborted badge '[!1]', got %q", titleCell)
	}
}

func TestSyncTableRows_NoProgressForChildless(t *testing.T) {
	s := newTestTaskListScreen()
	s.recalcColumns(120)
	task := &orchestrator.Task{
		ID:              "t",
		Title:           "Solo",
		Status:          orchestrator.TaskStatusExecuting,
		TotalChildCount: 0,
		CreatedAt:       time.Now(),
	}
	s.tasks = []*orchestrator.Task{task}
	s.syncTableRows()

	rows := s.table.Rows()
	titleCell := rows[0][1]
	// Should not contain a progress separator like "0/0" or "N/M"
	if containsStr(titleCell, "/") {
		t.Errorf("childless task title should not contain '/', got %q", titleCell)
	}
}

func TestSyncTableRows_TreeOrder(t *testing.T) {
	// Verify displayTasks reflects tree order (parent before child).
	s := newTestTaskListScreen()
	s.recalcColumns(120)
	parent := &orchestrator.Task{ID: "p", Title: "Parent", Status: orchestrator.TaskStatusExecuting, CreatedAt: time.Now()}
	child := &orchestrator.Task{ID: "c", Title: "Child", ParentID: "p", Status: orchestrator.TaskStatusExecuting, CreatedAt: time.Now()}
	s.tasks = []*orchestrator.Task{parent, child}
	s.syncTableRows()

	if len(s.displayTasks) != 2 {
		t.Fatalf("want 2 displayTasks, got %d", len(s.displayTasks))
	}
	if s.displayTasks[0].ID != "p" || s.displayTasks[1].ID != "c" {
		t.Errorf("displayTasks order: want [p, c], got [%s, %s]", s.displayTasks[0].ID, s.displayTasks[1].ID)
	}
}

// --- applyStateFilter tests ---

func TestApplyStateFilter_OpenMode_KeepsOpenTask(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "t", Status: orchestrator.TaskStatusExecuting, OpenChildCount: 0},
	}
	got := applyStateFilter(tasks, false)
	if len(got) != 1 {
		t.Errorf("open mode: executing task should pass filter, got %d tasks", len(got))
	}
}

func TestApplyStateFilter_OpenMode_KeepsClosedParentWithOpenChild(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "t", Status: orchestrator.TaskStatusDone, OpenChildCount: 2},
	}
	got := applyStateFilter(tasks, false)
	if len(got) != 1 {
		t.Errorf("open mode: done parent with open children should pass, got %d tasks", len(got))
	}
}

func TestApplyStateFilter_OpenMode_ExcludesClosedWithNoOpenChild(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "t", Status: orchestrator.TaskStatusDone, OpenChildCount: 0},
	}
	got := applyStateFilter(tasks, false)
	if len(got) != 0 {
		t.Errorf("open mode: done task with no open children should be excluded, got %d tasks", len(got))
	}
}

func TestApplyStateFilter_ClosedMode_KeepsClosedTask(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "t", Status: orchestrator.TaskStatusDone, OpenChildCount: 0},
	}
	got := applyStateFilter(tasks, true)
	if len(got) != 1 {
		t.Errorf("closed mode: done task should pass filter, got %d tasks", len(got))
	}
}

func TestApplyStateFilter_ClosedMode_ExcludesClosedWithOpenChild(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "t", Status: orchestrator.TaskStatusDone, OpenChildCount: 1},
	}
	got := applyStateFilter(tasks, true)
	if len(got) != 0 {
		t.Errorf("closed mode: done task with open children should be excluded, got %d tasks", len(got))
	}
}

func TestApplyStateFilter_ClosedMode_ExcludesOpenTask(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "t", Status: orchestrator.TaskStatusExecuting, OpenChildCount: 0},
	}
	got := applyStateFilter(tasks, true)
	if len(got) != 0 {
		t.Errorf("closed mode: executing task should be excluded, got %d tasks", len(got))
	}
}

func TestApplyStateFilter_OpenMode_AllStatuses(t *testing.T) {
	openTasks := []*orchestrator.Task{
		{Status: orchestrator.TaskStatusExecuting},
		{Status: orchestrator.TaskStatusReworking},
		{Status: orchestrator.TaskStatusVerifying},
		{Status: orchestrator.TaskStatusPending},
	}
	got := applyStateFilter(openTasks, false)
	if len(got) != 4 {
		t.Errorf("open mode: all open statuses should pass, got %d", len(got))
	}
}
