package tui

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func newTestTaskListScreen() *TaskListScreen {
	shared := &SharedState{Panes: make(map[string]string)}
	return &TaskListScreen{
		shared:       shared,
		statusFilter: "active",
	}
}

func TestTaskFilterTabCycle(t *testing.T) {
	s := newTestTaskListScreen()
	// Default is "active" (index 0)
	expected := []string{"pending", "done", "aborted", "all", "active"}
	for _, want := range expected {
		s.Update(tea.KeyMsg{Type: tea.KeyTab})
		if s.statusFilter != want {
			t.Errorf("tab: want filter %q, got %q", want, s.statusFilter)
		}
	}
}

func TestTaskFilterShiftTabCycle(t *testing.T) {
	s := newTestTaskListScreen()
	// Default is "active" (index 0), shift-tab goes backwards
	expected := []string{"all", "aborted", "done", "pending", "active"}
	for _, want := range expected {
		s.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
		if s.statusFilter != want {
			t.Errorf("shift+tab: want filter %q, got %q", want, s.statusFilter)
		}
	}
}

func TestTaskFilterTabResetsCursor(t *testing.T) {
	s := newTestTaskListScreen()
	s.cursor = 5
	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.cursor != 0 {
		t.Errorf("expected cursor 0 after filter change, got %d", s.cursor)
	}
}

func TestActiveFilterStatuses(t *testing.T) {
	want := map[orchestrator.TaskStatus]bool{
		orchestrator.TaskStatusExecuting:          true,
		orchestrator.TaskStatusReworking:          true,
		orchestrator.TaskStatusVerifying:          true,
		orchestrator.TaskStatusInReview:           true,
		orchestrator.TaskStatusCollectingFeedback: true,
	}

	if len(activeStatuses) != len(want) {
		t.Fatalf("activeStatuses has %d entries, want %d", len(activeStatuses), len(want))
	}
	for status := range want {
		if !activeStatuses[status] {
			t.Errorf("expected %q in activeStatuses", status)
		}
	}

	notActive := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
	}
	for _, status := range notActive {
		if activeStatuses[status] {
			t.Errorf("expected %q NOT in activeStatuses", status)
		}
	}
}

func TestCursorMovement(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(5)

	// Move down
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.cursor != 1 {
		t.Errorf("after j: want cursor 1, got %d", s.cursor)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.cursor != 2 {
		t.Errorf("after j j: want cursor 2, got %d", s.cursor)
	}

	// Move up
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.cursor != 1 {
		t.Errorf("after k: want cursor 1, got %d", s.cursor)
	}

	// Can't go below 0
	s.cursor = 0
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.cursor != 0 {
		t.Errorf("cursor should not go below 0, got %d", s.cursor)
	}

	// Can't go above len-1
	s.cursor = 4
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.cursor != 4 {
		t.Errorf("cursor should not exceed task count, got %d", s.cursor)
	}
}

func TestCursorMovementArrowKeys(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(3)

	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	if s.cursor != 1 {
		t.Errorf("after down: want cursor 1, got %d", s.cursor)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyUp})
	if s.cursor != 0 {
		t.Errorf("after up: want cursor 0, got %d", s.cursor)
	}
}

func TestProjectFilterCycle(t *testing.T) {
	s := newTestTaskListScreen()
	s.projects = []*orchestrator.Project{
		{ID: "p1", Meta: orchestrator.ProjectMeta{Name: "proj1"}},
		{ID: "p2", Meta: orchestrator.ProjectMeta{Name: "proj2"}},
	}

	// Start at all (0)
	if s.selectedProjectName() != "all" {
		t.Fatalf("initial project should be all, got %q", s.selectedProjectName())
	}

	// p -> proj1
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if s.selectedProjectName() != "proj1" {
		t.Errorf("after p: want proj1, got %q", s.selectedProjectName())
	}
	if s.selectedProjectID() != "p1" {
		t.Errorf("after p: want project ID p1, got %q", s.selectedProjectID())
	}

	// p -> proj2
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if s.selectedProjectName() != "proj2" {
		t.Errorf("after p p: want proj2, got %q", s.selectedProjectName())
	}

	// p -> back to all
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if s.selectedProjectName() != "all" {
		t.Errorf("after p p p: want all, got %q", s.selectedProjectName())
	}
}

func TestTaskListView(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(2)

	view := s.View(120, 40)
	if view == "" {
		t.Error("View() returned empty string")
	}
	// Should contain filter labels
	if !containsStr(view, "active") {
		t.Error("View should contain 'active' filter label")
	}
	if !containsStr(view, "project: all") {
		t.Error("View should contain 'project: all'")
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
		{orchestrator.TaskStatusInReview, "●", "in_review"},
		{orchestrator.TaskStatusCollectingFeedback, "●", "feedback"},
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

func TestMiniSelectorBlocksNormalKeys(t *testing.T) {
	s := newTestTaskListScreen()
	s.tasks = makeDummyTasks(3)
	s.cursor = 0
	s.mini = miniSelector{jobs: makeDummyJobs(2), cursor: 0, active: true}

	// j should move mini cursor, not task cursor
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.cursor != 0 {
		t.Errorf("mini active: task cursor should not move, got %d", s.cursor)
	}
	if s.mini.cursor != 1 {
		t.Errorf("mini active: mini cursor should be 1, got %d", s.mini.cursor)
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
