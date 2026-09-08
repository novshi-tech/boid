package api

// Pins the claim-time card-status re-check end-to-end, against a real sqlite
// store: a card carrying a queued card_requests row that an accepted verb
// (complete/drop) then moves past parked/working before the row is ever
// claimed. The claim-time status re-check must drain the row instead of
// launching it against a card that is already terminal.

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// fakeCardEventResolver is a minimal orchestrator.CardEventResolver fake —
// a plain map, no project.yaml/ProjectStore involved.
type fakeCardEventResolver map[string]string

func (f fakeCardEventResolver) CardEventCommand(projectID string) (string, bool) {
	key, ok := f[projectID]
	return key, ok
}

// cardEventTransactor is realTransactor (task_resolve_or_capture_test.go)
// plus a wired CardEventResolver, so a WithinTx call's own
// orchestrator.CreateAction actually runs IngestCardEventRequest — needed to
// reproduce the SAME transaction queuing a row against a stale status.
type cardEventTransactor struct {
	conn       *sql.DB
	cardEvents orchestrator.CardEventResolver
}

func (t cardEventTransactor) WithinTx(fn func(TxStore) error) error {
	return db.InTxDB(t.conn, func(tx db.DBTX) error {
		repo := orchestrator.NewTaskRepository(tx)
		repo.SetCardEventResolver(t.cardEvents)
		return fn(realTaskRepoTxStore{repo})
	})
}

func TestApplyAnswered_AcceptComplete_DrainsRaceQueuedRowInsteadOfLaunching(t *testing.T) {
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
	card := &orchestrator.Task{ProjectID: "proj-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	if err := repo.CreateTask(card); err != nil {
		t.Fatalf("create card: %v", err)
	}
	if err := repo.UpsertTaskTriage(&orchestrator.CardAttrs{
		TaskID: card.ID,
		Detail: json.RawMessage(`{"attrs":{"suggestion":{"verb":"complete","reason":"all done"}}}`),
	}); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}

	jobs := newFakeTriggerJobStore()
	exec := &fakeTriggerExecDispatcher{jobs: jobs}
	svc := &TaskWorkflowService{
		Tasks:        repo,
		TaskTriage:   repo,
		CardRequests: repo,
		Exec:         exec,
		Meta: fakeTriggerMetaStore{byProject: map[string]*orchestrator.ProjectMeta{
			"proj-1": testCardMeta(map[string]orchestrator.CardCommand{"review": {Label: "Run", Run: "echo hi"}}),
		}},
		Tx: cardEventTransactor{conn: d.Conn, cardEvents: fakeCardEventResolver{"proj-1": "review"}},
	}

	// Queued while the card was still working — by a note, a summary write, or
	// any other action that leaves new material on the card.
	enqueueForDispatch(t, svc, card.ID, "review", "cause-earlier")

	ctx := orchestrator.WithActor(context.Background(), orchestrator.ActorHuman)
	payload, _ := json.Marshal(map[string]string{"answer": answeredAnswerAccept, "verb": "complete"})
	if _, err := svc.ApplyAction(ctx, card.ID, ApplyActionRequest{Type: "answered", Payload: payload}); err != nil {
		t.Fatalf("ApplyAction(answered, accept complete): %v", err)
	}

	fresh, err := repo.GetTask(card.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if fresh.Status != orchestrator.TaskStatusDone {
		t.Fatalf("card status = %q, want done", fresh.Status)
	}

	// The commit-triggered dispatch attempt must NOT have launched anything
	// — the card was done by the time claim re-checked its status.
	if len(exec.calls) != 0 {
		t.Fatalf("StartExec calls = %d, want 0 — a done card must never be auto-dispatched", len(exec.calls))
	}

	rows, err := repo.ListCardRequestsByCard(card.ID)
	if err != nil {
		t.Fatalf("ListCardRequestsByCard: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("card_requests rows = %d, want exactly 1 (the row queued while status was still working)", len(rows))
	}
	if rows[0].Status != orchestrator.CardRequestStatusFailed {
		t.Errorf("queued row status = %q, want failed (drained, not left queued forever and not launched)", rows[0].Status)
	}
}
