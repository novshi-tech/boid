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

// --- buildTreeTimeline filtering tests ---

// TestBuildTreeTimeline_IncludesRunningJobs verifies that running jobs are now
// included in the timeline (Active section has been removed).
func TestBuildTreeTimeline_IncludesRunningJobs(t *testing.T) {
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{ID: "t", Status: orchestrator.TaskStatusExecuting},
		Jobs: []*api.Job{
			{
				ID:        "j1",
				Role:      "main",
				Status:    api.JobStatusRunning,
				CreatedAt: time.Now(),
			},
		},
	}
	groups := buildTreeTimeline(detail)
	found := false
	for _, g := range groups {
		for _, ev := range g.Events {
			if ev.Kind == timelineKindJob {
				found = true
			}
		}
	}
	if !found {
		t.Error("buildTreeTimeline: running job should now be included in the timeline")
	}
}

func TestBuildTreeTimeline_ExcludesNonTransitionNonHookActions(t *testing.T) {
	now := time.Now()
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{ID: "t", Status: orchestrator.TaskStatusExecuting},
		Actions: []*orchestrator.Action{
			{
				// Non-transition, non-hook action → must be excluded.
				ID: "a1", Type: "rerun",
				FromStatus: orchestrator.TaskStatusExecuting,
				ToStatus:   orchestrator.TaskStatusExecuting,
				CreatedAt:  now.Add(-3 * time.Minute),
			},
			{
				// State transition → must be included.
				ID: "a2", Type: "start",
				FromStatus: orchestrator.TaskStatusPending,
				ToStatus:   orchestrator.TaskStatusExecuting,
				CreatedAt:  now.Add(-1 * time.Minute),
			},
		},
	}
	groups := buildTreeTimeline(detail)
	for _, g := range groups {
		for _, ev := range g.Events {
			if ev.Kind == timelineKindAction && containsStr(ev.Label, "rerun") {
				t.Errorf("buildTreeTimeline: non-transition rerun should be excluded, got %q", ev.Label)
			}
		}
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
