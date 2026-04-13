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

func TestTaskDetailDescriptionScroll_Overview(t *testing.T) {
	s := newTestTaskDetailScreen()
	// makeDetailWithJobs has description "Test description\nLine 2" (2 lines)
	s.detail = makeDetailWithJobs(3)

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.descScroll != 1 {
		t.Errorf("after j in overview: want descScroll 1, got %d", s.descScroll)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.descScroll != 0 {
		t.Errorf("after k in overview: want descScroll 0, got %d", s.descScroll)
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

func TestTaskDetailDescriptionScrollArrowKeys_Overview(t *testing.T) {
	s := newTestTaskDetailScreen()
	// description "Test description\nLine 2" (2 lines)
	s.detail = makeDetailWithJobs(2)

	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	if s.descScroll != 1 {
		t.Errorf("after down in overview: want descScroll 1, got %d", s.descScroll)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyUp})
	if s.descScroll != 0 {
		t.Errorf("after up in overview: want descScroll 0, got %d", s.descScroll)
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
	if !containsStr(view, "[O]verview") {
		t.Error("View should contain tab bar with '[O]verview'")
	}
	if !containsStr(view, "Active") {
		t.Error("View should contain 'Active' section header")
	}
	if !containsStr(view, "Description") {
		t.Error("View should contain 'Description' section header")
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
	// "start" → 's', "abort" → 'a', "done" → 'o' ('d' is reserved for delete)
	if m['s'] != "start" {
		t.Errorf("key 's' = %q, want 'start'", m['s'])
	}
	if m['a'] != "abort" {
		t.Errorf("key 'a' = %q, want 'abort'", m['a'])
	}
	if _, exists := m['d']; exists {
		t.Errorf("key 'd' should not be assigned (reserved for delete), got %q", m['d'])
	}
	if m['o'] != "done" {
		t.Errorf("key 'o' = %q, want 'done'", m['o'])
	}
}

func TestAssignKeys_Conflict(t *testing.T) {
	// 'd' is reserved; "done" → 'o', "debug" → 'e'
	m := assignKeys([]string{"done", "debug"})
	if _, exists := m['d']; exists {
		t.Errorf("key 'd' should not be assigned (reserved for delete), got %q", m['d'])
	}
	if m['o'] != "done" {
		t.Errorf("key 'o' = %q, want 'done'", m['o'])
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

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if s.activeTab != tabTimeline {
		t.Errorf("after t: want %q, got %q", tabTimeline, s.activeTab)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if s.activeTab != tabPayload {
		t.Errorf("after p: want %q, got %q", tabPayload, s.activeTab)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if s.activeTab != tabOverview {
		t.Errorf("after o: want %q, got %q", tabOverview, s.activeTab)
	}
}

func TestTabSwitch_ViewShowsPlaceholder(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithJobs(1)

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	view := s.View(80, 20)
	if !containsStr(view, "coming soon") {
		t.Error("timeline tab: expected 'coming soon' placeholder")
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

func TestRenderOverview_WithDeps(t *testing.T) {
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
	if !containsStr(view, "Deps summary") {
		t.Error("renderOverview: expected 'Deps summary' section")
	}
	if !containsStr(view, "task-a") {
		t.Error("renderOverview: expected dep task title")
	}
	if !containsStr(view, "done") {
		t.Error("renderOverview: expected dep task status")
	}
}

func TestRenderOverview_DescriptionScroll(t *testing.T) {
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

	// With scroll = 0, line1 should appear
	view := s.renderOverview(80, 20)
	if !containsStr(view, "line1") {
		t.Error("scroll=0: expected 'line1'")
	}

	// With scroll = 2, line3 should appear and line1 should not
	s.descScroll = 2
	view = s.renderOverview(80, 20)
	if !containsStr(view, "line3") {
		t.Error("scroll=2: expected 'line3'")
	}
	if containsStr(view, "line1") {
		t.Error("scroll=2: 'line1' should not be visible")
	}
}
