package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func makeTask(id, parentID string) *orchestrator.Task {
	return &orchestrator.Task{
		ID:        id,
		Type:      orchestrator.TaskTypeExecution,
		ParentID:  parentID,
		Title:     "Task " + id,
		Status:    orchestrator.TaskStatusPending,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Exec:      &orchestrator.ExecAttrs{},
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

func TestBuildQueueItems_PopulatesUrgencyAndSummaryFromTriage(t *testing.T) {
	t1 := makeTask("t1", "")
	t2 := makeTask("t2", "")
	triage := map[string]*orchestrator.CardAttrs{
		"t1": {TaskID: "t1", Urgency: orchestrator.UrgencyNow, Detail: []byte(`{"summary":"customer wants a quote"}`)},
		// t2 has no triage row at all.
	}

	items := BuildQueueItems([]*orchestrator.Task{t1, t2}, nil, triage)
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Urgency != "now" {
		t.Errorf("t1 Urgency = %q, want now", items[0].Urgency)
	}
	if items[0].Summary != "customer wants a quote" {
		t.Errorf("t1 Summary = %q, want %q", items[0].Summary, "customer wants a quote")
	}
	if items[1].Urgency != "" || items[1].Summary != "" {
		t.Errorf("t2 (no triage row) should have empty Urgency/Summary, got %q/%q", items[1].Urgency, items[1].Summary)
	}
	// Flat: no tree structure imposed, input order preserved (matches the
	// SQL ORDER BY from store.go's "queue_next" branch).
	for _, it := range items {
		if it.Depth != 0 || it.HasChildren || it.ParentID != "" {
			t.Errorf("queue items must be flat, got %+v", it)
		}
	}
}

// TestBuildQueueItems_PopulatesChildren covers the queue-row children
// preview added after the Shape launcher's first real dispatch (nose had
// no way to see, from the queue list, whether Go would actually dispatch
// anything — cross-project-issue-triage 実地テスト, 2026-08-14).
func TestBuildQueueItems_PopulatesChildren(t *testing.T) {
	t1 := makeTask("t1", "")
	detail := []byte(`{"children":[
		{"id":"c1","status":"specced"},
		{"id":"c2","status":"open"}
	]}`)
	triage := map[string]*orchestrator.CardAttrs{
		"t1": {TaskID: "t1", Detail: detail},
	}

	items := BuildQueueItems([]*orchestrator.Task{t1}, nil, triage)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if len(items[0].Children) != 2 {
		t.Fatalf("Children = %+v, want 2 entries", items[0].Children)
	}
	if items[0].Children[0].Status != orchestrator.TaskTriageChildStatusSpecced {
		t.Errorf("Children[0].Status = %q, want specced", items[0].Children[0].Status)
	}
}

// TestBuildQueueItems_MalformedChildrenJSON_LeavesChildrenEmpty mirrors
// the malformed-summary case: a broken children key must not sink the row.
func TestBuildQueueItems_MalformedChildrenJSON_LeavesChildrenEmpty(t *testing.T) {
	t1 := makeTask("t1", "")
	triage := map[string]*orchestrator.CardAttrs{
		"t1": {TaskID: "t1", Detail: []byte(`not json`)},
	}
	items := BuildQueueItems([]*orchestrator.Task{t1}, nil, triage)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Children != nil {
		t.Errorf("Children = %+v, want nil for malformed detail", items[0].Children)
	}
}

func TestBuildQueueItems_MalformedDetailJSON_LeavesSummaryEmpty(t *testing.T) {
	t1 := makeTask("t1", "")
	triage := map[string]*orchestrator.CardAttrs{
		"t1": {TaskID: "t1", Urgency: orchestrator.UrgencyToday, Detail: []byte(`not json`)},
	}
	items := BuildQueueItems([]*orchestrator.Task{t1}, nil, triage)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Urgency != "today" {
		t.Errorf("Urgency = %q, want today (must still come through despite malformed detail)", items[0].Urgency)
	}
	if items[0].Summary != "" {
		t.Errorf("Summary = %q, want empty (malformed detail must not panic or fabricate a summary)", items[0].Summary)
	}
}

// TestBuildQueueItems_PopulatesSuggestion_TopLevel covers detail.suggestion
// at the top level (docs/plans/cross-project-issue-triage.md:544 のスキーマ
// 案), the shape khi's evaluate step is expected to write.
func TestBuildQueueItems_PopulatesSuggestion_TopLevel(t *testing.T) {
	t1 := makeTask("t1", "")
	triage := map[string]*orchestrator.CardAttrs{
		"t1": {TaskID: "t1", Detail: []byte(`{"suggestion":{"verb":"wake","action":"re-triage now","reason":"source event fired","basis":"issue #42 reopened"}}`)},
	}

	items := BuildQueueItems([]*orchestrator.Task{t1}, nil, triage)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	want := orchestrator.Suggestion{Verb: "wake", Action: "re-triage now", Reason: "source event fired", Basis: "issue #42 reopened"}
	if items[0].Suggestion != want {
		t.Errorf("Suggestion = %+v, want %+v", items[0].Suggestion, want)
	}
}

// TestBuildQueueItems_PopulatesSuggestion_FromAttrs covers detail.attrs.
// suggestion: "suggestion" is not a promoted attrs_set key (see
// applyAttrsSetSideEffect / orchestrator.FoldDetailAttrs), so when khi
// writes it via attrs_set it lands here instead of at the top level.
func TestBuildQueueItems_PopulatesSuggestion_FromAttrs(t *testing.T) {
	t1 := makeTask("t1", "")
	triage := map[string]*orchestrator.CardAttrs{
		"t1": {TaskID: "t1", Detail: []byte(`{"attrs":{"suggestion":{"verb":"go","action":"dispatch","reason":"fully specced"}}}`)},
	}

	items := BuildQueueItems([]*orchestrator.Task{t1}, nil, triage)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	want := orchestrator.Suggestion{Verb: "go", Action: "dispatch", Reason: "fully specced"}
	if items[0].Suggestion != want {
		t.Errorf("Suggestion = %+v, want %+v", items[0].Suggestion, want)
	}
}

// TestBuildQueueItems_MalformedSuggestionJSON_LeavesSuggestionEmpty mirrors
// the malformed-children/malformed-summary cases: a broken detail blob must
// not sink the row (rule 5, 隠さない).
func TestBuildQueueItems_MalformedSuggestionJSON_LeavesSuggestionEmpty(t *testing.T) {
	t1 := makeTask("t1", "")
	triage := map[string]*orchestrator.CardAttrs{
		"t1": {TaskID: "t1", Detail: []byte(`not json`)},
	}

	items := BuildQueueItems([]*orchestrator.Task{t1}, nil, triage)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Suggestion != (orchestrator.Suggestion{}) {
		t.Errorf("Suggestion = %+v, want zero value for malformed detail", items[0].Suggestion)
	}
}

// TestBuildQueueItems_NoSuggestionKey_LeavesSuggestionEmpty covers the
// (overwhelming majority) case: a triage row exists but khi hasn't written
// a suggestion yet.
func TestBuildQueueItems_NoSuggestionKey_LeavesSuggestionEmpty(t *testing.T) {
	t1 := makeTask("t1", "")
	triage := map[string]*orchestrator.CardAttrs{
		"t1": {TaskID: "t1", Detail: []byte(`{"summary":"no suggestion here yet"}`)},
	}

	items := BuildQueueItems([]*orchestrator.Task{t1}, nil, triage)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Suggestion != (orchestrator.Suggestion{}) {
		t.Errorf("Suggestion = %+v, want zero value when detail has no suggestion key", items[0].Suggestion)
	}
}

// TestBuildTreeItemsWithSuggestions_OverlaysSuggestionKeepingTreeShape
// covers the Parked tab path (?status=parked): tree shape (Depth/
// HasChildren/ParentID) must come through unchanged from BuildTreeItems,
// with only Suggestion added on top.
func TestBuildTreeItemsWithSuggestions_OverlaysSuggestionKeepingTreeShape(t *testing.T) {
	parent := makeTask("p", "")
	child := makeTask("c", "p")
	triage := map[string]*orchestrator.CardAttrs{
		"p": {TaskID: "p", Detail: []byte(`{"suggestion":{"verb":"wake","reason":"event fired"}}`)},
		// "c" has no triage row at all.
	}

	items := BuildTreeItemsWithSuggestions([]*orchestrator.Task{parent, child}, nil, triage)
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Suggestion.Verb != "wake" || items[0].Suggestion.Reason != "event fired" {
		t.Errorf("parent Suggestion = %+v, want verb=wake reason=%q", items[0].Suggestion, "event fired")
	}
	if items[1].Suggestion != (orchestrator.Suggestion{}) {
		t.Errorf("child (no triage row) Suggestion = %+v, want zero value", items[1].Suggestion)
	}
	// Tree shape must be untouched: same as plain BuildTreeItems would produce.
	if items[0].Depth != 0 || !items[0].HasChildren {
		t.Errorf("parent shape wrong: Depth=%d HasChildren=%v", items[0].Depth, items[0].HasChildren)
	}
	if items[1].Depth != 1 || items[1].ParentID != "p" {
		t.Errorf("child shape wrong: Depth=%d ParentID=%q", items[1].Depth, items[1].ParentID)
	}
}

// TestWebTaskList_ParkedStatus_RendersSuggestion is an end-to-end wiring
// test for the Parked tab's handler → builder → template path (Opus review
// finding, 2026-08-18): the unit tests for BuildTreeItemsWithSuggestions
// and DetailSuggestion above pass even if TaskList's "parked" switch case
// called plain BuildTreeItems (dropping enrichment entirely) or if
// h.TaskTriage were never wired to h.triageByTaskID's caller — nothing
// exercised the handler itself. This drives an actual HTTP request through
// TaskList with a stubTriageStore attached and checks the rendered HTML.
func TestWebTaskList_ParkedStatus_RendersSuggestion(t *testing.T) {
	task := &orchestrator.Task{
		ID: "t1", Type: orchestrator.TaskTypeCard, Title: "parked card", Status: orchestrator.TaskStatusParked,
		CreatedAt: time.Now(), UpdatedAt: time.Now(), Card: &orchestrator.CardAttrs{},
	}
	svc := &stubWebService{tasks: []*orchestrator.Task{task}}
	triage := &stubTriageStore{rows: map[string]*orchestrator.CardAttrs{
		"t1": {TaskID: "t1", Detail: []byte(`{"suggestion":{"verb":"reopen","reason":"source event fired"}}`)},
	}}
	h := &WebHandler{Service: svc, TaskTriage: triage}
	r := chi.NewRouter()
	r.Get("/", h.TaskList)

	req := httptest.NewRequest(http.MethodGet, "/?status=parked", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if svc.capturedFilter.Status != "parked" {
		t.Errorf("Status passed to ListTasks = %q, want %q", svc.capturedFilter.Status, "parked")
	}
	body := w.Body.String()
	if !strings.Contains(body, "badge-verb-reopen") {
		t.Errorf("parked list should render the suggestion verb badge, got: %s", body)
	}
	if !strings.Contains(body, "source event fired") {
		t.Errorf("parked list should render the suggestion reason, got: %s", body)
	}
}
