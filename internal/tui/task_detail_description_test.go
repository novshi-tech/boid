package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- renderDescription tests ---

func makeDetailWithDescription(desc string) *api.TaskDetailView {
	return &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:          "test-task-id",
			Title:       "Test Task",
			Status:      orchestrator.TaskStatusExecuting,
			Behavior:    "dev",
			Description: desc,
			CreatedAt:   time.Now(),
		},
	}
}

func TestRenderDescription_Empty(t *testing.T) {
	detail := makeDetailWithDescription("")
	out := renderDescription(detail, 0, 80, 20)
	if !containsStr(out, "no description") {
		t.Errorf("expected '(no description)', got %q", out)
	}
}

func TestRenderDescription_NilDetail(t *testing.T) {
	out := renderDescription(nil, 0, 80, 20)
	if !containsStr(out, "loading") {
		t.Errorf("nil detail: expected 'loading', got %q", out)
	}
}

func TestRenderDescription_ShowsText(t *testing.T) {
	detail := makeDetailWithDescription("hello\nworld\nfoo")
	out := renderDescription(detail, 0, 80, 20)
	if !containsStr(out, "hello") {
		t.Error("expected 'hello' in output")
	}
	if !containsStr(out, "world") {
		t.Error("expected 'world' in output")
	}
}

func TestRenderDescription_Scroll(t *testing.T) {
	desc := "line1\nline2\nline3\nline4\nline5"
	detail := makeDetailWithDescription(desc)

	// scroll=0: line1 visible
	out := renderDescription(detail, 0, 80, 3)
	if !containsStr(out, "line1") {
		t.Error("scroll=0: expected 'line1'")
	}

	// scroll=2: line3 visible, line1 not visible
	out = renderDescription(detail, 2, 80, 3)
	if !containsStr(out, "line3") {
		t.Error("scroll=2: expected 'line3'")
	}
	if containsStr(out, "line1") {
		t.Error("scroll=2: 'line1' should not be visible")
	}
}

func TestRenderDescription_MoreHint(t *testing.T) {
	desc := strings.Repeat("line\n", 20) // 20 lines
	detail := makeDetailWithDescription(desc)

	// height=5 → only 5 lines visible → should show "... N more lines"
	out := renderDescription(detail, 0, 80, 5)
	if !containsStr(out, "more lines") {
		t.Errorf("expected '... N more lines' hint when content exceeds height, got %q", out)
	}
}

// --- Description tab keyboard tests ---

func TestDescriptionTab_JK_ScrollsDescScroll(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithDescription("line1\nline2\nline3")
	s.activeTab = tabDescription

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.descScroll != 1 {
		t.Errorf("j in description tab: want descScroll 1, got %d", s.descScroll)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.descScroll != 0 {
		t.Errorf("k in description tab: want descScroll 0, got %d", s.descScroll)
	}

	// can't go below 0
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.descScroll != 0 {
		t.Errorf("k at min: descScroll should stay 0, got %d", s.descScroll)
	}
}

func TestDescriptionTab_PgDown_ScrollsByPage(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithDescription("l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10")
	s.activeTab = tabDescription
	s.descPageHeight = 3 // simulate 3 lines per page
	s.descScroll = 0

	s.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if s.descScroll != 3 {
		t.Errorf("pgdown: want descScroll 3, got %d", s.descScroll)
	}
}

func TestDescriptionTab_PgUp_ScrollsByPage(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithDescription("l1\nl2\nl3\nl4\nl5\nl6")
	s.activeTab = tabDescription
	s.descPageHeight = 3
	s.descScroll = 4

	s.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if s.descScroll != 1 {
		t.Errorf("pgup: want descScroll 1, got %d", s.descScroll)
	}

	// pgup at 0 stays at 0
	s.descScroll = 0
	s.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if s.descScroll != 0 {
		t.Errorf("pgup at 0: want descScroll 0, got %d", s.descScroll)
	}
}

func TestDescriptionTab_E_PushesDescriptionScreen(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithDescription("some description")
	s.activeTab = tabDescription

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if cmd == nil {
		t.Fatal("e in description tab: expected non-nil cmd")
	}
	msg := cmd()
	push, ok := msg.(pushScreenMsg)
	if !ok {
		t.Fatalf("e in description tab: expected pushScreenMsg, got %T", msg)
	}
	if _, ok := push.screen.(*DescriptionScreen); !ok {
		t.Errorf("e in description tab: expected *DescriptionScreen, got %T", push.screen)
	}
}

func TestDescriptionTab_ViewRenders(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithDescription("my description text")
	s.activeTab = tabDescription

	view := s.View(80, 20)
	if !containsStr(view, "my description text") {
		t.Error("Description tab view: expected description text")
	}
	// Tab bar should include "Description" as active
	if !containsStr(view, "Description") {
		t.Error("Description tab view: expected 'Description' in tab bar")
	}
}

// --- buildOverviewTimeline filtering tests ---

func TestBuildOverviewTimeline_ExcludesRunningJobs(t *testing.T) {
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{ID: "t"},
		Jobs: []*api.Job{
			{
				ID:        "j1",
				Role:      "main",
				Status:    api.JobStatusRunning,
				CreatedAt: time.Now(),
			},
		},
	}
	events := buildOverviewTimeline(detail)
	for _, ev := range events {
		if ev.Kind == timelineKindJob {
			t.Error("buildOverviewTimeline: running job should be excluded")
		}
	}
}

func TestBuildOverviewTimeline_IncludesCompletedJobs(t *testing.T) {
	now := time.Now()
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{ID: "t"},
		Jobs: []*api.Job{
			{
				ID:        "j1",
				Role:      "verifier",
				Status:    api.JobStatusCompleted,
				ExitCode:  0,
				CreatedAt: now.Add(-5 * time.Minute),
				UpdatedAt: now,
			},
			{
				ID:        "j2",
				Role:      "main",
				Status:    api.JobStatusFailed,
				ExitCode:  1,
				CreatedAt: now.Add(-3 * time.Minute),
				UpdatedAt: now.Add(-1 * time.Minute),
			},
		},
	}
	events := buildOverviewTimeline(detail)
	jobEvents := 0
	for _, ev := range events {
		if ev.Kind == timelineKindJob {
			jobEvents++
		}
	}
	if jobEvents != 2 {
		t.Errorf("buildOverviewTimeline: want 2 job events, got %d", jobEvents)
	}
}

func TestBuildOverviewTimeline_ExcludesInternalActions(t *testing.T) {
	now := time.Now()
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{ID: "t"},
		Actions: []*orchestrator.Action{
			{ID: "a1", TaskID: "t", Type: "hook_fired", CreatedAt: now.Add(-3 * time.Minute)},
			{ID: "a2", TaskID: "t", Type: "auto_advance", CreatedAt: now.Add(-2 * time.Minute)},
			{ID: "a3", TaskID: "t", Type: "start", CreatedAt: now.Add(-1 * time.Minute)},
		},
	}
	events := buildOverviewTimeline(detail)
	for _, ev := range events {
		if ev.Kind == timelineKindAction {
			if containsStr(ev.Label, "hook_fired") || containsStr(ev.Label, "auto_advance") {
				t.Errorf("buildOverviewTimeline: internal action %q should be excluded", ev.Label)
			}
		}
	}
	// Only "start" should be included
	actionCount := 0
	for _, ev := range events {
		if ev.Kind == timelineKindAction {
			actionCount++
		}
	}
	if actionCount != 1 {
		t.Errorf("buildOverviewTimeline: want 1 user-driven action event, got %d", actionCount)
	}
}

func TestBuildOverviewTimeline_IncludesFindings(t *testing.T) {
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID: "t",
			Payload: []byte(`{
				"verification": {
					"gate-a": {
						"findings": [
							{"message": "err1", "status": "open"},
							{"message": "ok1", "status": "resolved"}
						]
					}
				}
			}`),
		},
	}
	events := buildOverviewTimeline(detail)
	findingCount := 0
	for _, ev := range events {
		if ev.Kind == timelineKindFinding {
			findingCount++
		}
	}
	if findingCount != 2 {
		t.Errorf("buildOverviewTimeline: want 2 finding events, got %d", findingCount)
	}
}

func TestBuildOverviewTimeline_JobLabelFormat(t *testing.T) {
	now := time.Now()
	job := &api.Job{
		ID:        "j1",
		Role:      "verifier",
		Status:    api.JobStatusCompleted,
		ExitCode:  0,
		CreatedAt: now.Add(-2 * time.Minute),
		UpdatedAt: now,
	}
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{ID: "t"},
		Jobs: []*api.Job{job},
	}
	events := buildOverviewTimeline(detail)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	label := events[0].Label
	if !containsStr(label, "[verifier]") {
		t.Errorf("job label: expected '[verifier]', got %q", label)
	}
	if !containsStr(label, "✓") {
		t.Errorf("completed job label: expected '✓', got %q", label)
	}
}

// TestPayloadTab_EnterIsNoOp verifies that pressing Enter in Payload tab does not open jobs.
func TestPayloadTab_EnterIsNoOp(t *testing.T) {
	s := newTestTaskDetailScreen()
	s.detail = makeDetailWithPayloadForNav(`{"instructions": {"main": {}}}`)
	s.activeTab = tabPayload
	s.shared.TmuxEnabled = true

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		msg := cmd()
		if _, ok := msg.(openResultMsg); ok {
			t.Error("enter in payload tab: should not trigger openJobCmd")
		}
		if push, ok := msg.(pushScreenMsg); ok {
			if _, isJob := push.screen.(*JobDetailScreen); isJob {
				t.Error("enter in payload tab: should not push JobDetailScreen")
			}
		}
	}
	if s.statusMsg != "" {
		t.Errorf("enter in payload tab: expected empty statusMsg, got %q", s.statusMsg)
	}
}
