package dispatcher

// Pins that Runner.Dispatch's tokenCtx construction copies
// spec.CardID/CardRequestID into sandbox.TokenContext.CardID/CardRequestID.
// Mirrors TestDispatch_SignalServiceConnector_ThreadedIntoTokenContext.

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestDispatch_CardContext_ThreadedIntoTokenContext(t *testing.T) {
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

	spec := &orchestrator.JobSpec{
		ProjectID:  "proj-1",
		Argv:       []string{"echo", "hi"},
		Kind:       orchestrator.JobKindExec,
		Visibility: orchestrator.Visibility{Writable: false},
		BuiltinPolicies: map[string]orchestrator.BuiltinPolicy{
			"boid": {AllowedOps: []string{orchestrator.OpBoidCardContext}},
		},
		CardID:        "card-1",
		CardRequestID: "req-1",
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if broker.calls != 1 {
		t.Fatalf("RegisterCommands calls = %d, want 1", broker.calls)
	}
	if broker.lastCtx.CardID != "card-1" {
		t.Errorf("TokenContext.CardID = %q, want %q", broker.lastCtx.CardID, "card-1")
	}
	if broker.lastCtx.CardRequestID != "req-1" {
		t.Errorf("TokenContext.CardRequestID = %q, want %q", broker.lastCtx.CardRequestID, "req-1")
	}

	// jobs.card_id/card_request_id must be persisted too — the in-memory
	// token registry does not survive a daemon restart, but this row does,
	// which is what lets the card_requests recovery scan reverse-lookup a
	// session's continuation job after one (see
	// internal/orchestrator/card_request_release.go's
	// RecoverLaunchingCardRequests).
	job, err := GetJob(d.Conn, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.CardID != "card-1" || job.CardRequestID != "req-1" {
		t.Errorf("job.CardID/CardRequestID = %q/%q, want card-1/req-1", job.CardID, job.CardRequestID)
	}
}

// TestDispatch_NoCardContext_TokenContextStaysEmpty pins the unchanged
// default: every ordinary job (CardID/CardRequestID left at their zero
// value — every job today) gets an empty TokenContext.CardID/CardRequestID,
// so `boid card context` stays unreachable-with-a-clear-error until a real
// producer sets them.
func TestDispatch_NoCardContext_TokenContextStaysEmpty(t *testing.T) {
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

	spec := &orchestrator.JobSpec{
		ProjectID:  "proj-1",
		Argv:       []string{"echo", "hi"},
		Kind:       orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{Writable: true},
		BuiltinPolicies: map[string]orchestrator.BuiltinPolicy{
			"boid": {AllowedOps: []string{orchestrator.OpBoidTaskCreate}},
		},
	}

	if _, err := r.Dispatch(context.Background(), spec, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if broker.lastCtx.CardID != "" || broker.lastCtx.CardRequestID != "" {
		t.Errorf("TokenContext.CardID/CardRequestID = %q/%q, want empty", broker.lastCtx.CardID, broker.lastCtx.CardRequestID)
	}
}

// TestDispatch_CallerSuppliedJobID_UsedVerbatim pins JobSpec.ID: a
// card-command launcher must be able to learn its own job id BEFORE
// Dispatch runs (to claim the card's execution slot atomically — see
// orchestrator.ClaimQueuedCardRequests), so a non-empty spec.ID must be used
// as-is instead of Dispatch generating a fresh uuid.
func TestDispatch_CallerSuppliedJobID_UsedVerbatim(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	r := &Runner{
		DB:         d.Conn,
		Projects:   orchestrator.DBProjectCatalog{DB: d.Conn},
		Backend:    &gwFakeBackend{},
		BoidBinary: "/boid",
	}

	const wantID = "11111111-1111-1111-1111-111111111111"
	spec := &orchestrator.JobSpec{
		ID:         wantID,
		ProjectID:  "proj-1",
		Argv:       []string{"echo", "hi"},
		Kind:       orchestrator.JobKindExec,
		Visibility: orchestrator.Visibility{Writable: false},
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if jobID != wantID {
		t.Fatalf("jobID = %q, want caller-supplied %q", jobID, wantID)
	}
	if _, err := GetJob(d.Conn, wantID); err != nil {
		t.Fatalf("GetJob(%q): %v", wantID, err)
	}
}

// TestDispatch_NoCallerSuppliedJobID_GeneratesFresh pins the unchanged
// default: every ordinary caller (JobSpec.ID left empty) still gets a fresh
// generated id, distinct across dispatches.
func TestDispatch_NoCallerSuppliedJobID_GeneratesFresh(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	r := &Runner{
		DB:         d.Conn,
		Projects:   orchestrator.DBProjectCatalog{DB: d.Conn},
		Backend:    &gwFakeBackend{},
		BoidBinary: "/boid",
	}

	spec := func() *orchestrator.JobSpec {
		return &orchestrator.JobSpec{
			ProjectID:  "proj-1",
			Argv:       []string{"echo", "hi"},
			Kind:       orchestrator.JobKindExec,
			Visibility: orchestrator.Visibility{Writable: false},
		}
	}

	id1, err := r.Dispatch(context.Background(), spec(), nil)
	if err != nil {
		t.Fatalf("Dispatch 1: %v", err)
	}
	id2, err := r.Dispatch(context.Background(), spec(), nil)
	if err != nil {
		t.Fatalf("Dispatch 2: %v", err)
	}
	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("want two distinct generated ids, got %q and %q", id1, id2)
	}
}

// TestDispatch_DuplicateCallerSuppliedJobID_RejectedCleanly pins the Opus-
// review fix: a caller-supplied spec.ID colliding with an EXISTING job must
// fail before ever registering any token under that id — otherwise the
// deferred cleanup-on-error path would call UnregisterJob(j.ID) and revoke
// the EXISTING (unrelated) job's tokens.
func TestDispatch_DuplicateCallerSuppliedJobID_RejectedCleanly(t *testing.T) {
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

	const dupeID = "22222222-2222-2222-2222-222222222222"
	first := &orchestrator.JobSpec{
		ID:         dupeID,
		ProjectID:  "proj-1",
		Argv:       []string{"echo", "first"},
		Kind:       orchestrator.JobKindExec,
		Visibility: orchestrator.Visibility{Writable: false},
		BuiltinPolicies: map[string]orchestrator.BuiltinPolicy{
			"boid": {AllowedOps: []string{orchestrator.OpBoidCardContext}},
		},
	}
	if _, err := r.Dispatch(context.Background(), first, nil); err != nil {
		t.Fatalf("first Dispatch: %v", err)
	}

	second := &orchestrator.JobSpec{
		ID:         dupeID,
		ProjectID:  "proj-1",
		Argv:       []string{"echo", "second"},
		Kind:       orchestrator.JobKindExec,
		Visibility: orchestrator.Visibility{Writable: false},
		BuiltinPolicies: map[string]orchestrator.BuiltinPolicy{
			"boid": {AllowedOps: []string{orchestrator.OpBoidCardContext}},
		},
	}
	if _, err := r.Dispatch(context.Background(), second, nil); err == nil {
		t.Fatal("second Dispatch with a duplicate id: want an error, got success")
	}

	// The first job's registered broker token must still be tracked — the
	// second dispatch's failure must not have unregistered it via
	// r.UnregisterJob(dupeID).
	r.tokenMu.Lock()
	_, stillTracked := r.jobTokens[dupeID]
	r.tokenMu.Unlock()
	if !stillTracked {
		t.Fatal("the first job's broker token was revoked by the second, colliding dispatch's error-cleanup path")
	}
}
