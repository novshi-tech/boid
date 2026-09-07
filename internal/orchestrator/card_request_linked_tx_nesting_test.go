package orchestrator_test

// Pins that TaskRepository.CreateTaskLinkedToCardRequest, when bound to an
// already-open transaction (its db.DBTX is a *sql.Tx, not a *sql.DB), does
// NOT try to open a second nested transaction — so calling it from inside an
// outer transaction does not deadlock under SetMaxOpenConns(1), which only
// ever hands out one connection.

import (
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestCreateTaskLinkedToCardRequest_TxBoundRepo_DoesNotDeadlockInAnOuterTx(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}

	var createdID string
	done := make(chan error, 1)
	go func() {
		done <- db.InTxDB(d.Conn, func(tx db.DBTX) error {
			repo := orchestrator.NewTaskRepository(tx)
			task := &orchestrator.Task{ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "executor"}}
			if err := repo.CreateTaskLinkedToCardRequest(task, req.ID, "launcher-1"); err != nil {
				return err
			}
			// Also exercise a SECOND call against the same tx-bound repo
			// inside the same outer tx — proves the connection isn't
			// exhausted by the first call either.
			if _, err := repo.GetTask(task.ID); err != nil {
				return err
			}
			createdID = task.ID
			return nil
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CreateTaskLinkedToCardRequest inside an outer tx: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deadlocked calling CreateTaskLinkedToCardRequest with a tx-bound repo inside an outer transaction (SetMaxOpenConns(1))")
	}

	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusAttached || got.TargetID != createdID {
		t.Errorf("card request = %+v, want attached to %q", got, createdID)
	}
}
