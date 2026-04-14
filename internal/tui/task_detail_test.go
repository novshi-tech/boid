package tui

import (
	"encoding/json"
	"fmt"
	"strings"
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
	case orchestrator.TaskStatusVerifying:
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

// makeDetailWithCompletedJob returns a detail with one completed job,
// which appears in the Overview timeline (running jobs are excluded).
func makeDetailWithCompletedJob() *api.TaskDetailView {
	now := time.Now()
	return &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:        "test-task-id",
			Title:     "Test Task",
			Status:    orchestrator.TaskStatusDone,
			Behavior:  "dev",
			CreatedAt: now.Add(-10 * time.Minute),
		},
		Jobs: []*api.Job{
			{
				ID:        "job-00000001",
				Role:      "main",
				Status:    api.JobStatusCompleted,
				ExitCode:  0,
				CreatedAt: now.Add(-5 * time.Minute),
				UpdatedAt: now.Add(-1 * time.Minute),
			},
		},
	}
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

// TestTaskDetailDescriptionScroll_DescriptionTab verifies j/k moves descScroll in Description tab.
func TestTaskDetailDescriptionScroll_DescriptionTab(t *testing.T) {
	s := newTestTaskDetailScreen()
	// makeDetailWithJobs has description "Test description\nLine 2" (2 lines)
	s.detail = makeDetailWithJobs(3)
	s.activeTab = tabDescription

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.descScroll != 1 {
		t.Errorf("after j in description tab: want descScroll 1, got %d", s.descScroll)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.descScroll != 0 {
		t.Errorf("after k in description tab: want descScroll 0, got %d", s.descScroll)
	}

	// can't go below 0
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.descScroll != 0 {
		t.Errorf("descScroll should not go below 0, got %d", s.descScroll)
	}

	// can't go past last line (description has 2 lines, max scroll = 1)
	s.descScroll = 1
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.descScroll != 1 {
		t.Errorf("descScroll should not exceed line count, got %d", s.descScroll)
	}
}

// TestTaskDetailOverviewJK_MovesTimelineCursor verifies j/k moves timelineCursor in Overview.
func TestTaskDetailOverviewJK_MovesTimelineCursor(t *testing.T) {
	s := newTestTaskDetailScreen()
	// makeDetailWithCompletedJob has 1 completed job → 1 overview timeline event
	s.detail = makeDetailWithCompletedJob()
	s.activeTab = tabOverview
	s.timelineCursor = 0

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	// cursor is already at max (1 event → max index 0), should stay at 0
	if s.timelineCursor != 0 {
		t.Errorf("after j at max in overview: want timelineCursor 0, got %d", s.timelineCursor)
	}

	// k should not go below 0
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.timelineCursor != 0 {
		t.Errorf("after k at min in overview: want timelineCursor 0, got %d", s.timelineCursor)
	}
}

// TestTaskDetailDescriptionScrollArrowKeys_DescriptionTab verifies down/up arrow moves descScroll.
func TestTaskDetailDescriptionScrollArrowKeys_DescriptionTab(t *testing.T) {
	s := newTestTaskDetailScreen()
	// description "Test description\nLine 2" (2 lines)
	s.detail = makeDetailWithJobs(2)
	s.activeTab = tabDescription

	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	if s.descScroll != 1 {
		t.Errorf("after down in description tab: want descScroll 1, got %d", s.descScroll)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyUp})
	if s.descScroll != 0 {
		t.Errorf("after up in description tab: want descScroll 0, got %d", s.descScroll)
	}
}

// TestTaskDetailEnterOpenJob_Overview_NoTimelineEvents verifies that Enter in Overview
// with no completed jobs (only running) is a no-op (no statusMsg).
func TestTaskDetailEnterOpenJob_Overview_NoTimelineEvents(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(1) // running job only → excluded from overview timeline
	s.shared.TmuxEnabled = false
	s.activeTab = tabOverview

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.statusMsg != "" {
		t.Errorf("enter in overview with only running jobs: expected empty statusMsg, got %q", s.statusMsg)
	}
	if cmd != nil {
		msg := cmd()
		if _, ok := msg.(pushScreenMsg); ok {
			t.Error("enter in overview with no timeline events: should not push any screen")
		}
	}
}

// TestTaskDetailEnterPayload_IsNoOp verifies that Enter in Payload tab does nothing.
func TestTaskDetailEnterPayload_IsNoOp(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(3)
	s.shared.TmuxEnabled = true
	s.activeTab = tabPayload

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	// Payload Enter should be no-op: no cmd, no status message.
	if s.statusMsg != "" {
		t.Errorf("enter in payload tab: expected empty statusMsg, got %q", s.statusMsg)
	}
	if cmd != nil {
		msg := cmd()
		if _, ok := msg.(pushScreenMsg); ok {
			t.Error("enter in payload tab: should not push any screen")
		}
		if _, ok := msg.(openResultMsg); ok {
			t.Error("enter in payload tab: should not open a job pane")
		}
	}
}

// TestTaskDetailQKey_ReturnsPopScreen verifies q returns popScreenMsg (same as esc).
func TestTaskDetailQKey_ReturnsPopScreen(t *testing.T) {
	s := newTestTaskDetailScreen()

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("q: expected popScreenMsg, got %T", msg)
	}
}

// TestTaskDetailQKey_PendingState_ReturnsPopScreen verifies q returns popScreen
// even when a confirmation prompt is active (same as esc behavior).
func TestTaskDetailQKey_PendingState_ReturnsPopScreen(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.deletePending = true
	s.statusMsg = "Press d again to delete"

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q with deletePending: expected non-nil cmd")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("q with deletePending: expected popScreenMsg, got %T", msg)
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
	if !containsStr(view, "Overview") {
		t.Error("View should contain tab bar with 'Overview'")
	}
	if !containsStr(view, "Description") {
		t.Error("View should contain 'Description' tab in tab bar")
	}
	if !containsStr(view, "Active") {
		t.Error("View should contain 'Active' section header")
	}
}

func TestTaskDetailView_DescriptionTab_Renders(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(2)
	s.activeTab = tabDescription

	view := s.View(120, 40)
	if !containsStr(view, "Test description") {
		t.Error("Description tab: View should contain description text")
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
	// "start" → 's', "abort" → 'a'. "done" must not get any key:
	// 一文字ミスタイプで実行中 hook を停止させないよう、キーボードからの発火を禁じている。
	if m['s'] != "start" {
		t.Errorf("key 's' = %q, want 'start'", m['s'])
	}
	if m['a'] != "abort" {
		t.Errorf("key 'a' = %q, want 'abort'", m['a'])
	}
	if _, exists := m['d']; exists {
		t.Errorf("key 'd' should not be assigned (reserved for delete), got %q", m['d'])
	}
	for ch, action := range m {
		if action == "done" {
			t.Errorf("action %q must not be assigned to any key (got %q)", action, string(ch))
		}
	}
}

func TestAssignKeys_Conflict(t *testing.T) {
	// 'd' is reserved; "done" is excluded entirely; "debug" → 'e' ('d' reserved, 'e' free).
	m := assignKeys([]string{"done", "debug"})
	if _, exists := m['d']; exists {
		t.Errorf("key 'd' should not be assigned (reserved for delete), got %q", m['d'])
	}
	for ch, action := range m {
		if action == "done" {
			t.Errorf("action %q must not be assigned to any key (got %q)", action, string(ch))
		}
	}
	if m['e'] != "debug" {
		t.Errorf("key 'e' = %q, want 'debug'", m['e'])
	}
}

func TestAssignKeys_DoneExcluded(t *testing.T) {
	// "done" はキー割当から除外される。single key で state を done に飛ばすのは
	// 実行中の hook を誤って停止させるリスクが大きいため UI 経由ではなく
	// 自動遷移 (execution_complete 等) か専用モーダルで行う設計。
	m := assignKeys([]string{"done"})
	if len(m) != 0 {
		t.Errorf("assignKeys([\"done\"]) = %v, want empty (done must not be reachable via single keypress)", m)
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

// --- delete keybinding tests ---

func TestDeleteKey_FirstPress_SetsPending(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithStatus(orchestrator.TaskStatusDone)

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if !s.deletePending {
		t.Error("d first press: expected deletePending=true")
	}
	if s.statusMsg == "" {
		t.Error("d first press: expected statusMsg to be set")
	}
	if cmd == nil {
		t.Error("d first press: expected non-nil cmd (tick)")
	}
}

func TestDeleteKey_SecondPress_ReturnsDeleteResultMsg(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithStatus(orchestrator.TaskStatusDone)
	s.deletePending = true

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if cmd == nil {
		t.Error("d second press: expected non-nil cmd (deleteTaskCmd)")
	}
}

func TestDeleteConfirmDeadline_ResetsPending(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.deletePending = true
	s.statusMsg = "Press d again to delete"

	s.Update(deleteConfirmDeadlineMsg{})
	if s.deletePending {
		t.Error("deadline: expected deletePending=false")
	}
	if s.statusMsg != "" {
		t.Errorf("deadline: expected empty statusMsg, got %q", s.statusMsg)
	}
}

func TestDeleteResult_Success_PopScreen(t *testing.T) {
	s := newTestTaskDetailScreen()

	_, cmd := s.Update(deleteResultMsg{err: nil})
	if cmd == nil {
		t.Fatal("delete success: expected non-nil cmd (popScreenMsg)")
	}
	msg := cmd()
	if _, ok := msg.(popScreenMsg); !ok {
		t.Errorf("delete success: expected popScreenMsg, got %T", msg)
	}
}

func TestDeleteResult_Error_SetsStatusMsg(t *testing.T) {
	s := newTestTaskDetailScreen()

	s.Update(deleteResultMsg{err: fmt.Errorf("task is active")})
	if s.statusMsg == "" {
		t.Error("delete error: expected statusMsg to be set")
	}
	if !s.isError {
		t.Error("delete error: expected isError=true")
	}
}

func TestShortHelp_DeleteAlwaysShown(t *testing.T) {
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusVerifying,
		orchestrator.TaskStatusReworking,
	}
	for _, st := range statuses {
		s := newTestTaskDetailScreen()
		s.detail = makeDetailWithStatus(st)
		help := s.ShortHelp()
		if !containsStr(help, "d: delete") {
			t.Errorf("ShortHelp for %q: expected 'd: delete', got %q", st, help)
		}
	}
}

// --- delete keybinding tests end ---

// --- duplicate keybinding tests ---

func TestDuplicateKey_FirstPress_SetsPending(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithStatus(orchestrator.TaskStatusDone)

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	if !s.duplicatePending {
		t.Error("D first press: expected duplicatePending=true")
	}
	if s.statusMsg == "" {
		t.Error("D first press: expected statusMsg to be set")
	}
	if cmd == nil {
		t.Error("D first press: expected non-nil cmd (tick)")
	}
}

func TestDuplicateKey_SecondPress_ExecutesDuplicate(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithStatus(orchestrator.TaskStatusDone)
	s.duplicatePending = true

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	if cmd == nil {
		t.Error("D second press: expected non-nil cmd (duplicateTaskCmd)")
	}
}

func TestDuplicateConfirmDeadline_ResetsPending(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.duplicatePending = true
	s.statusMsg = "Press D again to duplicate"

	s.Update(duplicateConfirmDeadlineMsg{})
	if s.duplicatePending {
		t.Error("deadline: expected duplicatePending=false")
	}
	if s.statusMsg != "" {
		t.Errorf("deadline: expected empty statusMsg, got %q", s.statusMsg)
	}
}

func TestDuplicateResult_Success_NavigatesToNewTask(t *testing.T) {
	s := newTestTaskDetailScreen()

	_, cmd := s.Update(duplicateResultMsg{newTaskID: "new-task-id"})
	if cmd == nil {
		t.Fatal("duplicate success: expected non-nil cmd (pushScreenMsg)")
	}
	msg := cmd()
	if push, ok := msg.(pushScreenMsg); !ok {
		t.Errorf("duplicate success: expected pushScreenMsg, got %T", msg)
	} else {
		if detail, ok := push.screen.(*TaskDetailScreen); !ok {
			t.Errorf("duplicate success: expected *TaskDetailScreen, got %T", push.screen)
		} else if detail.taskID != "new-task-id" {
			t.Errorf("duplicate success: want taskID %q, got %q", "new-task-id", detail.taskID)
		}
	}
}

func TestDuplicateResult_Error_SetsStatusMsg(t *testing.T) {
	s := newTestTaskDetailScreen()

	s.Update(duplicateResultMsg{err: fmt.Errorf("duplicate failed")})
	if s.statusMsg == "" {
		t.Error("duplicate error: expected statusMsg to be set")
	}
	if !s.isError {
		t.Error("duplicate error: expected isError=true")
	}
}

func TestShortHelp_DuplicateAlwaysShown(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithStatus(orchestrator.TaskStatusDone)
	help := s.ShortHelp()
	if !containsStr(help, "D: duplicate") {
		t.Errorf("ShortHelp: expected 'D: duplicate', got %q", help)
	}
}

// --- duplicate keybinding tests end ---

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

// --- tab switching tests ---

func TestTabSwitch(t *testing.T) {
	s := newTestTaskDetailScreen()

	if s.activeTab != tabOverview {
		t.Errorf("initial tab: want %q, got %q", tabOverview, s.activeTab)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.activeTab != tabDescription {
		t.Errorf("after tab: want %q, got %q", tabDescription, s.activeTab)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.activeTab != tabDeps {
		t.Errorf("after tab: want %q, got %q", tabDeps, s.activeTab)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.activeTab != tabPayload {
		t.Errorf("after tab: want %q, got %q", tabPayload, s.activeTab)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if s.activeTab != tabDeps {
		t.Errorf("after shift+tab: want %q, got %q", tabDeps, s.activeTab)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if s.activeTab != tabDescription {
		t.Errorf("after shift+tab: want %q, got %q", tabDescription, s.activeTab)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if s.activeTab != tabOverview {
		t.Errorf("after shift+tab: want %q, got %q", tabOverview, s.activeTab)
	}
}

func TestTabSwitch_ViewShowsDescription(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(1)

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.activeTab != tabDescription {
		t.Errorf("after tab: want %q, got %q", tabDescription, s.activeTab)
	}
	view := s.View(80, 20)
	// Description tab renders task description; makeDetailWithJobs sets Description field.
	if !containsStr(view, "no description") && !containsStr(view, "Test description") {
		t.Error("description tab: expected description content or '(no description)'")
	}
}

// --- parseOpenFindings tests ---

func TestParseOpenFindings_Empty(t *testing.T) {
	findings := parseOpenFindings(nil)
	if len(findings) != 0 {
		t.Errorf("nil payload: want 0 findings, got %d", len(findings))
	}

	findings = parseOpenFindings([]byte("{}"))
	if len(findings) != 0 {
		t.Errorf("empty payload: want 0 findings, got %d", len(findings))
	}
}

func TestParseOpenFindings_OpenAndResolved(t *testing.T) {
	payload := []byte(`{
		"verification": {
			"mergeable-check": {
				"source_state": "verifying",
				"findings": [
					{"message": "2 conflicts", "status": "open"},
					{"message": "fixed", "status": "resolved"}
				]
			}
		}
	}`)

	findings := parseOpenFindings(payload)
	if len(findings) != 1 {
		t.Fatalf("want 1 open finding, got %d", len(findings))
	}
	if findings[0].gate != "mergeable-check" {
		t.Errorf("gate: want %q, got %q", "mergeable-check", findings[0].gate)
	}
	if findings[0].message != "2 conflicts" {
		t.Errorf("message: want %q, got %q", "2 conflicts", findings[0].message)
	}
}

func TestParseOpenFindings_MultipleGates(t *testing.T) {
	payload := []byte(`{
		"verification": {
			"gate-a": {
				"findings": [{"message": "err-a", "status": "open"}]
			},
			"gate-b": {
				"findings": [{"message": "err-b", "status": "open"}]
			}
		}
	}`)

	findings := parseOpenFindings(payload)
	if len(findings) != 2 {
		t.Errorf("want 2 open findings, got %d", len(findings))
	}
}

func TestParseOpenFindings_AllResolved(t *testing.T) {
	payload := []byte(`{
		"verification": {
			"gate-a": {
				"findings": [{"message": "ok", "status": "resolved"}]
			}
		}
	}`)

	findings := parseOpenFindings(payload)
	if len(findings) != 0 {
		t.Errorf("all resolved: want 0 open findings, got %d", len(findings))
	}
}

// --- renderOverview tests ---

func TestRenderOverview_WithRunningJob(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(2) // 2 running jobs

	view := s.renderOverview(80, 20)
	if !containsStr(view, "running job") {
		t.Error("renderOverview: expected 'running job' for active job")
	}
	if !containsStr(view, "[main]") {
		t.Error("renderOverview: expected '[main]' role label")
	}
}

func TestRenderOverview_NoJobs(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:        "test",
			Title:     "Test",
			Status:    orchestrator.TaskStatusPending,
			Behavior:  "dev",
			CreatedAt: time.Now(),
		},
	}

	view := s.renderOverview(80, 20)
	if !containsStr(view, "no active job") {
		t.Error("renderOverview: expected 'no active job' when no running jobs")
	}
}

func TestRenderOverview_WithOpenFindings(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:       "test",
			Title:    "Test",
			Status:   orchestrator.TaskStatusVerifying,
			Behavior: "dev",
			Payload: []byte(`{
				"verification": {
					"mergeable-check": {
						"findings": [{"message": "2 conflicts", "status": "open"}]
					}
				}
			}`),
			CreatedAt: time.Now(),
		},
	}

	view := s.renderOverview(80, 20)
	if !containsStr(view, "Findings") {
		t.Error("renderOverview: expected 'Findings' section when open findings exist")
	}
	if !containsStr(view, "mergeable-check") {
		t.Error("renderOverview: expected gate name in findings")
	}
	if !containsStr(view, "2 conflicts") {
		t.Error("renderOverview: expected finding message")
	}
}

// TestRenderOverview_NoDepsSection verifies the Deps summary section is removed from Overview.
func TestRenderOverview_NoDepsSection(t *testing.T) {
	s := newTestTaskDetailScreen()
	depTask := &orchestrator.Task{
		ID:     "dep-1",
		Title:  "task-a",
		Status: orchestrator.TaskStatusDone,
	}
	s.detail = &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:        "test",
			Title:     "Test",
			Status:    orchestrator.TaskStatusPending,
			Behavior:  "dev",
			CreatedAt: time.Now(),
		},
		DependsOnResolved: []*orchestrator.Task{depTask},
	}

	view := s.renderOverview(80, 20)
	if containsStr(view, "Deps summary") {
		t.Error("renderOverview: 'Deps summary' section should be removed from Overview")
	}
}

// TestRenderOverview_NoDescriptionSection verifies the Description section is removed from Overview.
func TestRenderOverview_NoDescriptionSection(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:          "test",
			Title:       "Test",
			Status:      orchestrator.TaskStatusExecuting,
			Behavior:    "dev",
			Description: "line1\nline2\nline3\nline4\nline5",
			CreatedAt:   time.Now(),
		},
	}

	view := s.renderOverview(80, 20)
	// Description section header should not appear (it's now its own tab).
	if containsStr(view, "─── Description") {
		t.Error("renderOverview: '─── Description' section header should not appear in Overview")
	}
}

// TestRenderOverview_HasTimelineSection verifies that the Timeline section is shown in Overview.
func TestRenderOverview_HasTimelineSection(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithCompletedJob()

	view := s.renderOverview(80, 20)
	if !containsStr(view, "Timeline") {
		t.Error("renderOverview: expected 'Timeline' section header")
	}
}

// --- DescriptionScreen transition tests ---

// TestTaskDetail_EKey_Description_PushesDescriptionScreen verifies that pressing e
// in the Description tab pushes DescriptionScreen (for editing).
func TestTaskDetail_EKey_Description_PushesDescriptionScreen(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(1)
	s.activeTab = tabDescription

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if cmd == nil {
		t.Fatal("e key in description tab: expected non-nil cmd (pushScreenMsg)")
	}
	msg := cmd()
	push, ok := msg.(pushScreenMsg)
	if !ok {
		t.Fatalf("e key in description tab: expected pushScreenMsg, got %T", msg)
	}
	if _, ok := push.screen.(*DescriptionScreen); !ok {
		t.Errorf("e key in description tab: expected *DescriptionScreen, got %T", push.screen)
	}
}

// TestTaskDetail_VKey_NoOp verifies that the v key is no longer handled (removed).
func TestTaskDetail_VKey_NoOp(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(0)
	s.activeTab = tabOverview

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	// v key is not bound to anything any more; no cmd expected.
	if cmd != nil {
		msg := cmd()
		if _, ok := msg.(pushScreenMsg); ok {
			t.Error("v key: should no longer push DescriptionScreen")
		}
	}
}

// TestTaskDetail_Enter_Overview_NoEvents_IsNoOp verifies that Enter in Overview
// when there are no timeline events is a no-op.
func TestTaskDetail_Enter_Overview_NoEvents_IsNoOp(t *testing.T) {
	s := newTestTaskDetailScreen()
	// makeDetailWithJobs(1) has 1 running job which is excluded from overview timeline.
	s.detail = makeDetailWithJobs(1)
	s.activeTab = tabOverview

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	// No completed jobs in timeline → no-op.
	if cmd != nil {
		msg := cmd()
		if _, ok := msg.(pushScreenMsg); ok {
			t.Error("enter in overview with only running jobs: should not push any screen")
		}
	}
	if s.statusMsg != "" {
		t.Errorf("enter in overview with no timeline events: expected empty statusMsg, got %q", s.statusMsg)
	}
}

// TestTaskDetail_Enter_Overview_CompletedJob_PushesJobDetail verifies that Enter
// in Overview when a completed job is at the cursor pushes JobDetailScreen.
func TestTaskDetail_Enter_Overview_CompletedJob_PushesJobDetail(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithCompletedJob()
	s.activeTab = tabOverview
	s.timelineCursor = 0

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter in overview with completed job: expected non-nil cmd")
	}
	msg := cmd()
	push, ok := msg.(pushScreenMsg)
	if !ok {
		t.Fatalf("enter in overview with completed job: expected pushScreenMsg, got %T", msg)
	}
	if _, ok := push.screen.(*JobDetailScreen); !ok {
		t.Errorf("enter in overview with completed job: expected *JobDetailScreen, got %T", push.screen)
	}
}

func TestShortHelp_NoViewDesc(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithStatus(orchestrator.TaskStatusExecuting)

	help := s.ShortHelp()
	if containsStr(help, "v: view desc") {
		t.Errorf("ShortHelp: 'v: view desc' should be removed, got %q", help)
	}
}

// --- title inline editing tests ---

func TestTitleEdit_EKey_StartsEditing(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(1)
	s.activeTab = tabOverview

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if !s.titleEditing {
		t.Error("e key in overview: expected titleEditing=true")
	}
	if s.titleInput.Value() != "Test Task" {
		t.Errorf("e key: expected titleInput value %q, got %q", "Test Task", s.titleInput.Value())
	}
}

func TestTitleEdit_EKey_NoDetail_DoesNothing(t *testing.T) {
	s := newTestTaskDetailScreen()
	// detail is nil

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if s.titleEditing {
		t.Error("e key with nil detail: expected titleEditing=false")
	}
}

func TestTitleEdit_EscCancels(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(1)
	s.titleEditing = true
	tf := NewTextField()
	tf.SetLabel("edit title")
	tf.SetValue("Modified Title")
	tf.Focus()
	s.titleInput = tf

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEscape})
	if s.titleEditing {
		t.Error("esc during title edit: expected titleEditing=false")
	}
	if cmd != nil {
		t.Errorf("esc during title edit: expected nil cmd, got non-nil")
	}
}

func TestTitleEdit_EscCancels_DoesNotPopScreen(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(1)
	s.titleEditing = true
	tf := NewTextField()
	tf.Focus()
	s.titleInput = tf

	// Esc while editing should cancel editing, NOT pop the screen
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEscape})
	if s.titleEditing {
		t.Error("esc during editing: expected titleEditing=false")
	}
	if cmd != nil {
		// cmd should be nil (not popScreenMsg)
		msg := cmd()
		if _, ok := msg.(popScreenMsg); ok {
			t.Error("esc during title edit: should not return popScreenMsg")
		}
	}
}

func TestTitleEdit_Enter_Saves(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(1)
	s.titleEditing = true
	tf := NewTextField()
	tf.SetLabel("edit title")
	tf.SetValue("New Title")
	tf.Focus()
	s.titleInput = tf

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.titleEditing {
		t.Error("enter: expected titleEditing=false after save")
	}
	if s.statusMsg != "saving..." {
		t.Errorf("enter: expected statusMsg %q, got %q", "saving...", s.statusMsg)
	}
	if s.isError {
		t.Error("enter: expected isError=false")
	}
	if cmd == nil {
		t.Error("enter: expected non-nil cmd (updateTitleCmd)")
	}
}

func TestTitleEdit_Enter_EmptyTitle_DoesNotSave(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(1)
	s.titleEditing = true
	tf := NewTextField()
	tf.SetValue("")
	tf.Focus()
	s.titleInput = tf

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.titleEditing {
		t.Error("enter with empty title: expected titleEditing=false")
	}
	if s.statusMsg == "saving..." {
		t.Error("enter with empty title: should not set saving... status")
	}
	if cmd != nil {
		t.Error("enter with empty title: expected nil cmd")
	}
}

func TestTitleUpdateResult_Success_ClearsStatusAndRefreshes(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.statusMsg = "saving..."

	_, cmd := s.Update(titleUpdateResultMsg{err: nil})
	if s.statusMsg != "" {
		t.Errorf("success: expected empty statusMsg, got %q", s.statusMsg)
	}
	if s.isError {
		t.Error("success: expected isError=false")
	}
	if cmd == nil {
		t.Error("success: expected non-nil cmd (fetchTaskDetailCmd)")
	}
}

func TestTitleUpdateResult_Error_SetsStatusMsg(t *testing.T) {
	s := newTestTaskDetailScreen()

	s.Update(titleUpdateResultMsg{err: fmt.Errorf("update failed")})
	if s.statusMsg == "" {
		t.Error("error: expected statusMsg to be set")
	}
	if !s.isError {
		t.Error("error: expected isError=true")
	}
}

func TestTitleEdit_View_ShowsTextField(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(1)
	s.titleEditing = true
	tf := NewTextField()
	tf.SetLabel("edit title")
	tf.SetValue("Test Task")
	tf.Focus()
	s.titleInput = tf

	view := s.View(80, 20)
	if !containsStr(view, "edit title") {
		t.Error("titleEditing view: expected 'edit title' in view")
	}
	if !containsStr(view, "Enter: save") {
		t.Error("titleEditing view: expected 'Enter: save' hint in view")
	}
}

func TestShortHelp_IncludesEditTitle(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithStatus(orchestrator.TaskStatusExecuting)
	s.activeTab = tabOverview // e: edit title is shown in Overview tab

	help := s.ShortHelp()
	if !containsStr(help, "e: edit title") {
		t.Errorf("ShortHelp (overview): expected 'e: edit title', got %q", help)
	}
}

func TestShortHelp_DescriptionTab_IncludesEditDescription(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithStatus(orchestrator.TaskStatusExecuting)
	s.activeTab = tabDescription

	help := s.ShortHelp()
	if !containsStr(help, "e: edit description") {
		t.Errorf("ShortHelp (description): expected 'e: edit description', got %q", help)
	}
}

// --- Tab/Shift+Tab cycling tests ---

func TestTabCycle_ForwardFromOverview(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.activeTab = tabOverview

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.activeTab != tabDescription {
		t.Errorf("tab from overview: want %q, got %q", tabDescription, s.activeTab)
	}
}

func TestTabCycle_ForwardFromDescription(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.activeTab = tabDescription

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.activeTab != tabDeps {
		t.Errorf("tab from description: want %q, got %q", tabDeps, s.activeTab)
	}
}

func TestTabCycle_ForwardFromDeps(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.activeTab = tabDeps

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.activeTab != tabPayload {
		t.Errorf("tab from deps: want %q, got %q", tabPayload, s.activeTab)
	}
}

func TestTabCycle_ForwardFromPayload_Wraps(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.activeTab = tabPayload

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.activeTab != tabOverview {
		t.Errorf("tab from payload (wrap): want %q, got %q", tabOverview, s.activeTab)
	}
}

func TestTabCycle_BackwardFromOverview_Wraps(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.activeTab = tabOverview

	s.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if s.activeTab != tabPayload {
		t.Errorf("shift+tab from overview (wrap): want %q, got %q", tabPayload, s.activeTab)
	}
}

func TestTabCycle_DepsTabReachable(t *testing.T) {
	// Verify Deps tab is reachable via Tab from Description.
	s := newTestTaskDetailScreen()
	s.activeTab = tabDescription

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.activeTab != tabDeps {
		t.Errorf("deps tab not reachable via tab from description: got %q", s.activeTab)
	}
}

func TestTabCycle_DepsTabReachable_Backward(t *testing.T) {
	// Verify Deps tab is reachable via Shift+Tab from Payload.
	s := newTestTaskDetailScreen()
	s.activeTab = tabPayload

	s.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if s.activeTab != tabDeps {
		t.Errorf("deps tab not reachable via shift+tab from payload: got %q", s.activeTab)
	}
}

func TestShortHelp_IncludesTabCycle(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithStatus(orchestrator.TaskStatusExecuting)

	help := s.ShortHelp()
	if !containsStr(help, "tab") {
		t.Errorf("ShortHelp: expected 'tab' in help, got %q", help)
	}
}

// TestTaskDetailView_TitleUsesFullScreenWidth verifies that the title in the
// sub-header is not capped at a fixed 50-char limit but instead expands to fill
// the available screen width.
func TestTaskDetailView_TitleUsesFullScreenWidth(t *testing.T) {
	s := newTestTaskDetailScreen()
	longTitle := strings.Repeat("X", 80)
	s.detail = &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:        "test-task-id",
			Title:     longTitle,
			Status:    orchestrator.TaskStatusExecuting,
			Behavior:  "dev",
			CreatedAt: time.Now().Add(-5 * time.Minute),
		},
		AvailableActions: []string{},
	}

	// Width 120: status "executing" is 9 visible chars.
	// maxTitleWidth = 120 - 9 - 1 = 110, so the 80-char title fits untruncated.
	view := s.View(120, 40)

	// With the old fixed-50 limit the output would cut at char 49 and append "…".
	// After the fix, all 80 X's must be present.
	if !containsStr(view, strings.Repeat("X", 60)) {
		t.Error("title of 80 chars should not be truncated to 50 when screen width is 120")
	}
}
