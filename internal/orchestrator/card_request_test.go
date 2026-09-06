package orchestrator_test

// card_requests 台帳の store 層テスト。

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// newTestCard creates a project (if not already present) and a parked card
// task, returning the card's task id. Shared by every test below.
func newTestCard(t *testing.T, d *db.DB, projectID, cardID string) string {
	t.Helper()
	if _, err := orchestrator.GetProject(d.Conn, projectID); err != nil {
		if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: projectID, WorkDir: "/tmp/" + projectID}); err != nil {
			t.Fatalf("create project: %v", err)
		}
	}
	card := &orchestrator.Task{ID: cardID, ProjectID: projectID, Type: orchestrator.TaskTypeCard, Card: &orchestrator.CardAttrs{}}
	if err := orchestrator.CreateTask(d.Conn, card); err != nil {
		t.Fatalf("create card: %v", err)
	}
	return card.ID
}

func TestCreateCardRequest_AssignsIDAndDefaultsToQueued(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}
	if req.ID == "" {
		t.Error("CreateCardRequest did not assign an ID")
	}
	if req.Status != orchestrator.CardRequestStatusQueued {
		t.Errorf("Status = %q, want %q", req.Status, orchestrator.CardRequestStatusQueued)
	}

	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.CardID != cardID || got.CommandKey != "review" {
		t.Errorf("GetCardRequest = %+v, want CardID=%q CommandKey=review", got, cardID)
	}
}

// TestCreateCardRequest_ActiveSlotUniqueAcrossRawInsert bypasses every Go
// helper and inserts directly via raw SQL, pinning that the single-active-
// slot invariant is a real DB constraint (idx_card_requests_active_unique)
// and not merely an application-level check that CreateCardRequest happens
// to perform. The task instructions explicitly call for this: "DB 制約が
// 本当に効いているかを、アプリ層を迂回して...確認する".
func TestCreateCardRequest_ActiveSlotUniqueAcrossRawInsert(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	now := time.Now().UTC()

	if _, err := d.Conn.Exec(
		`INSERT INTO card_requests (id, card_id, status, created_at, updated_at) VALUES (?, ?, 'launching', ?, ?)`,
		"req-a", cardID, now, now,
	); err != nil {
		t.Fatalf("raw insert first launching row: %v", err)
	}

	_, err := d.Conn.Exec(
		`INSERT INTO card_requests (id, card_id, status, created_at, updated_at) VALUES (?, ?, 'attached', ?, ?)`,
		"req-b", cardID, now, now,
	)
	if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("raw insert of a second launching/attached row for the same card: err = %v, want a UNIQUE constraint error", err)
	}
}

// TestCreateCardRequest_CauseIDUniqueAcrossRawInsert is the cause_id
// counterpart of TestCreateCardRequest_ActiveSlotUniqueAcrossRawInsert:
// idx_card_requests_cause_unique must reject a raw SQL insert too, not
// merely CreateCardRequest's own Go-level check.
func TestCreateCardRequest_CauseIDUniqueAcrossRawInsert(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	now := time.Now().UTC()

	if _, err := d.Conn.Exec(
		`INSERT INTO card_requests (id, card_id, cause_id, status, created_at, updated_at) VALUES (?, ?, 'signal-1', 'queued', ?, ?)`,
		"req-a", cardID, now, now,
	); err != nil {
		t.Fatalf("raw insert first row: %v", err)
	}

	_, err := d.Conn.Exec(
		`INSERT INTO card_requests (id, card_id, cause_id, status, created_at, updated_at) VALUES (?, ?, 'signal-1', 'queued', ?, ?)`,
		"req-b", cardID, now, now,
	)
	if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("raw insert of a second row with the same cause_id: err = %v, want a UNIQUE constraint error", err)
	}
}

func TestCreateCardRequest_LaunchingFastPath_ClaimsSlotAtomically(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	first := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching}
	if err := orchestrator.CreateCardRequest(d.Conn, first); err != nil {
		t.Fatalf("CreateCardRequest(first, launching): %v", err)
	}

	second := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching}
	err := orchestrator.CreateCardRequest(d.Conn, second)
	if !errors.Is(err, orchestrator.ErrCardRequestSlotOccupied) {
		t.Fatalf("CreateCardRequest(second, launching) = %v, want ErrCardRequestSlotOccupied", err)
	}

	if n, cerr := orchestrator.CountActiveCardRequests(d.Conn, cardID); cerr != nil || n != 1 {
		t.Fatalf("CountActiveCardRequests = (%d, %v), want (1, nil)", n, cerr)
	}
}

func TestCreateCardRequest_DuplicateCauseID_Rejected(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	first := &orchestrator.CardRequest{CardID: cardID, CauseID: "signal-42"}
	if err := orchestrator.CreateCardRequest(d.Conn, first); err != nil {
		t.Fatalf("CreateCardRequest(first): %v", err)
	}
	second := &orchestrator.CardRequest{CardID: cardID, CauseID: "signal-42"}
	err := orchestrator.CreateCardRequest(d.Conn, second)
	if !errors.Is(err, orchestrator.ErrCardRequestDuplicateCause) {
		t.Fatalf("CreateCardRequest(second, same cause_id) = %v, want ErrCardRequestDuplicateCause", err)
	}

	// A DIFFERENT card's request with the same cause_id must still be
	// rejected — cause_id dedup is table-wide (redelivery is per-event, not
	// per-card): two cards cannot both claim to be "the" handler of the
	// same external cause.
	otherCard := newTestCard(t, d, "proj-1", "card-2")
	third := &orchestrator.CardRequest{CardID: otherCard, CauseID: "signal-42"}
	if err := orchestrator.CreateCardRequest(d.Conn, third); !errors.Is(err, orchestrator.ErrCardRequestDuplicateCause) {
		t.Fatalf("CreateCardRequest(different card, same cause_id) = %v, want ErrCardRequestDuplicateCause", err)
	}

	// An empty cause_id (the "no cause" sentinel) never collides, no matter
	// how many requests use it.
	fourth := &orchestrator.CardRequest{CardID: cardID}
	fifth := &orchestrator.CardRequest{CardID: cardID}
	if err := orchestrator.CreateCardRequest(d.Conn, fourth); err != nil {
		t.Fatalf("CreateCardRequest(fourth, no cause): %v", err)
	}
	if err := orchestrator.CreateCardRequest(d.Conn, fifth); err != nil {
		t.Fatalf("CreateCardRequest(fifth, no cause): %v", err)
	}
}

func TestCardRequest_FullLifecycle_QueuedToFinished(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "review"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("CreateCardRequest: %v", err)
	}

	def := orchestrator.CardRequestDefinition{CommandKey: "review", Label: "Run", Run: "python3 scripts/card_review.py", Version: "v1"}
	primary, folded, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, def)
	if err != nil {
		t.Fatalf("ClaimQueuedCardRequests: %v", err)
	}
	if primary.ID != req.ID {
		t.Fatalf("primary.ID = %q, want %q", primary.ID, req.ID)
	}
	if len(folded) != 0 {
		t.Fatalf("folded = %+v, want empty (only one queued request)", folded)
	}
	if primary.Status != orchestrator.CardRequestStatusLaunching {
		t.Errorf("primary.Status = %q, want launching", primary.Status)
	}
	if primary.Launched != def {
		t.Errorf("primary.Launched = %+v, want %+v", primary.Launched, def)
	}

	if err := orchestrator.SetCardRequestLauncherJobID(d.Conn, req.ID, "job-1"); err != nil {
		t.Fatalf("SetCardRequestLauncherJobID: %v", err)
	}
	if err := orchestrator.AttachCardRequest(d.Conn, req.ID, orchestrator.CardRequestTargetKindTask, "task-1"); err != nil {
		t.Fatalf("AttachCardRequest: %v", err)
	}
	if err := orchestrator.FinishCardRequest(d.Conn, req.ID, "no further action"); err != nil {
		t.Fatalf("FinishCardRequest: %v", err)
	}

	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusFinished {
		t.Errorf("Status = %q, want finished", got.Status)
	}
	if got.LauncherJobID != "job-1" || got.TargetKind != orchestrator.CardRequestTargetKindTask || got.TargetID != "task-1" {
		t.Errorf("got = %+v, missing launcher_job_id/target", got)
	}
	if got.Result != "no further action" {
		t.Errorf("Result = %q, want %q", got.Result, "no further action")
	}

	// The slot must be free again.
	if n, cerr := orchestrator.CountActiveCardRequests(d.Conn, cardID); cerr != nil || n != 0 {
		t.Fatalf("CountActiveCardRequests after finish = (%d, %v), want (0, nil)", n, cerr)
	}
}

func TestClaimQueuedCardRequests_NoPending_ReturnsErrNoQueuedCardRequests(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	_, _, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, orchestrator.CardRequestDefinition{})
	if !errors.Is(err, orchestrator.ErrNoQueuedCardRequests) {
		t.Fatalf("ClaimQueuedCardRequests(no pending) = %v, want ErrNoQueuedCardRequests", err)
	}
}

func TestClaimQueuedCardRequests_SlotOccupied_Rejected(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	occupying := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching}
	if err := orchestrator.CreateCardRequest(d.Conn, occupying); err != nil {
		t.Fatalf("create occupying request: %v", err)
	}
	pending := &orchestrator.CardRequest{CardID: cardID}
	if err := orchestrator.CreateCardRequest(d.Conn, pending); err != nil {
		t.Fatalf("create pending request: %v", err)
	}

	_, _, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, orchestrator.CardRequestDefinition{})
	if !errors.Is(err, orchestrator.ErrCardRequestSlotOccupied) {
		t.Fatalf("ClaimQueuedCardRequests(slot occupied) = %v, want ErrCardRequestSlotOccupied", err)
	}

	// The queued request must be untouched — this failed claim made no
	// partial progress.
	got, err := orchestrator.GetCardRequest(d.Conn, pending.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusQueued {
		t.Errorf("pending.Status = %q, want still queued", got.Status)
	}
}

// TestClaimQueuedCardRequests_FoldsExactlyThePreClaimSnapshot pins that a
// claim's boundary is exactly its pre-claim queued snapshot (oldest
// promoted to primary, the rest folded into it) — and that a request
// created after the claim returns starts fresh as queued rather than being
// retroactively swept into that already-closed boundary.
func TestClaimQueuedCardRequests_FoldsExactlyThePreClaimSnapshot(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	var early []*orchestrator.CardRequest
	for i := 0; i < 3; i++ {
		r := &orchestrator.CardRequest{CardID: cardID}
		if err := orchestrator.CreateCardRequest(d.Conn, r); err != nil {
			t.Fatalf("create early request %d: %v", i, err)
		}
		early = append(early, r)
		time.Sleep(time.Millisecond) // force distinct created_at ordering
	}

	primary, folded, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, orchestrator.CardRequestDefinition{CommandKey: "review"})
	if err != nil {
		t.Fatalf("ClaimQueuedCardRequests: %v", err)
	}
	if primary.ID != early[0].ID {
		t.Errorf("primary.ID = %q, want oldest (%q)", primary.ID, early[0].ID)
	}
	if len(folded) != 2 {
		t.Fatalf("folded = %d requests, want 2", len(folded))
	}
	for _, f := range folded {
		if f.Status != orchestrator.CardRequestStatusFolded || f.FoldedInto != primary.ID {
			t.Errorf("folded request %+v: want status=folded folded_into=%q", f, primary.ID)
		}
	}

	// A request created AFTER the claim must not have been swept in.
	late := &orchestrator.CardRequest{CardID: cardID}
	if err := orchestrator.CreateCardRequest(d.Conn, late); err != nil {
		t.Fatalf("create late request: %v", err)
	}
	got, err := orchestrator.GetCardRequest(d.Conn, late.ID)
	if err != nil {
		t.Fatalf("GetCardRequest(late): %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusQueued {
		t.Errorf("late.Status = %q, want queued (excluded from the earlier boundary)", got.Status)
	}
}

// TestFinishCardRequest_ClosesFoldedAndAbsorbsOldFailures pins that
// finishing the primary closes every folded sibling from the same
// boundary, and separately absorbs any older failed request for the same
// card (a previous attempt's leftover).
func TestFinishCardRequest_ClosesFoldedAndAbsorbsOldFailures(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	// An old failed request from a previous, unrelated attempt.
	oldFailed := &orchestrator.CardRequest{CardID: cardID}
	if err := orchestrator.CreateCardRequest(d.Conn, oldFailed); err != nil {
		t.Fatalf("create old request: %v", err)
	}
	if err := db.InTxDB(d.Conn, func(tx db.DBTX) error {
		_, _, err := orchestrator.ClaimQueuedCardRequests(tx, cardID, orchestrator.CardRequestDefinition{})
		return err
	}); err != nil {
		t.Fatalf("claim old request: %v", err)
	}
	if err := orchestrator.FailCardRequest(d.Conn, oldFailed.ID, "launcher crashed"); err != nil {
		t.Fatalf("fail old request: %v", err)
	}

	// Two new requests queued together, claimed as one boundary.
	primaryReq := &orchestrator.CardRequest{CardID: cardID}
	siblingReq := &orchestrator.CardRequest{CardID: cardID}
	if err := orchestrator.CreateCardRequest(d.Conn, primaryReq); err != nil {
		t.Fatalf("create primary: %v", err)
	}
	time.Sleep(time.Millisecond)
	if err := orchestrator.CreateCardRequest(d.Conn, siblingReq); err != nil {
		t.Fatalf("create sibling: %v", err)
	}

	var primary *orchestrator.CardRequest
	if err := db.InTxDB(d.Conn, func(tx db.DBTX) error {
		var err error
		primary, _, err = orchestrator.ClaimQueuedCardRequests(tx, cardID, orchestrator.CardRequestDefinition{})
		return err
	}); err != nil {
		t.Fatalf("claim new boundary: %v", err)
	}
	if err := orchestrator.AttachCardRequest(d.Conn, primary.ID, orchestrator.CardRequestTargetKindTask, "task-1"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := orchestrator.FinishCardRequest(d.Conn, primary.ID, "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}

	all, err := orchestrator.ListCardRequestsByCard(d.Conn, cardID)
	if err != nil {
		t.Fatalf("ListCardRequestsByCard: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListCardRequestsByCard returned %d rows, want 3", len(all))
	}
	for _, r := range all {
		if r.Status != orchestrator.CardRequestStatusFinished {
			t.Errorf("request %q: Status = %q, want finished", r.ID, r.Status)
		}
	}
}

// TestFinishCardRequest_DoesNotAbsorbFailuresFromALaterBoundary pins that a
// request created (and later failed) AFTER the primary's own boundary was
// fixed is NOT swept up as "absorbed" by that earlier success — the
// primary never read it, so it must still surface as its own retryable
// failure, not get silently marked finished.
func TestFinishCardRequest_DoesNotAbsorbFailuresFromALaterBoundary(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	primaryReq := &orchestrator.CardRequest{CardID: cardID}
	if err := orchestrator.CreateCardRequest(d.Conn, primaryReq); err != nil {
		t.Fatalf("create primary: %v", err)
	}
	var primary *orchestrator.CardRequest
	if err := db.InTxDB(d.Conn, func(tx db.DBTX) error {
		var err error
		primary, _, err = orchestrator.ClaimQueuedCardRequests(tx, cardID, orchestrator.CardRequestDefinition{})
		return err
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := orchestrator.AttachCardRequest(d.Conn, primary.ID, orchestrator.CardRequestTargetKindTask, "task-1"); err != nil {
		t.Fatalf("attach: %v", err)
	}

	// Created and failed AFTER the primary's boundary was fixed. The
	// primary still occupies the card's slot (attached), so this new
	// request can only be queued — exactly the "its command definition
	// disappeared while waiting" case that fails a still-queued request.
	time.Sleep(time.Millisecond)
	later := &orchestrator.CardRequest{CardID: cardID}
	if err := orchestrator.CreateCardRequest(d.Conn, later); err != nil {
		t.Fatalf("create later: %v", err)
	}
	if err := orchestrator.FailCardRequest(d.Conn, later.ID, "unrelated failure"); err != nil {
		t.Fatalf("fail later: %v", err)
	}

	if err := orchestrator.FinishCardRequest(d.Conn, primary.ID, "done"); err != nil {
		t.Fatalf("finish primary: %v", err)
	}

	got, err := orchestrator.GetCardRequest(d.Conn, later.ID)
	if err != nil {
		t.Fatalf("GetCardRequest(later): %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusFailed {
		t.Errorf("later.Status = %q, want still failed (must not be absorbed by an earlier boundary's success)", got.Status)
	}
	if got.Error != "unrelated failure" {
		t.Errorf("later.Error = %q, want preserved", got.Error)
	}
}

// TestFailCardRequest_ReleasesFoldedRequestsBackToQueued pins the failure
// side of the boundary contract: if the primary of a batch fails, the
// requests folded into it are not silently lost — they return to queued so
// the NEXT claim reconsiders them.
func TestFailCardRequest_ReleasesFoldedRequestsBackToQueued(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	first := &orchestrator.CardRequest{CardID: cardID}
	second := &orchestrator.CardRequest{CardID: cardID}
	if err := orchestrator.CreateCardRequest(d.Conn, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	time.Sleep(time.Millisecond)
	if err := orchestrator.CreateCardRequest(d.Conn, second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	primary, folded, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, orchestrator.CardRequestDefinition{})
	if err != nil {
		t.Fatalf("ClaimQueuedCardRequests: %v", err)
	}
	if len(folded) != 1 {
		t.Fatalf("folded = %d, want 1", len(folded))
	}

	if err := orchestrator.FailCardRequest(d.Conn, primary.ID, "launcher failed to start"); err != nil {
		t.Fatalf("FailCardRequest: %v", err)
	}

	got, err := orchestrator.GetCardRequest(d.Conn, second.ID)
	if err != nil {
		t.Fatalf("GetCardRequest(second): %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusQueued || got.FoldedInto != "" {
		t.Errorf("second = %+v, want status=queued folded_into=\"\"", got)
	}

	primaryGot, err := orchestrator.GetCardRequest(d.Conn, primary.ID)
	if err != nil {
		t.Fatalf("GetCardRequest(primary): %v", err)
	}
	if primaryGot.Status != orchestrator.CardRequestStatusFailed || primaryGot.Error != "launcher failed to start" {
		t.Errorf("primary = %+v, want status=failed with recorded error", primaryGot)
	}

	// The slot is free again — a fresh claim must succeed and pick up the
	// released sibling.
	nextPrimary, _, err := orchestrator.ClaimQueuedCardRequests(d.Conn, cardID, orchestrator.CardRequestDefinition{})
	if err != nil {
		t.Fatalf("ClaimQueuedCardRequests after failure: %v", err)
	}
	if nextPrimary.ID != second.ID {
		t.Errorf("nextPrimary.ID = %q, want %q (the released sibling)", nextPrimary.ID, second.ID)
	}
}

// TestRetryCardRequest_RequeuesFailedRequest pins that Retry clears every
// trace of the previous attempt — error, launch-time snapshot, AND the
// continuation target/result — so a fresh claim never inherits a stale
// pointer to a dead task/session from the attempt that failed.
func TestRetryCardRequest_RequeuesFailedRequest(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := orchestrator.AttachCardRequest(d.Conn, req.ID, orchestrator.CardRequestTargetKindTask, "dead-task-1"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := orchestrator.FailCardRequest(d.Conn, req.ID, "boom"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if err := orchestrator.RetryCardRequest(d.Conn, req.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}

	got, err := orchestrator.GetCardRequest(d.Conn, req.ID)
	if err != nil {
		t.Fatalf("GetCardRequest: %v", err)
	}
	if got.Status != orchestrator.CardRequestStatusQueued {
		t.Errorf("Status = %q, want queued", got.Status)
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want cleared", got.Error)
	}
	if got.TargetKind != "" || got.TargetID != "" {
		t.Errorf("TargetKind/TargetID = %q/%q, want both cleared (stale pointer to the dead attempt's continuation)", got.TargetKind, got.TargetID)
	}

	// Retrying anything but a failed request is rejected.
	if err := orchestrator.RetryCardRequest(d.Conn, req.ID); !errors.Is(err, orchestrator.ErrCardRequestInvalidTransition) {
		t.Errorf("RetryCardRequest(already queued) = %v, want ErrCardRequestInvalidTransition", err)
	}
}

func TestFinishCardRequest_RejectsNonAttached(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := orchestrator.FinishCardRequest(d.Conn, req.ID, "premature"); !errors.Is(err, orchestrator.ErrCardRequestInvalidTransition) {
		t.Fatalf("FinishCardRequest(queued) = %v, want ErrCardRequestInvalidTransition", err)
	}
}

func TestCreateCardRequest_RejectsEmptyCardID(t *testing.T) {
	d := testutil.NewTestDB(t)
	err := orchestrator.CreateCardRequest(d.Conn, &orchestrator.CardRequest{})
	if err == nil {
		t.Fatal("CreateCardRequest with empty CardID: expected an error, got nil")
	}
}

func TestCreateCardRequest_RejectsInvalidStartingStatus(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	for _, status := range []orchestrator.CardRequestStatus{
		orchestrator.CardRequestStatusAttached,
		orchestrator.CardRequestStatusFolded,
		orchestrator.CardRequestStatusFinished,
		orchestrator.CardRequestStatusFailed,
	} {
		err := orchestrator.CreateCardRequest(d.Conn, &orchestrator.CardRequest{CardID: cardID, Status: status})
		if err == nil {
			t.Errorf("CreateCardRequest(status=%q): expected an error, got nil", status)
		}
	}
}

func TestSetCardRequestLauncherJobID_RejectsNonLaunching(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := orchestrator.SetCardRequestLauncherJobID(d.Conn, req.ID, "job-1"); !errors.Is(err, orchestrator.ErrCardRequestInvalidTransition) {
		t.Fatalf("SetCardRequestLauncherJobID(queued) = %v, want ErrCardRequestInvalidTransition", err)
	}
	if err := orchestrator.SetCardRequestLauncherJobID(d.Conn, "does-not-exist", "job-1"); !errors.Is(err, orchestrator.ErrCardRequestNotFound) {
		t.Fatalf("SetCardRequestLauncherJobID(missing id) = %v, want ErrCardRequestNotFound", err)
	}
}

func TestAttachCardRequest_ValidatesArguments(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := orchestrator.AttachCardRequest(d.Conn, req.ID, "bogus-kind", "task-1"); err == nil {
		t.Error("AttachCardRequest(invalid target kind): expected an error, got nil")
	}
	if err := orchestrator.AttachCardRequest(d.Conn, req.ID, orchestrator.CardRequestTargetKindTask, ""); err == nil {
		t.Error("AttachCardRequest(empty target id): expected an error, got nil")
	}
	if err := orchestrator.AttachCardRequest(d.Conn, "does-not-exist", orchestrator.CardRequestTargetKindTask, "task-1"); !errors.Is(err, orchestrator.ErrCardRequestNotFound) {
		t.Errorf("AttachCardRequest(missing id) = %v, want ErrCardRequestNotFound", err)
	}
}

func TestAttachCardRequest_RejectsDoubleAttach(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID, Status: orchestrator.CardRequestStatusLaunching}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := orchestrator.AttachCardRequest(d.Conn, req.ID, orchestrator.CardRequestTargetKindTask, "task-1"); err != nil {
		t.Fatalf("first attach: %v", err)
	}
	err := orchestrator.AttachCardRequest(d.Conn, req.ID, orchestrator.CardRequestTargetKindTask, "task-2")
	if !errors.Is(err, orchestrator.ErrCardRequestInvalidTransition) {
		t.Fatalf("second attach = %v, want ErrCardRequestInvalidTransition", err)
	}
	// The original target must survive the rejected second attach.
	got, gerr := orchestrator.GetCardRequest(d.Conn, req.ID)
	if gerr != nil {
		t.Fatalf("GetCardRequest: %v", gerr)
	}
	if got.TargetID != "task-1" {
		t.Errorf("TargetID = %q, want unchanged %q", got.TargetID, "task-1")
	}
}

func TestGetCardRequest_NotFound(t *testing.T) {
	d := testutil.NewTestDB(t)
	_, err := orchestrator.GetCardRequest(d.Conn, "does-not-exist")
	if !errors.Is(err, orchestrator.ErrCardRequestNotFound) {
		t.Fatalf("GetCardRequest(missing) = %v, want ErrCardRequestNotFound", err)
	}
}

func TestGCCardRequests_KeepsActiveDeletesOldTerminal(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)

	mustInsert := func(id, status string, updatedAt time.Time) {
		t.Helper()
		if _, err := d.Conn.Exec(
			`INSERT INTO card_requests (id, card_id, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			id, cardID, status, updatedAt, updatedAt,
		); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	mustInsert("req-queued-old", "queued", old)
	mustInsert("req-finished-old", "finished", old)
	mustInsert("req-failed-old", "failed", old)
	mustInsert("req-finished-recent", "finished", time.Now().UTC())

	n, err := orchestrator.GCCardRequests(d.Conn, 30*24*time.Hour, false)
	if err != nil {
		t.Fatalf("GCCardRequests: %v", err)
	}
	if n != 2 {
		t.Fatalf("GCCardRequests deleted %d rows, want 2 (the two old terminal rows)", n)
	}

	for _, id := range []string{"req-queued-old", "req-finished-recent"} {
		if _, err := orchestrator.GetCardRequest(d.Conn, id); err != nil {
			t.Errorf("GetCardRequest(%q) after GC: %v, want it to still exist", id, err)
		}
	}
	for _, id := range []string{"req-finished-old", "req-failed-old"} {
		if _, err := orchestrator.GetCardRequest(d.Conn, id); !errors.Is(err, orchestrator.ErrCardRequestNotFound) {
			t.Errorf("GetCardRequest(%q) after GC: %v, want ErrCardRequestNotFound", id, err)
		}
	}
}

func TestGCCardRequests_LaunchingNeverDeletedRegardlessOfAge(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	old := time.Now().UTC().Add(-365 * 24 * time.Hour)

	if _, err := d.Conn.Exec(
		`INSERT INTO card_requests (id, card_id, status, created_at, updated_at) VALUES ('req-launching-ancient', ?, 'launching', ?, ?)`,
		cardID, old, old,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if _, err := orchestrator.GCCardRequests(d.Conn, 30*24*time.Hour, false); err != nil {
		t.Fatalf("GCCardRequests: %v", err)
	}
	if _, err := orchestrator.GetCardRequest(d.Conn, "req-launching-ancient"); err != nil {
		t.Errorf("GetCardRequest(launching) after GC: %v, want it to still exist regardless of age", err)
	}
}

// TestGC_CardRequestsCleanup_ViaTaskGCStore pins that TaskGCStore.GC (the
// path `boid gc` / POST /api/gc actually calls) purges card_requests in the
// same pass as tasks/jobs/actions/trigger_runs/signals, reporting the count
// on GCResult.CardRequests — same posture as
// TestGC_TriggerRunsCleanup_ViaTaskGCStore.
func TestGC_CardRequestsCleanup_ViaTaskGCStore(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")
	old := time.Now().UTC().Add(-60 * 24 * time.Hour)
	if _, err := d.Conn.Exec(
		`INSERT INTO card_requests (id, card_id, status, created_at, updated_at) VALUES ('req-old', ?, 'finished', ?, ?)`,
		cardID, old, old,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	gcStore := orchestrator.NewTaskGCStore(d.Conn)
	result, err := gcStore.GC(30*24*time.Hour, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.CardRequests != 1 {
		t.Fatalf("result.CardRequests = %d, want 1", result.CardRequests)
	}
	if _, err := orchestrator.GetCardRequest(d.Conn, "req-old"); !errors.Is(err, orchestrator.ErrCardRequestNotFound) {
		t.Fatalf("GetCardRequest(req-old) after gc = %v, want ErrCardRequestNotFound", err)
	}
}

func TestCardRequest_CardTaskCascadeDeletesRequests(t *testing.T) {
	d := testutil.NewTestDB(t)
	cardID := newTestCard(t, d, "proj-1", "card-1")

	req := &orchestrator.CardRequest{CardID: cardID}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := orchestrator.DeleteTask(d.Conn, cardID); err != nil {
		t.Fatalf("delete card task: %v", err)
	}

	if _, err := orchestrator.GetCardRequest(d.Conn, req.ID); !errors.Is(err, orchestrator.ErrCardRequestNotFound) {
		t.Fatalf("GetCardRequest after card deletion = %v, want ErrCardRequestNotFound (cascade)", err)
	}
}
