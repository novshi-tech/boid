package api

// Pins accept(go) against the card_events seam, through the REAL
// orchestrator.CreateAction ingest path: the "answered" action a human accept
// records is itself ingest-eligible (cardEventIngestActionTypes), so accepting
// a suggestion on a card whose project declares card_events queues a
// card_requests row caused by that very action. accept(go) needs the card's
// single work slot for its own reservation, so that queued row must not be
// launched ahead of the go it was caused by.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// newCardEventAcceptService wires a TaskWorkflowService against a real sqlite
// DB whose CreateAction actually runs IngestCardEventRequest, and returns it
// with the parked card that detail describes.
func newCardEventAcceptService(t *testing.T, detail string) (*TaskWorkflowService, *fakeTriggerExecDispatcher, *fakeTaskCreator, *orchestrator.TaskRepository, *orchestrator.Task) {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	repo := orchestrator.NewTaskRepository(d.Conn)
	card := &orchestrator.Task{ProjectID: "proj-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	if err := repo.CreateTask(card); err != nil {
		t.Fatalf("create card: %v", err)
	}
	if err := repo.UpsertTaskTriage(&orchestrator.CardAttrs{TaskID: card.ID, Detail: json.RawMessage(detail)}); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}

	exec := &fakeTriggerExecDispatcher{jobs: newFakeTriggerJobStore()}
	// Persist the child for real: acceptGo's post-create reconciliation reads
	// the row back, and a non-existent one would only ever exercise its
	// "child task vanished" branch.
	creator := &fakeTaskCreator{createFn: func(req CreateTaskRequest) (*orchestrator.Task, error) {
		child := &orchestrator.Task{
			ProjectID: req.ProjectID,
			ParentID:  req.ParentID,
			Ref:       req.Ref,
			Title:     req.Title,
			Type:      orchestrator.TaskTypeExecution,
			Status:    orchestrator.TaskStatusExecuting,
			Exec:      &orchestrator.ExecAttrs{Behavior: req.Behavior},
		}
		if err := repo.CreateTask(child); err != nil {
			return nil, err
		}
		return child, nil
	}}
	svc := &TaskWorkflowService{
		Tasks:        repo,
		TaskTriage:   repo,
		CardRequests: repo,
		TaskCreator:  creator,
		Exec:         exec,
		Meta: fakeTriggerMetaStore{byProject: map[string]*orchestrator.ProjectMeta{
			"proj-1": testCardMeta(map[string]orchestrator.CardCommand{"review": {Label: "Run", Run: "echo hi"}}),
		}},
		Tx: cardEventTransactor{conn: d.Conn, cardEvents: fakeCardEventResolver{"proj-1": "review"}},
	}
	return svc, exec, creator, repo, card
}

func TestApplyAnswered_AcceptGo_NotBlockedByTheCardEventItsOwnActionQueued(t *testing.T) {
	svc, exec, creator, repo, card := newCardEventAcceptService(t,
		`{"attrs":{"suggestion":{"verb":"go","reason":"the child is specced and ready"}},`+
			`"children":[{"id":"ch_00","title":"the work","status":"specced","spec":{"project":"proj-1","behavior":"implement"}}]}`)

	ctx := orchestrator.WithActor(context.Background(), orchestrator.ActorHuman)
	payload, _ := json.Marshal(map[string]string{"answer": answeredAnswerAccept, "verb": "go"})
	if _, err := svc.ApplyAction(ctx, card.ID, ApplyActionRequest{Type: "answered", Payload: payload}); err != nil {
		t.Fatalf("ApplyAction(answered, accept go): %v", err)
	}

	fresh, err := repo.GetTask(card.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if fresh.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("card status = %q, want working", fresh.Status)
	}
	if len(creator.calls) != 1 {
		t.Fatalf("CreateTask calls = %d, want 1 (the specced child must be dispatched)", len(creator.calls))
	}

	tt, err := repo.GetTaskTriage(card.ID)
	if err != nil {
		t.Fatalf("GetTaskTriage: %v", err)
	}
	children, err := orchestrator.DetailChildren(tt.Detail)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	if len(children) != 1 || children[0].Status != orchestrator.TaskTriageChildStatusDispatched {
		t.Fatalf("children = %+v, want the single child marked dispatched", children)
	}

	// The card_events row the accept's own "answered" action queued must still
	// be queued: go holds the slot, so an immediate dispatch attempt has
	// nothing to claim. Launching it instead is what used to take the slot go
	// needed and turn every accept(go) into a 409.
	if len(exec.calls) != 0 {
		t.Fatalf("StartExec calls = %d, want 0 — the queued card event must not launch ahead of go", len(exec.calls))
	}
	rows, err := repo.ListCardRequestsByCard(card.ID)
	if err != nil {
		t.Fatalf("ListCardRequestsByCard: %v", err)
	}
	byKey := map[string]*orchestrator.CardRequest{}
	for _, r := range rows {
		byKey[r.CommandKey] = r
	}
	if len(rows) != 2 {
		t.Fatalf("card_requests rows = %d (%+v), want 2 (go's reservation + the queued card event)", len(rows), rows)
	}
	goRow, ok := byKey[orchestrator.CardRequestCommandKeyGo]
	if !ok || goRow.Status != orchestrator.CardRequestStatusLaunching {
		t.Errorf("go reservation = %+v, want a launching row", goRow)
	}
	eventRow, ok := byKey["review"]
	if !ok || eventRow.Status != orchestrator.CardRequestStatusQueued {
		t.Errorf("card event row = %+v, want it still queued (the periodic sweep runs it once the slot frees)", eventRow)
	}
}

// TestApplyAnswered_AcceptGoFails_StillDispatchesTheQueuedCardEvent is the
// twin of the test above: deferring the dispatch attempt past accept(go) must
// not drop it on accept(go)'s own failure paths, which leave the slot free.
func TestApplyAnswered_AcceptGoFails_StillDispatchesTheQueuedCardEvent(t *testing.T) {
	// A go suggestion with no specced child to run: acceptGo rejects it with
	// a 409 before ever reserving the slot.
	svc, exec, creator, repo, card := newCardEventAcceptService(t,
		`{"attrs":{"suggestion":{"verb":"go","reason":"stale — the child was dropped"}},"children":[]}`)

	ctx := orchestrator.WithActor(context.Background(), orchestrator.ActorHuman)
	payload, _ := json.Marshal(map[string]string{"answer": answeredAnswerAccept, "verb": "go"})
	if _, err := svc.ApplyAction(ctx, card.ID, ApplyActionRequest{Type: "answered", Payload: payload}); err == nil {
		t.Fatal("ApplyAction(answered, accept go) succeeded, want a 409 for a card with no specced child")
	}
	if len(creator.calls) != 0 {
		t.Fatalf("CreateTask calls = %d, want 0", len(creator.calls))
	}

	fresh, err := repo.GetTask(card.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if fresh.Status != orchestrator.TaskStatusParked {
		t.Fatalf("card status = %q, want parked (a failed accept must not transition)", fresh.Status)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("StartExec calls = %d, want 1 (the queued card event still runs once go declined the slot)", len(exec.calls))
	}
}
