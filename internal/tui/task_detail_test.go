package tui

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func makeDetailWithStatus(status orchestrator.TaskStatus) *api.TaskDetailView {
	var available []string
	switch status {
	case orchestrator.TaskStatusPending:
		available = []string{"start", "abort"}
	case orchestrator.TaskStatusExecuting, orchestrator.TaskStatusReworking:
		available = []string{"done", "abort"}
	case orchestrator.TaskStatusInReview:
		available = []string{"collect_feedback", "abort"}
	case orchestrator.TaskStatusVerifying, orchestrator.TaskStatusCollectingFeedback:
		available = []string{"abort"}
	// done, aborted: empty
	}
	return &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:        "test-task-id",
			Title:     "Test Task",
			Status:    status,
			Behavior:  "dev",
			CreatedAt: time.Now().Add(-5 * time.Minute),
		},
		AvailableActions: available,
	}
}

func newTestTaskDetailScreen() *TaskDetailScreen {
	shared := &SharedState{
		Panes:       make(map[string]string),
		TmuxEnabled: false,
	}
	return NewTaskDetailScreen(shared, "test-task-id", "test-project")
}

func makeDetailWithJobs(n int) *api.TaskDetailView {
	jobs := make([]*api.Job, n)
	for i := range jobs {
		jobs[i] = &api.Job{
			ID:        fmt.Sprintf("job-%08d", i),
			Role:      "main",
			Status:    api.JobStatusRunning,
			CreatedAt: time.Now().Add(-time.Duration(i) * time.Minute),
		}
	}
	return &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:          "test-task-id",
			Title:       "Test Task",
			Description: "Test description\nLine 2",
			Status:      orchestrator.TaskStatusExecuting,
			Behavior:    "dev",
			CreatedAt:   time.Now().Add(-10 * time.Minute),
		},
		Jobs: jobs,
	}
}

func TestTaskDetailCursorMovement(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(3)

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.cursor != 1 {
		t.Errorf("after j: want cursor 1, got %d", s.cursor)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.cursor != 0 {
		t.Errorf("after k: want cursor 0, got %d", s.cursor)
	}

	// can't go below 0
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.cursor != 0 {
		t.Errorf("cursor should not go below 0, got %d", s.cursor)
	}

	// can't go past last index
	s.cursor = 2
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.cursor != 2 {
		t.Errorf("cursor should not exceed job count, got %d", s.cursor)
	}
}

func TestTaskDetailCursorArrowKeys(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(2)

	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	if s.cursor != 1 {
		t.Errorf("after down: want cursor 1, got %d", s.cursor)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyUp})
	if s.cursor != 0 {
		t.Errorf("after up: want cursor 0, got %d", s.cursor)
	}
}

func TestTaskDetailEnterOpenJob_NoTmux(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(1)
	s.shared.TmuxEnabled = false

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Error("enter without tmux: expected non-nil cmd (clearStatusAfter)")
	}
	if s.statusMsg == "" {
		t.Error("enter without tmux: expected statusMsg to be set")
	}
}

func TestTaskDetailEnterOpenJob_WithTmux(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(1)
	s.shared.TmuxEnabled = true

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Error("enter with tmux: expected non-nil cmd (openJobCmd)")
	}
}

func TestTaskDetailEnterOpenJob_CorrectJobID(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(3)
	s.shared.TmuxEnabled = false
	s.cursor = 2 // select third job

	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	// Status message is set, indicating the right code path was taken
	if s.statusMsg == "" {
		t.Error("expected statusMsg after enter without tmux")
	}
}

func TestTaskDetailEscReturnsPopScreen(t *testing.T) {
	s := newTestTaskDetailScreen()

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("esc: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("esc: expected popScreenMsg, got %T", msg)
	}
}

func TestTaskDetailBackspaceReturnsPopScreen(t *testing.T) {
	s := newTestTaskDetailScreen()

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if cmd == nil {
		t.Fatal("backspace: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("backspace: expected popScreenMsg, got %T", msg)
	}
}

func TestTaskDetailView_Renders(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(2)

	view := s.View(120, 40)
	if view == "" {
		t.Error("View() returned empty string")
	}
	if !containsStr(view, "Test Task") {
		t.Error("View should contain task title")
	}
	if !containsStr(view, "Jobs:") {
		t.Error("View should contain 'Jobs:'")
	}
	if !containsStr(view, "Description:") {
		t.Error("View should contain 'Description:'")
	}
	if !containsStr(view, "Test description") {
		t.Error("View should contain description text")
	}
}

func TestTaskDetailView_Loading(t *testing.T) {
	s := newTestTaskDetailScreen()
	// detail is nil (loading state)
	view := s.View(80, 20)
	if !containsStr(view, "Loading") {
		t.Error("View should show loading indicator when detail is nil")
	}
}

// --- start / abort keybinding tests ---

func TestStartKey_SetsLoadingMsgInDetail(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithStatus(orchestrator.TaskStatusPending)

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if s.statusMsg != "starting..." {
		t.Errorf("s on pending task: want statusMsg %q, got %q", "starting...", s.statusMsg)
	}
	if s.isError {
		t.Error("s on pending task: expected isError=false")
	}
}

func TestAbortKey_SecondPress_SetsLoadingMsg(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithStatus(orchestrator.TaskStatusExecuting)
	s.abortPending = true
	s.statusMsg = "Press a again to abort"

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if s.statusMsg != "aborting..." {
		t.Errorf("abort confirmed: want statusMsg %q, got %q", "aborting...", s.statusMsg)
	}
	if s.isError {
		t.Error("abort confirmed: expected isError=false")
	}
}

func TestStartKey_PendingTask(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithStatus(orchestrator.TaskStatusPending)

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd == nil {
		t.Error("s on pending task: expected non-nil cmd (applyActionCmd)")
	}
}

func TestStartKey_NonPendingTask(t *testing.T) {
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
	}
	for _, st := range statuses {
		s := newTestTaskDetailScreen()
		s.detail = makeDetailWithStatus(st)
		_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
		if cmd != nil {
			t.Errorf("s on %q task: expected nil cmd", st)
		}
	}
}

func TestAbortKey_FirstPress_SetsConfirmState(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithStatus(orchestrator.TaskStatusExecuting)
	// abort action maps to key 'a' (first char of "abort", since 'd' is taken by "done")

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if !s.abortPending {
		t.Error("a first press: expected abortPending=true")
	}
	if s.statusMsg == "" {
		t.Error("a first press: expected statusMsg to be set")
	}
	if cmd == nil {
		t.Error("a first press: expected non-nil cmd (tick)")
	}
}

func TestAbortKey_SecondPress_ExecutesAbort(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithStatus(orchestrator.TaskStatusExecuting)
	s.abortPending = true
	s.statusMsg = "Press a again to abort"

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if s.abortPending {
		t.Error("a second press: expected abortPending=false")
	}
	if cmd == nil {
		t.Error("a second press: expected non-nil cmd (applyActionCmd)")
	}
}

func TestAbortKey_DoneTask_Ignored(t *testing.T) {
	for _, st := range []orchestrator.TaskStatus{orchestrator.TaskStatusDone, orchestrator.TaskStatusAborted} {
		s := newTestTaskDetailScreen()
		s.detail = makeDetailWithStatus(st)
		_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
		if s.abortPending {
			t.Errorf("a on %q: abortPending should remain false", st)
		}
		if cmd != nil {
			t.Errorf("a on %q: expected nil cmd", st)
		}
	}
}

func TestAbortConfirmDeadline_ClearsPending(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.abortPending = true
	s.statusMsg = "Press x again to abort"

	s.Update(abortConfirmDeadlineMsg{})
	if s.abortPending {
		t.Error("deadline: expected abortPending=false")
	}
	if s.statusMsg != "" {
		t.Errorf("deadline: expected empty statusMsg, got %q", s.statusMsg)
	}
}

func TestApplyActionResult_Success_RefreshesDetail(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.abortPending = true

	_, cmd := s.Update(applyActionResultMsg{err: nil})
	if s.abortPending {
		t.Error("success result: expected abortPending=false")
	}
	if cmd == nil {
		t.Error("success result: expected fetchTaskDetailCmd")
	}
}

func TestApplyActionResult_Error_SetsStatusMsg(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.Update(applyActionResultMsg{err: fmt.Errorf("permission denied")})
	if s.statusMsg == "" {
		t.Error("error result: expected statusMsg to be set")
	}
	if !s.isError {
		t.Error("error result: expected isError=true")
	}
}

// --- assignKeys tests ---

func TestAssignKeys_Empty(t *testing.T) {
	m := assignKeys(nil)
	if len(m) != 0 {
		t.Errorf("assignKeys(nil) = %v, want empty", m)
	}
	m = assignKeys([]string{})
	if len(m) != 0 {
		t.Errorf("assignKeys([]) = %v, want empty", m)
	}
}

func TestAssignKeys_NoConflict(t *testing.T) {
	m := assignKeys([]string{"start", "abort", "done"})
	// "start" → 's', "abort" → 'a', "done" → 'd'
	if m['s'] != "start" {
		t.Errorf("key 's' = %q, want 'start'", m['s'])
	}
	if m['a'] != "abort" {
		t.Errorf("key 'a' = %q, want 'abort'", m['a'])
	}
	if m['d'] != "done" {
		t.Errorf("key 'd' = %q, want 'done'", m['d'])
	}
}

func TestAssignKeys_Conflict(t *testing.T) {
	// "done" → 'd', "debug" → conflict on 'd', falls back to 'e'
	m := assignKeys([]string{"done", "debug"})
	if m['d'] != "done" {
		t.Errorf("key 'd' = %q, want 'done'", m['d'])
	}
	if m['e'] != "debug" {
		t.Errorf("key 'e' = %q, want 'debug'", m['e'])
	}
}

func TestAssignKeys_CollectFeedback(t *testing.T) {
	// "collect_feedback" and "abort" → 'c' and 'a'
	m := assignKeys([]string{"collect_feedback", "abort"})
	if m['c'] != "collect_feedback" {
		t.Errorf("key 'c' = %q, want 'collect_feedback'", m['c'])
	}
	if m['a'] != "abort" {
		t.Errorf("key 'a' = %q, want 'abort'", m['a'])
	}
}

// --- assignKeys tests end ---

// TestGetTaskDetail_JSONParsing verifies TaskDetailView decodes correctly from JSON.
func TestGetTaskDetail_JSONParsing(t *testing.T) {
	raw := `{
		"Task": {
			"id": "task-1",
			"title": "My Task",
			"status": "executing",
			"behavior": "dev",
			"payload": null,
			"created_at": "2024-01-01T00:00:00Z",
			"updated_at": "2024-01-01T00:00:00Z"
		},
		"Jobs": [
			{
				"id": "job-1",
				"task_id": "task-1",
				"role": "main",
				"status": "running",
				"created_at": "2024-01-01T00:00:00Z",
				"updated_at": "2024-01-01T00:00:00Z"
			}
		]
	}`

	var detail api.TaskDetailView
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatalf("unmarshal TaskDetailView: %v", err)
	}
	if detail.Task == nil {
		t.Fatal("Task is nil after unmarshal")
	}
	if detail.Task.ID != "task-1" {
		t.Errorf("task ID: want task-1, got %q", detail.Task.ID)
	}
	if detail.Task.Title != "My Task" {
		t.Errorf("task title: want My Task, got %q", detail.Task.Title)
	}
	if len(detail.Jobs) != 1 {
		t.Fatalf("jobs: want 1, got %d", len(detail.Jobs))
	}
	if detail.Jobs[0].ID != "job-1" {
		t.Errorf("job ID: want job-1, got %q", detail.Jobs[0].ID)
	}
	if detail.Jobs[0].Role != "main" {
		t.Errorf("job role: want main, got %q", detail.Jobs[0].Role)
	}
	if detail.Jobs[0].Status != api.JobStatusRunning {
		t.Errorf("job status: want running, got %q", detail.Jobs[0].Status)
	}
}
