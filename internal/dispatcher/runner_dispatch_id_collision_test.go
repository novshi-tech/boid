package dispatcher

// Pins Runner.Dispatch's token-cleanup defer
// (`if dispatchErr != nil { r.UnregisterJob(j.ID) } `) running only AFTER
// CreateJob succeeds. A caller (the card-command launcher, acceptGo) can
// supply spec.ID up front, so a swallowed GetJob error on the duplicate-id
// pre-check (or a genuine race) can let Dispatch reach CreateJob with an id
// that already belongs to another, already-dispatched job. CreateJob's
// UNIQUE constraint then fails — if the cleanup defer were registered
// before CreateJob ran, it would call r.UnregisterJob(j.ID) and revoke the
// OTHER job's live broker token instead of this call's own (which
// registered none). Registering the defer only after CreateJob succeeds
// means a failing CreateJob never unregisters anything this call didn't
// itself just create.
//
// r.idCheckForTest is swapped out here to simulate the "GetJob's error was
// swallowed" trigger deterministically, without needing a real concurrent
// race to land in the same narrow window.

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestDispatch_CreateJobFailsOnIDCollision_DoesNotRevokeTheExistingJobsToken(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	broker := &recordingBroker{}
	r := &Runner{
		DB:         d.Conn,
		Projects:   orchestrator.DBProjectCatalog{DB: d.Conn},
		Backend:    &gwFakeBackend{},
		BoidBinary: "/boid",
		Broker:     broker,
	}

	const sharedID = "shared-job-id"
	winnerSpec := &orchestrator.JobSpec{
		ID:        sharedID,
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindExec,
		BuiltinPolicies: map[string]orchestrator.BuiltinPolicy{
			"boid": {AllowedOps: []string{orchestrator.OpBoidCardContext}},
		},
	}
	winnerJobID, err := r.Dispatch(context.Background(), winnerSpec, nil)
	if err != nil {
		t.Fatalf("winner Dispatch: %v", err)
	}
	if winnerJobID != sharedID {
		t.Fatalf("winnerJobID = %q, want %q", winnerJobID, sharedID)
	}

	r.tokenMu.Lock()
	winnerToken, tracked := r.jobTokens[sharedID]
	r.tokenMu.Unlock()
	if !tracked || winnerToken == "" {
		t.Fatalf("winner job's broker token was not tracked after a successful Dispatch")
	}

	// Simulate a swallowed GetJob error (or a race losing the interleaving)
	// on the second call's duplicate-id pre-check: it falsely reports
	// "no such job", so Dispatch proceeds to reuse sharedID as j.ID and
	// hits the real UNIQUE constraint at CreateJob.
	r.idCheckForTest = func(dbtx db.DBTX, id string) (*Job, error) {
		return nil, nil
	}

	loserSpec := &orchestrator.JobSpec{
		ID:        sharedID,
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindExec,
	}
	if _, err := r.Dispatch(context.Background(), loserSpec, nil); err == nil {
		t.Fatal("loser Dispatch: want an error (CreateJob's UNIQUE constraint on the reused id)")
	}

	if broker.unregisterCalls != 0 {
		t.Errorf("broker.UnregisterCommandToken calls = %d, want 0 — the loser's failed CreateJob must not revoke the winner's token", broker.unregisterCalls)
	}
	r.tokenMu.Lock()
	_, stillTracked := r.jobTokens[sharedID]
	r.tokenMu.Unlock()
	if !stillTracked {
		t.Error("winner job's broker token was removed by the loser's failed Dispatch")
	}
}
