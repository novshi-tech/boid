package orchestrator_test

// TestListActiveCardRequestsByCardIDs_* pins PR-5c's batched read: the list
// row's activity state (docs/plans/card-next-step-and-timeline.md §5.5)
// needs "which cards have an active command, and which one" across a whole
// page of cards in one query rather than one per card.

import (
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestListActiveCardRequestsByCardIDs_OnePerCard(t *testing.T) {
	d := testutil.NewTestDB(t)
	card1 := newTestCard(t, d, "proj-1", "card-1")
	card2 := newTestCard(t, d, "proj-1", "card-2")

	launching := &orchestrator.CardRequest{
		CardID: card1, CommandKey: "discuss", Status: orchestrator.CardRequestStatusLaunching,
		LauncherJobID: "job-1", Launched: orchestrator.CardRequestDefinition{Label: "Discuss"},
	}
	if err := orchestrator.CreateCardRequest(d.Conn, launching); err != nil {
		t.Fatalf("create launching: %v", err)
	}
	queued := &orchestrator.CardRequest{CardID: card2, CommandKey: "sweep", Status: orchestrator.CardRequestStatusQueued}
	if err := orchestrator.CreateCardRequest(d.Conn, queued); err != nil {
		t.Fatalf("create queued: %v", err)
	}

	got, err := orchestrator.ListActiveCardRequestsByCardIDs(d.Conn, []string{card1, card2})
	if err != nil {
		t.Fatalf("ListActiveCardRequestsByCardIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2, got %+v", len(got), got)
	}
	if got[card1] == nil || got[card1].ID != launching.ID {
		t.Errorf("card1 = %+v, want the launching row", got[card1])
	}
	if got[card2] == nil || got[card2].ID != queued.ID {
		t.Errorf("card2 = %+v, want the queued row", got[card2])
	}
}

// The known PR-4c non-determinism: a human command claims launching directly
// (bypassing the claim/fold path) while an unrelated internal-event request
// for the SAME card is still queued. Both rows coexist — the list must show
// the launching one, not silently masked by the older queued row.
func TestListActiveCardRequestsByCardIDs_LaunchingWinsOverCoexistingQueued(t *testing.T) {
	d := testutil.NewTestDB(t)
	card := newTestCard(t, d, "proj-1", "card-1")

	queued := &orchestrator.CardRequest{CardID: card, CommandKey: "sweep", Status: orchestrator.CardRequestStatusQueued}
	if err := orchestrator.CreateCardRequest(d.Conn, queued); err != nil {
		t.Fatalf("create queued: %v", err)
	}
	launching := &orchestrator.CardRequest{
		CardID: card, CommandKey: "discuss", Status: orchestrator.CardRequestStatusLaunching,
		LauncherJobID: "job-1", Launched: orchestrator.CardRequestDefinition{Label: "Discuss"},
	}
	if err := orchestrator.CreateCardRequest(d.Conn, launching); err != nil {
		t.Fatalf("create launching: %v", err)
	}

	got, err := orchestrator.ListActiveCardRequestsByCardIDs(d.Conn, []string{card})
	if err != nil {
		t.Fatalf("ListActiveCardRequestsByCardIDs: %v", err)
	}
	if got[card] == nil || got[card].ID != launching.ID {
		t.Errorf("chosen = %+v, want the launching row (%q), not the older queued one", got[card], launching.ID)
	}
}

// A card outside the requested id set must never appear in the result — the
// list only ever wants activity for the page it is currently rendering.
func TestListActiveCardRequestsByCardIDs_ScopedToRequestedIDs(t *testing.T) {
	d := testutil.NewTestDB(t)
	inScope := newTestCard(t, d, "proj-1", "card-in")
	outOfScope := newTestCard(t, d, "proj-1", "card-out")

	for _, id := range []string{inScope, outOfScope} {
		req := &orchestrator.CardRequest{CardID: id, CommandKey: "discuss", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-" + id}
		if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
			t.Fatalf("create for %q: %v", id, err)
		}
	}

	got, err := orchestrator.ListActiveCardRequestsByCardIDs(d.Conn, []string{inScope})
	if err != nil {
		t.Fatalf("ListActiveCardRequestsByCardIDs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (out-of-scope card must not leak in), got %+v", len(got), got)
	}
	if _, ok := got[outOfScope]; ok {
		t.Errorf("out-of-scope card %q leaked into result: %+v", outOfScope, got)
	}
}

// Two same-rank (queued) rows must resolve in the SAME order this batch
// lookup and the detail page's own ListCardRequestsByCard would agree on
// (created_at ASC, id ASC) — otherwise the list and the detail page could
// point at two different "active" commands for the same card.
func TestListActiveCardRequestsByCardIDs_SameRankTieBreak_OldestWins(t *testing.T) {
	d := testutil.NewTestDB(t)
	card := newTestCard(t, d, "proj-1", "card-1")

	older := &orchestrator.CardRequest{CardID: card, CommandKey: "sweep", Status: orchestrator.CardRequestStatusQueued, CreatedAt: time.Now().UTC().Add(-time.Hour)}
	if err := orchestrator.CreateCardRequest(d.Conn, older); err != nil {
		t.Fatalf("create older: %v", err)
	}
	newer := &orchestrator.CardRequest{CardID: card, CommandKey: "discuss", Status: orchestrator.CardRequestStatusQueued}
	if err := orchestrator.CreateCardRequest(d.Conn, newer); err != nil {
		t.Fatalf("create newer: %v", err)
	}

	viaDetail, err := orchestrator.ListCardRequestsByCard(d.Conn, card)
	if err != nil {
		t.Fatalf("ListCardRequestsByCard: %v", err)
	}
	wantID := orchestrator.PickActiveCardRequest(viaDetail).ID
	if wantID != older.ID {
		t.Fatalf("test setup: detail-page pick = %q, want the older row %q", wantID, older.ID)
	}

	got, err := orchestrator.ListActiveCardRequestsByCardIDs(d.Conn, []string{card})
	if err != nil {
		t.Fatalf("ListActiveCardRequestsByCardIDs: %v", err)
	}
	if got[card] == nil || got[card].ID != wantID {
		t.Errorf("list-side pick = %+v, want the same row the detail page picks (%q)", got[card], wantID)
	}
}

func TestListActiveCardRequestsByCardIDs_EmptyInput_NoRowsNoError(t *testing.T) {
	d := testutil.NewTestDB(t)
	got, err := orchestrator.ListActiveCardRequestsByCardIDs(d.Conn, nil)
	if err != nil {
		t.Fatalf("ListActiveCardRequestsByCardIDs(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

// A card whose only rows are terminal (finished/failed) or the Go work slot
// must be absent — the list's command badge is about a currently active
// command, not history, and Go is the work slot rather than a command.
func TestListActiveCardRequestsByCardIDs_ExcludesTerminalAndGo(t *testing.T) {
	d := testutil.NewTestDB(t)
	card := newTestCard(t, d, "proj-1", "card-1")

	goReq := &orchestrator.CardRequest{
		CardID: card, CommandKey: orchestrator.CardRequestCommandKeyGo,
		Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-go",
	}
	if err := orchestrator.CreateCardRequest(d.Conn, goReq); err != nil {
		t.Fatalf("create go: %v", err)
	}

	got, err := orchestrator.ListActiveCardRequestsByCardIDs(d.Conn, []string{card})
	if err != nil {
		t.Fatalf("ListActiveCardRequestsByCardIDs: %v", err)
	}
	if _, ok := got[card]; ok {
		t.Errorf("Go's own row must not surface as a command activity, got %+v", got[card])
	}
}

func TestPickActiveCardRequest_PriorityOrder(t *testing.T) {
	queued := &orchestrator.CardRequest{ID: "q", Status: orchestrator.CardRequestStatusQueued}
	launching := &orchestrator.CardRequest{ID: "l", Status: orchestrator.CardRequestStatusLaunching}
	attached := &orchestrator.CardRequest{ID: "a", Status: orchestrator.CardRequestStatusAttached}

	if got := orchestrator.PickActiveCardRequest([]*orchestrator.CardRequest{queued, launching, attached}); got == nil || got.ID != "a" {
		t.Errorf("chosen = %+v, want attached", got)
	}
	if got := orchestrator.PickActiveCardRequest([]*orchestrator.CardRequest{queued, launching}); got == nil || got.ID != "l" {
		t.Errorf("chosen = %+v, want launching", got)
	}
	if got := orchestrator.PickActiveCardRequest([]*orchestrator.CardRequest{queued}); got == nil || got.ID != "q" {
		t.Errorf("chosen = %+v, want queued", got)
	}
}

func TestPickActiveCardRequest_SkipsGoAndTerminal(t *testing.T) {
	goReq := &orchestrator.CardRequest{ID: "go", CommandKey: orchestrator.CardRequestCommandKeyGo, Status: orchestrator.CardRequestStatusLaunching}
	finished := &orchestrator.CardRequest{ID: "f", Status: orchestrator.CardRequestStatusFinished}
	if got := orchestrator.PickActiveCardRequest([]*orchestrator.CardRequest{goReq, finished}); got != nil {
		t.Errorf("chosen = %+v, want nil (Go and terminal rows must never be picked)", got)
	}
}
