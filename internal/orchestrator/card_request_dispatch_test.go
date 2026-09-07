package orchestrator_test

// Pins the automatic-dispatch claim path: ClaimQueuedCardRequestsForDispatch
// layers two extra guards on top of the existing ClaimQueuedCardRequests
// (card status re-check, force-release barrier) before delegating to it —
// both guards a plain ClaimQueuedCardRequests call has no way to express,
// since it never reads the card's own row.

import (
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestClaimQueuedCardRequestsForDispatch_Success(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", CauseID: "cause-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	def := orchestrator.CardRequestDefinition{CommandKey: "review", Label: "Run", Run: "echo hi"}
	primary, folded, err := orchestrator.ClaimQueuedCardRequestsForDispatch(d.Conn, cardID, "launcher-1", "review", def)
	if err != nil {
		t.Fatalf("ClaimQueuedCardRequestsForDispatch: %v", err)
	}
	if primary == nil || primary.ID != req.ID {
		t.Fatalf("primary = %+v, want the enqueued request", primary)
	}
	if primary.Status != orchestrator.CardRequestStatusLaunching {
		t.Errorf("primary.Status = %q, want launching", primary.Status)
	}
	if len(folded) != 0 {
		t.Errorf("folded = %v, want none for a single queued row", folded)
	}

	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusLaunching || got.LauncherJobID != "launcher-1" {
		t.Errorf("persisted row = %+v, want launching owned by launcher-1", got)
	}
}

// TestClaimQueuedCardRequestsForDispatch_FoldsExtraQueued pins that a second
// (and later) queued request for the same card folds into the promoted head
// — the same fold ClaimQueuedCardRequests already does, exercised for real
// through the auto-dispatch entry point rather than assumed.
func TestClaimQueuedCardRequestsForDispatch_FoldsExtraQueued(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	first := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", CauseID: "cause-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, first); err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	second := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", CauseID: "cause-2"}
	if err := orchestrator.CreateCardRequest(d.Conn, second); err != nil {
		t.Fatalf("enqueue second: %v", err)
	}

	def := orchestrator.CardRequestDefinition{CommandKey: "review", Label: "Run", Run: "echo hi"}
	primary, folded, err := orchestrator.ClaimQueuedCardRequestsForDispatch(d.Conn, cardID, "launcher-1", "review", def)
	if err != nil {
		t.Fatalf("ClaimQueuedCardRequestsForDispatch: %v", err)
	}
	if primary.ID != first.ID {
		t.Fatalf("primary = %q, want the OLDEST queued request %q", primary.ID, first.ID)
	}
	if len(folded) != 1 || folded[0].ID != second.ID {
		t.Fatalf("folded = %+v, want exactly the second request", folded)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, second.ID)
	if err != nil {
		t.Fatalf("GetCardRequest(second): %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusFolded || got.FoldedInto != first.ID {
		t.Errorf("second request = %+v, want folded into %q", got, first.ID)
	}
}

// TestClaimQueuedCardRequestsForDispatch_NoQueued pins the empty case.
func TestClaimQueuedCardRequestsForDispatch_NoQueued(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	def := orchestrator.CardRequestDefinition{CommandKey: "review", Label: "Run", Run: "echo hi"}
	_, _, err := orchestrator.ClaimQueuedCardRequestsForDispatch(d.Conn, cardID, "launcher-1", "review", def)
	if !errors.Is(err, orchestrator.ErrNoQueuedCardRequests) {
		t.Fatalf("err = %v, want ErrNoQueuedCardRequests", err)
	}
}

// TestClaimQueuedCardRequestsForDispatch_OccupiedSlotPropagates pins that an
// already launching/attached row for the card is reported the same way a
// bare ClaimQueuedCardRequests call would (ErrCardRequestSlotOccupied) — the
// two extra guards above it do not mask this.
func TestClaimQueuedCardRequestsForDispatch_OccupiedSlotPropagates(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	occupant := &orchestrator.CardRequest{CardID: cardID, CommandKey: "other", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "occupant-job"}
	if err := orchestrator.CreateCardRequest(d.Conn, occupant); err != nil {
		t.Fatalf("create occupant: %v", err)
	}
	// A queued row cannot coexist under the card's real status guard once an
	// occupant exists in production (the occupant IS what card_events would
	// have skipped via self-loop exclusion), but this test only needs to
	// prove the occupancy check itself, so insert one directly.
	if _, err := d.Conn.Exec(
		`INSERT INTO card_requests (id, card_id, command_key, status, created_at, updated_at) VALUES (?, ?, ?, 'queued', datetime('now'), datetime('now'))`,
		"queued-1", cardID, "review",
	); err != nil {
		t.Fatalf("raw enqueue: %v", err)
	}

	def := orchestrator.CardRequestDefinition{CommandKey: "review", Label: "Run", Run: "echo hi"}
	_, _, err := orchestrator.ClaimQueuedCardRequestsForDispatch(d.Conn, cardID, "launcher-2", "review", def)
	if !errors.Is(err, orchestrator.ErrCardRequestSlotOccupied) {
		t.Fatalf("err = %v, want ErrCardRequestSlotOccupied", err)
	}
}

// TestClaimQueuedCardRequestsForDispatch_CommandKeyChanged pins the guard
// against a peeked commandKey (resolved into a project.yaml command
// definition by the caller BEFORE this call opens its own transaction) going
// stale by the time this call actually reads the head row.
func TestClaimQueuedCardRequestsForDispatch_CommandKeyChanged(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", CauseID: "cause-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	def := orchestrator.CardRequestDefinition{CommandKey: "judge", Label: "Judge", Run: "echo judge"}
	_, _, err := orchestrator.ClaimQueuedCardRequestsForDispatch(d.Conn, cardID, "launcher-1", "judge", def)
	if !errors.Is(err, orchestrator.ErrCardRequestCommandKeyChanged) {
		t.Fatalf("err = %v, want ErrCardRequestCommandKeyChanged", err)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusQueued {
		t.Errorf("Status = %q, want the row left untouched (queued)", got.Status)
	}
}

// TestClaimQueuedCardRequestsForDispatch_CardNotEligible pins that automatic
// dispatch targets parked/working cards only: a card that reached
// done/dropped by claim time (e.g. suggestion_accept.go's answered{accept:
// complete} committing a queued row and the terminal transition in the SAME
// transaction) must never be launched, and every queued request for it is
// drained instead of left to loop forever.
func TestClaimQueuedCardRequestsForDispatch_CardNotEligible(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", CauseID: "cause-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	card, err := orchestrator.GetTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	card.Status = orchestrator.TaskStatusDone
	if err := orchestrator.UpdateTask(d.Conn, card); err != nil {
		t.Fatalf("UpdateTask(done): %v", err)
	}

	def := orchestrator.CardRequestDefinition{CommandKey: "review", Label: "Run", Run: "echo hi"}
	_, _, err = orchestrator.ClaimQueuedCardRequestsForDispatch(d.Conn, cardID, "launcher-1", "review", def)
	if !errors.Is(err, orchestrator.ErrCardNotEligibleForDispatch) {
		t.Fatalf("err = %v, want ErrCardNotEligibleForDispatch", err)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusFailed {
		t.Errorf("Status = %q, want failed (drained, not left queued forever)", got.Status)
	}
}

// TestClaimQueuedCardRequestsForDispatch_ForceReleaseBarrier pins that a
// card an operator force-released must not be auto-relaunched by a later
// write from the orphaned continuation, until a human operation clears the
// barrier. The queued row is left untouched (still queued) — not failed —
// since the barrier is meant to be temporary.
func TestClaimQueuedCardRequestsForDispatch_ForceReleaseBarrier(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", CauseID: "cause-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := orchestrator.SetCardForceReleaseBarrier(d.Conn, cardID); err != nil {
		t.Fatalf("SetCardForceReleaseBarrier: %v", err)
	}

	def := orchestrator.CardRequestDefinition{CommandKey: "review", Label: "Run", Run: "echo hi"}
	_, _, err := orchestrator.ClaimQueuedCardRequestsForDispatch(d.Conn, cardID, "launcher-1", "review", def)
	if !errors.Is(err, orchestrator.ErrCardForceReleaseBarrierActive) {
		t.Fatalf("err = %v, want ErrCardForceReleaseBarrierActive", err)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusQueued {
		t.Errorf("Status = %q, want left queued (barrier is temporary, not a failure)", got.Status)
	}
}

func TestCardForceReleaseBarrier_SetHasClearRoundTrip(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	if has, err := orchestrator.HasCardForceReleaseBarrier(d.Conn, cardID); err != nil || has {
		t.Fatalf("has(before set) = %v, %v; want false, nil", has, err)
	}
	if err := orchestrator.SetCardForceReleaseBarrier(d.Conn, cardID); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if has, err := orchestrator.HasCardForceReleaseBarrier(d.Conn, cardID); err != nil || !has {
		t.Fatalf("has(after set) = %v, %v; want true, nil", has, err)
	}
	// Setting twice must not error (ForceReleaseCardRequest could fire
	// against the same card more than once before it is ever cleared).
	if err := orchestrator.SetCardForceReleaseBarrier(d.Conn, cardID); err != nil {
		t.Fatalf("Set (again): %v", err)
	}
	if err := orchestrator.ClearCardForceReleaseBarrier(d.Conn, cardID); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if has, err := orchestrator.HasCardForceReleaseBarrier(d.Conn, cardID); err != nil || has {
		t.Fatalf("has(after clear) = %v, %v; want false, nil", has, err)
	}
	// Clearing an already-clear barrier must not error.
	if err := orchestrator.ClearCardForceReleaseBarrier(d.Conn, cardID); err != nil {
		t.Fatalf("Clear (already clear): %v", err)
	}
}

// TestForceReleaseCardRequest_SetsBarrier pins that force-release itself is
// what plants the barrier — the automatic-dispatch guard above has nothing
// to check without this.
func TestForceReleaseCardRequest_SetsBarrier(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := orchestrator.ForceReleaseCardRequest(d.Conn, req.ID, "operator stop"); err != nil {
		t.Fatalf("ForceReleaseCardRequest: %v", err)
	}
	if has, err := orchestrator.HasCardForceReleaseBarrier(d.Conn, cardID); err != nil || !has {
		t.Fatalf("has(after force release) = %v, %v; want true, nil", has, err)
	}
}

// TestRetryCardRequest_ClearsBarrier pins the third way to clear the
// barrier: an explicit Retry of the force-released request itself.
func TestRetryCardRequest_ClearsBarrier(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := orchestrator.ForceReleaseCardRequest(d.Conn, req.ID, "operator stop"); err != nil {
		t.Fatalf("ForceReleaseCardRequest: %v", err)
	}
	if err := orchestrator.RetryCardRequest(d.Conn, req.ID); err != nil {
		t.Fatalf("RetryCardRequest: %v", err)
	}
	if has, err := orchestrator.HasCardForceReleaseBarrier(d.Conn, cardID); err != nil || has {
		t.Fatalf("has(after retry) = %v, %v; want false, nil", has, err)
	}
}

func TestPeekOldestQueuedCardRequest(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	if _, _, err := orchestrator.PeekOldestQueuedCardRequest(d.Conn, cardID); !errors.Is(err, orchestrator.ErrNoQueuedCardRequests) {
		t.Fatalf("peek(empty) err = %v, want ErrNoQueuedCardRequests", err)
	}

	first := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review", CauseID: "cause-1"}
	if err := orchestrator.CreateCardRequest(d.Conn, first); err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	second := &orchestrator.CardRequest{CardID: cardID, CommandKey: "judge", CauseID: "cause-2"}
	if err := orchestrator.CreateCardRequest(d.Conn, second); err != nil {
		t.Fatalf("enqueue second: %v", err)
	}

	id, commandKey, err := orchestrator.PeekOldestQueuedCardRequest(d.Conn, cardID)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if id != first.ID || commandKey != "review" {
		t.Fatalf("peek = (%q, %q), want the OLDEST request (%q, review)", id, commandKey, first.ID)
	}
}

// TestTaskRepository_ClaimQueuedCardRequestsForDispatch_IneligibleDrainCommits
// pins that the ineligible-card drain (failAllQueuedCardRequests) actually
// COMMITS through TaskRepository's own transaction wrapper, not just when
// called directly against a raw *sql.DB (where every statement auto-commits
// on its own and would hide a wrapper bug like this). db.InTxDB rolls the
// whole transaction back on any non-nil returned error — a naive wrapper
// that let ErrCardNotEligibleForDispatch propagate as that error would undo
// its own drain, and this is the layer production code actually uses.
func TestTaskRepository_ClaimQueuedCardRequestsForDispatch_IneligibleDrainCommits(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	repo := orchestrator.NewTaskRepository(d.Conn)
	if err := repo.CreateCardRequest(&orchestrator.CardRequest{CardID: cardID, CommandKey: "review", CauseID: "cause-1"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	card, err := orchestrator.GetTask(d.Conn, cardID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	card.Status = orchestrator.TaskStatusDropped
	if err := orchestrator.UpdateTask(d.Conn, card); err != nil {
		t.Fatalf("UpdateTask(dropped): %v", err)
	}

	def := orchestrator.CardRequestDefinition{CommandKey: "review", Label: "Run", Run: "echo hi"}
	primary, folded, err := repo.ClaimQueuedCardRequestsForDispatch(cardID, "launcher-1", "review", def)
	if !errors.Is(err, orchestrator.ErrCardNotEligibleForDispatch) {
		t.Fatalf("err = %v, want ErrCardNotEligibleForDispatch", err)
	}
	if primary != nil || len(folded) != 0 {
		t.Fatalf("primary = %+v, folded = %v, want both empty", primary, folded)
	}

	rows, err := repo.ListCardRequestsByCard(cardID)
	if err != nil {
		t.Fatalf("ListCardRequestsByCard: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != orchestrator.CardRequestStatusFailed {
		t.Fatalf("rows = %+v, want exactly one FAILED row (the drain must commit despite the returned sentinel)", rows)
	}
}

func TestListCardIDsWithQueuedCardRequests(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardA := newTestCard(t, d, "proj-1", "card-a")
	cardB := newTestCard(t, d, "proj-1", "card-b")
	newTestCard(t, d, "proj-1", "card-c") // no queued requests — must not appear

	if err := orchestrator.CreateCardRequest(d.Conn, &orchestrator.CardRequest{CardID: cardA, CommandKey: "review"}); err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	if err := orchestrator.CreateCardRequest(d.Conn, &orchestrator.CardRequest{CardID: cardB, CommandKey: "review"}); err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	// An attached (non-queued) row for a card must not make it appear either.
	if err := orchestrator.CreateCardRequest(d.Conn, &orchestrator.CardRequest{CardID: cardB, CommandKey: "review", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "l-1"}); err != nil {
		t.Fatalf("enqueue B launching: %v", err)
	}

	got, err := orchestrator.ListCardIDsWithQueuedCardRequests(d.Conn)
	if err != nil {
		t.Fatalf("ListCardIDsWithQueuedCardRequests: %v", err)
	}
	want := map[string]bool{cardA: true, cardB: true}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want exactly %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected card id %q in result %v", id, got)
		}
	}
}
