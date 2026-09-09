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
	children := []*orchestrator.Task{
		{ID: "child-1", ParentID: "root", Title: "child 1", Status: orchestrator.TaskStatusDone},
		{ID: "grandchild-1", ParentID: "child-1", Title: "grandchild 1", Status: orchestrator.TaskStatusExecuting},
		{ID: "child-2", ParentID: "root", Title: "child 2", Status: orchestrator.TaskStatusExecuting},
	}
	svc := &stubWebService{tasks: children, taskDetails: map[string]*TaskDetailView{
		"child-1": {Task: children[0]}, "child-2": {Task: children[2]},
	}}
	h := &WebHandler{Service: svc}
	root := &orchestrator.Task{ID: "root", ParentID: ""}

	nodes, err := h.execChildTree(root)
	if err != nil {
		t.Fatal(err)
	}

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
			"root":        {Task: root},
			"child-await": {Task: awaiting},
			"child-done":  {Task: done, Actions: []*orchestrator.Action{{TaskID: done.ID, ToStatus: orchestrator.TaskStatusDone, CreatedAt: finished}}},
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
	grandchild := &orchestrator.Task{ID: "grandchild", ParentID: "child-1", Title: "should not show"}
	svc := &stubWebService{tasks: []*orchestrator.Task{grandchild}, taskDetails: map[string]*TaskDetailView{"grandchild": {Task: grandchild}}}
	h := &WebHandler{Service: svc}
	nonRoot := &orchestrator.Task{ID: "child-1", ParentID: "root"}

	got, err := h.execChildTree(nonRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Task.ID != "grandchild" {
		t.Errorf("execChildTree(non-root) = %v, want grandchild", got)
	}
}

func TestExecChildTree_NoChildren_ReturnsNil(t *testing.T) {
	h := &WebHandler{Service: &stubWebService{}}
	root := &orchestrator.Task{ID: "root", ParentID: ""}

	if got, err := h.execChildTree(root); err != nil || got != nil {
		t.Errorf("execChildTree(childless root) = %v, want nil", got)
	}
}

func TestExecChildTree_NilTask_ReturnsNil(t *testing.T) {
	h := &WebHandler{Service: &stubWebService{}}
	if got, err := h.execChildTree(nil); err != nil || got != nil {
		t.Errorf("execChildTree(nil) = %v, want nil", got)
	}
}

type failingChildReadService struct {
	*stubWebService
	listErr error
}

func (s *failingChildReadService) ListTasks(orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return nil, s.listErr
}

func TestTaskDetailFragment_ChildListFailureReturnsError(t *testing.T) {
	root := &orchestrator.Task{ID: "root", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}}
	svc := &failingChildReadService{stubWebService: &stubWebService{taskDetails: map[string]*TaskDetailView{"root": {Task: root}}}, listErr: assertTransientError{}}
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}/fragment", h.TaskDetailFragment)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/root/fragment?kind=timeline", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

func TestTaskDetailFragment_ChildEnrichmentFailureReturnsError(t *testing.T) {
	root := &orchestrator.Task{ID: "root", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}}
	child := &orchestrator.Task{ID: "child", ParentID: root.ID, Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}}
	svc := &stubWebService{
		tasks:          []*orchestrator.Task{child},
		taskDetails:    map[string]*TaskDetailView{"root": {Task: root}},
		taskDetailErrs: map[string]error{"child": assertTransientError{}},
	}
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}/fragment", h.TaskDetailFragment)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/root/fragment?kind=timeline", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

func TestTaskDetail_ChildListFailureShowsUnavailableState(t *testing.T) {
	root := &orchestrator.Task{ID: "root", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}}
	svc := &failingChildReadService{stubWebService: &stubWebService{taskDetails: map[string]*TaskDetailView{"root": {Task: root}}}, listErr: assertTransientError{}}
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/root", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Unable to load subtasks") {
		t.Fatalf("missing explicit unavailable state; status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestTaskDetail_ChildHistoryPreservesEveryTerminalStatusSnapshot(t *testing.T) {
	created := time.Date(2026, 9, 9, 10, 0, 0, 0, time.Local)
	root := &orchestrator.Task{ID: "root", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}}
	child := &orchestrator.Task{ID: "child", ParentID: root.ID, Type: orchestrator.TaskTypeExecution, Title: "rerun child", Status: orchestrator.TaskStatusExecuting, CreatedAt: created, Exec: &orchestrator.ExecAttrs{}}
	svc := &stubWebService{
		tasks: []*orchestrator.Task{child},
		taskDetails: map[string]*TaskDetailView{
			"root": {Task: root},
			"child": {Task: child, Actions: []*orchestrator.Action{
				{ToStatus: orchestrator.TaskStatusDone, CreatedAt: created.Add(time.Hour)},
				{ToStatus: orchestrator.TaskStatusPending, CreatedAt: created.Add(2 * time.Hour)},
				{ToStatus: orchestrator.TaskStatusAborted, CreatedAt: created.Add(3 * time.Hour)},
				{ToStatus: orchestrator.TaskStatusExecuting, CreatedAt: created.Add(4 * time.Hour)},
			}},
		},
	}
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/root", nil))
	body := w.Body.String()
	if strings.Count(body, "Finished: rerun child") != 2 || !strings.Contains(body, ">done</span>") || !strings.Contains(body, ">aborted</span>") {
		t.Fatalf("terminal history did not preserve snapshots; body=%s", body)
	}
}

// --- HTTP acceptance: root exec detail shows its subtree, a non-root's does not ---

func TestWebHandler_TaskDetail_ExecRoot_ShowsChildTree(t *testing.T) {
	root := &orchestrator.Task{ID: "root-1", Type: orchestrator.TaskTypeExecution, ParentID: "", Title: "root task", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}}
	child := &orchestrator.Task{ID: "child-1", ParentID: "root-1", Title: "child task", Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{}}
	svc := &stubWebService{
		taskDetails: map[string]*TaskDetailView{"root-1": {Task: root}, "child-1": {Task: child}},
		tasks:       []*orchestrator.Task{child},
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
	parent := &orchestrator.Task{ID: "child-1", Type: orchestrator.TaskTypeExecution, ParentID: "root-1", Title: "child task", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}}
	child := &orchestrator.Task{ID: "grandchild-1", ParentID: "child-1", Title: "should not render here", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}}
	svc := &stubWebService{
		taskDetails: map[string]*TaskDetailView{"child-1": {Task: parent}, "grandchild-1": {Task: child}},
		tasks:       []*orchestrator.Task{child},
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
