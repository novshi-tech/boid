package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// makeDetailWithDeps builds a TaskDetailView with the given depends_on / dependents.
func makeDetailWithDeps(dependsOn []*orchestrator.Task, dependents []*orchestrator.Task, unresolvedIDs []string) *api.TaskDetailView {
	dependsOnIDs := make([]string, 0, len(dependsOn)+len(unresolvedIDs))
	for _, t := range dependsOn {
		dependsOnIDs = append(dependsOnIDs, t.ID)
	}
	dependsOnIDs = append(dependsOnIDs, unresolvedIDs...)

	return &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:        "main-task-id",
			Title:     "Main Task",
			Status:    orchestrator.TaskStatusPending,
			Behavior:  "dev",
			DependsOn: dependsOnIDs,
			CreatedAt: time.Now(),
		},
		DependsOnResolved: dependsOn,
		Dependents:        dependents,
	}
}

func makeTask(id, title string, status orchestrator.TaskStatus) *orchestrator.Task {
	return &orchestrator.Task{
		ID:        id,
		Title:     title,
		Status:    status,
		Behavior:  "dev",
		CreatedAt: time.Now(),
	}
}

// --- renderDeps tests ---

func TestRenderDeps_NilDetail(t *testing.T) {
	out := renderDeps(nil, 80, 20, 0)
	if !containsStr(out, "loading") {
		t.Errorf("nil detail: expected 'loading', got %q", out)
	}
}

func TestRenderDeps_NoDependencies(t *testing.T) {
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:        "task-1",
			Title:     "Test",
			Status:    orchestrator.TaskStatusPending,
			Behavior:  "dev",
			CreatedAt: time.Now(),
		},
	}
	out := renderDeps(detail, 80, 20, 0)
	if !containsStr(out, "no dependencies") {
		t.Errorf("no deps: expected 'no dependencies', got %q", out)
	}
}

func TestRenderDeps_WithDependsOn(t *testing.T) {
	taskA := makeTask("aaaabbbb-0000-0000-0000-000000000001", "refactor auth module", orchestrator.TaskStatusDone)
	taskB := makeTask("ccccdddd-0000-0000-0000-000000000002", "add tests", orchestrator.TaskStatusPending)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA, taskB}, nil, nil)

	out := renderDeps(detail, 80, 20, 0)
	if !containsStr(out, "Depends on") {
		t.Error("expected 'Depends on' section header")
	}
	if !containsStr(out, "refactor auth module") {
		t.Error("expected taskA title in output")
	}
	if !containsStr(out, "add tests") {
		t.Error("expected taskB title in output")
	}
	if !containsStr(out, "done") {
		t.Error("expected 'done' status for taskA")
	}
	if !containsStr(out, "pending") {
		t.Error("expected 'pending' status for taskB")
	}
}

func TestRenderDeps_WithDependents(t *testing.T) {
	taskC := makeTask("eeeeffff-0000-0000-0000-000000000003", "run e2e", orchestrator.TaskStatusPending)
	detail := makeDetailWithDeps(nil, []*orchestrator.Task{taskC}, nil)

	out := renderDeps(detail, 80, 20, 0)
	if !containsStr(out, "Dependents") {
		t.Error("expected 'Dependents' section header")
	}
	if !containsStr(out, "run e2e") {
		t.Error("expected taskC title in output")
	}
}

func TestRenderDeps_Unresolved(t *testing.T) {
	unresolvedID := "99990000-0000-0000-0000-000000000099"
	detail := makeDetailWithDeps(nil, nil, []string{unresolvedID})

	out := renderDeps(detail, 80, 20, 0)
	if !containsStr(out, "unresolved") {
		t.Errorf("unresolved: expected '(unresolved: ...)' in output, got %q", out)
	}
	// shortID returns first 8 chars
	if !containsStr(out, "99990000") {
		t.Errorf("unresolved: expected shortID '99990000' in output, got %q", out)
	}
}

func TestRenderDeps_CursorOnFirstDependsOn(t *testing.T) {
	taskA := makeTask("aaaabbbb-cccc-0000-0000-000000000001", "task alpha", orchestrator.TaskStatusDone)
	taskB := makeTask("eeeeffff-cccc-0000-0000-000000000002", "task beta", orchestrator.TaskStatusPending)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA, taskB}, nil, nil)

	// cursor=0: first item selected
	out := renderDeps(detail, 80, 20, 0)
	// The ▸ cursor should appear before the first item's line
	if !containsStr(out, "▸") {
		t.Error("cursor=0: expected ▸ marker")
	}
}

func TestRenderDeps_CursorOnDependent(t *testing.T) {
	taskA := makeTask("aaaabbbb-0000-0000-0000-000000000001", "depends-on task", orchestrator.TaskStatusDone)
	taskC := makeTask("cccccccc-0000-0000-0000-000000000003", "dependent task", orchestrator.TaskStatusExecuting)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA}, []*orchestrator.Task{taskC}, nil)

	// selectable = [taskA, taskC]; cursor=1 selects taskC
	out := renderDeps(detail, 80, 20, 1)
	if !containsStr(out, "▸") {
		t.Error("cursor=1 on dependent: expected ▸ marker")
	}
	if !containsStr(out, "dependent task") {
		t.Error("expected dependent task title in output")
	}
}

func TestRenderDeps_EmptyDependsOnSection(t *testing.T) {
	taskC := makeTask("cccccccc-0000-0000-0000-000000000001", "dep task", orchestrator.TaskStatusPending)
	detail := makeDetailWithDeps(nil, []*orchestrator.Task{taskC}, nil)

	out := renderDeps(detail, 80, 20, 0)
	if !containsStr(out, "Depends on") {
		t.Error("expected 'Depends on' section even when empty")
	}
	if !containsStr(out, "(none)") {
		t.Error("expected '(none)' for empty depends_on")
	}
}

// --- depSelectableItems tests ---

func TestDepSelectableItems_Nil(t *testing.T) {
	items := depSelectableItems(nil)
	if len(items) != 0 {
		t.Errorf("nil detail: expected 0 items, got %d", len(items))
	}
}

func TestDepSelectableItems_Order(t *testing.T) {
	taskA := makeTask("aaaa", "A", orchestrator.TaskStatusDone)
	taskB := makeTask("bbbb", "B", orchestrator.TaskStatusPending)
	taskC := makeTask("cccc", "C", orchestrator.TaskStatusExecuting)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA, taskB}, []*orchestrator.Task{taskC}, nil)

	items := depSelectableItems(detail)
	if len(items) != 3 {
		t.Fatalf("expected 3 selectable items, got %d", len(items))
	}
	if items[0].ID != "aaaa" {
		t.Errorf("item[0]: want 'aaaa', got %q", items[0].ID)
	}
	if items[1].ID != "bbbb" {
		t.Errorf("item[1]: want 'bbbb', got %q", items[1].ID)
	}
	if items[2].ID != "cccc" {
		t.Errorf("item[2]: want 'cccc', got %q", items[2].ID)
	}
}

// --- cursor movement via keyboard ---

func newDepsScreen(detail *api.TaskDetailView) *TaskDetailScreen {
	s := newTestTaskDetailScreen()
	s.detail = detail
	s.activeTab = tabDeps
	return s
}

func TestDepsCursor_JMoveDown(t *testing.T) {
	taskA := makeTask("aaaa", "A", orchestrator.TaskStatusDone)
	taskB := makeTask("bbbb", "B", orchestrator.TaskStatusPending)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA, taskB}, nil, nil)

	s := newDepsScreen(detail)
	if s.depsCursor != 0 {
		t.Fatalf("initial depsCursor: want 0, got %d", s.depsCursor)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.depsCursor != 1 {
		t.Errorf("after j: want depsCursor 1, got %d", s.depsCursor)
	}
}

func TestDepsCursor_KMoveUp(t *testing.T) {
	taskA := makeTask("aaaa", "A", orchestrator.TaskStatusDone)
	taskB := makeTask("bbbb", "B", orchestrator.TaskStatusPending)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA, taskB}, nil, nil)

	s := newDepsScreen(detail)
	s.depsCursor = 1

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.depsCursor != 0 {
		t.Errorf("after k: want depsCursor 0, got %d", s.depsCursor)
	}
}

func TestDepsCursor_ClampAtZero(t *testing.T) {
	taskA := makeTask("aaaa", "A", orchestrator.TaskStatusDone)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA}, nil, nil)

	s := newDepsScreen(detail)
	s.depsCursor = 0

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if s.depsCursor != 0 {
		t.Errorf("k at 0: depsCursor should stay 0, got %d", s.depsCursor)
	}
}

func TestDepsCursor_ClampAtMax(t *testing.T) {
	taskA := makeTask("aaaa", "A", orchestrator.TaskStatusDone)
	taskB := makeTask("bbbb", "B", orchestrator.TaskStatusPending)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA, taskB}, nil, nil)

	s := newDepsScreen(detail)
	s.depsCursor = 1 // at last item

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if s.depsCursor != 1 {
		t.Errorf("j at max: depsCursor should stay 1, got %d", s.depsCursor)
	}
}

func TestDepsCursor_ArrowKeys(t *testing.T) {
	taskA := makeTask("aaaa", "A", orchestrator.TaskStatusDone)
	taskB := makeTask("bbbb", "B", orchestrator.TaskStatusPending)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA, taskB}, nil, nil)

	s := newDepsScreen(detail)
	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	if s.depsCursor != 1 {
		t.Errorf("down arrow: want depsCursor 1, got %d", s.depsCursor)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyUp})
	if s.depsCursor != 0 {
		t.Errorf("up arrow: want depsCursor 0, got %d", s.depsCursor)
	}
}

// --- Enter handler for deps tab ---

func TestDepsEnter_PushesTaskDetailScreen(t *testing.T) {
	taskA := makeTask("aaaa-1111-2222-3333-444455556666", "dep task A", orchestrator.TaskStatusDone)
	taskA.ProjectID = "proj-id-abc"
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA}, nil, nil)

	s := newDepsScreen(detail)
	s.depsCursor = 0

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on deps: expected non-nil cmd (pushScreenMsg)")
	}
	msg := cmd()
	push, ok := msg.(pushScreenMsg)
	if !ok {
		t.Fatalf("enter on deps: expected pushScreenMsg, got %T", msg)
	}
	detail2, ok := push.screen.(*TaskDetailScreen)
	if !ok {
		t.Fatalf("enter on deps: expected *TaskDetailScreen, got %T", push.screen)
	}
	if detail2.taskID != taskA.ID {
		t.Errorf("enter on deps: want taskID %q, got %q", taskA.ID, detail2.taskID)
	}
	if detail2.projectName != taskA.ProjectID {
		t.Errorf("enter on deps: want projectName %q, got %q", taskA.ProjectID, detail2.projectName)
	}
}

func TestDepsEnter_EmptyItems_NoOp(t *testing.T) {
	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:        "task-1",
			Title:     "Test",
			Status:    orchestrator.TaskStatusPending,
			Behavior:  "dev",
			CreatedAt: time.Now(),
		},
	}

	s := newDepsScreen(detail)
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("enter with no selectable items: expected nil cmd")
	}
}

func TestDepsEnter_SelectsDependent(t *testing.T) {
	taskA := makeTask("aaaa-1111-0000-0000-000000000001", "dep-on", orchestrator.TaskStatusDone)
	taskC := makeTask("cccc-3333-0000-0000-000000000003", "dependent", orchestrator.TaskStatusPending)
	taskC.ProjectID = "proj-other"
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA}, []*orchestrator.Task{taskC}, nil)

	s := newDepsScreen(detail)
	s.depsCursor = 1 // select taskC (the dependent)

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on dependent: expected non-nil cmd")
	}
	msg := cmd()
	push, ok := msg.(pushScreenMsg)
	if !ok {
		t.Fatalf("enter on dependent: expected pushScreenMsg, got %T", msg)
	}
	detail2 := push.screen.(*TaskDetailScreen)
	if detail2.taskID != taskC.ID {
		t.Errorf("enter on dependent: want taskID %q, got %q", taskC.ID, detail2.taskID)
	}
}

// --- View integration ---

func TestDepsTab_ViewRenders(t *testing.T) {
	taskA := makeTask("aaaa-1111-2222-3333-444455556666", "auth refactor", orchestrator.TaskStatusDone)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA}, nil, nil)

	s := newTestTaskDetailScreen()
	s.detail = detail
	s.activeTab = tabDeps

	view := s.View(80, 20)
	if !containsStr(view, "Depends on") {
		t.Error("deps View: expected 'Depends on' section")
	}
	if !containsStr(view, "auth refactor") {
		t.Error("deps View: expected task title")
	}
}
