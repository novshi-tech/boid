package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// docs/plans/webui-detail-list-redesign.md §3.4 item 2 / §7 PR-2: an
// execution root task's descendant subtree (app-side depth-first
// traversal). A card's own children moved to the timeline read model —
// see web_card_timeline_test.go for the card-side equivalent of what this
// file used to also cover.

// --- execChildTree: direct children at every depth ---

func TestExecChildTree_ReturnsDirectChildrenOnly(t *testing.T) {
	svc := &stubWebService{tasks: []*orchestrator.Task{
		{ID: "child-1", ParentID: "root", Title: "child 1", Status: orchestrator.TaskStatusDone},
		{ID: "grandchild-1", ParentID: "child-1", Title: "grandchild 1", Status: orchestrator.TaskStatusExecuting},
		{ID: "child-2", ParentID: "root", Title: "child 2", Status: orchestrator.TaskStatusExecuting},
	}}
	h := &WebHandler{Service: svc}
	root := &orchestrator.Task{ID: "root", ParentID: ""}

	nodes := h.execChildTree(root)

	want := []string{"child-1", "child-2"}
	if len(nodes) != len(want) {
		t.Fatalf("nodes = %d, want %d, got %+v", len(nodes), len(want), nodes)
	}
	for i, w := range want {
		if nodes[i].Task.ID != w {
			t.Errorf("nodes[%d] = %s, want %s", i, nodes[i].Task.ID, w)
		}
	}
}

func TestWebHandler_TaskDetail_ExecChildrenShowCurrentQuestionAndHistoricalEvents(t *testing.T) {
	created := time.Date(2026, 9, 9, 10, 0, 0, 0, time.Local)
	finished := created.Add(time.Hour)
	awaitingPayload, _ := json.Marshal(map[string]any{"awaiting": map[string]any{"question_id": "q-1"}})
	root := &orchestrator.Task{ID: "root", Type: orchestrator.TaskTypeExecution, Title: "root", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}}
	awaiting := &orchestrator.Task{ID: "child-await", ParentID: root.ID, Title: "needs an answer", Status: orchestrator.TaskStatusAwaiting, CreatedAt: created, Exec: &orchestrator.ExecAttrs{Behavior: "research", Payload: awaitingPayload}}
	done := &orchestrator.Task{ID: "child-done", ParentID: root.ID, Title: "completed child", Status: orchestrator.TaskStatusDone, CreatedAt: created.Add(time.Minute), Exec: &orchestrator.ExecAttrs{Behavior: "implement"}}
	svc := &stubWebService{
		taskDetails: map[string]*TaskDetailView{
			"root":       {Task: root},
			"child-done": {Task: done, Actions: []*orchestrator.Action{{TaskID: done.ID, ToStatus: orchestrator.TaskStatusDone, CreatedAt: finished}}},
		},
		tasks: []*orchestrator.Task{awaiting, done},
	}
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/root", nil))
	body := w.Body.String()
	for _, want := range []string{`href="/tasks/child-await"`, `href="/tasks/child-await/questions/q-1"`, "Created: needs an answer", "Created: completed child", "Finished: completed child"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q; body=%s", want, body)
		}
	}
}

func TestExecChildTree_NonRootTask_ReturnsDirectChildren(t *testing.T) {
	svc := &stubWebService{tasks: []*orchestrator.Task{
		{ID: "grandchild", ParentID: "child-1", Title: "should not show"},
	}}
	h := &WebHandler{Service: svc}
	nonRoot := &orchestrator.Task{ID: "child-1", ParentID: "root"}

	got := h.execChildTree(nonRoot)
	if len(got) != 1 || got[0].Task.ID != "grandchild" {
		t.Errorf("execChildTree(non-root) = %v, want grandchild", got)
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

func TestWebHandler_TaskDetail_ExecNonRoot_ShowsDirectChildrenAndParentReturn(t *testing.T) {
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
	body := w.Body.String()
	if !strings.Contains(body, "should not render here") || !strings.Contains(body, `href="/tasks/grandchild-1"`) {
		t.Errorf("non-root exec task should link its direct child; got:\n%s", body)
	}
	if !strings.Contains(body, `href="/tasks/root-1"`) {
		t.Errorf("non-root exec task should return to its parent; got:\n%s", body)
	}
}
