package timeline

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// newTimelineTestDB is testutil.NewTestDB's own body, copied rather than
// imported: testutil transitively imports internal/server, which imports
// this package back (server.go's Web UI wiring) — importing testutil from a
// package-internal (`package timeline`) test file would be a real import
// cycle, not just an unnecessary dependency.
func newTimelineTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestCardForTimeline creates a project (if absent) and a parked card
// task, mirroring internal/orchestrator/card_request_test.go's own fixture
// helper (kept local here since that one is unexported to its package).
func newTestCardForTimeline(t *testing.T, conn db.DBTX, projectID, cardID string) string {
	t.Helper()
	if _, err := orchestrator.GetProject(conn, projectID); err != nil {
		if err := orchestrator.CreateProject(conn, &orchestrator.Project{ID: projectID, WorkDir: "/tmp/" + projectID}); err != nil {
			t.Fatalf("create project: %v", err)
		}
	}
	card := &orchestrator.Task{ID: cardID, ProjectID: projectID, Type: orchestrator.TaskTypeCard, Card: &orchestrator.CardAttrs{}}
	if err := orchestrator.CreateTask(conn, card); err != nil {
		t.Fatalf("create card: %v", err)
	}
	return card.ID
}

// createExecTask creates a minimal execution-type task under cardID, in the
// given terminal status, for exercising a dispatched-and-closed child.
func createExecTask(t *testing.T, conn db.DBTX, projectID, taskID, parentID string, status orchestrator.TaskStatus) {
	t.Helper()
	task := &orchestrator.Task{
		ID:        taskID,
		ProjectID: projectID,
		ParentID:  parentID,
		Type:      orchestrator.TaskTypeExecution,
		Status:    orchestrator.TaskStatusPending,
		Exec:      &orchestrator.ExecAttrs{},
	}
	if err := orchestrator.CreateTask(conn, task); err != nil {
		t.Fatalf("create exec task: %v", err)
	}
	task.Status = status
	if err := orchestrator.UpdateTask(conn, task); err != nil {
		t.Fatalf("update exec task status: %v", err)
	}
}

// createAction is a small helper around orchestrator.CreateAction with the
// nil resolver/cardEvents this read model's own fixtures never need to
// exercise (ingest is PR-4's concern, already covered there).
func createAction(t *testing.T, conn db.DBTX, taskID, actionType string, payload any) *orchestrator.Action {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	a := &orchestrator.Action{TaskID: taskID, Type: actionType, Payload: raw, Actor: orchestrator.ActorDaemon}
	if err := orchestrator.CreateAction(context.Background(), conn, a, nil, nil); err != nil {
		t.Fatalf("create action %s: %v", actionType, err)
	}
	return a
}

// addAndCloseChild is the standard fixture: adds a child, specs it, marks it
// dispatched with a task row, then closes it via a real child_closed
// self-record — mirroring what internal/api's own write paths do, without
// pulling in the internal/api package (which would import this one back).
func addAndCloseChild(t *testing.T, conn db.DBTX, cardID, projectID, childID, taskID, summary string, resultStatus orchestrator.TaskStatus) {
	t.Helper()
	createAction(t, conn, cardID, "child_added", map[string]string{"id": childID, "title": "child " + childID})
	tt, err := orchestrator.GetTaskTriage(conn, cardID)
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	newDetail, err := orchestrator.AddDetailChild(tt.Detail, orchestrator.TaskTriageChild{ID: childID, Title: "child " + childID})
	if err != nil {
		t.Fatalf("AddDetailChild: %v", err)
	}
	tt.Detail = newDetail
	if err := orchestrator.UpsertTaskTriage(conn, tt); err != nil {
		t.Fatalf("upsert task_triage (add): %v", err)
	}

	createAction(t, conn, cardID, "child_specced", map[string]string{"id": childID, "project": projectID})
	tt, err = orchestrator.GetTaskTriage(conn, cardID)
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	newDetail, err = orchestrator.SpecDetailChild(tt.Detail, childID, orchestrator.TaskTriageChildSpec{Project: projectID}, "")
	if err != nil {
		t.Fatalf("SpecDetailChild: %v", err)
	}
	tt.Detail = newDetail
	if err := orchestrator.UpsertTaskTriage(conn, tt); err != nil {
		t.Fatalf("upsert task_triage (spec): %v", err)
	}

	createExecTask(t, conn, projectID, taskID, cardID, resultStatus)

	tt, err = orchestrator.GetTaskTriage(conn, cardID)
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	children, err := orchestrator.DetailChildren(tt.Detail)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	for i := range children {
		if children[i].ID == childID {
			children[i].TaskRef = taskID
			children[i].Status = orchestrator.TaskTriageChildStatusDispatched
		}
	}
	newDetail, err = orchestrator.SetDetailChildren(tt.Detail, children)
	if err != nil {
		t.Fatalf("SetDetailChildren: %v", err)
	}
	tt.Detail = newDetail
	if err := orchestrator.UpsertTaskTriage(conn, tt); err != nil {
		t.Fatalf("upsert task_triage (dispatch): %v", err)
	}

	newDetail, changed, err := orchestrator.MarkDetailChildClosed(tt.Detail, taskID)
	if err != nil {
		t.Fatalf("MarkDetailChildClosed: %v", err)
	}
	if !changed {
		t.Fatalf("MarkDetailChildClosed: expected changed=true")
	}
	tt.Detail = newDetail
	if err := orchestrator.UpsertTaskTriage(conn, tt); err != nil {
		t.Fatalf("upsert task_triage (close): %v", err)
	}
	createAction(t, conn, cardID, "child_closed", map[string]string{
		"child_id":      taskID,
		"child_status":  string(resultStatus),
		"child_title":   "child " + childID,
		"child_project": projectID,
		"summary":       summary,
	})
}

func TestBuildCardTimeline_ClosedChild_ProducesChildAndFinishedItems(t *testing.T) {
	d := newTimelineTestDB(t)
	cardID := newTestCardForTimeline(t, d.Conn, "proj-1", "card-1")
	addAndCloseChild(t, d.Conn, cardID, "proj-1", "c1", "task-c1", "did the thing", orchestrator.TaskStatusDone)

	page, err := BuildCardTimeline(d.Conn, cardID, "", 10)
	if err != nil {
		t.Fatalf("BuildCardTimeline: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %+v, want 2 (child + finished)", page.Items)
	}
	// Newest-first: the finished item (child_closed, latest) comes first.
	finished, child := page.Items[0], page.Items[1]
	if finished.Kind != CardItemChildFinished {
		t.Fatalf("items[0].Kind = %v, want CardItemChildFinished", finished.Kind)
	}
	if child.Kind != CardItemChild {
		t.Fatalf("items[1].Kind = %v, want CardItemChild", child.Kind)
	}
	if finished.CorrelationID != "c1" || child.CorrelationID != "c1" {
		t.Fatalf("correlation ids = %q / %q, want both c1", finished.CorrelationID, child.CorrelationID)
	}
	if child.Child == nil || !child.Child.HasResult || child.Child.Result != "did the thing" {
		t.Fatalf("child.Child = %+v, want HasResult with the child_closed summary", child.Child)
	}
	if child.Child.ResultStatus != orchestrator.TaskStatusDone {
		t.Fatalf("child.Child.ResultStatus = %q, want done", child.Child.ResultStatus)
	}
	if !child.Time.Before(finished.Time) {
		t.Fatalf("child item (creation position) should sort BEFORE its finished item in real time: child=%v finished=%v", child.Time, finished.Time)
	}
	if child.ID == finished.ID {
		t.Fatalf("child and finished items must have distinct stable ids, got %q for both", child.ID)
	}
}

// TestBuildCardTimeline_GCSurvival_ChildTaskRowDeleted pins the core
// GC-survival contract: a closed child's result and identity must still
// render after its own task row is gone, simulated here with a direct
// delete rather than running the daemon's GC loop.
func TestBuildCardTimeline_GCSurvival_ChildTaskRowDeleted(t *testing.T) {
	d := newTimelineTestDB(t)
	cardID := newTestCardForTimeline(t, d.Conn, "proj-1", "card-1")
	addAndCloseChild(t, d.Conn, cardID, "proj-1", "c1", "task-c1", "summary survives", orchestrator.TaskStatusDone)

	if _, err := d.Conn.Exec(`DELETE FROM tasks WHERE id = ?`, "task-c1"); err != nil {
		t.Fatalf("simulate GC delete: %v", err)
	}

	page, err := BuildCardTimeline(d.Conn, cardID, "", 10)
	if err != nil {
		t.Fatalf("BuildCardTimeline after GC: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items after GC = %+v, want 2", page.Items)
	}
	var child *CardItem
	for i := range page.Items {
		if page.Items[i].Kind == CardItemChild {
			child = &page.Items[i]
		}
	}
	if child == nil {
		t.Fatalf("no CardItemChild in %+v", page.Items)
	}
	if child.Child.TaskExists {
		t.Fatalf("TaskExists = true after deleting the row, want false (link must be dropped)")
	}
	if !child.Child.HasResult || child.Child.Result != "summary survives" {
		t.Fatalf("Child = %+v, want the result to survive task-row GC", child.Child)
	}
}

// TestBuildCardTimeline_GCSurvival_CardRequestRowDeleted pins the same
// GC-survival contract for a command's terminal outcome.
func TestBuildCardTimeline_GCSurvival_CardRequestRowDeleted(t *testing.T) {
	d := newTimelineTestDB(t)
	cardID := newTestCardForTimeline(t, d.Conn, "proj-1", "card-1")
	req := &orchestrator.CardRequest{
		CardID:        cardID,
		CommandKey:    "discuss",
		Status:        orchestrator.CardRequestStatusLaunching,
		LauncherJobID: "launcher-job-1",
		Instruction:   "please look into this",
	}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	if _, _, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, "launcher-job-1", orchestrator.CardRequestDefinition{}); err != nil && err != orchestrator.ErrNoQueuedCardRequests {
		// req was created directly in launching, not queued; ignore.
		_ = err
	}
	if err := orchestrator.AttachCardRequest(d.Conn, req.ID, orchestrator.CardRequestTargetKindTask, "task-cmd-1"); err != nil {
		t.Fatalf("AttachCardRequest: %v", err)
	}
	createExecTask(t, d.Conn, "proj-1", "task-cmd-1", "", orchestrator.TaskStatusDone)
	if err := orchestrator.FinishCardRequest(d.Conn, req.ID, "continuation reached a terminal successful state"); err != nil {
		t.Fatalf("FinishCardRequest: %v", err)
	}

	if _, err := d.Conn.Exec(`DELETE FROM card_requests WHERE id = ?`, req.ID); err != nil {
		t.Fatalf("simulate GC delete: %v", err)
	}
	if _, err := d.Conn.Exec(`DELETE FROM tasks WHERE id = ?`, "task-cmd-1"); err != nil {
		t.Fatalf("simulate GC delete: %v", err)
	}

	page, err := BuildCardTimeline(d.Conn, cardID, "", 10)
	if err != nil {
		t.Fatalf("BuildCardTimeline after GC: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items after GC = %+v, want 1 command item", page.Items)
	}
	item := page.Items[0]
	if item.Kind != CardItemCommand {
		t.Fatalf("Kind = %v, want CardItemCommand", item.Kind)
	}
	if item.Command == nil || item.Command.Outcome != "finished" {
		t.Fatalf("Command = %+v, want Outcome=finished", item.Command)
	}
	if item.Command.CommandKey != "discuss" {
		t.Fatalf("CommandKey = %q, want discuss (from the terminal action payload, not the deleted row)", item.Command.CommandKey)
	}
	if item.Command.TargetExists {
		t.Fatalf("TargetExists = true after deleting task-cmd-1, want false")
	}
	if item.Command.Instruction != "" {
		t.Fatalf("Instruction = %q, want empty once the live card_requests row is gone (best-effort only)", item.Command.Instruction)
	}
}

// TestCardPinnedItems_ExcludesFromHistory_ThenReturnsOnceResolved pins the
// no-duplicate contract and the "same ID once resolved" contract together:
// a specced-but-undispatched child is pinned and absent from history; once
// dropped, the SAME id surfaces in history.
func TestCardPinnedItems_ExcludesFromHistory_ThenReturnsOnceResolved(t *testing.T) {
	d := newTimelineTestDB(t)
	cardID := newTestCardForTimeline(t, d.Conn, "proj-1", "card-1")

	added := createAction(t, d.Conn, cardID, "child_added", map[string]string{"id": "c1", "title": "spec me"})
	tt, err := orchestrator.GetTaskTriage(d.Conn, cardID)
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	newDetail, err := orchestrator.AddDetailChild(tt.Detail, orchestrator.TaskTriageChild{ID: "c1", Title: "spec me"})
	if err != nil {
		t.Fatalf("AddDetailChild: %v", err)
	}
	tt.Detail = newDetail
	if err := orchestrator.UpsertTaskTriage(d.Conn, tt); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// While open, it must be pinned and absent from history.
	pinned, err := CardPinnedItems(d.Conn, cardID)
	if err != nil {
		t.Fatalf("CardPinnedItems: %v", err)
	}
	if len(pinned) != 1 || pinned[0].Kind != CardItemChild || pinned[0].ID != added.ID {
		t.Fatalf("pinned = %+v, want exactly one CardItemChild with id %q", pinned, added.ID)
	}
	page, err := BuildCardTimeline(d.Conn, cardID, "", 10)
	if err != nil {
		t.Fatalf("BuildCardTimeline: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("history while pinned = %+v, want empty (no duplication)", page.Items)
	}

	// Drop it (khi decides not to pursue) — it must return to history at
	// the SAME id, and disappear from pinned.
	createAction(t, d.Conn, cardID, "child_dropped", map[string]string{"id": "c1", "reason": "duplicate"})
	tt, err = orchestrator.GetTaskTriage(d.Conn, cardID)
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	newDetail, changed, err := orchestrator.DropDetailChild(tt.Detail, "c1")
	if err != nil || !changed {
		t.Fatalf("DropDetailChild: changed=%v err=%v", changed, err)
	}
	tt.Detail = newDetail
	if err := orchestrator.UpsertTaskTriage(d.Conn, tt); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	pinned, err = CardPinnedItems(d.Conn, cardID)
	if err != nil {
		t.Fatalf("CardPinnedItems: %v", err)
	}
	if len(pinned) != 0 {
		t.Fatalf("pinned after drop = %+v, want empty", pinned)
	}
	page, err = BuildCardTimeline(d.Conn, cardID, "", 10)
	if err != nil {
		t.Fatalf("BuildCardTimeline after drop: %v", err)
	}
	var childItem *CardItem
	for i := range page.Items {
		if page.Items[i].Kind == CardItemChild {
			childItem = &page.Items[i]
		}
	}
	if childItem == nil || childItem.ID != added.ID {
		t.Fatalf("history after drop = %+v, want the SAME id %q to reappear", page.Items, added.ID)
	}
	if childItem.Child.ClosingActionType != "child_dropped" {
		t.Fatalf("ClosingActionType = %q, want child_dropped", childItem.Child.ClosingActionType)
	}
}

// TestCardPinnedItems_ActiveSuggestion_ExcludedFromHistoryUntilAnswered pins
// the suggestion half of the same no-duplicate contract.
func TestCardPinnedItems_ActiveSuggestion_ExcludedFromHistoryUntilAnswered(t *testing.T) {
	d := newTimelineTestDB(t)
	cardID := newTestCardForTimeline(t, d.Conn, "proj-1", "card-1")

	suggestionAction := createAction(t, d.Conn, cardID, "attrs_set", map[string]any{
		"suggestion": map[string]string{"verb": "park", "reason": "waiting on input"},
	})
	tt, err := orchestrator.GetTaskTriage(d.Conn, cardID)
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	newDetail, err := orchestrator.FoldDetailAttrs(tt.Detail, map[string]json.RawMessage{
		"suggestion": json.RawMessage(`{"verb":"park","reason":"waiting on input"}`),
	})
	if err != nil {
		t.Fatalf("FoldDetailAttrs: %v", err)
	}
	tt.Detail = newDetail
	tt.SuggestionVerb = "park"
	if err := orchestrator.UpsertTaskTriage(d.Conn, tt); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	pinned, err := CardPinnedItems(d.Conn, cardID)
	if err != nil {
		t.Fatalf("CardPinnedItems: %v", err)
	}
	wantID := suggestionAction.ID + ":suggestion"
	if len(pinned) != 1 || pinned[0].Kind != CardItemSuggestion || pinned[0].ID != wantID {
		t.Fatalf("pinned = %+v, want exactly one CardItemSuggestion with id %q", pinned, wantID)
	}
	page, err := BuildCardTimeline(d.Conn, cardID, "", 10)
	if err != nil {
		t.Fatalf("BuildCardTimeline: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("history while suggestion pending = %+v, want empty", page.Items)
	}

	// Answer it (strip the suggestion) — the answered action is a new,
	// independent item; the ORIGINAL suggestion item returns to history at
	// its own (earlier) position, same id.
	createAction(t, d.Conn, cardID, "answered", map[string]string{"answer": "reject"})
	tt, err = orchestrator.GetTaskTriage(d.Conn, cardID)
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	strippedDetail, err := stripSuggestionForTest(tt.Detail)
	if err != nil {
		t.Fatalf("strip suggestion: %v", err)
	}
	tt.Detail = strippedDetail
	tt.SuggestionVerb = ""
	if err := orchestrator.UpsertTaskTriage(d.Conn, tt); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	pinned, err = CardPinnedItems(d.Conn, cardID)
	if err != nil {
		t.Fatalf("CardPinnedItems: %v", err)
	}
	if len(pinned) != 0 {
		t.Fatalf("pinned after answer = %+v, want empty", pinned)
	}
	page, err = BuildCardTimeline(d.Conn, cardID, "", 10)
	if err != nil {
		t.Fatalf("BuildCardTimeline after answer: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("history after answer = %+v, want 2 (suggestion + answered)", page.Items)
	}
	foundSuggestion := false
	for _, it := range page.Items {
		if it.Kind == CardItemSuggestion {
			foundSuggestion = true
			if it.ID != wantID {
				t.Fatalf("suggestion item id after answer = %q, want unchanged %q", it.ID, wantID)
			}
		}
	}
	if !foundSuggestion {
		t.Fatalf("history after answer = %+v, want the suggestion item back", page.Items)
	}
}

// stripSuggestionForTest removes the "suggestion" key from detail.attrs —
// a minimal stand-in for applyAnsweredSideEffect (internal/api), which this
// package cannot import (it imports internal/timeline back).
func stripSuggestionForTest(detail json.RawMessage) (json.RawMessage, error) {
	return orchestrator.StripDetailAttrs(detail, "suggestion")
}

// TestBuildCardTimeline_ItemCursor_PagesWithoutDuplicationOrGaps pins the
// core cursor contract: paging with a small limit visits every item exactly
// once, in the same order a single large-limit call returns them, and the
// limit counts ITEMS (2 per closed child here) rather than raw actions.
func TestBuildCardTimeline_ItemCursor_PagesWithoutDuplicationOrGaps(t *testing.T) {
	d := newTimelineTestDB(t)
	cardID := newTestCardForTimeline(t, d.Conn, "proj-1", "card-1")
	for i, id := range []string{"c1", "c2", "c3"} {
		addAndCloseChild(t, d.Conn, cardID, "proj-1", id, "task-"+id, "result "+id, orchestrator.TaskStatusDone)
		_ = i
	}

	full, err := BuildCardTimeline(d.Conn, cardID, "", 100)
	if err != nil {
		t.Fatalf("BuildCardTimeline (full): %v", err)
	}
	if len(full.Items) != 6 {
		t.Fatalf("full.Items = %d, want 6 (3 children x 2 items)", len(full.Items))
	}
	if full.HasMore {
		t.Fatalf("full.HasMore = true, want false")
	}

	var paged []CardItem
	cursor := ""
	for pageN := 0; pageN < 10; pageN++ {
		page, err := BuildCardTimeline(d.Conn, cardID, cursor, 2)
		if err != nil {
			t.Fatalf("BuildCardTimeline (page %d): %v", pageN, err)
		}
		if len(page.Items) == 0 {
			break
		}
		if len(page.Items) > 2 {
			t.Fatalf("page %d returned %d items, want <= 2", pageN, len(page.Items))
		}
		paged = append(paged, page.Items...)
		cursor = page.NextCursor
		if !page.HasMore {
			break
		}
	}
	if len(paged) != len(full.Items) {
		t.Fatalf("paged item count = %d, want %d (full)", len(paged), len(full.Items))
	}
	for i := range full.Items {
		if paged[i].ID != full.Items[i].ID || paged[i].Kind != full.Items[i].Kind {
			t.Fatalf("paged[%d] = {%s,%s}, want {%s,%s} (order must match the full read exactly, no dup/gap)",
				i, paged[i].ID, paged[i].Kind, full.Items[i].ID, full.Items[i].Kind)
		}
	}
}

// TestBuildCardTimeline_TiedCreatedAt_StableOrderNoDuplication pins the
// same-instant tie-break: two actions sharing an identical created_at (the
// real-world case EncodeActionCursor's own doc comment documents —
// dispatcher's markStaleTasksAborted taking one now() outside a loop) must
// still page deterministically with no duplicate or skipped item.
func TestBuildCardTimeline_TiedCreatedAt_StableOrderNoDuplication(t *testing.T) {
	d := newTimelineTestDB(t)
	cardID := newTestCardForTimeline(t, d.Conn, "proj-1", "card-1")

	tie := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a1 := createAction(t, d.Conn, cardID, "noted", map[string]string{"text": "first"})
	a2 := createAction(t, d.Conn, cardID, "noted", map[string]string{"text": "second"})
	for _, id := range []string{a1.ID, a2.ID} {
		if _, err := d.Conn.Exec(`UPDATE actions SET created_at = ? WHERE id = ?`, tie, id); err != nil {
			t.Fatalf("force tie on %s: %v", id, err)
		}
	}

	full, err := BuildCardTimeline(d.Conn, cardID, "", 100)
	if err != nil {
		t.Fatalf("BuildCardTimeline (full): %v", err)
	}
	if len(full.Items) != 2 {
		t.Fatalf("items = %+v, want 2", full.Items)
	}

	page1, err := BuildCardTimeline(d.Conn, cardID, "", 1)
	if err != nil {
		t.Fatalf("BuildCardTimeline (page1): %v", err)
	}
	if len(page1.Items) != 1 || !page1.HasMore {
		t.Fatalf("page1 = %+v, want exactly 1 item with HasMore=true", page1)
	}
	page2, err := BuildCardTimeline(d.Conn, cardID, page1.NextCursor, 1)
	if err != nil {
		t.Fatalf("BuildCardTimeline (page2): %v", err)
	}
	if len(page2.Items) != 1 {
		t.Fatalf("page2 = %+v, want exactly 1 item", page2)
	}
	if page1.Items[0].ID == page2.Items[0].ID {
		t.Fatalf("page1 and page2 returned the SAME item id %q — tie broke into a duplicate", page1.Items[0].ID)
	}
	seen := map[string]bool{page1.Items[0].ID: true, page2.Items[0].ID: true}
	if !seen[full.Items[0].ID] || !seen[full.Items[1].ID] {
		t.Fatalf("paged ids %v do not match full read ids %v", seen, []string{full.Items[0].ID, full.Items[1].ID})
	}
	page3, err := BuildCardTimeline(d.Conn, cardID, page2.NextCursor, 1)
	if err != nil {
		t.Fatalf("BuildCardTimeline (page3): %v", err)
	}
	if len(page3.Items) != 0 {
		t.Fatalf("page3 = %+v, want empty (fully drained)", page3.Items)
	}
}

// TestBuildCardTimeline_LimitAppliesToItemsNotActions locks the "cursor is
// item-scoped, not action-scoped" contract from the opposite direction: one
// closed child contributes 2 actions (child_added, child_closed — plus
// child_specced, 3 total) but only 2 ITEMS. A limit of 1 must return
// exactly the single newest item, not silently include partial data.
func TestBuildCardTimeline_LimitAppliesToItemsNotActions(t *testing.T) {
	d := newTimelineTestDB(t)
	cardID := newTestCardForTimeline(t, d.Conn, "proj-1", "card-1")
	addAndCloseChild(t, d.Conn, cardID, "proj-1", "c1", "task-c1", "r", orchestrator.TaskStatusDone)

	page, err := BuildCardTimeline(d.Conn, cardID, "", 1)
	if err != nil {
		t.Fatalf("BuildCardTimeline: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %+v, want exactly 1", page.Items)
	}
	if page.Items[0].Kind != CardItemChildFinished {
		t.Fatalf("Kind = %v, want CardItemChildFinished (the newest item)", page.Items[0].Kind)
	}
	if !page.HasMore {
		t.Fatalf("HasMore = false, want true (the child item is still pending)")
	}
}

// TestBuildCardTimeline_HasMoreFalseWhenExactlyLimitRemaining pins the exact
// boundary an off-by-one in HasMore's "> limit" comparison would miss: when
// the remaining item count equals limit exactly, this page returns
// everything and HasMore must be false, not true.
func TestBuildCardTimeline_HasMoreFalseWhenExactlyLimitRemaining(t *testing.T) {
	d := newTimelineTestDB(t)
	cardID := newTestCardForTimeline(t, d.Conn, "proj-1", "card-1")
	addAndCloseChild(t, d.Conn, cardID, "proj-1", "c1", "task-c1", "r", orchestrator.TaskStatusDone)

	page, err := BuildCardTimeline(d.Conn, cardID, "", 2)
	if err != nil {
		t.Fatalf("BuildCardTimeline: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %+v, want exactly 2 (all of them)", page.Items)
	}
	if page.HasMore {
		t.Fatalf("HasMore = true, want false: exactly %d items exist and all %d were returned", len(page.Items), len(page.Items))
	}
}

func TestClampCardTimelineLimit(t *testing.T) {
	cases := []struct {
		requested int
		want      int
	}{
		{0, DefaultCardTimelineLimit},
		{-5, DefaultCardTimelineLimit},
		{5, 5},
		{MaxCardTimelineLimit + 1, MaxCardTimelineLimit},
	}
	for _, c := range cases {
		if got := ClampCardTimelineLimit(c.requested); got != c.want {
			t.Errorf("ClampCardTimelineLimit(%d) = %d, want %d", c.requested, got, c.want)
		}
	}
}
