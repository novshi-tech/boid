package orchestrator_test

import (
	"context"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// TestCardRequestLifecycleLoop_RunStartupRecovery_ReattachesFoundContinuation
// pins that the loop's startup hook is a thin wrapper over
// RecoverLaunchingCardRequests, not a no-op placeholder.
func TestCardRequestLifecycleLoop_RunStartupRecovery_ReattachesFoundContinuation(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-crashed"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	insertTestJob(t, d, "launcher-crashed", "proj-1", "hook", "failed", req.ID)
	insertTestJob(t, d, "session-job", "proj-1", "session", "running", req.ID)

	loop := &orchestrator.CardRequestLifecycleLoop{DB: d.Conn, Interval: time.Hour, InitialDelay: time.Hour}
	loop.RunStartupRecovery()

	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusAttached || got.TargetID != "session-job" {
		t.Fatalf("got = %+v, want attached to session-job", got)
	}
}

// TestCardRequestLifecycleLoop_Run_PeriodicallyReleasesTerminatedSlots pins
// that Run's ticker actually drives ReconcileCardRequestSlots repeatedly,
// not just once — a slot that only becomes releasable AFTER the loop starts
// still gets released on a later tick.
func TestCardRequestLifecycleLoop_Run_PeriodicallyReleasesTerminatedSlots(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	liveTask := newTestExecutionTask(t, d, "task-1", "proj-1", orchestrator.TaskStatusExecuting)
	req := attachedCardRequest(t, d, cardID, "launcher-1", orchestrator.CardRequestTargetKindTask, liveTask)

	loop := &orchestrator.CardRequestLifecycleLoop{DB: d.Conn, Interval: 10 * time.Millisecond, InitialDelay: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	// The task is still executing at loop start — nothing should release yet.
	time.Sleep(20 * time.Millisecond)
	if got, err := orchestrator.GetCardRequest(d.Conn, req.ID); err != nil || got.Status != orchestrator.CardRequestStatusAttached {
		t.Fatalf("GetCardRequest before task completion = %+v, %v, want still attached", got, err)
	}

	task, err := orchestrator.GetTask(d.Conn, liveTask)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	task.Status = orchestrator.TaskStatusDone
	if err := orchestrator.UpdateTask(d.Conn, task); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
		if err != nil {
			t.Fatalf("GetCardRequest: %v", err)
		}
		if got.Status == orchestrator.CardRequestStatusFinished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("card request never released after task completion, still %q", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
