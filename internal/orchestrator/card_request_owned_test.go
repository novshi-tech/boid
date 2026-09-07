package orchestrator_test

// Pins orchestrator.AttachCardRequestOwned — the ownership-scoped attach
// that closes the TOCTOU plan doc docs/plans/card-next-step-and-timeline.md
// §10 flagged: a caller (BoidOpTaskCreate / BoidOpAgentStart) verifies
// row.LauncherJobID == its own job id via a separate, earlier GetCardRequest
// read, then attaches later — leaving a window where a force-release plus a
// DIFFERENT launcher's reclaim could move the row back to "launching" under
// a new owner in between, letting the stale caller's attach steal the new
// owner's slot. AttachCardRequestOwned re-asserts the caller's claimed
// owner directly in the UPDATE's WHERE clause instead.

import (
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestAttachCardRequestOwned_RejectsWhenLauncherJobIDDiffers(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-current"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Simulate the race: a force-release + a different launcher's reclaim
	// landed between the caller's own earlier ownership read and this call.
	// ForceReleaseCardRequest fails the row (terminal); RetryCardRequest
	// requeues it; the new launcher's claim then re-promotes it to
	// launching under a DIFFERENT launcher_job_id — the exact shape a
	// force-release-and-reclaim leaves behind.
	if _, err := orchestrator.ForceReleaseCardRequest(d.Conn, req.ID, "operator kill"); err != nil {
		t.Fatalf("ForceReleaseCardRequest: %v", err)
	}
	if err := orchestrator.RetryCardRequest(d.Conn, req.ID); err != nil {
		t.Fatalf("RetryCardRequest: %v", err)
	}
	if _, _, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, "job-new-owner", orchestrator.CardRequestDefinition{}); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	err := orchestrator.AttachCardRequestOwned(d.Conn, req.ID, "job-current", orchestrator.CardRequestTargetKindTask, "task-stale")
	if !errors.Is(err, orchestrator.ErrCardRequestOwnerMismatch) {
		t.Fatalf("err = %v, want ErrCardRequestOwnerMismatch", err)
	}

	got, gerr := orchestrator.GetCardRequest(d.Conn, req.ID)
	if gerr != nil {
		t.Fatalf("GetCardRequest: %v", gerr)
	}
	if got.LauncherJobID != "job-new-owner" || got.TargetID != "" {
		t.Errorf("row = %+v, want unchanged (still owned by job-new-owner, no target written by the rejected stale attach)", got)
	}
}

func TestAttachCardRequestOwned_SucceedsForTheCurrentOwner(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-current"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := orchestrator.AttachCardRequestOwned(d.Conn, req.ID, "job-current", orchestrator.CardRequestTargetKindTask, "task-1"); err != nil {
		t.Fatalf("AttachCardRequestOwned: %v", err)
	}

	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusAttached || got.TargetKind != orchestrator.CardRequestTargetKindTask || got.TargetID != "task-1" {
		t.Errorf("row = %+v, want attached/task/task-1", got)
	}
}

// TestAttachCardRequestOwned_RetryOfOwnAttachConverges pins that a retry of
// the SAME owner's own already-successful attach converges (via
// ErrCardRequestInvalidTransition, not ErrCardRequestOwnerMismatch) — the
// idempotent-retry case CreateTaskLinkedToCardRequest's re-read logic relies
// on must not be misclassified as an ownership violation.
func TestAttachCardRequestOwned_RetryOfOwnAttachConverges(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-current"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := orchestrator.AttachCardRequestOwned(d.Conn, req.ID, "job-current", orchestrator.CardRequestTargetKindTask, "task-1"); err != nil {
		t.Fatalf("first attach: %v", err)
	}

	err := orchestrator.AttachCardRequestOwned(d.Conn, req.ID, "job-current", orchestrator.CardRequestTargetKindTask, "task-1")
	if !errors.Is(err, orchestrator.ErrCardRequestInvalidTransition) {
		t.Fatalf("retry err = %v, want ErrCardRequestInvalidTransition (convergence path), not ErrCardRequestOwnerMismatch", err)
	}
	if errors.Is(err, orchestrator.ErrCardRequestOwnerMismatch) {
		t.Fatalf("retry err = %v must NOT also be ErrCardRequestOwnerMismatch — the caller still owns this row", err)
	}
}

func TestAttachCardRequestOwned_RequiresNonEmptyExpectedOwner(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-current"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := orchestrator.AttachCardRequestOwned(d.Conn, req.ID, "", orchestrator.CardRequestTargetKindTask, "task-1"); err == nil {
		t.Fatal("AttachCardRequestOwned(empty expected owner): expected an error, got nil")
	}
}
