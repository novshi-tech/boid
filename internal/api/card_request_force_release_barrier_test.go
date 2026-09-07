package api

// Pins that a card an operator force-released stays suppressed from
// automatic dispatch until one of three human operations touches it —
// RunCardCommandAsHuman, Go (reserveGoCardRequest), and RetryCardRequest.
// The orchestrator package tests (card_request_dispatch_test.go) pin the
// store-level round trip; these pin that the api-layer callers actually
// invoke it.

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestRunCardCommandAsHuman_ClearsForceReleaseBarrier(t *testing.T) {
	svc, _, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	repo := svc.CardRequests.(*orchestrator.TaskRepository)
	if err := repo.SetCardForceReleaseBarrier(card.ID); err != nil {
		t.Fatalf("SetCardForceReleaseBarrier: %v", err)
	}

	if _, err := svc.RunCardCommandAsHuman(context.Background(), card.ID, "review", ""); err != nil {
		t.Fatalf("RunCardCommandAsHuman: %v", err)
	}

	if has, err := repo.HasCardForceReleaseBarrier(card.ID); err != nil || has {
		t.Fatalf("HasCardForceReleaseBarrier = %v, %v; want false, nil after a human command", has, err)
	}
}

func TestReserveGoCardRequest_ClearsForceReleaseBarrier(t *testing.T) {
	svc, _, card := newCardCommandTestService(t, "proj-1", testCardMeta(map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	}))
	repo := svc.CardRequests.(*orchestrator.TaskRepository)
	if err := repo.SetCardForceReleaseBarrier(card.ID); err != nil {
		t.Fatalf("SetCardForceReleaseBarrier: %v", err)
	}

	if _, err := svc.reserveGoCardRequest(card.ID); err != nil {
		t.Fatalf("reserveGoCardRequest: %v", err)
	}

	if has, err := repo.HasCardForceReleaseBarrier(card.ID); err != nil || has {
		t.Fatalf("HasCardForceReleaseBarrier = %v, %v; want false, nil after Go", has, err)
	}
}
