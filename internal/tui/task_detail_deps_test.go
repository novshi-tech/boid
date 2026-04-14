package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// makeDetailWithDeps builds a TaskDetailView with the given depends_on / dependents.
// DependsOnTree and DependentsTree are populated from the provided slices (1-level deep).
func makeDetailWithDeps(dependsOn []*orchestrator.Task, dependents []*orchestrator.Task, unresolvedIDs []string) *api.TaskDetailView {
	dependsOnIDs := make([]string, 0, len(dependsOn)+len(unresolvedIDs))
	for _, t := range dependsOn {
		dependsOnIDs = append(dependsOnIDs, t.ID)
	}
	dependsOnIDs = append(dependsOnIDs, unresolvedIDs...)

	var dependsOnTree []*api.TaskNode
	for _, t := range dependsOn {
		dependsOnTree = append(dependsOnTree, &api.TaskNode{Task: t})
	}
	var dependentsTree []*api.TaskNode
	for _, t := range dependents {
		dependentsTree = append(dependentsTree, &api.TaskNode{Task: t})
	}

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
		DependsOnTree:     dependsOnTree,
		DependentsTree:    dependentsTree,
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

func TestRenderDeps_SelfRowAppearsInMiddle(t *testing.T) {
	taskA := makeTask("aaaabbbb-0000-0000-0000-000000000001", "upstream task", orchestrator.TaskStatusDone)
	taskC := makeTask("ccccdddd-0000-0000-0000-000000000002", "downstream task", orchestrator.TaskStatusPending)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA}, []*orchestrator.Task{taskC}, nil)

	out := renderDeps(detail, 80, 20, 0)
	if !containsStr(out, "this task") {
		t.Error("expected '(this task)' label in output")
	}
	// Upstream task should appear above self (earlier lines).
	upPos := strings.Index(out, "upstream task")
	selfPos := strings.Index(out, "this task")
	downPos := strings.Index(out, "downstream task")
	if upPos < 0 || selfPos < 0 || downPos < 0 {
		t.Fatalf("missing content: upPos=%d selfPos=%d downPos=%d", upPos, selfPos, downPos)
	}
	if upPos >= selfPos {
		t.Error("upstream task should appear before self row")
	}
	if selfPos >= downPos {
		t.Error("self row should appear before downstream task")
	}
}

func TestRenderDeps_WithDependsOn(t *testing.T) {
	taskA := makeTask("aaaabbbb-0000-0000-0000-000000000001", "refactor auth module", orchestrator.TaskStatusDone)
	taskB := makeTask("ccccdddd-0000-0000-0000-000000000002", "add tests", orchestrator.TaskStatusPending)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA, taskB}, nil, nil)

	out := renderDeps(detail, 80, 20, 0)
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
	if !containsStr(out, "this task") {
		t.Error("expected '(this task)' marker")
	}
}

func TestRenderDeps_WithDependents(t *testing.T) {
	taskC := makeTask("eeeeffff-0000-0000-0000-000000000003", "run e2e", orchestrator.TaskStatusPending)
	detail := makeDetailWithDeps(nil, []*orchestrator.Task{taskC}, nil)

	out := renderDeps(detail, 80, 20, 0)
	if !containsStr(out, "run e2e") {
		t.Error("expected taskC title in output")
	}
	if !containsStr(out, "this task") {
		t.Error("expected '(this task)' marker")
	}
}

// TestRenderDeps_Unresolved: unresolved IDs have no tree entry → treated as no-deps.
func TestRenderDeps_Unresolved(t *testing.T) {
	unresolvedID := "99990000-0000-0000-0000-000000000099"
	detail := makeDetailWithDeps(nil, nil, []string{unresolvedID})

	out := renderDeps(detail, 80, 20, 0)
	// Unresolved deps are not displayed in the tree view (tree fields are empty).
	if !containsStr(out, "no dependencies") {
		t.Errorf("expected 'no dependencies' when only unresolved deps exist, got %q", out)
	}
}

// TestRenderDeps_EmptyDependsOnSection: only downstream dep → self row still shows.
func TestRenderDeps_EmptyDependsOnSection(t *testing.T) {
	taskC := makeTask("cccccccc-0000-0000-0000-000000000001", "dep task", orchestrator.TaskStatusPending)
	detail := makeDetailWithDeps(nil, []*orchestrator.Task{taskC}, nil)

	out := renderDeps(detail, 80, 20, 0)
	// New tree view: no "Depends on" header; self row present.
	if !containsStr(out, "this task") {
		t.Error("expected '(this task)' marker")
	}
	if !containsStr(out, "dep task") {
		t.Error("expected downstream task title in output")
	}
}

// TestRenderDeps_SelectedRowHasBackground verifies that a selected row carries
// the background-color SGR sequence (background 237).
func TestRenderDeps_SelectedRowHasBackground(t *testing.T) {
	taskA := makeTask("aaaabbbb-cccc-0000-0000-000000000001", "task alpha", orchestrator.TaskStatusDone)
	taskB := makeTask("eeeeffff-cccc-0000-0000-000000000002", "task beta", orchestrator.TaskStatusPending)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA, taskB}, nil, nil)

	out := renderDeps(detail, 80, 20, 0)
	// The selected row (cursor=0 → taskA) should have background SGR 237.
	if !containsStr(out, selectedBgSGR) {
		t.Error("cursor=0: expected background-color SGR in output")
	}
}

// TestRenderDeps_CursorOnDependent verifies selection and background on a downstream row.
func TestRenderDeps_CursorOnDependent(t *testing.T) {
	taskA := makeTask("aaaabbbb-0000-0000-0000-000000000001", "depends-on task", orchestrator.TaskStatusDone)
	taskC := makeTask("cccccccc-0000-0000-0000-000000000003", "dependent task", orchestrator.TaskStatusExecuting)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA}, []*orchestrator.Task{taskC}, nil)

	// selectable = [taskA, taskC]; cursor=1 selects taskC
	out := renderDeps(detail, 80, 20, 1)
	if !containsStr(out, selectedBgSGR) {
		t.Error("cursor=1 on dependent: expected background-color SGR")
	}
	if !containsStr(out, "dependent task") {
		t.Error("expected dependent task title in output")
	}
}

// --- Multi-level tree display tests ---

func TestRenderDeps_DeepUpstream_DeepestAtTop(t *testing.T) {
	// self depends on A; A depends on B; B depends on C.
	taskC := makeTask("c-task", "Oldest Ancestor", orchestrator.TaskStatusDone)
	taskB := makeTask("b-task", "Middle Ancestor", orchestrator.TaskStatusDone)
	taskA := makeTask("a-task", "Direct Dep", orchestrator.TaskStatusDone)

	taskB.DependsOn = []string{"c-task"}
	taskA.DependsOn = []string{"b-task"}

	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:        "main-task-id",
			Title:     "Main Task",
			Status:    orchestrator.TaskStatusPending,
			Behavior:  "dev",
			DependsOn: []string{"a-task"},
			CreatedAt: time.Now(),
		},
		DependsOnTree: []*api.TaskNode{
			{Task: taskA, Children: []*api.TaskNode{
				{Task: taskB, Children: []*api.TaskNode{
					{Task: taskC},
				}},
			}},
		},
	}

	out := renderDeps(detail, 80, 20, 0)

	posC := strings.Index(out, "Oldest Ancestor")
	posB := strings.Index(out, "Middle Ancestor")
	posA := strings.Index(out, "Direct Dep")
	posSelf := strings.Index(out, "this task")
	if posC < 0 || posB < 0 || posA < 0 || posSelf < 0 {
		t.Fatalf("missing content: C=%d B=%d A=%d self=%d", posC, posB, posA, posSelf)
	}
	// Deepest ancestor first: C < B < A < self
	if !(posC < posB && posB < posA && posA < posSelf) {
		t.Errorf("order wrong: Oldest=%d Middle=%d Direct=%d Self=%d", posC, posB, posA, posSelf)
	}
}

func TestRenderDeps_DownstreamConnectors(t *testing.T) {
	taskX := makeTask("x-task", "Direct Downstream", orchestrator.TaskStatusPending)
	taskY := makeTask("y-task", "Deep Downstream", orchestrator.TaskStatusPending)

	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:        "main-task-id",
			Title:     "Main Task",
			Status:    orchestrator.TaskStatusExecuting,
			Behavior:  "dev",
			CreatedAt: time.Now(),
		},
		DependentsTree: []*api.TaskNode{
			{Task: taskX, Children: []*api.TaskNode{
				{Task: taskY},
			}},
		},
	}

	out := renderDeps(detail, 80, 20, 0)
	// Downstream section should use tree connectors.
	if !containsStr(out, "└─") {
		t.Error("expected '└─' connector for downstream tree")
	}
	if !containsStr(out, "Direct Downstream") {
		t.Error("expected direct downstream task in output")
	}
	if !containsStr(out, "Deep Downstream") {
		t.Error("expected deep downstream task in output")
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

// TestDepsEnter_MultiLevelUpstream verifies Enter on a deeply-nested upstream task.
func TestDepsEnter_MultiLevelUpstream(t *testing.T) {
	taskB := makeTask("bbbb-1111-0000-0000-000000000001", "grandparent", orchestrator.TaskStatusDone)
	taskB.ProjectID = "proj-b"
	taskA := makeTask("aaaa-2222-0000-0000-000000000002", "parent", orchestrator.TaskStatusDone)
	taskA.ProjectID = "proj-a"

	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:        "main-task-id",
			Title:     "Main Task",
			Status:    orchestrator.TaskStatusPending,
			Behavior:  "dev",
			DependsOn: []string{taskA.ID},
			CreatedAt: time.Now(),
		},
		DependsOnTree: []*api.TaskNode{
			{Task: taskA, Children: []*api.TaskNode{
				{Task: taskB},
			}},
		},
	}

	// depSelectableItems: post-order DFS = [taskB, taskA]
	items := depSelectableItems(detail)
	if len(items) != 2 {
		t.Fatalf("expected 2 selectable items, got %d: %v", len(items), items)
	}
	if items[0].ID != taskB.ID {
		t.Errorf("items[0] should be deepest ancestor (taskB), got %q", items[0].ID)
	}
	if items[1].ID != taskA.ID {
		t.Errorf("items[1] should be direct dep (taskA), got %q", items[1].ID)
	}

	s := newDepsScreen(detail)
	s.depsCursor = 0 // select taskB (deepest ancestor)

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on grandparent: expected non-nil cmd")
	}
	msg := cmd()
	push, ok := msg.(pushScreenMsg)
	if !ok {
		t.Fatalf("expected pushScreenMsg, got %T", msg)
	}
	screen := push.screen.(*TaskDetailScreen)
	if screen.taskID != taskB.ID {
		t.Errorf("want taskID %q, got %q", taskB.ID, screen.taskID)
	}
}

// --- Upstream connector tests (TDD: written before implementation) ---

// TestRenderDeps_UpstreamHasConnectors checks that a multi-level upstream tree
// uses ├─/└─ connectors, not bare spaces.
func TestRenderDeps_UpstreamHasConnectors(t *testing.T) {
	taskA := makeTask("a-task", "Direct Dep", orchestrator.TaskStatusDone)
	taskB := makeTask("b-task", "Grandparent", orchestrator.TaskStatusDone)

	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:        "main-task-id",
			Title:     "Main Task",
			Status:    orchestrator.TaskStatusPending,
			Behavior:  "dev",
			DependsOn: []string{"a-task"},
			CreatedAt: time.Now(),
		},
		DependsOnTree: []*api.TaskNode{
			{Task: taskA, Children: []*api.TaskNode{
				{Task: taskB},
			}},
		},
	}

	out := renderDeps(detail, 80, 20, 0)

	// Both tasks should be visible.
	if !containsStr(out, "Direct Dep") {
		t.Error("expected Direct Dep in output")
	}
	if !containsStr(out, "Grandparent") {
		t.Error("expected Grandparent in output")
	}

	// Upstream should contain connectors, not just spaces.
	if !containsStr(out, "└─") {
		t.Error("expected '└─' connector in upstream section")
	}
}

// TestRenderDeps_UpstreamMultipleDepsHasConnectors checks that when self depends
// on multiple tasks, both ├─ and └─ connectors appear.
func TestRenderDeps_UpstreamMultipleDepsHasConnectors(t *testing.T) {
	taskA := makeTask("a-task", "Dep Alpha", orchestrator.TaskStatusDone)
	taskB := makeTask("b-task", "Dep Beta", orchestrator.TaskStatusDone)

	detail := &api.TaskDetailView{
		Task: &orchestrator.Task{
			ID:        "main-task-id",
			Title:     "Main Task",
			Status:    orchestrator.TaskStatusPending,
			Behavior:  "dev",
			DependsOn: []string{"a-task", "b-task"},
			CreatedAt: time.Now(),
		},
		DependsOnTree: []*api.TaskNode{
			{Task: taskA},
			{Task: taskB},
		},
	}

	out := renderDeps(detail, 80, 20, 0)

	// With two sibling deps, we expect both ├─ and └─.
	if !containsStr(out, "├─") {
		t.Error("expected '├─' connector for non-last sibling upstream dep")
	}
	if !containsStr(out, "└─") {
		t.Error("expected '└─' connector for last sibling upstream dep")
	}
	if !containsStr(out, "Dep Alpha") {
		t.Error("expected Dep Alpha in output")
	}
	if !containsStr(out, "Dep Beta") {
		t.Error("expected Dep Beta in output")
	}
}

// TestRenderDeps_NoArrowPrefix verifies that no '▸' cursor arrow appears in
// the deps view (selection should use background highlight only).
func TestRenderDeps_NoArrowPrefix(t *testing.T) {
	taskA := makeTask("aaaa-1111-0000-0000-000000000001", "task alpha", orchestrator.TaskStatusDone)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA}, nil, nil)

	// Check with cursor on the only selectable item.
	out := renderDeps(detail, 80, 20, 0)
	if containsStr(out, "▸") {
		t.Error("expected no '▸' arrow prefix in deps view; use background highlight instead")
	}
}

// TestDepsSelectable_SelfNotSelectable verifies that the self task is never
// included in the selectable cursor targets.
func TestDepsSelectable_SelfNotSelectable(t *testing.T) {
	taskA := makeTask("aaaa", "A", orchestrator.TaskStatusDone)
	taskC := makeTask("cccc", "C", orchestrator.TaskStatusPending)
	detail := makeDetailWithDeps([]*orchestrator.Task{taskA}, []*orchestrator.Task{taskC}, nil)

	items := depSelectableItems(detail)
	for _, item := range items {
		if item.ID == detail.Task.ID {
			t.Errorf("self task (ID=%q) must not appear in selectable items", detail.Task.ID)
		}
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
	if !containsStr(view, "auth refactor") {
		t.Error("deps View: expected task title")
	}
	if !containsStr(view, "this task") {
		t.Error("deps View: expected '(this task)' marker")
	}
}

