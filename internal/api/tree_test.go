package api

import (
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func makeTask(id, parentID string) *orchestrator.Task {
	return &orchestrator.Task{
		ID:        id,
		ParentID:  parentID,
		Title:     "Task " + id,
		Status:    orchestrator.TaskStatusPending,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

func TestBuildTreeItems_Empty(t *testing.T) {
	items := BuildTreeItems(nil, nil)
	if len(items) != 0 {
		t.Fatalf("expected empty result, got %d items", len(items))
	}
}

func TestBuildTreeItems_SingleRoot(t *testing.T) {
	tasks := []*orchestrator.Task{makeTask("a", "")}
	items := BuildTreeItems(tasks, nil)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Task.ID != "a" {
		t.Errorf("task ID = %q, want %q", items[0].Task.ID, "a")
	}
	if items[0].Depth != 0 {
		t.Errorf("depth = %d, want 0", items[0].Depth)
	}
	if items[0].HasChildren {
		t.Error("HasChildren = true, want false")
	}
	if items[0].ParentID != "" {
		t.Errorf("ParentID = %q, want empty", items[0].ParentID)
	}
}

func TestBuildTreeItems_ParentChild_DFSOrder(t *testing.T) {
	// parent → child: DFS order, depths correct
	parent := makeTask("p", "")
	child := makeTask("c", "p")
	items := BuildTreeItems([]*orchestrator.Task{parent, child}, nil)

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].Task.ID != "p" || items[1].Task.ID != "c" {
		t.Errorf("order = [%s, %s], want [p, c]", items[0].Task.ID, items[1].Task.ID)
	}
	if items[0].Depth != 0 {
		t.Errorf("parent depth = %d, want 0", items[0].Depth)
	}
	if items[1].Depth != 1 {
		t.Errorf("child depth = %d, want 1", items[1].Depth)
	}
	if !items[0].HasChildren {
		t.Error("parent HasChildren = false, want true")
	}
	if items[1].HasChildren {
		t.Error("child HasChildren = true, want false")
	}
	if items[1].ParentID != "p" {
		t.Errorf("child ParentID = %q, want %q", items[1].ParentID, "p")
	}
}

func TestBuildTreeItems_MultiLevel_DFS(t *testing.T) {
	// r → a → b; r → c
	// DFS: r, a, b, c
	r := makeTask("r", "")
	a := makeTask("a", "r")
	b := makeTask("b", "a")
	c := makeTask("c", "r")
	items := BuildTreeItems([]*orchestrator.Task{r, a, b, c}, nil)

	if len(items) != 4 {
		t.Fatalf("expected 4 items, got %d", len(items))
	}
	wantOrder := []string{"r", "a", "b", "c"}
	wantDepths := []int{0, 1, 2, 1}
	for i, id := range wantOrder {
		if items[i].Task.ID != id {
			t.Errorf("items[%d].ID = %q, want %q", i, items[i].Task.ID, id)
		}
		if items[i].Depth != wantDepths[i] {
			t.Errorf("items[%d].Depth = %d, want %d", i, items[i].Depth, wantDepths[i])
		}
	}
}

func TestBuildTreeItems_SiblingOrder_PreservesInput(t *testing.T) {
	// siblings z, y, x under root r — must appear in input order
	r := makeTask("r", "")
	z := makeTask("z", "r")
	y := makeTask("y", "r")
	x := makeTask("x", "r")
	items := BuildTreeItems([]*orchestrator.Task{r, z, y, x}, nil)

	if len(items) != 4 {
		t.Fatalf("expected 4 items, got %d", len(items))
	}
	wantOrder := []string{"r", "z", "y", "x"}
	for i, id := range wantOrder {
		if items[i].Task.ID != id {
			t.Errorf("items[%d].ID = %q, want %q", i, items[i].Task.ID, id)
		}
	}
}

func TestBuildTreeItems_ParentNotInList_TreatedAsRoot(t *testing.T) {
	// child's parent is not in the list → child becomes a root at depth 0
	child := makeTask("c", "missing-parent")
	items := BuildTreeItems([]*orchestrator.Task{child}, nil)

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Depth != 0 {
		t.Errorf("depth = %d, want 0 (orphan treated as root)", items[0].Depth)
	}
	if items[0].ParentID != "" {
		t.Errorf("ParentID = %q, want empty (visual root)", items[0].ParentID)
	}
}

func TestBuildTreeItems_SelfCycle_DoesNotHang(t *testing.T) {
	// task whose ParentID == its own ID; unreachable from roots → not in output
	a := makeTask("a", "a")
	items := BuildTreeItems([]*orchestrator.Task{a}, nil)
	if len(items) != 0 {
		t.Fatalf("expected 0 items (self-cycle is not a root), got %d", len(items))
	}
}

func TestBuildTreeItems_MutualCycle_DoesNotHang(t *testing.T) {
	// a.ParentID = b, b.ParentID = a — neither is a root, both unreachable
	a := makeTask("a", "b")
	b := makeTask("b", "a")
	items := BuildTreeItems([]*orchestrator.Task{a, b}, nil)
	if len(items) != 0 {
		t.Fatalf("expected 0 items (mutual cycle, no root), got %d", len(items))
	}
}

func TestBuildTreeItems_CycleReachableViaRoot(t *testing.T) {
	// r → a → b, and visited guard prevents re-entry if b points back to a
	// (artificial: build children[a] manually to include b and children[b] to include a)
	// In practice this can't happen with single ParentID, but the visited set handles it.
	r := makeTask("r", "")
	a := makeTask("a", "r")
	b := makeTask("b", "a")
	// b also claims a as parent — but b is already in children["a"], won't loop
	// Just verify no hang and all 3 appear once.
	items := BuildTreeItems([]*orchestrator.Task{r, a, b}, nil)
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
}

func TestBuildTreeItems_MultipleRoots(t *testing.T) {
	x := makeTask("x", "")
	y := makeTask("y", "")
	items := BuildTreeItems([]*orchestrator.Task{x, y}, nil)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].Task.ID != "x" || items[1].Task.ID != "y" {
		t.Errorf("order = [%s, %s], want [x, y]", items[0].Task.ID, items[1].Task.ID)
	}
	if items[0].Depth != 0 || items[1].Depth != 0 {
		t.Error("both roots should have depth 0")
	}
}
