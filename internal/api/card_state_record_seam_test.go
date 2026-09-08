package api

// The seam these records exist for: a card's own state change must reach
// IngestCardEventRequest through CreateAction, and must carry the writer off
// the context so a judge's own edit does not relaunch the judge.
//
// The transactor here mirrors internal/server/api_store.go's: the per-tx
// repository gets the same resolvers the singleton has. Without that the
// assertions below are vacuous — a test on a resolver-less transactor passes
// whether or not the ctx is threaded.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type seamCardEventResolver map[string]string

func (r seamCardEventResolver) CardEventCommand(projectID string) (string, bool) {
	key, ok := r[projectID]
	return key, ok && key != ""
}

// seamTransactor re-seeds both ingest resolvers on the per-tx repository.
type seamTransactor struct {
	conn     *sql.DB
	resolver orchestrator.CardEventResolver
}

func (t seamTransactor) WithinTx(fn func(TxStore) error) error {
	return db.InTxDB(t.conn, func(tx db.DBTX) error {
		repo := orchestrator.NewTaskRepository(tx)
		repo.SetCardEventResolver(t.resolver)
		return fn(realTaskRepoTxStore{repo})
	})
}

func newSeamService(t *testing.T) (*TaskAppService, *orchestrator.TaskRepository, *sql.DB) {
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
	resolver := seamCardEventResolver{"proj-1": "judge"}
	tasks := orchestrator.NewTaskRepository(d.Conn)
	tasks.SetCardEventResolver(resolver)
	return &TaskAppService{
		Tasks:      tasks,
		Actions:    tasks,
		Identities: tasks,
		Tx:         seamTransactor{conn: d.Conn, resolver: resolver},
	}, tasks, d.Conn
}

func seedSeamCard(t *testing.T, tasks *orchestrator.TaskRepository) *orchestrator.Task {
	t.Helper()
	card := &orchestrator.Task{
		ProjectID: "proj-1", Type: orchestrator.TaskTypeCard,
		Title: "a card", Description: "before", Status: orchestrator.TaskStatusWorking,
		Card: &orchestrator.CardAttrs{},
	}
	if err := tasks.CreateTask(card); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return card
}

// seedLiveJudgeRequest gives the card a launching card_requests row attached to
// a continuation task — the shape a running judge has.
func seedLiveJudgeRequest(t *testing.T, conn *sql.DB, cardID string) string {
	t.Helper()
	req := &orchestrator.CardRequest{
		CardID: cardID, CommandKey: "judge", CauseID: "cause-1",
		Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-1",
	}
	if err := orchestrator.CreateCardRequest(conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	if _, err := conn.Exec(
		`UPDATE card_requests SET status = ?, target_kind = 'task', target_id = ?, updated_at = ? WHERE id = ?`,
		orchestrator.CardRequestStatusAttached, "cont-1", time.Now().UTC(), req.ID,
	); err != nil {
		t.Fatalf("attach card request: %v", err)
	}
	return req.ID
}

func countCardRequests(t *testing.T, conn *sql.DB, cardID string) int {
	t.Helper()
	rows, err := orchestrator.ListCardRequestsByCard(conn, cardID)
	if err != nil {
		t.Fatalf("ListCardRequestsByCard: %v", err)
	}
	return len(rows)
}

// TestCardEditSeam_JudgesOwnEditDoesNotRelaunchIt is the regression this whole
// ctx threading exists for. The control case proves the assertion is load
// bearing: drop the writer off the context and the judge queues itself again.
func TestCardEditSeam_JudgesOwnEditDoesNotRelaunchIt(t *testing.T) {
	for _, tc := range []struct {
		name         string
		carriesWrite bool
		wantRequests int
	}{
		{"judge's own edit carries its request id", true, 1},
		{"control: the same edit with the writer dropped", false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, tasks, conn := newSeamService(t)
			card := seedSeamCard(t, tasks)
			requestID := seedLiveJudgeRequest(t, conn, card.ID)

			ctx := orchestrator.WithActor(context.Background(), orchestrator.ActorTask("cont-1"))
			if tc.carriesWrite {
				ctx = orchestrator.WithWriterCardRequestID(ctx, requestID)
			}

			if _, err := svc.UpdateTask(ctx, card.ID, UpdateTaskRequest{Description: "the judge's summary"}); err != nil {
				t.Fatalf("UpdateTask: %v", err)
			}
			if got := countCardRequests(t, conn, card.ID); got != tc.wantRequests {
				t.Fatalf("card_requests = %d, want %d", got, tc.wantRequests)
			}
		})
	}
}

// TestCardEditSeam_HumanEditQueuesAJudgeRun: the other half — an edit with no
// writer at all is a person changing the card, and must reach the trigger.
func TestCardEditSeam_HumanEditQueuesAJudgeRun(t *testing.T) {
	svc, tasks, conn := newSeamService(t)
	card := seedSeamCard(t, tasks)
	// A title is only editable before dispatch, so this half runs on a parked
	// card — the status the human-facing rename actually happens in.
	card.Status = orchestrator.TaskStatusParked
	if err := tasks.UpdateTask(card); err != nil {
		t.Fatalf("park the card: %v", err)
	}

	if _, err := svc.UpdateTask(context.Background(), card.ID, UpdateTaskRequest{Title: "renamed by a person"}); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if got := countCardRequests(t, conn, card.ID); got != 1 {
		t.Fatalf("card_requests = %d, want 1 (a human card edit must reach the trigger)", got)
	}
}

// TestCardEditSeam_IdentityLinkFollowsTheSameRule pins that the link path
// shares the guard rather than reimplementing it.
func TestCardEditSeam_IdentityLinkFollowsTheSameRule(t *testing.T) {
	svc, tasks, conn := newSeamService(t)
	card := seedSeamCard(t, tasks)
	requestID := seedLiveJudgeRequest(t, conn, card.ID)

	ctx := orchestrator.WithWriterCardRequestID(context.Background(), requestID)
	if err := svc.LinkIdentity(ctx, "proj-1", "jira:SEAM-1", card.ID); err != nil {
		t.Fatalf("LinkIdentity: %v", err)
	}
	if got := countCardRequests(t, conn, card.ID); got != 1 {
		t.Fatalf("card_requests = %d, want 1 (the live judge's own link must not relaunch it)", got)
	}
}
