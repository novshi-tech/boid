package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// docs/plans/webui-detail-list-redesign.md §3.4 item 2 / §7 PR-2: an
// execution root task's descendant subtree (app-side depth-first
// traversal). A card's own children moved to the timeline read model —
// see web_card_timeline_test.go for the card-side equivalent of what this
// file used to also cover.

// --- execChildTree: app-side depth-first traversal, root-only ---

func TestExecChildTree_RootTask_ReturnsDepthFirstMultiLevelSubtree(t *testing.T) {
	svc := &stubWebService{tasks: []*orchestrator.Task{
		{ID: "child-1", ParentID: "root", Title: "child 1", Status: orchestrator.TaskStatusDone},
		{ID: "grandchild-1", ParentID: "child-1", Title: "grandchild 1", Status: orchestrator.TaskStatusExecuting},
		{ID: "child-2", ParentID: "root", Title: "child 2", Status: orchestrator.TaskStatusExecuting},
	}}
	h := &WebHandler{Service: svc}
	root := &orchestrator.Task{ID: "root", ParentID: ""}

	nodes := h.execChildTree(root)

	want := []struct {
		id    string
		depth int
	}{
		{"child-1", 1},
		{"grandchild-1", 2},
		{"child-2", 1},
	}
	if len(nodes) != len(want) {
		t.Fatalf("nodes = %d, want %d, got %+v", len(nodes), len(want), nodes)
	}
	for i, w := range want {
		if nodes[i].Task.ID != w.id || nodes[i].Depth != w.depth {
			t.Errorf("nodes[%d] = (%s, depth %d), want (%s, depth %d)", i, nodes[i].Task.ID, nodes[i].Depth, w.id, w.depth)
		}
	}
}

func TestExecChildTree_NonRootTask_ReturnsNil(t *testing.T) {
	svc := &stubWebService{tasks: []*orchestrator.Task{
		{ID: "grandchild", ParentID: "child-1", Title: "should not show"},
	}}
	h := &WebHandler{Service: svc}
	nonRoot := &orchestrator.Task{ID: "child-1", ParentID: "root"}

	if got := h.execChildTree(nonRoot); got != nil {
		t.Errorf("execChildTree(non-root) = %v, want nil — only a root task's own detail page shows the subtree", got)
	}
}

func TestExecChildTree_NoChildren_ReturnsNil(t *testing.T) {
	h := &WebHandler{Service: &stubWebService{}}
	root := &orchestrator.Task{ID: "root", ParentID: ""}

	if got := h.execChildTree(root); got != nil {
		t.Errorf("execChildTree(childless root) = %v, want nil", got)
	}
}

func TestExecChildTree_NilTask_ReturnsNil(t *testing.T) {
	h := &WebHandler{Service: &stubWebService{}}
	if got := h.execChildTree(nil); got != nil {
		t.Errorf("execChildTree(nil) = %v, want nil", got)
	}
}

// --- HTTP acceptance: root exec detail shows its subtree, a non-root's does not ---

func TestWebHandler_TaskDetail_ExecRoot_ShowsChildTree(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{Task: &orchestrator.Task{
			ID: "root-1", Type: orchestrator.TaskTypeExecution, ParentID: "", Title: "root task",
			Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{},
		}},
		tasks: []*orchestrator.Task{
			{ID: "child-1", ParentID: "root-1", Title: "child task", Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{}},
		},
	}
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)

	req := httptest.NewRequest(http.MethodGet, "/tasks/root-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "child task") || !strings.Contains(body, `href="/tasks/child-1"`) {
		t.Errorf("body missing the root task's child tree; got:\n%s", body)
	}
}

func TestWebHandler_TaskDetail_ExecNonRoot_NoChildTree(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{Task: &orchestrator.Task{
			ID: "child-1", Type: orchestrator.TaskTypeExecution, ParentID: "root-1", Title: "child task",
			Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{},
		}},
		tasks: []*orchestrator.Task{
			{ID: "grandchild-1", ParentID: "child-1", Title: "should not render here", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}},
		},
	}
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)

	req := httptest.NewRequest(http.MethodGet, "/tasks/child-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), "should not render here") {
		t.Error("non-root exec task's detail page rendered a child tree; want none — only the root's own detail page shows the subtree")
	}
}
