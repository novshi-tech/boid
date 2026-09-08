package orchestrator_test

// A card's own action stream is not workspace input. Every reaction a card
// action is meant to cause goes through the card_events path
// (IngestCardEventRequest), which reaches the same decision-maker directly;
// routing the same actions into the signal inbox as well only asks the
// workspace's intake "does a card exist for this?" about a card that exists
// by construction.

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func listSignalsForWorkspace(t *testing.T, dbtx db.DBTX, workspaceID string) []*orchestrator.Signal {
	t.Helper()
	signals, err := orchestrator.ListSignals(dbtx, orchestrator.SignalFilter{WorkspaceID: workspaceID, State: orchestrator.SignalStateAll})
	if err != nil {
		t.Fatalf("ListSignals: %v", err)
	}
	return signals
}

// TestCreateAction_CardAction_WritesNoSignalRow covers every action type a
// card can carry, including the ones that DO wake a card_events command —
// none of them may reach the signal inbox.
func TestCreateAction_CardAction_WritesNoSignalRow(t *testing.T) {
	for _, actionType := range []string{
		"answered", "go", "child_dispatched", "child_closed", "noted", "attrs_set",
		orchestrator.ActionTypeCardCreated, orchestrator.ActionTypeCardEdited,
		orchestrator.ActionTypeIdentityLinked,
	} {
		t.Run(actionType, func(t *testing.T) {
			d := testutil.NewTestDB(t)
			seedProject(t, d.Conn, "proj-a", "ws-1")
			seedCardTask(t, d.Conn, "card-1", "proj-a")

			a := newAction("act-1", "card-1", actionType, orchestrator.ActorHuman)
			if err := orchestrator.CreateAction(context.Background(), d.Conn, a, nil); err != nil {
				t.Fatalf("CreateAction: %v", err)
			}
			if got := listSignalsForWorkspace(t, d.Conn, "ws-1"); len(got) != 0 {
				t.Fatalf("signals = %d, want 0 for a card action", len(got))
			}
		})
	}
}
