package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// PR-2 (docs/plans/webui-detail-list-redesign.md §3.3 item 2 / §3.4 item 2 /
// §7 PR-2): card detail's integrated child list (spec ledger × live task
// status) and an execution root task's descendant subtree.

// awaitingPayloadJSON builds an Exec.Payload carrying the "awaiting" trait
// with the given question id — the shape orchestrator.GetAwaitingPayload
// expects. web/templates/tasks_test.go's awaitingTask helper builds the same
// shape for template-level tests; it is unexported in a different package,
// so this is the internal/api-side equivalent.
func awaitingPayloadJSON(t *testing.T, qid string) json.RawMessage {
	t.Helper()
	ap, err := json.Marshal(orchestrator.AwaitingPayload{QuestionID: qid})
	if err != nil {
		t.Fatalf("marshal awaiting payload: %v", err)
	}
	payload, err := json.Marshal(map[string]json.RawMessage{string(orchestrator.TraitAwaiting): ap})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return payload
}

// --- cardChildrenFromTriage: ledger x live status merge ---

func TestCardChildrenFromTriage_DispatchedChild_LiveStatusReplacesLedgerChip(t *testing.T) {
	triage := &orchestrator.CardAttrs{TaskID: "card-1", Detail: json.RawMessage(`{"children":[
		{"id":"c1","title":"do it","status":"dispatched","task_ref":"task-x"}
	]}`)}
	svc := &stubWebService{tasks: []*orchestrator.Task{
		{ID: "task-x", ParentID: "card-1", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}},
	}}
	h := &WebHandler{Service: svc}

	rows := h.cardChildrenFromTriage("card-1", triage)

	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].LiveStatus != "executing" {
		t.Errorf("LiveStatus = %q, want executing", rows[0].LiveStatus)
	}
	if got := rows[0].DisplayStatus(); got != "executing" {
		t.Errorf("DisplayStatus() = %q, want executing (live status must win over the ledger's bare \"dispatched\")", got)
	}
}

func TestCardChildrenFromTriage_AwaitingChild_CapturesQuestionID(t *testing.T) {
	triage := &orchestrator.CardAttrs{TaskID: "card-1", Detail: json.RawMessage(`{"children":[
		{"id":"c1","title":"ask something","status":"dispatched","task_ref":"task-x"}
	]}`)}
	svc := &stubWebService{tasks: []*orchestrator.Task{
		{ID: "task-x", ParentID: "card-1", Status: orchestrator.TaskStatusAwaiting, Exec: &orchestrator.ExecAttrs{Payload: awaitingPayloadJSON(t, "q-9")}},
	}}
	h := &WebHandler{Service: svc}

	rows := h.cardChildrenFromTriage("card-1", triage)

	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].AwaitingQuestionID != "q-9" {
		t.Errorf("AwaitingQuestionID = %q, want q-9", rows[0].AwaitingQuestionID)
	}
}

func TestCardChildrenFromTriage_NonDispatchedChild_UsesLedgerStatusOnly(t *testing.T) {
	triage := &orchestrator.CardAttrs{TaskID: "card-1", Detail: json.RawMessage(`{"children":[
		{"id":"c1","title":"not yet","status":"specced"}
	]}`)}
	h := &WebHandler{Service: &stubWebService{}}

	rows := h.cardChildrenFromTriage("card-1", triage)

	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].LiveStatus != "" {
		t.Errorf("LiveStatus = %q, want empty (only a dispatched ledger entry gets a live lookup)", rows[0].LiveStatus)
	}
	if got := rows[0].DisplayStatus(); got != "specced" {
		t.Errorf("DisplayStatus() = %q, want specced", got)
	}
}

// 論点8 (docs/plans/webui-detail-list-redesign.md §5): a dispatched child
// whose task_ref doesn't resolve (deleted/GC'd/still propagating) must fall
// back to the ledger status, never error.
func TestCardChildrenFromTriage_DispatchedChildTaskRefUnreachable_FallsBackToLedger(t *testing.T) {
	triage := &orchestrator.CardAttrs{TaskID: "card-1", Detail: json.RawMessage(`{"children":[
		{"id":"c1","title":"vanished","status":"dispatched","task_ref":"task-gone"}
	]}`)}
	h := &WebHandler{Service: &stubWebService{}} // no live tasks at all

	rows := h.cardChildrenFromTriage("card-1", triage)

	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].LiveStatus != "" {
		t.Errorf("LiveStatus = %q, want empty when task_ref doesn't resolve", rows[0].LiveStatus)
	}
	if got := rows[0].DisplayStatus(); got != "dispatched" {
		t.Errorf("DisplayStatus() = %q, want the ledger's own \"dispatched\"", got)
	}
}

func TestCardChildrenFromTriage_NilTriage_ReturnsNil(t *testing.T) {
	h := &WebHandler{Service: &stubWebService{}}
	if got := h.cardChildrenFromTriage("card-1", nil); got != nil {
		t.Errorf("rows = %v, want nil for a card with no triage sidecar row at all", got)
	}
}

func TestCardChildrenFromTriage_SortsByUrgencyOrder(t *testing.T) {
	// Scrambled input order: closed, open, awaiting(dispatched),
	// executing(dispatched), specced.
	triage := &orchestrator.CardAttrs{TaskID: "card-1", Detail: json.RawMessage(`{"children":[
		{"id":"c-closed","title":"closed one","status":"closed"},
		{"id":"c-open","title":"open one","status":"open"},
		{"id":"c-awaiting","title":"awaiting one","status":"dispatched","task_ref":"task-await"},
		{"id":"c-executing","title":"executing one","status":"dispatched","task_ref":"task-exec"},
		{"id":"c-specced","title":"specced one","status":"specced"}
	]}`)}
	svc := &stubWebService{tasks: []*orchestrator.Task{
		{ID: "task-await", ParentID: "card-1", Status: orchestrator.TaskStatusAwaiting, Exec: &orchestrator.ExecAttrs{}},
		{ID: "task-exec", ParentID: "card-1", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}},
	}}
	h := &WebHandler{Service: svc}

	rows := h.cardChildrenFromTriage("card-1", triage)

	var gotOrder []string
	for _, r := range rows {
		gotOrder = append(gotOrder, r.Child.ID)
	}
	wantOrder := []string{"c-awaiting", "c-executing", "c-open", "c-specced", "c-closed"}
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("got %d rows %v, want %d: %v", len(gotOrder), gotOrder, len(wantOrder), wantOrder)
	}
	for i, want := range wantOrder {
		if gotOrder[i] != want {
			t.Errorf("order[%d] = %q, want %q (full order: %v)", i, gotOrder[i], want, gotOrder)
		}
	}
}

// --- HTTP acceptance: awaiting child gets a warning marker + direct link
// to its question (design doc §7 PR-2 受け入れ条件) ---

func TestWebHandler_TaskDetail_Card_AwaitingChild_RendersWarningBadgeAndQuestionLink(t *testing.T) {
	svc := &stubWebService{
		taskDetail: &TaskDetailView{Task: &orchestrator.Task{
			ID: "card-1", Type: orchestrator.TaskTypeCard, Title: "card title",
			Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{},
		}},
		tasks: []*orchestrator.Task{
			{ID: "task-x", ParentID: "card-1", Title: "child task", Status: orchestrator.TaskStatusAwaiting, Exec: &orchestrator.ExecAttrs{Payload: awaitingPayloadJSON(t, "q-9")}},
		},
	}
	triage := &stubTriageStore{rows: map[string]*orchestrator.CardAttrs{
		"card-1": {TaskID: "card-1", Detail: json.RawMessage(`{"children":[
			{"id":"c1","title":"child task","status":"dispatched","task_ref":"task-x"}
		]}`)},
	}}
	h := &WebHandler{Service: svc, TaskTriage: triage}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)

	req := httptest.NewRequest(http.MethodGet, "/tasks/card-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/tasks/task-x/questions/q-9"`) {
		t.Errorf("body missing the child's direct question link; got:\n%s", body)
	}
	if !strings.Contains(body, "⚠") {
		t.Errorf("body missing the warning marker for an awaiting child; got:\n%s", body)
	}
	if !strings.Contains(body, "badge-awaiting") {
		t.Errorf("body missing the awaiting chip (live status should replace the dispatched ledger status); got:\n%s", body)
	}
}

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
